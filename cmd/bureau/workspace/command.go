// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// Command returns the "workspace" command group.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "workspace",
		Summary: "Manage project workspaces",
		Description: `Manage project workspaces across the Bureau fleet.

A workspace is a directory structure under /var/bureau/workspace/ that
holds source code (git worktrees), datasets, models, documents, or any
other files that sandboxed principals need access to. The first path
segment is the project (e.g., "iree", "lore"), and everything below it
is the project's structure.

The room alias maps directly to the filesystem path:

  #iree/amdgpu/inference  →  /var/bureau/workspace/iree/amdgpu/inference/

For git-backed projects, all worktrees share a single bare object store
at /var/bureau/workspace/<project>/.bare/, so cloning a large repo (like
LLVM) once serves all agents working on that project.

Workspace operations work across the fleet via Matrix messaging. Commands
like "status" and "fetch" are sent to the target machine's daemon, which
executes them and replies. No SSH required — works through NAT and
firewalls.`,
		Subcommands: []*cli.Command{
			createCommand(),
			destroyCommand(),
			listCommand(),
			statusCommand(),
			duCommand(),
			worktreeCommand(),
			fetchCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Create a workspace from the standard dev template",
				Command:     "bureau workspace create iree/amdgpu/inference --template dev-workspace --param repository=https://github.com/iree-org/iree.git",
			},
			{
				Description: "List all workspaces across the fleet",
				Command:     "bureau workspace list",
			},
			{
				Description: "Check workspace status on a remote machine",
				Command:     "bureau workspace status iree/amdgpu/inference",
			},
			{
				Description: "Add a worktree for a new feature branch",
				Command:     "bureau workspace worktree add iree/amdgpu/pm --branch feature/amdgpu-pm",
			},
			{
				Description: "Tear down a workspace and archive the data",
				Command:     "bureau workspace destroy iree/amdgpu/inference --archive",
			},
		},
	}
}

func destroyCommand() *cli.Command {
	var (
		session    cli.SessionConfig
		mode       string
		serverName string
	)

	return &cli.Command{
		Name:    "destroy",
		Summary: "Tear down a workspace",
		Description: `Tear down a workspace on the hosting machine. Sets the workspace
status to "teardown", which triggers the daemon's continuous enforcement:
agent principals gated on "active" stop, and the teardown principal
gated on "teardown" starts.

The teardown principal executes the dev-workspace-deinit pipeline, which
checks for uncommitted changes, archives the data (with --mode archive,
the default), or removes everything (with --mode delete). The pipeline
then publishes the final status ("archived" or "removed").

The Matrix room is preserved — its message history remains accessible.
Use "bureau matrix room leave" separately to remove the room.`,
		Usage: "bureau workspace destroy <alias> [flags]",
		Examples: []cli.Example{
			{
				Description: "Archive a workspace (default)",
				Command:     "bureau workspace destroy iree/amdgpu/inference --credential-file ./creds",
			},
			{
				Description: "Delete a workspace and all its data",
				Command:     "bureau workspace destroy iree/amdgpu/inference --mode delete --credential-file ./creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("workspace destroy", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			flagSet.StringVar(&mode, "mode", "archive", "teardown mode: archive or delete")
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("workspace alias is required\n\nUsage: bureau workspace destroy <alias> [flags]")
			}
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}
			if mode != "archive" && mode != "delete" {
				return fmt.Errorf("--mode must be \"archive\" or \"delete\", got %q", mode)
			}

			return runDestroy(args[0], &session, mode, serverName)
		},
	}
}

