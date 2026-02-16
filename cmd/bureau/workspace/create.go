// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// createParams holds the parameters for the workspace create command.
type createParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Machine    string   `json:"machine"     flag:"machine"     desc:"machine localpart to host the workspace (required; use 'local' to auto-detect from launcher session)"`
	Template   string   `json:"template"    flag:"template"    desc:"sandbox template ref for agent principals (required, e.g., bureau/template:base)"`
	Param      []string `json:"param"       flag:"param"       desc:"key=value parameter (repeatable; recognized: repository, branch)"`
	ServerName string   `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
	AgentCount int      `json:"agent_count" flag:"agent-count" desc:"number of agent principals to create" default:"1"`
}

// createResult is the JSON output for workspace create.
type createResult struct {
	Alias      string   `json:"alias"                desc:"workspace alias"`
	RoomAlias  string   `json:"room_alias"           desc:"full Matrix room alias"`
	RoomID     string   `json:"room_id"              desc:"Matrix room ID"`
	Project    string   `json:"project"              desc:"project name"`
	Repository string   `json:"repository,omitempty" desc:"git repository URL"`
	Branch     string   `json:"branch,omitempty"     desc:"git branch checked out"`
	Machine    string   `json:"machine"              desc:"hosting machine name"`
	Principals []string `json:"principals"           desc:"created principal localparts"`
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

  bureau workspace create iree/amdgpu/inference --machine machine/workstation \
    --template bureau/template:base --param repository=https://github.com/iree-org/iree.git

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
				Command:     "bureau workspace create iree/amdgpu/inference --machine machine/workstation --template bureau/template:base --param repository=https://github.com/iree-org/iree.git --credential-file ./creds",
			},
			{
				Description: "Create a workspace with multiple agents",
				Command:     "bureau workspace create iree/amdgpu/inference --machine machine/workstation --template bureau/template:base --param repository=https://github.com/iree-org/iree.git --agent-count 3 --credential-file ./creds",
			},
			{
				Description: "Create a workspace on a specific branch",
				Command:     "bureau workspace create iree/amdgpu/inference --machine machine/workstation --template bureau/template:base --param repository=https://github.com/iree-org/iree.git --param branch=develop --credential-file ./creds",
			},
			{
				Description: "Create a workspace on the local machine (auto-detect identity)",
				Command:     "bureau workspace create iree/amdgpu/inference --machine local --template bureau/template:base --param repository=https://github.com/iree-org/iree.git --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &createResult{} },
		RequiredGrants: []string{"command/workspace/create"},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("workspace alias is required\n\nUsage: bureau workspace create <alias> --machine <machine> --template <ref> [flags]")
			}
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}
			if params.Machine == "" {
				return fmt.Errorf("--machine is required")
			}
			if params.Template == "" {
				return fmt.Errorf("--template is required")
			}
			if params.AgentCount < 0 {
				return fmt.Errorf("--agent-count must be non-negative, got %d", params.AgentCount)
			}

			return runCreate(args[0], &params.SessionConfig, params.Machine, params.Template, params.Param, params.ServerName, params.AgentCount, &params.JSONOutput)
		},
	}
}

func runCreate(alias string, session *cli.SessionConfig, machine, templateRef string, rawParams []string, serverName string, agentCount int, jsonOutput *cli.JSONOutput) error {
	// Validate the workspace alias.
	if err := principal.ValidateLocalpart(alias); err != nil {
		return fmt.Errorf("invalid workspace alias: %w", err)
	}

	// The alias must have at least two segments: project/worktree.
	project, worktreePath, hasWorktree := strings.Cut(alias, "/")
	if !hasWorktree {
		return fmt.Errorf("workspace alias must have at least two segments (project/path), got %q", alias)
	}

	// Resolve "local" to the actual machine localpart from the launcher's
	// session file. This lets operators skip looking up the full machine name
	// when running on the target machine itself.
	if machine == "local" {
		resolved, err := cli.ResolveLocalMachine()
		if err != nil {
			return fmt.Errorf("resolving local machine identity: %w", err)
		}
		machine = resolved
		fmt.Fprintf(os.Stderr, "Resolved --machine=local to %s\n", machine)
	}

	// Validate the machine localpart.
	if err := principal.ValidateLocalpart(machine); err != nil {
		return fmt.Errorf("invalid machine name: %w", err)
	}

	// Parse key=value parameters.
	paramMap, err := parseParams(rawParams)
	if err != nil {
		return err
	}
	repository := paramMap["repository"]
	branch := paramMap["branch"]
	if branch == "" {
		branch = "main"
	}
	// Ensure the resolved branch is in the param map for buildPrincipalAssignments
	// (setup principal's payload reads it from here).
	paramMap["branch"] = branch

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := session.Connect(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer sess.Close()

	adminUserID := sess.UserID()
	machineUserID := principal.MatrixUserID(machine, serverName)

	// Resolve the Bureau space. The workspace room will be added as a child.
	spaceAlias := principal.RoomAlias("bureau", serverName)
	spaceRoomID, err := sess.ResolveAlias(ctx, spaceAlias)
	if err != nil {
		return fmt.Errorf("resolve Bureau space %s: %w (has 'bureau matrix setup' been run?)", spaceAlias, err)
	}

	// Ensure the workspace room exists (idempotent).
	workspaceRoomID, err := ensureWorkspaceRoom(ctx, sess, alias, serverName, adminUserID, machineUserID, spaceRoomID)
	if err != nil {
		return fmt.Errorf("ensuring workspace room: %w", err)
	}

	// Publish the ProjectConfig state event.
	projectConfig := schema.ProjectConfig{
		WorkspacePath: project,
		Repository:    repository,
	}
	if repository != "" {
		projectConfig.DefaultBranch = branch
		projectConfig.Worktrees = map[string]schema.WorktreeConfig{
			worktreePath: {Branch: branch},
		}
	} else {
		projectConfig.Directories = map[string]schema.DirectoryConfig{
			worktreePath: {},
		}
	}
	_, err = sess.SendStateEvent(ctx, workspaceRoomID, schema.EventTypeProject, project, projectConfig)
	if err != nil {
		return fmt.Errorf("publishing project config: %w", err)
	}

	// Publish the initial workspace lifecycle event. The setup principal
	// updates this to "active" when setup completes, and "bureau workspace
	// destroy" updates it to "teardown" to trigger agent shutdown and
	// teardown principal launch.
	_, err = sess.SendStateEvent(ctx, workspaceRoomID, schema.EventTypeWorkspace, "", schema.WorkspaceState{
		Status:    "pending",
		Project:   project,
		Machine:   machine,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("publishing workspace state: %w", err)
	}

	// Build principal assignments (setup + agents + teardown).
	assignments := buildPrincipalAssignments(alias, templateRef, agentCount, serverName, machine, workspaceRoomID, paramMap)

	// Update the machine's MachineConfig with the new principals.
	err = updateMachineConfig(ctx, sess, machine, serverName, assignments)
	if err != nil {
		return fmt.Errorf("updating machine config: %w", err)
	}

	// Invite the machine daemon to the workspace room so it can read
	// workspace state events (needed for StartCondition evaluation).
	err = sess.InviteUser(ctx, workspaceRoomID, machineUserID)
	if err != nil {
		// M_FORBIDDEN is fine — the machine may already be a member.
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			return fmt.Errorf("inviting machine %s to workspace room: %w", machineUserID, err)
		}
	}

	fullAlias := principal.RoomAlias(alias, serverName)

	var principalNames []string
	for _, assignment := range assignments {
		principalNames = append(principalNames, assignment.Localpart)
	}
	if done, err := jsonOutput.EmitJSON(createResult{
		Alias:      alias,
		RoomAlias:  fullAlias,
		RoomID:     workspaceRoomID,
		Project:    project,
		Repository: repository,
		Branch:     branch,
		Machine:    machine,
		Principals: principalNames,
	}); done {
		return err
	}

	fmt.Fprintf(os.Stderr, "Workspace created:\n")
	fmt.Fprintf(os.Stderr, "  Room:     %s (%s)\n", fullAlias, workspaceRoomID)
	fmt.Fprintf(os.Stderr, "  Project:  %s\n", project)
	if repository != "" {
		fmt.Fprintf(os.Stderr, "  Repo:     %s (branch: %s)\n", repository, branch)
	}
	fmt.Fprintf(os.Stderr, "  Machine:  %s\n", machine)
	fmt.Fprintf(os.Stderr, "  Principals:\n")
	for _, assignment := range assignments {
		suffix := ""
		if assignment.StartCondition != nil {
			if status, ok := assignment.StartCondition.ContentMatch["status"]; ok {
				suffix = fmt.Sprintf(" (waits for workspace %s)", status.StringValue())
			} else {
				suffix = " (conditional)"
			}
		}
		fmt.Fprintf(os.Stderr, "    %s (template=%s)%s\n", assignment.Localpart, assignment.Template, suffix)
	}
	fmt.Fprintf(os.Stderr, "\nNext steps:\n")
	fmt.Fprintf(os.Stderr, "  1. Provision credentials: bureau-credentials provision --principal <localpart> --machine %s ...\n", machine)
	fmt.Fprintf(os.Stderr, "  2. Observe setup: bureau observe %s/setup\n", alias)

	return nil
}

// ensureWorkspaceRoom creates the workspace room if it doesn't already exist.
// The room is created with WorkspaceRoomPowerLevels and added as a child of
// the Bureau space. Handles the M_ROOM_IN_USE race condition (resolve alias →
// create → retry resolve if someone else created it between our check and
// create).
func ensureWorkspaceRoom(ctx context.Context, session *messaging.Session, alias, serverName, adminUserID, machineUserID, spaceRoomID string) (string, error) {
	fullAlias := principal.RoomAlias(alias, serverName)

	// Check if the room already exists.
	roomID, err := session.ResolveAlias(ctx, fullAlias)
	if err == nil {
		return roomID, nil
	}
	if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return "", fmt.Errorf("resolving workspace alias %q: %w", fullAlias, err)
	}

	// Room doesn't exist — create it.
	response, err := session.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:                      alias,
		Alias:                     alias,
		Topic:                     "Workspace: " + alias,
		Preset:                    "private_chat",
		Invite:                    []string{machineUserID},
		Visibility:                "private",
		PowerLevelContentOverride: schema.WorkspaceRoomPowerLevels(adminUserID, machineUserID),
	})
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeRoomInUse) {
			// Race: room created between our resolve and creation attempt.
			roomID, err = session.ResolveAlias(ctx, fullAlias)
			if err != nil {
				return "", fmt.Errorf("room exists but cannot resolve alias %q: %w", fullAlias, err)
			}
			return roomID, nil
		}
		return "", fmt.Errorf("creating workspace room: %w", err)
	}

	// Add as a child of the Bureau space.
	_, err = session.SendStateEvent(ctx, spaceRoomID, "m.space.child", response.RoomID,
		map[string]any{
			"via": []string{serverName},
		})
	if err != nil {
		return "", fmt.Errorf("adding workspace room as child of Bureau space: %w", err)
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
func buildPrincipalAssignments(alias, agentTemplate string, agentCount int, serverName, machine, workspaceRoomID string, params map[string]string) []schema.PrincipalAssignment {
	workspaceRoomAlias := principal.RoomAlias(alias, serverName)

	// Derive the project name from the alias (first path segment).
	project, _, _ := strings.Cut(alias, "/")

	// Setup principal: clones repo, creates worktrees, publishes workspace active status.
	// Payload keys are UPPERCASE to match the pipeline variable declarations
	// in dev-workspace-init.jsonc. The pipeline_ref tells the executor which
	// pipeline to run; all other keys become pipeline variables.
	setupLocalpart := alias + "/setup"
	assignments := []schema.PrincipalAssignment{
		{
			Localpart: setupLocalpart,
			Template:  "bureau/template:base",
			AutoStart: true,
			Labels:    map[string]string{"role": "setup"},
			Payload: map[string]any{
				"pipeline_ref":      "bureau/pipeline:dev-workspace-init",
				"REPOSITORY":        params["repository"],
				"PROJECT":           project,
				"WORKSPACE_ROOM_ID": workspaceRoomID,
				"MACHINE":           machine,
			},
		},
	}

	// Agent principals: wait for workspace to become active before starting.
	for index := 0; index < agentCount; index++ {
		agentLocalpart := alias + "/agent/" + strconv.Itoa(index)
		assignments = append(assignments, schema.PrincipalAssignment{
			Localpart: agentLocalpart,
			Template:  agentTemplate,
			AutoStart: true,
			Labels:    map[string]string{"role": "agent"},
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
	teardownLocalpart := alias + "/teardown"
	assignments = append(assignments, schema.PrincipalAssignment{
		Localpart: teardownLocalpart,
		Template:  "bureau/template:base",
		AutoStart: true,
		Labels:    map[string]string{"role": "teardown"},
		Payload: map[string]any{
			"pipeline_ref":      "bureau/pipeline:dev-workspace-deinit",
			"PROJECT":           project,
			"WORKSPACE_ROOM_ID": workspaceRoomID,
			"MACHINE":           machine,
		},
		StartCondition: &schema.StartCondition{
			EventType:    schema.EventTypeWorkspace,
			StateKey:     "",
			RoomAlias:    workspaceRoomAlias,
			ContentMatch: schema.ContentMatch{"status": schema.Eq("teardown")},
		},
	})

	return assignments
}

// updateMachineConfig performs a read-modify-write on the machine's
// MachineConfig state event: reads the existing config (if any), appends
// new principal assignments (skipping duplicates by localpart), and
// publishes the updated config.
func updateMachineConfig(ctx context.Context, session *messaging.Session, machine, serverName string, newAssignments []schema.PrincipalAssignment) error {
	configRoomAlias := principal.RoomAlias("bureau/config/"+machine, serverName)
	configRoomID, err := session.ResolveAlias(ctx, configRoomAlias)
	if err != nil {
		return fmt.Errorf("resolve config room %s: %w (has the machine been registered?)", configRoomAlias, err)
	}

	// Read the existing MachineConfig.
	var config schema.MachineConfig
	existingContent, err := session.GetStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machine)
	if err == nil {
		if unmarshalError := json.Unmarshal(existingContent, &config); unmarshalError != nil {
			return fmt.Errorf("parsing existing machine config: %w", unmarshalError)
		}
	} else if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return fmt.Errorf("reading machine config: %w", err)
	}

	// Build a set of existing localparts for deduplication.
	existingLocalparts := make(map[string]bool)
	for _, assignment := range config.Principals {
		existingLocalparts[assignment.Localpart] = true
	}

	// Append new assignments, skipping any that already exist.
	addedCount := 0
	for _, assignment := range newAssignments {
		if existingLocalparts[assignment.Localpart] {
			fmt.Fprintf(os.Stderr, "  (skipping %s — already assigned)\n", assignment.Localpart)
			continue
		}
		config.Principals = append(config.Principals, assignment)
		addedCount++
	}

	if addedCount == 0 {
		fmt.Fprintf(os.Stderr, "  All principals already assigned, skipping MachineConfig update.\n")
		return nil
	}

	_, err = session.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machine, config)
	if err != nil {
		return fmt.Errorf("publishing machine config: %w", err)
	}

	return nil
}

// parseParams parses a list of "key=value" strings into a map. Returns an
// error for malformed entries (missing "=").
func parseParams(rawParams []string) (map[string]string, error) {
	result := make(map[string]string)
	for _, param := range rawParams {
		key, value, found := strings.Cut(param, "=")
		if !found {
			return nil, fmt.Errorf("invalid --param %q: expected key=value", param)
		}
		if key == "" {
			return nil, fmt.Errorf("invalid --param %q: empty key", param)
		}
		result[key] = value
	}
	return result, nil
}
