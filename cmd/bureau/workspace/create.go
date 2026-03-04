// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package workspace

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/pipeline"
	"github.com/bureau-foundation/bureau/cmd/bureau/ticket"
	"github.com/bureau-foundation/bureau/lib/credential"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/workspace"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/templatedef"
	"github.com/bureau-foundation/bureau/messaging"
)

// createParams holds the parameters for the workspace create command.
// Alias is positional in CLI mode (args[0]) and a named property in
// JSON/MCP mode.
type createParams struct {
	cli.SessionConfig
	cli.PrincipalOverrides
	cli.JSONOutput
	Alias      string   `json:"alias"       desc:"workspace alias (e.g. iree/amdgpu/inference)" required:"true"`
	Machine    string   `json:"machine"     flag:"machine"     desc:"fleet-scoped machine localpart (e.g., bureau/fleet/prod/machine/workstation; use 'local' to auto-detect)"`
	Template   string   `json:"template"    flag:"template"    desc:"sandbox template ref for agent principals (required, e.g., bureau/template:base)"`
	Param      []string `json:"param"       flag:"param"       desc:"key=value parameter (repeatable; recognized: repository, branch)"`
	ServerName string   `json:"server_name" flag:"server-name" desc:"Matrix server name (auto-detected from machine.conf)"`
	AgentCount int      `json:"agent_count" flag:"agent-count" desc:"number of agent principals to create" default:"1"`
	DevTeam    string   `json:"dev_team"    flag:"dev-team"    desc:"dev team room alias (e.g., #iree/dev:bureau.local); defaults to #<namespace>/dev:<server>"`
}

// CreateParams holds the parameters for workspace creation. Callers that
// use the programmatic API pass these directly; CLI callers build them
// from flags and positional arguments.
type CreateParams struct {
	// Alias is the workspace alias (e.g., "iree/amdgpu/inference").
	// Must have at least two segments: project/worktree-path.
	Alias string

	// Machine is the typed machine reference within the fleet. The
	// server name is derived from this ref. Must be the resolved
	// machine, not "local" (the CLI wrapper handles that resolution).
	Machine ref.Machine

	// Template is the sandbox template ref for agent principals
	// (e.g., "bureau/template:base").
	Template string

	// Params holds key-value parameters like "repository" and "branch".
	Params map[string]string

	// AgentCount is the number of agent principals to create.
	AgentCount int

	// DevTeam overrides the dev team room alias for this workspace
	// (e.g., "#iree/dev:bureau.local"). When empty, defaults to the
	// namespace convention: #<project>/dev:<server>.
	DevTeam string

	// Client is an unauthenticated Matrix client for account registration.
	// Required for credential provisioning.
	Client *messaging.Client

	// RegistrationToken is the homeserver registration token for creating
	// principal accounts. Read from the credential file.
	RegistrationToken *secret.Buffer

	// HomeserverURL is included in each principal's encrypted credential
	// bundle so the proxy knows which homeserver to connect to.
	HomeserverURL string

	// MachineRoomID is the fleet-scoped machine room where the machine's
	// age public key is published. Required for credential encryption.
	MachineRoomID ref.RoomID

	// ExtraCredentials are additional key-value pairs included in each
	// agent principal's encrypted credential bundle (e.g., API keys).
	// Setup and teardown principals do not receive extra credentials.
	ExtraCredentials map[string]string

	// ExtraEnvironmentVariables are merged into the template's environment
	// variables for each agent principal instance. Setup and teardown
	// principals do not receive these.
	ExtraEnvironmentVariables map[string]string

	// CommandOverride replaces the template's Command for each agent
	// principal. When nil, the template's Command is used. Setup and
	// teardown principals are not affected.
	CommandOverride []string

	// EnvironmentOverride replaces the template's Environment (Nix store
	// path) for each agent principal. When empty, the template's
	// Environment is used. Setup and teardown principals are not affected.
	EnvironmentOverride string

	// RequiredServicesOverride replaces the template's RequiredServices
	// for each agent principal. When nil, the template's RequiredServices
	// are used. Setup and teardown principals are not affected.
	RequiredServicesOverride []string

	// SecretsOverride replaces the template's Secrets for each agent
	// principal. When nil, the template's Secrets are used. Setup and
	// teardown principals are not affected.
	SecretsOverride []schema.SecretBinding
}