// runDestroy transitions a workspace to "teardown" status. The daemon's
// continuous enforcement handles the rest: agents gated on "active" stop,
// and the teardown principal gated on "teardown" starts.
//
// The function patches the teardown principal's payload with the requested
// mode BEFORE updating the workspace status. Both changes arrive in the
// same /sync batch, so the daemon sees the correct payload when the
// teardown principal's condition becomes true.
func runDestroy(alias string, session *cli.SessionConfig, mode, serverName string) error {
	if err := principal.ValidateLocalpart(alias); err != nil {
		return fmt.Errorf("invalid workspace alias: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := session.Connect(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer sess.Close()

	// Resolve the workspace room.
	fullAlias := principal.RoomAlias(alias, serverName)
	workspaceRoomID, err := sess.ResolveAlias(ctx, fullAlias)
	if err != nil {
		return fmt.Errorf("resolve workspace room %s: %w", fullAlias, err)
	}

	// Read the current workspace state and verify status is "active".
	workspaceContent, err := sess.GetStateEvent(ctx, workspaceRoomID, schema.EventTypeWorkspace, "")
	if err != nil {
		return fmt.Errorf("reading workspace state: %w", err)
	}
	var workspaceState schema.WorkspaceState
	if err := json.Unmarshal(workspaceContent, &workspaceState); err != nil {
		return fmt.Errorf("parsing workspace state: %w", err)
	}
	if workspaceState.Status != "active" {
		return fmt.Errorf("workspace %s is in status %q, expected \"active\"", alias, workspaceState.Status)
	}

	// Transition the workspace to "teardown" with the requested mode.
	// The daemon's continuous enforcement handles the rest: agents gated
	// on "active" stop, and the teardown principal gated on "teardown"
	// starts. The teardown mode is carried in the workspace event itself
	// — the daemon captures this event content when the StartCondition
	// matches and delivers it as /run/bureau/trigger.json. No
	// MachineConfig patching needed.
	workspaceState.Status = "teardown"
	workspaceState.TeardownMode = mode
	workspaceState.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	_, err = sess.SendStateEvent(ctx, workspaceRoomID, schema.EventTypeWorkspace, "", workspaceState)
	if err != nil {
		return fmt.Errorf("publishing teardown status: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Workspace %s transitioning to teardown (mode=%s)\n", alias, mode)
	fmt.Fprintf(os.Stderr, "  Agents gated on \"active\" will stop on next reconcile cycle.\n")
	fmt.Fprintf(os.Stderr, "  Teardown principal will start and run the deinit pipeline.\n")

	return nil
}

// workspaceListParams holds the parameters for the workspace list command.
type workspaceListParams struct {
	OutputJSON bool `json:"-" flag:"json" desc:"output as JSON"`
}

func listCommand() *cli.Command {
	var params workspaceListParams

	return &cli.Command{
		Name:    "list",
		Summary: "List workspaces across the fleet",
		Description: `Query Matrix for rooms with workspace project configuration. Shows
the workspace alias, project, repository, and status.

This is a Matrix-only query — works from any machine without needing
access to the workspace filesystem.`,
		Flags: func() *pflag.FlagSet {
			return cli.FlagsFromParams("list", &params)
		},
		Params: func() any { return &params },
		Run: func(args []string) error {
			return runList(args, params.OutputJSON)
		},
	}
}

// workspaceInfo holds the display data for a single workspace, extracted
// from the Matrix room's state events.
type workspaceInfo struct {
	Alias      string `json:"alias"`
	Project    string `json:"project"`
	Repository string `json:"repository,omitempty"`
	Status     string `json:"status"`
}

func runList(args []string, outputJSON bool) error {
	// Use a 60-second timeout — this scans all joined rooms, issuing
	// one GetRoomState call per room. For a fleet with many rooms this
	// can take a while.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, operatorCancel, session, err := cli.ConnectOperator()
	if err != nil {
		return err
	}
	defer operatorCancel()
	defer session.Close()

	roomIDs, err := session.JoinedRooms(ctx)
	if err != nil {
		return fmt.Errorf("listing joined rooms: %w", err)
	}

	var workspaces []workspaceInfo
	for _, roomID := range roomIDs {
		workspace, err := extractWorkspaceInfo(ctx, session, roomID)
		if err != nil {
			// Skip rooms we can't read state for (permission errors,
			// rooms being tombstoned, etc.).
			continue
		}
		if workspace != nil {
			workspaces = append(workspaces, *workspace)
		}
	}

	if len(workspaces) == 0 {
		if outputJSON {
			return cli.WriteJSON([]workspaceInfo{})
		}
		fmt.Println("No workspaces found.")
		return nil
	}

	if outputJSON {
		return cli.WriteJSON(workspaces)
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "WORKSPACE\tPROJECT\tREPOSITORY\tSTATUS")
	for _, workspace := range workspaces {
		repository := workspace.Repository
		if repository == "" {
			repository = "-"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", workspace.Alias, workspace.Project, repository, workspace.Status)
	}
	writer.Flush()

	return nil
}

// extractWorkspaceInfo reads a room's state and returns workspace info if
// the room contains an m.bureau.project event. Returns nil (not an error)
// for non-workspace rooms.
func extractWorkspaceInfo(ctx context.Context, session *messaging.Session, roomID string) (*workspaceInfo, error) {
	events, err := session.GetRoomState(ctx, roomID)
	if err != nil {
		return nil, err
	}

	var project *schema.ProjectConfig
	var projectKey string
	var alias string
	var workspaceStatus string

	for _, event := range events {
		switch event.Type {
		case schema.EventTypeProject:
			// Event.Content is map[string]any — round-trip through JSON
			// to unmarshal into the typed struct.
			raw, marshalError := json.Marshal(event.Content)
			if marshalError != nil {
				continue
			}
			var config schema.ProjectConfig
			if json.Unmarshal(raw, &config) != nil {
				continue
			}
			project = &config
			if event.StateKey != nil {
				projectKey = *event.StateKey
			}

		case "m.room.canonical_alias":
			if aliasValue, ok := event.Content["alias"].(string); ok {
				alias = aliasValue
			}

		case schema.EventTypeWorkspace:
			if statusValue, ok := event.Content["status"].(string); ok {
				workspaceStatus = statusValue
			}
		}
	}

	if project == nil {
		return nil, nil
	}

	status := workspaceStatus
	if status == "" {
		status = "pending"
	}

	// Use the canonical alias (stripped of # and :server) for display.
	// Fall back to the project state key when no alias is set.
	displayAlias := alias
	if displayAlias == "" {
		displayAlias = projectKey
	} else {
		displayAlias = principal.RoomAliasLocalpart(displayAlias)
	}

	return &workspaceInfo{
		Alias:      displayAlias,
		Project:    project.WorkspacePath,
		Repository: project.Repository,
		Status:     status,
	}, nil
}

// aliasCommand builds a CLI command that takes a single positional alias
// argument and a --server-name flag, then delegates to the provided run
// function. This pattern is shared by status, du, fetch, and worktree list.
func aliasCommand(name, summary, description, usage string, run func(alias, serverName string) error) *cli.Command {
	var serverName string
	return &cli.Command{
		Name:        name,
		Summary:     summary,
		Description: description,
		Usage:       usage,
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet(name, pflag.ContinueOnError)
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("workspace alias is required\n\nUsage: %s", usage)
			}
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}
			return run(args[0], serverName)
		},
	}
}

func statusCommand() *cli.Command {
	return aliasCommand(
		"status",
		"Show detailed workspace status",
		`Query the hosting machine for detailed workspace status including
whether the workspace directory exists, has a bare repo, and its
current Matrix lifecycle state.

This is a remote command — the daemon on the hosting machine executes
it and replies via Matrix.`,
		"bureau workspace status <alias> [flags]",
		runStatus,
	)
}

func runStatus(alias, serverName string) error {
	ctx, cancel, session, err := cli.ConnectOperator()
	if err != nil {
		return err
	}
	defer cancel()
	defer session.Close()

	roomID, err := resolveWorkspaceRoom(ctx, session, alias, serverName)
	if err != nil {
		return err
	}

	eventID, requestID, err := sendWorkspaceCommand(ctx, session, roomID, "workspace.status", alias, nil)
	if err != nil {
		return err
	}

	result, err := waitForCommandResult(ctx, session, roomID, eventID, requestID)
	if err != nil {
		return err
	}

	if result.Status == "error" {
		return fmt.Errorf("daemon error: %s", result.Error)
	}

	// Display the result.
	workspace, _ := result.Result["workspace"].(string)
	exists, _ := result.Result["exists"].(bool)
	fmt.Printf("Workspace: %s\n", workspace)
	fmt.Printf("  exists: %v\n", exists)
	if exists {
		if hasBare, ok := result.Result["has_bare_repo"].(bool); ok && hasBare {
			fmt.Printf("  has_bare_repo: true\n")
		}
		if isDir, ok := result.Result["is_dir"].(bool); ok {
			fmt.Printf("  is_dir: %v\n", isDir)
		}
	}
	return nil
}

func duCommand() *cli.Command {
	return aliasCommand(
		"du",
		"Show workspace disk usage breakdown",
		`Show disk usage for workspace subdirectories: .bare/ (git objects),
each worktree, .shared/ (virtualenvs, build caches), and .cache/
(cross-project caches). Helps identify where disk space went.`,
		"bureau workspace du <alias> [flags]",
		runDu,
	)
}

func runDu(alias, serverName string) error {
	ctx, cancel, session, err := cli.ConnectOperator()
	if err != nil {
		return err
	}
	defer cancel()
	defer session.Close()

	roomID, err := resolveWorkspaceRoom(ctx, session, alias, serverName)
	if err != nil {
		return err
	}

	eventID, requestID, err := sendWorkspaceCommand(ctx, session, roomID, "workspace.du", alias, nil)
	if err != nil {
		return err
	}

	result, err := waitForCommandResult(ctx, session, roomID, eventID, requestID)
	if err != nil {
		return err
	}

	if result.Status == "error" {
		return fmt.Errorf("daemon error: %s", result.Error)
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	workspace, _ := result.Result["workspace"].(string)
	size, _ := result.Result["size"].(string)
	fmt.Fprintf(writer, "WORKSPACE\tSIZE\n")
	fmt.Fprintf(writer, "%s\t%s\n", workspace, size)
	writer.Flush()
	return nil
}

func worktreeCommand() *cli.Command {
	return &cli.Command{
		Name:    "worktree",
		Summary: "Manage git worktrees within a workspace",
		Description: `Add, remove, or list git worktrees in a workspace project.

The worktree alias extends the workspace alias: if "iree" is a workspace,
then "iree/feature/amdgpu" is a worktree within it. The CLI discovers
the parent workspace automatically by walking up the alias path.`,
		Subcommands: []*cli.Command{
			worktreeAddCommand(),
			worktreeRemoveCommand(),
			worktreeListCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Add a worktree for a feature branch",
				Command:     "bureau workspace worktree add iree/feature/amdgpu --branch feature/amdgpu-pm",
			},
			{
				Description: "Remove a worktree (archives uncommitted changes by default)",
				Command:     "bureau workspace worktree remove iree/feature/amdgpu",
			},
			{
				Description: "List all worktrees in a workspace",
				Command:     "bureau workspace worktree list iree",
			},
		},
	}
}

func worktreeAddCommand() *cli.Command {
	var (
		branch     string
		serverName string
	)

	return &cli.Command{
		Name:    "add",
		Summary: "Add a git worktree to a workspace",
		Description: `Create a new git worktree in a workspace project. The alias extends
the parent workspace alias — for workspace "iree", creating worktree
"iree/feature/amdgpu" adds a worktree at the "feature/amdgpu" subpath.

The daemon spawns a pipeline executor to perform the git operations
(worktree creation, submodule init, project-level setup scripts).
This is an async operation — the command returns immediately with
an "accepted" status.`,
		Usage: "bureau workspace worktree add <alias> [flags]",
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("worktree add", pflag.ContinueOnError)
			flagSet.StringVar(&branch, "branch", "", "git branch or commit to check out (empty for detached HEAD)")
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("worktree alias is required\n\nUsage: bureau workspace worktree add <alias>")
			}
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}
			return runWorktreeAdd(args[0], branch, serverName)
		},
	}
}

