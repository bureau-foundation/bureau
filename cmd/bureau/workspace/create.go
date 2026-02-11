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

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

func createCommand() *cli.Command {
	var (
		session    cli.SessionConfig
		machine    string
		template   string
		params     []string
		serverName string
		agentCount int
	)

	return &cli.Command{
		Name:    "create",
		Summary: "Create a new workspace",
		Description: `Create a workspace by setting up the Matrix room, publishing state
events, and configuring principals on the target machine. The daemon
picks up the configuration via /sync and spawns a setup principal to
clone the repo, create worktrees, and prepare the workspace. Agent
principals start only after the setup principal publishes
m.bureau.workspace.ready.

The alias becomes both the Matrix room alias and filesystem path:

  bureau workspace create iree/amdgpu/inference --machine machine/workstation \
    --template bureau/templates:base --param repository=https://github.com/iree-org/iree.git

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
				Command:     "bureau workspace create iree/amdgpu/inference --machine machine/workstation --template bureau/templates:base --param repository=https://github.com/iree-org/iree.git --credential-file ./creds",
			},
			{
				Description: "Create a workspace with multiple agents",
				Command:     "bureau workspace create iree/amdgpu/inference --machine machine/workstation --template bureau/templates:base --param repository=https://github.com/iree-org/iree.git --agent-count 3 --credential-file ./creds",
			},
			{
				Description: "Create a workspace on a specific branch",
				Command:     "bureau workspace create iree/amdgpu/inference --machine machine/workstation --template bureau/templates:base --param repository=https://github.com/iree-org/iree.git --param branch=develop --credential-file ./creds",
			},
			{
				Description: "Create a workspace on the local machine (auto-detect identity)",
				Command:     "bureau workspace create iree/amdgpu/inference --machine local --template bureau/templates:base --param repository=https://github.com/iree-org/iree.git --credential-file ./creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("workspace create", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			flagSet.StringVar(&machine, "machine", "", "machine localpart to host the workspace (required; use \"local\" to auto-detect from launcher session)")
			flagSet.StringVar(&template, "template", "", "sandbox template ref for agent principals (required, e.g., bureau/templates:base)")
			flagSet.StringArrayVar(&params, "param", nil, "key=value parameter (repeatable; recognized: repository, branch)")
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
			flagSet.IntVar(&agentCount, "agent-count", 1, "number of agent principals to create")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("workspace alias is required\n\nUsage: bureau workspace create <alias> --machine <machine> --template <ref> [flags]")
			}
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}
			if machine == "" {
				return fmt.Errorf("--machine is required")
			}
			if template == "" {
				return fmt.Errorf("--template is required")
			}
			if agentCount < 0 {
				return fmt.Errorf("--agent-count must be non-negative, got %d", agentCount)
			}

			return runCreate(args[0], &session, machine, template, params, serverName, agentCount)
		},
	}
}

func runCreate(alias string, session *cli.SessionConfig, machine, templateRef string, rawParams []string, serverName string, agentCount int) error {
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

	// Build principal assignments (setup + agents).
	assignments := buildPrincipalAssignments(alias, templateRef, agentCount, serverName, paramMap)

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

	// Print summary.
	fullAlias := principal.RoomAlias(alias, serverName)
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
			suffix = " (waits for workspace.ready)"
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
// workspace: one setup principal (no StartCondition, runs immediately) and
// N agent principals (gated on workspace.ready).
func buildPrincipalAssignments(alias, agentTemplate string, agentCount int, serverName string, params map[string]string) []schema.PrincipalAssignment {
	workspaceRoomAlias := principal.RoomAlias(alias, serverName)

	// Setup principal: clones repo, creates worktrees, publishes workspace.ready.
	setupLocalpart := alias + "/setup"
	assignments := []schema.PrincipalAssignment{
		{
			Localpart: setupLocalpart,
			Template:  "bureau/templates:base",
			AutoStart: true,
			Labels:    map[string]string{"role": "setup"},
			Payload: map[string]any{
				"workspace_alias": alias,
				"repository":      params["repository"],
				"branch":          params["branch"],
			},
		},
	}

	// Agent principals: wait for workspace.ready before starting.
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
				EventType: schema.EventTypeWorkspaceReady,
				StateKey:  "",
				RoomAlias: workspaceRoomAlias,
			},
		})
	}

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