// CreateResult holds the result of workspace creation.
type CreateResult struct {
	Alias      string     `json:"alias"                desc:"workspace alias"`
	RoomAlias  string     `json:"room_alias"           desc:"full Matrix room alias"`
	RoomID     ref.RoomID `json:"room_id"              desc:"Matrix room ID"`
	Project    string     `json:"project"              desc:"project name"`
	Repository string     `json:"repository,omitempty" desc:"git repository URL"`
	Branch     string     `json:"branch,omitempty"     desc:"git branch checked out"`
	Machine    string     `json:"machine"              desc:"hosting machine name"`
	Principals []string   `json:"principals"           desc:"created principal localparts"`
}

func createCommand() *cli.Command {
	var params createParams

	return &cli.Command{
		Name:    "create",
		Summary: "Create a new workspace",
		Description: `Create a workspace by setting up the Matrix room, publishing state
events, and configuring principals on the target machine. The daemon
picks up the configuration via /sync and spawns a setup principal to
clone the repo, create worktrees, and prepare the workspace. Agent
principals start only after the setup principal publishes
m.bureau.workspace with status "active".

The alias becomes both the Matrix room alias and filesystem path:

  bureau workspace create iree/amdgpu/inference \
    --machine bureau/fleet/prod/machine/workstation \
    --template bureau/template:base \
    --param repository=https://github.com/iree-org/iree.git

creates room #iree/amdgpu/inference:bureau.local, publishes project
config, adds setup + agent principals to the workstation's MachineConfig,
and invites the workstation daemon to the workspace room.

The first segment of the alias is the project name (e.g., "iree").
Everything after the first "/" is the worktree path within the project.
All worktrees in a project share a single bare git object store at
/var/bureau/workspace/<project>/.bare/.`,
		Usage: "bureau workspace create <alias> --machine <machine> --template <ref> [flags]",
		Examples: []cli.Example{
			{
				Description: "Create a workspace with a git repository",
				Command:     "bureau workspace create iree/amdgpu/inference --machine bureau/fleet/prod/machine/workstation --template bureau/template:base --param repository=https://github.com/iree-org/iree.git --credential-file ./creds",
			},
			{
				Description: "Create a workspace with multiple agents",
				Command:     "bureau workspace create iree/amdgpu/inference --machine bureau/fleet/prod/machine/workstation --template bureau/template:base --param repository=https://github.com/iree-org/iree.git --agent-count 3 --credential-file ./creds",
			},
			{
				Description: "Create a workspace on a specific branch",
				Command:     "bureau workspace create iree/amdgpu/inference --machine bureau/fleet/prod/machine/workstation --template bureau/template:base --param repository=https://github.com/iree-org/iree.git --param branch=develop --credential-file ./creds",
			},
			{
				Description: "Create a workspace on the local machine (auto-detect identity)",
				Command:     "bureau workspace create iree/amdgpu/inference --machine local --template bureau/template:base --param repository=https://github.com/iree-org/iree.git --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &CreateResult{} },
		RequiredGrants: []string{"command/workspace/create"},
		Annotations:    cli.Create(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			// In CLI mode, alias comes as a positional argument.
			// In JSON/MCP mode, it's populated from the JSON input.
			if len(args) == 1 {
				params.Alias = args[0]
			} else if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}
			if params.Alias == "" {
				return cli.Validation("workspace alias is required\n\nUsage: bureau workspace create <alias> --machine <machine> --template <ref> [flags]")
			}
			if params.Machine == "" {
				return cli.Validation("--machine is required")
			}
			if params.Template == "" {
				return cli.Validation("--template is required")
			}
			if params.AgentCount < 0 {
				return cli.Validation("--agent-count must be non-negative, got %d", params.AgentCount)
			}

			params.ServerName = cli.ResolveServerName(params.ServerName)

			return runCreate(ctx, logger, params)
		},
	}
}

// runCreate is the CLI wrapper: resolves "local" machine, parses raw
// params, connects a session, calls Create, and formats the output.
func runCreate(ctx context.Context, logger *slog.Logger, params createParams) error {
	serverName, err := ref.ParseServerName(params.ServerName)
	if err != nil {
		return cli.Validation("invalid --server-name %q: %w", params.ServerName, err)
	}

	machineName := params.Machine

	// Resolve "local" to the actual machine localpart from the launcher's
	// session file.
	if machineName == "local" {
		resolved, err := cli.ResolveLocalMachine()
		if err != nil {
			return err
		}
		machineName = resolved
		logger.Info("resolved --machine=local", "machine", machineName)
	}

	// Parse the machine localpart into a typed ref.
	machineRef, err := ref.ParseMachine(machineName, serverName)
	if err != nil {
		return cli.Validation("invalid machine name: %v", err)
	}

	// Parse key=value parameters.
	paramMap, err := parseParams(params.Param)
	if err != nil {
		return err
	}

	// Parse agent override flags.
	overrides, err := params.PrincipalOverrides.Parse()
	if err != nil {
		return err
	}

	// Read the credential file for the registration token. Workspace
	// creation registers Matrix accounts for each principal (setup,
	// agents, teardown) and provisions encrypted credentials.
	if params.SessionConfig.CredentialFile == "" {
		return cli.Validation("--credential-file is required")
	}
	credentials, err := cli.ReadCredentialFile(params.SessionConfig.CredentialFile)
	if err != nil {
		return cli.Internal("read credential file: %w", err)
	}
	registrationToken := credentials["MATRIX_REGISTRATION_TOKEN"]
	if registrationToken == "" {
		return cli.Validation("credential file missing MATRIX_REGISTRATION_TOKEN").
			WithHint("The credential file must contain MATRIX_REGISTRATION_TOKEN. " +
				"Re-run 'bureau matrix setup' to regenerate the credential file.")
	}
	registrationTokenBuffer, err := secret.NewFromString(registrationToken)
	if err != nil {
		return cli.Internal("protecting registration token: %w", err)
	}
	defer registrationTokenBuffer.Close()

	homeserverURL, err := params.SessionConfig.ResolveHomeserverURL()
	if err != nil {
		return err
	}

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
	})
	if err != nil {
		return cli.Internal("create matrix client: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	// Resolve the fleet machine room for credential provisioning.
	fleet := machineRef.Fleet()
	machineRoomAlias := fleet.MachineRoomAlias()
	machineRoomID, err := session.ResolveAlias(ctx, machineRoomAlias)
	if err != nil {
		return cli.NotFound("fleet machine room %s not found: %w", machineRoomAlias, err).
			WithHint("Run 'bureau machine list' to see machines, or 'bureau machine provision' to register one.")
	}

	result, err := Create(ctx, session, CreateParams{
		Alias:                     params.Alias,
		Machine:                   machineRef,
		Template:                  params.Template,
		Params:                    paramMap,
		AgentCount:                params.AgentCount,
		DevTeam:                   params.DevTeam,
		Client:                    matrixClient,
		RegistrationToken:         registrationTokenBuffer,
		HomeserverURL:             homeserverURL,
		MachineRoomID:             machineRoomID,
		ExtraCredentials:          overrides.ExtraCredentials,
		ExtraEnvironmentVariables: overrides.ExtraEnvironmentVariables,
		CommandOverride:           overrides.CommandOverride,
		EnvironmentOverride:       overrides.EnvironmentOverride,
		RequiredServicesOverride:  overrides.RequiredServicesOverride,
		SecretsOverride:           overrides.SecretsOverride,
	}, logger)
	if err != nil {
		return err
	}

	if done, emitError := params.JSONOutput.EmitJSON(result); done {
		return emitError
	}

	logger.Info("workspace created",
		"room_alias", result.RoomAlias,
		"room_id", result.RoomID,
		"project", result.Project,
		"repository", result.Repository,
		"branch", result.Branch,
		"machine", result.Machine,
		"principals", result.Principals,
	)

	return nil
}

// Create creates a new workspace by setting up the Matrix room, publishing
// state events, and configuring principals on the target machine. The daemon
// picks up the configuration via /sync and spawns a setup principal to
// clone the repo, create worktrees, and prepare the workspace. Agent
// principals start only after the setup principal publishes
// m.bureau.workspace with status "active".
//
// The caller provides a connected session and a context with an appropriate
// deadline. This function does not create its own session, which allows
// callers (integration tests, MCP tools, batched commands) to reuse an
// existing session and avoid competing for /sync responses.
func Create(ctx context.Context, session messaging.Session, params CreateParams, logger *slog.Logger) (*CreateResult, error) {
	// Validate the workspace alias.
	if err := principal.ValidateLocalpart(params.Alias); err != nil {
		return nil, cli.Validation("invalid workspace alias: %w", err)
	}

	// The alias must have at least two segments: project/worktree.
	project, worktreePath, hasWorktree := strings.Cut(params.Alias, "/")
	if !hasWorktree {
		return nil, cli.Validation("workspace alias must have at least two segments (project/path), got %q", params.Alias)
	}

	machineRef := params.Machine
	serverName := machineRef.Server()

	// Copy the params map so we don't mutate the caller's map, and
	// resolve the branch parameter.
	paramMap := make(map[string]string)
	for key, value := range params.Params {
		paramMap[key] = value
	}
	repository := paramMap["repository"]
	branch := paramMap["branch"]
	if branch == "" {
		branch = "main"
	}
	// Ensure the resolved branch is in the param map for buildPrincipalAssignments
	// (setup principal's payload reads it from here).
	paramMap["branch"] = branch

	adminUserID := session.UserID()
	machineUserID := machineRef.UserID()

	// Resolve the Bureau namespace space. The workspace room will be added
	// as a child of the namespace's space room. The namespace is derived
	// from the machine's fleet, so workspaces are always scoped to the
	// same namespace as their fleet.
	spaceAlias := machineRef.Fleet().Namespace().SpaceAlias()
	spaceRoomID, err := session.ResolveAlias(ctx, spaceAlias)
	if err != nil {
		return nil, cli.NotFound("resolve namespace space %s: %w", spaceAlias, err).
			WithHint("Run 'bureau matrix setup' to initialize the homeserver and create the Bureau space.")
	}

	// Ensure the workspace room exists (idempotent).
	workspaceRoomID, err := ensureWorkspaceRoom(ctx, session, params.Alias, serverName, adminUserID, machineUserID, spaceRoomID)
	if err != nil {
		return nil, cli.Internal("ensuring workspace room: %w", err)
	}

	// Publish dev team metadata. If --dev-team was specified, use that
	// alias. Otherwise derive from the project name (first alias segment),
	// which is the namespace by convention.
	var devTeamAlias ref.RoomAlias
	if params.DevTeam != "" {
		devTeamAlias, err = ref.ParseRoomAlias(params.DevTeam)
		if err != nil {
			return nil, cli.Validation("invalid --dev-team alias %q: %v", params.DevTeam, err)
		}
	} else {
		namespace, namespaceError := ref.NewNamespace(serverName, project)
		if namespaceError != nil {
			return nil, cli.Internal("deriving namespace from project %q: %w", project, namespaceError)
		}
		devTeamAlias = schema.DevTeamRoomAlias(namespace)
	}
	if _, err := session.SendStateEvent(ctx, workspaceRoomID, schema.EventTypeDevTeam, "", schema.DevTeamContent{Room: devTeamAlias}); err != nil {
		return nil, cli.Internal("publishing dev team metadata: %w", err)
	}

	// Publish the ProjectConfig state event.
	projectConfig := workspace.ProjectConfig{
		WorkspacePath: project,
		Repository:    repository,
	}
	if repository != "" {
		projectConfig.DefaultBranch = branch
		projectConfig.Worktrees = map[string]workspace.WorktreeConfig{
			worktreePath: {Branch: branch},
		}
	} else {
		projectConfig.Directories = map[string]workspace.DirectoryConfig{
			worktreePath: {},
		}
	}
	_, err = session.SendStateEvent(ctx, workspaceRoomID, schema.EventTypeProject, project, projectConfig)
	if err != nil {
		return nil, cli.Internal("publishing project config: %w", err)
	}

	// Publish the initial workspace lifecycle event. The setup principal
	// updates this to "active" when setup completes, and Destroy updates
	// it to "teardown" to trigger agent shutdown and teardown principal
	// launch.
	_, err = session.SendStateEvent(ctx, workspaceRoomID, schema.EventTypeWorkspace, "", workspace.WorkspaceState{
		Status:    workspace.WorkspaceStatusPending,
		Project:   project,
		Machine:   machineRef.Localpart(),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, cli.Internal("publishing workspace state: %w", err)
	}

	// Enable ticket management in the workspace room. Workspace rooms
	// need a ticket service for pipeline tickets (pip-* IDs for
	// setup/teardown tracking) and work tickets (tkt-* IDs for
	// task/bug/feature tracking). ConfigureRoom publishes ticket_config,
	// the service binding (so the daemon can resolve the ticket
	// socket for RequiredServices), power levels, and service invitation.
	// EnsureServiceInRoom then waits for the service to join — the daemon
	// creates pip- tickets during reconciliation, and if the ticket
	// service hasn't joined by then, ticket creation fails.
	ticketService, found, err := service.QueryFirst(ctx, session, machineRef.Fleet(),
		service.And(service.OnMachine(machineRef), service.HasCapability("dependency-graph")))
	if err != nil {
		return nil, cli.Internal("querying ticket service: %w", err)
	}
	if !found {
		return nil, cli.NotFound("no ticket service deployed on %s", machineRef.Localpart()).
			WithHint("Run 'bureau ticket enable' to deploy a ticket service on this machine.")
	}
	if err := ticket.ConfigureRoom(ctx, logger, session, workspaceRoomID, ticketService.Principal, ticket.ConfigureRoomParams{
		Prefix: "tkt",
	}); err != nil {
		return nil, cli.Internal("configuring ticket management: %w", err)
	}
	if err := service.EnsureServiceInRoom(ctx, session, workspaceRoomID, ticketService.Principal.UserID()); err != nil {
		return nil, cli.Internal("ticket service failed to join workspace room %s: %w", params.Alias, err)
	}

	// Enable pipeline execution in the workspace room. Pipeline tickets
	// (pip-* IDs) require the room to have pipeline_config — without it,
	// the daemon skips pipeline execution for this room.
	if err := pipeline.ConfigureRoom(ctx, logger, session, workspaceRoomID, pipeline.ConfigureRoomParams{}); err != nil {
		return nil, cli.Internal("configuring pipeline execution: %w", err)
	}

	// Build principal assignments and provision credentials. Each
	// workspace principal (setup, agents, teardown) needs a registered
	// Matrix account and encrypted credentials before the daemon can
	// create its sandbox.
	assignments, err := buildPrincipalAssignments(params.Alias, params.Template, params.AgentCount, serverName, machineRef, workspaceRoomID, paramMap)
	if err != nil {
		return nil, cli.Internal("building principal assignments: %w", err)
	}

	// Convert assignments to principal.CreateParams for account
	// registration and credential provisioning.
	validateTemplate := func(ctx context.Context, templateRef schema.TemplateRef, serverName ref.ServerName) error {
		_, err := templatedef.Fetch(ctx, session, templateRef, serverName)
		return err
	}
	var createParamsList []principal.CreateParams
	for _, assignment := range assignments {
		templateRef, parseError := schema.ParseTemplateRef(assignment.Template)
		if parseError != nil {
			return nil, cli.Internal("parsing template ref %q: %w", assignment.Template, parseError)
		}

		// Agent principals receive extra credentials, environment
		// overrides, and instance customization; setup and teardown
		// principals are pipeline executors that only need their
		// Matrix token and the template defaults.
		cp := principal.CreateParams{
			Machine:          machineRef,
			Principal:        assignment.Principal,
			TemplateRef:      templateRef,
			ValidateTemplate: validateTemplate,
			HomeserverURL:    params.HomeserverURL,
			AutoStart:        assignment.AutoStart,
			MachineRoomID:    params.MachineRoomID,
			StartCondition:   assignment.StartCondition,
			Labels:           assignment.Labels,
			Payload:          assignment.Payload,
			// Start with the assignment's own extra env vars (e.g.,
			// WORKSPACE_ALIAS set by buildPrincipalAssignments).
			ExtraEnvironmentVariables: assignment.ExtraEnvironmentVariables,
		}

		if assignment.Labels["role"] == "agent" {
			cp.ExtraCredentials = params.ExtraCredentials
			cp.CommandOverride = params.CommandOverride
			cp.EnvironmentOverride = params.EnvironmentOverride
			cp.RequiredServicesOverride = params.RequiredServicesOverride
			cp.SecretsOverride = params.SecretsOverride

			// Merge CLI-provided extra env vars into the
			// assignment's built-in env vars. CLI values win on
			// conflict.
			if len(params.ExtraEnvironmentVariables) > 0 {
				if cp.ExtraEnvironmentVariables == nil {
					cp.ExtraEnvironmentVariables = make(map[string]string, len(params.ExtraEnvironmentVariables))
				}
				for key, value := range params.ExtraEnvironmentVariables {
					cp.ExtraEnvironmentVariables[key] = value
				}
			}
		}

		createParamsList = append(createParamsList, cp)
	}

	// Register accounts, provision encrypted credentials, and publish
	// MachineConfig in one atomic operation.
	_, err = principal.CreateMultiple(ctx, params.Client, session, params.RegistrationToken, credential.AsProvisionFunc(), createParamsList)
	if err != nil {
		return nil, cli.Internal("provisioning workspace principals: %w", err)
	}

	// Invite pipeline principals (setup, teardown) to the pipeline room
	// so the pipeline executor can read pipeline definitions via the
	// proxy. The proxy authenticates as the principal and needs pipeline
	// room membership to access m.bureau.pipeline state events.
	namespace, err := ref.NewNamespace(serverName, "bureau")
	if err != nil {
		return nil, cli.Internal("deriving namespace for pipeline room: %w", err)
	}
	pipelineRoomAlias := namespace.PipelineRoomAlias()
	pipelineRoomID, err := session.ResolveAlias(ctx, pipelineRoomAlias)
	if err != nil {
		logger.Warn("could not resolve pipeline room for principal invitations",
			"alias", pipelineRoomAlias, "error", err)
	} else {
		for _, assignment := range assignments {
			role := assignment.Labels["role"]
			if role != "setup" && role != "teardown" {
				continue
			}
			if err := session.InviteUser(ctx, pipelineRoomID, assignment.Principal.UserID()); err != nil {
				if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
					return nil, cli.Internal("inviting %s to pipeline room: %w", assignment.Principal.Localpart(), err)
				}
			}
		}
	}

	// Invite the machine daemon to the workspace room so it can read
	// workspace state events (needed for StartCondition evaluation).
	err = session.InviteUser(ctx, workspaceRoomID, machineUserID)
	if err != nil {
		// M_FORBIDDEN is fine — the machine may already be a member.
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			return nil, cli.Internal("inviting machine %s to workspace room: %w", machineUserID, err)
		}
	}

	fullAlias := schema.FullRoomAlias(params.Alias, serverName)

	var principalNames []string
	for _, assignment := range assignments {
		principalNames = append(principalNames, assignment.Principal.Localpart())
	}

	return &CreateResult{
		Alias:      params.Alias,
		RoomAlias:  fullAlias,
		RoomID:     workspaceRoomID,
		Project:    project,
		Repository: repository,
		Branch:     branch,
		Machine:    machineRef.Localpart(),
		Principals: principalNames,
	}, nil
}