func runWorktreeAdd(alias, branch, serverName string) error {
	if err := principal.ValidateLocalpart(alias); err != nil {
		return fmt.Errorf("invalid worktree alias: %w", err)
	}

	ctx, cancel, session, err := cli.ConnectOperator()
	if err != nil {
		return err
	}
	defer cancel()
	defer session.Close()

	// Worktree operations route through the parent workspace room.
	// Worktrees don't get their own rooms — they're filesystem
	// artifacts, not organizational boundaries. If per-worktree rooms
	// become useful (e.g., for agent communication scoped to a
	// worktree), that's a separate information architecture decision.
	//
	// Walk up to find the parent workspace.
	workspaceRoomID, workspaceState, workspaceAlias, err := findParentWorkspace(ctx, session, alias, serverName)
	if err != nil {
		return err
	}

	if workspaceState.Status != "active" {
		return fmt.Errorf("workspace %s is in status %q (must be \"active\" to add worktrees)", workspaceAlias, workspaceState.Status)
	}

	// Derive the worktree subpath: the part of the alias after the
	// workspace alias prefix. findParentWorkspace always returns a
	// strict prefix (it strips at least one segment), but verify
	// the invariant explicitly rather than panicking on a bad slice.
	worktreeSubpath, err := extractSubpath(alias, workspaceAlias)
	if err != nil {
		return err
	}

	parameters := map[string]any{
		"path": worktreeSubpath,
	}
	if branch != "" {
		parameters["branch"] = branch
	}

	eventID, requestID, err := sendWorkspaceCommand(ctx, session, workspaceRoomID, "workspace.worktree.add", workspaceAlias, parameters)
	if err != nil {
		return err
	}

	// Wait for the "accepted" ack from the daemon.
	result, err := waitForCommandResult(ctx, session, workspaceRoomID, eventID, requestID)
	if err != nil {
		return err
	}

	if result.Status == "error" {
		return fmt.Errorf("daemon error: %s", result.Error)
	}

	principalName, _ := result.Result["principal"].(string)
	fmt.Fprintf(os.Stderr, "Worktree add accepted for %s\n", alias)
	fmt.Fprintf(os.Stderr, "  workspace: %s\n", workspaceAlias)
	fmt.Fprintf(os.Stderr, "  subpath:   %s\n", worktreeSubpath)
	if branch != "" {
		fmt.Fprintf(os.Stderr, "  branch:    %s\n", branch)
	}
	if principalName != "" {
		fmt.Fprintf(os.Stderr, "  executor:  %s\n", principalName)
		fmt.Fprintf(os.Stderr, "\nObserve progress: bureau observe %s\n", principalName)
	}
	return nil
}

func worktreeRemoveCommand() *cli.Command {
	var (
		mode       string
		serverName string
	)

	return &cli.Command{
		Name:    "remove",
		Summary: "Remove a git worktree from a workspace",
		Description: `Remove a git worktree from a workspace project. In archive mode
(the default), any uncommitted changes are committed to a timestamped
archive branch before removal, preserving work-in-progress. In delete
mode, the worktree is removed without preserving changes.

The daemon spawns a pipeline executor to perform the removal. This is
an async operation — the command returns immediately.`,
		Usage: "bureau workspace worktree remove <alias> [flags]",
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("worktree remove", pflag.ContinueOnError)
			flagSet.StringVar(&mode, "mode", "archive", "removal mode: archive (preserve uncommitted work) or delete")
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("worktree alias is required\n\nUsage: bureau workspace worktree remove <alias>")
			}
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}
			if mode != "archive" && mode != "delete" {
				return fmt.Errorf("--mode must be \"archive\" or \"delete\", got %q", mode)
			}
			return runWorktreeRemove(args[0], mode, serverName)
		},
	}
}

func runWorktreeRemove(alias, mode, serverName string) error {
	if err := principal.ValidateLocalpart(alias); err != nil {
		return fmt.Errorf("invalid worktree alias: %w", err)
	}

	ctx, cancel, session, err := cli.ConnectOperator()
	if err != nil {
		return err
	}
	defer cancel()
	defer session.Close()

	// Walk up to find the parent workspace.
	workspaceRoomID, workspaceState, workspaceAlias, err := findParentWorkspace(ctx, session, alias, serverName)
	if err != nil {
		return err
	}

	if workspaceState.Status != "active" {
		return fmt.Errorf("workspace %s is in status %q (must be \"active\" to remove worktrees)", workspaceAlias, workspaceState.Status)
	}

	worktreeSubpath, err := extractSubpath(alias, workspaceAlias)
	if err != nil {
		return err
	}

	parameters := map[string]any{
		"path": worktreeSubpath,
		"mode": mode,
	}

	eventID, requestID, err := sendWorkspaceCommand(ctx, session, workspaceRoomID, "workspace.worktree.remove", workspaceAlias, parameters)
	if err != nil {
		return err
	}

	result, err := waitForCommandResult(ctx, session, workspaceRoomID, eventID, requestID)
	if err != nil {
		return err
	}

	if result.Status == "error" {
		return fmt.Errorf("daemon error: %s", result.Error)
	}

	principalName, _ := result.Result["principal"].(string)
	fmt.Fprintf(os.Stderr, "Worktree remove accepted for %s (mode=%s)\n", alias, mode)
	fmt.Fprintf(os.Stderr, "  workspace: %s\n", workspaceAlias)
	fmt.Fprintf(os.Stderr, "  subpath:   %s\n", worktreeSubpath)
	if principalName != "" {
		fmt.Fprintf(os.Stderr, "  executor:  %s\n", principalName)
		fmt.Fprintf(os.Stderr, "\nObserve progress: bureau observe %s\n", principalName)
	}
	return nil
}