// ensureWorkspaceRoom creates the workspace room if it doesn't already exist.
// The room is created with WorkspaceRoomPowerLevels and added as a child of
// the Bureau space. Handles the M_ROOM_IN_USE race condition (resolve alias →
// create → retry resolve if someone else created it between our check and
// create).
func ensureWorkspaceRoom(ctx context.Context, session messaging.Session, alias string, serverName ref.ServerName, adminUserID, machineUserID ref.UserID, spaceRoomID ref.RoomID) (ref.RoomID, error) {
	fullAlias, err := ref.ParseRoomAlias(schema.FullRoomAlias(alias, serverName))
	if err != nil {
		return ref.RoomID{}, cli.Validation("invalid workspace alias %q: %w", alias, err)
	}

	// Check if the room already exists.
	roomID, err := session.ResolveAlias(ctx, fullAlias)
	if err == nil {
		return roomID, nil
	}
	if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return ref.RoomID{}, cli.Internal("resolving workspace alias %q: %w", fullAlias, err)
	}

	// Room doesn't exist — create it.
	response, err := session.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:                      alias,
		Alias:                     alias,
		Topic:                     "Workspace: " + alias,
		Preset:                    "private_chat",
		Invite:                    []string{machineUserID.String()},
		Visibility:                "private",
		PowerLevelContentOverride: schema.WorkspaceRoomPowerLevels(adminUserID, machineUserID),
	})
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeRoomInUse) {
			// Race: room created between our resolve and creation attempt.
			roomID, err = session.ResolveAlias(ctx, fullAlias)
			if err != nil {
				return ref.RoomID{}, cli.Internal("room exists but cannot resolve alias %q: %w", fullAlias, err)
			}
			return roomID, nil
		}
		return ref.RoomID{}, cli.Internal("creating workspace room: %w", err)
	}

	// Add as a child of the Bureau space. The state key is the room ID
	// string per the Matrix m.space.child spec.
	_, err = session.SendStateEvent(ctx, spaceRoomID, "m.space.child", response.RoomID.String(),
		map[string]any{
			"via": []string{serverName.String()},
		})
	if err != nil {
		return ref.RoomID{}, cli.Internal("adding workspace room as child of Bureau space: %w", err)
	}

	return response.RoomID, nil
}