func fetchCommand() *cli.Command {
	return aliasCommand(
		"fetch",
		"Fetch latest changes for a workspace",
		`Run git fetch on the workspace's bare object store. Uses flock
coordination to prevent concurrent fetch conflicts when multiple
agents share the same .bare/ directory.

This is a remote command — the daemon on the hosting machine
executes it. Fetch can take minutes for large repos, so the poll
timeout is extended to 5 minutes.`,
		"bureau workspace fetch <alias> [flags]",
		runFetch,
	)
}

func runFetch(alias, serverName string) error {
	ctx, cancel, session, err := cli.ConnectOperator()
	if err != nil {
		return err
	}
	defer cancel()
	defer session.Close()

	roomID, err := resolveWorkspaceRoom(ctx, session, alias, serverName)
	if err != nil {
		return err
	}

	eventID, requestID, err := sendWorkspaceCommand(ctx, session, roomID, "workspace.fetch", alias, nil)
	if err != nil {
		return err
	}

	// Poll with a 5-minute timeout — fetch can be slow for large repos.
	// The ConnectOperator context has a 30s deadline which is too short
	// for the polling phase, so create a new context here.
	pollCtx, pollCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer pollCancel()

	fmt.Fprintf(os.Stderr, "Fetching %s (this may take a while for large repos)...\n", alias)

	result, err := waitForCommandResult(pollCtx, session, roomID, eventID, requestID)
	if err != nil {
		return err
	}

	if result.Status == "error" {
		return fmt.Errorf("daemon error: %s", result.Error)
	}

	output, _ := result.Result["output"].(string)
	if output != "" {
		fmt.Println(output)
	}
	fmt.Fprintf(os.Stderr, "Fetch complete (%dms)\n", result.DurationMS)
	return nil
}