// buildPrincipalAssignments constructs the PrincipalAssignment slice for a
// workspace: one setup principal (runs immediately), N agent principals
// (gated on workspace status "active"), and one teardown principal (gated
// on workspace status "teardown").
//
// The teardown principal starts only when "bureau workspace destroy" sets
// the workspace status to "teardown". The daemon re-evaluates conditions
// every reconcile cycle, so when status changes from "active" to "teardown",
// agent principals stop (their condition becomes false) and the teardown
// principal starts (its condition becomes true).
func buildPrincipalAssignments(alias, agentTemplate string, agentCount int, serverName ref.ServerName, machineRef ref.Machine, workspaceRoomID ref.RoomID, params map[string]string) ([]schema.PrincipalAssignment, error) {
	workspaceRoomAlias, err := ref.ParseRoomAlias(schema.FullRoomAlias(alias, serverName))
	if err != nil {
		return nil, cli.Internal("constructing workspace room alias: %w", err)
	}
	fleet := machineRef.Fleet()
	namespace := fleet.Namespace()
	templatePrefix := namespace.TemplateRoomAliasLocalpart()
	pipelinePrefix := namespace.PipelineRoomAliasLocalpart()

	// Derive workspace path components from the alias. The first segment
	// is the project name; everything after is the worktree path (if any).
	// These flow into PrincipalAssignment.Payload so the launcher can
	// expand ${PROJECT} and ${WORKTREE_PATH} in template mount paths.
	project, worktreePath, _ := strings.Cut(alias, "/")

	// Helper to construct an Entity from an account localpart. Workspace
	// principals use the "agent" entity type with the workspace alias as
	// the hierarchical name (e.g., "agent/iree/amdgpu/inference/setup").
	makeEntity := func(accountLocalpart string) (ref.Entity, error) {
		return ref.NewEntityFromAccountLocalpart(fleet, accountLocalpart)
	}

	// Setup principal: clones repo, creates worktrees, publishes workspace active status.
	// Gated on status "pending" so the daemon stops it after the pipeline
	// publishes "active". Without this condition, the reconciliation loop
	// would restart the sandbox every time the pipeline executor exits.
	//
	// Payload keys are UPPERCASE to match the pipeline variable declarations
	// in dev-workspace-init.jsonc. The pipeline_ref tells the executor which
	// pipeline to run; all other keys become pipeline variables.
	setupEntity, err := makeEntity("agent/" + alias + "/setup")
	if err != nil {
		return nil, cli.Internal("constructing setup principal: %w", err)
	}
	assignments := []schema.PrincipalAssignment{
		{
			Principal: setupEntity,
			Template:  templatePrefix + ":base",
			AutoStart: true,
			Labels:    map[string]string{"role": "setup"},
			Payload: map[string]any{
				"pipeline_ref":      pipelinePrefix + ":dev-workspace-init",
				"REPOSITORY":        params["repository"],
				"PROJECT":           project,
				"WORKSPACE_ROOM_ID": workspaceRoomID.String(),
				"MACHINE":           machineRef.Localpart(),
			},
			StartCondition: &schema.StartCondition{
				EventType:    schema.EventTypeWorkspace,
				StateKey:     "",
				RoomAlias:    workspaceRoomAlias,
				ContentMatch: schema.ContentMatch{"status": schema.Eq("pending")},
			},
		},
	}

	// Agent principals: wait for workspace to become active before starting.
	// Payload carries PROJECT (and WORKTREE_PATH when the alias includes a
	// worktree component) so the launcher can expand these variables in the
	// agent's template mount paths (e.g., ${WORKSPACE_ROOT}/${PROJECT}).
	for index := 0; index < agentCount; index++ {
		agentEntity, err := makeEntity("agent/" + alias + "/" + strconv.Itoa(index))
		if err != nil {
			return nil, cli.Internal("constructing agent principal %d: %w", index, err)
		}
		agentPayload := map[string]any{
			"PROJECT": project,
		}
		if worktreePath != "" {
			agentPayload["WORKTREE_PATH"] = worktreePath
		}
		assignments = append(assignments, schema.PrincipalAssignment{
			Principal: agentEntity,
			Template:  agentTemplate,
			AutoStart: true,
			Labels:    map[string]string{"role": "agent"},
			Payload:   agentPayload,
			ExtraEnvironmentVariables: map[string]string{
				"WORKSPACE_ALIAS": alias,
			},
			StartCondition: &schema.StartCondition{
				EventType:    schema.EventTypeWorkspace,
				StateKey:     "",
				RoomAlias:    workspaceRoomAlias,
				ContentMatch: schema.ContentMatch{"status": schema.Eq("active")},
			},
		})
	}

	// Teardown principal: starts when workspace status becomes "teardown".
	// Archives or removes the workspace data, then publishes the final
	// status ("archived" or "removed").
	//
	// Payload carries static per-principal config: pipeline ref, project,
	// workspace room ID, and machine. These are known at create time and
	// don't change. Dynamic per-invocation context (teardown mode) comes
	// from the trigger event — the daemon captures the workspace state
	// event content when the StartCondition matches and delivers it as
	// /run/bureau/trigger.json. The pipeline reads the mode via the
	// EVENT_teardown_mode variable.
	teardownEntity, err := makeEntity("agent/" + alias + "/teardown")
	if err != nil {
		return nil, cli.Internal("constructing teardown principal: %w", err)
	}
	assignments = append(assignments, schema.PrincipalAssignment{
		Principal: teardownEntity,
		Template:  templatePrefix + ":base",
		AutoStart: true,
		Labels:    map[string]string{"role": "teardown"},
		Payload: map[string]any{
			"pipeline_ref":      pipelinePrefix + ":dev-workspace-deinit",
			"PROJECT":           project,
			"WORKSPACE_ROOM_ID": workspaceRoomID.String(),
			"MACHINE":           machineRef.Localpart(),
		},
		StartCondition: &schema.StartCondition{
			EventType:    schema.EventTypeWorkspace,
			StateKey:     "",
			RoomAlias:    workspaceRoomAlias,
			ContentMatch: schema.ContentMatch{"status": schema.Eq("teardown")},
		},
	})

	return assignments, nil
}

// parseParams parses a list of "key=value" strings into a map. Returns an
// error for malformed entries (missing "=").
func parseParams(rawParams []string) (map[string]string, error) {
	result := make(map[string]string)
	for _, param := range rawParams {
		key, value, found := strings.Cut(param, "=")
		if !found {
			return nil, cli.Validation("invalid --param %q: expected key=value", param)
		}
		if key == "" {
			return nil, cli.Validation("invalid --param %q: empty key", param)
		}
		result[key] = value
	}
	return result, nil
}