func worktreeListCommand() *cli.Command {
	return aliasCommand(
		"list",
		"List git worktrees in a workspace",
		`List all git worktrees in a workspace's .bare directory. Shows the
raw git worktree list output including paths and branch information.`,
		"bureau workspace worktree list <alias> [flags]",
		runWorktreeList,
	)
}

func runWorktreeList(alias, serverName string) error {
	ctx, cancel, session, err := cli.ConnectOperator()
	if err != nil {
		return err
	}
	defer cancel()
	defer session.Close()

	roomID, err := resolveWorkspaceRoom(ctx, session, alias, serverName)
	if err != nil {
		return err
	}

	eventID, requestID, err := sendWorkspaceCommand(ctx, session, roomID, "workspace.worktree.list", alias, nil)
	if err != nil {
		return err
	}

	result, err := waitForCommandResult(ctx, session, roomID, eventID, requestID)
	if err != nil {
		return err
	}

	if result.Status == "error" {
		return fmt.Errorf("daemon error: %s", result.Error)
	}

	worktrees, _ := result.Result["worktrees"].([]any)
	if len(worktrees) == 0 {
		fmt.Println("No worktrees found.")
		return nil
	}

	for _, worktree := range worktrees {
		if line, ok := worktree.(string); ok {
			fmt.Println(line)
		}
	}
	return nil
}
