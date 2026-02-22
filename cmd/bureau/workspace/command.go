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

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/command"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/workspace"
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

// DestroyParams holds the parameters for workspace destruction. Callers
// that use the programmatic API pass these directly; CLI callers build
// them from flags.
type DestroyParams struct {
	// Alias is the workspace alias (e.g., "iree/amdgpu/inference").
	Alias string

	// Mode is the teardown mode: "archive" or "delete".
	Mode string

	// ServerName is the Matrix server name.
	ServerName ref.ServerName
}

// destroyParams holds the CLI-specific parameters for the workspace destroy command.
type destroyParams struct {
	cli.SessionConfig
	Mode       string `json:"mode"        flag:"mode"        desc:"teardown mode: archive or delete" default:"archive"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
}

func destroyCommand() *cli.Command {
	var params destroyParams

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
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/workspace/destroy"},
		Annotations:    cli.Destructive(),
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("workspace alias is required\n\nUsage: bureau workspace destroy <alias> [flags]")
			}
			if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}
			if params.Mode != "archive" && params.Mode != "delete" {
				return cli.Validation("--mode must be \"archive\" or \"delete\", got %q", params.Mode)
			}

			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return fmt.Errorf("invalid --server-name: %w", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			session, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return cli.Internal("connect: %w", err)
			}
			defer session.Close()

			err = Destroy(ctx, session, DestroyParams{
				Alias:      args[0],
				Mode:       params.Mode,
				ServerName: serverName,
			})
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "Workspace %s transitioning to teardown (mode=%s)\n", args[0], params.Mode)
			fmt.Fprintf(os.Stderr, "  Agents gated on \"active\" will stop on next reconcile cycle.\n")
			fmt.Fprintf(os.Stderr, "  Teardown principal will start and run the deinit pipeline.\n")
			return nil
		},
	}
}

// Destroy transitions a workspace to "teardown" status. The daemon's
// continuous enforcement handles the rest: agents gated on "active" stop,
// and the teardown principal gated on "teardown" starts.
//
// The teardown mode is carried in the workspace event itself — the daemon
// captures this event content when the StartCondition matches and delivers
// it as /run/bureau/trigger.json.
//
// The caller provides a connected session and a context with an appropriate
// deadline. This function does not create its own session.
func Destroy(ctx context.Context, session messaging.Session, params DestroyParams) error {
	if err := principal.ValidateLocalpart(params.Alias); err != nil {
		return cli.Validation("invalid workspace alias: %w", err)
	}

	// Resolve the workspace room.
	fullAlias, err := ref.ParseRoomAlias(schema.FullRoomAlias(params.Alias, params.ServerName))
	if err != nil {
		return cli.Validation("invalid room alias %q: %w", params.Alias, err)
	}
	workspaceRoomID, err := session.ResolveAlias(ctx, fullAlias)
	if err != nil {
		return cli.NotFound("resolve workspace room %s: %w", fullAlias, err)
	}

	// Read the current workspace state and verify status is "active".
	workspaceContent, err := session.GetStateEvent(ctx, workspaceRoomID, schema.EventTypeWorkspace, "")
	if err != nil {
		return cli.Internal("reading workspace state: %w", err)
	}
	var workspaceState workspace.WorkspaceState
	if err := json.Unmarshal(workspaceContent, &workspaceState); err != nil {
		return cli.Internal("parsing workspace state: %w", err)
	}
	if workspaceState.Status != "active" {
		return cli.Conflict("workspace %s is in status %q, expected \"active\"", params.Alias, workspaceState.Status)
	}

	// Transition the workspace to "teardown" with the requested mode.
	// The daemon's continuous enforcement handles the rest: agents gated
	// on "active" stop, and the teardown principal gated on "teardown"
	// starts.
	workspaceState.Status = "teardown"
	workspaceState.TeardownMode = params.Mode
	workspaceState.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	_, err = session.SendStateEvent(ctx, workspaceRoomID, schema.EventTypeWorkspace, "", workspaceState)
	if err != nil {
		return cli.Internal("publishing teardown status: %w", err)
	}

	return nil
}

// workspaceListParams holds the parameters for the workspace list command.
type workspaceListParams struct {
	cli.JSONOutput
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
		Params:         func() any { return &params },
		Output:         func() any { return &[]workspaceInfo{} },
		RequiredGrants: []string{"command/workspace/list"},
		Annotations:    cli.ReadOnly(),
		Run: func(args []string) error {
			return runList(args, &params.JSONOutput)
		},
	}
}

// workspaceInfo holds the display data for a single workspace, extracted
// from the Matrix room's state events.
type workspaceInfo struct {
	Alias      string `json:"alias"                desc:"workspace alias"`
	Project    string `json:"project"              desc:"project name"`
	Repository string `json:"repository,omitempty" desc:"git repository URL"`
	Status     string `json:"status"               desc:"workspace lifecycle status"`
}

func runList(args []string, jsonOutput *cli.JSONOutput) error {
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
		return cli.Internal("listing joined rooms: %w", err)
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

	if done, err := jsonOutput.EmitJSON(workspaces); done {
		return err
	}

	if len(workspaces) == 0 {
		fmt.Println("No workspaces found.")
		return nil
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
func extractWorkspaceInfo(ctx context.Context, session messaging.Session, roomID ref.RoomID) (*workspaceInfo, error) {
	events, err := session.GetRoomState(ctx, roomID)
	if err != nil {
		return nil, err
	}

	var project *workspace.ProjectConfig
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
			var config workspace.ProjectConfig
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

// workspaceAliasParams holds common parameters for workspace commands that take
// a single positional alias argument and a server name.
type workspaceAliasParams struct {
	cli.JSONOutput
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
}

// aliasRunFunc is the signature for workspace commands that operate on
// a single alias with the standard server-name and json flags.
type aliasRunFunc func(alias string, serverName ref.ServerName, jsonOutput *cli.JSONOutput) error

// aliasCommand constructs a workspace subcommand that takes a single
// positional alias argument and the standard workspaceAliasParams flags.
func aliasCommand(name, summary, description, usage, grant string, annotations *cli.ToolAnnotations, run aliasRunFunc) *cli.Command {
	var params workspaceAliasParams
	return &cli.Command{
		Name:           name,
		Summary:        summary,
		Description:    description,
		Usage:          usage,
		RequiredGrants: []string{grant},
		Annotations:    annotations,
		Params:         func() any { return &params },
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("workspace alias is required\n\nUsage: %s", usage)
			}
			if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}
			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return fmt.Errorf("invalid --server-name: %w", err)
			}
			return run(args[0], serverName, &params.JSONOutput)
		},
	}
}

func statusCommand() *cli.Command {
	return aliasCommand("status",
		"Show detailed workspace status",
		`Query the hosting machine for detailed workspace status including
whether the workspace directory exists, has a bare repo, and its
current Matrix lifecycle state.

This is a remote command — the daemon on the hosting machine executes
it and replies via Matrix.`,
		"bureau workspace status <alias> [flags]",
		"command/workspace/status",
		cli.ReadOnly(),
		runStatus)
}

func runStatus(alias string, serverName ref.ServerName, jsonOutput *cli.JSONOutput) error {
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

	result, err := command.Execute(ctx, command.SendParams{
		Session:   session,
		RoomID:    roomID,
		Command:   "workspace.status",
		Workspace: alias,
	})
	if err != nil {
		return err
	}
	if err := result.Err(); err != nil {
		return err
	}

	if done, err := jsonOutput.EmitJSON(json.RawMessage(result.ResultData)); done {
		return err
	}

	var statusResult map[string]any
	if err := result.UnmarshalResult(&statusResult); err != nil {
		return cli.Internal("parsing status result: %v", err)
	}

	workspaceName, _ := statusResult["workspace"].(string)
	exists, _ := statusResult["exists"].(bool)
	fmt.Printf("Workspace: %s\n", workspaceName)
	fmt.Printf("  exists: %v\n", exists)
	if exists {
		if hasBare, ok := statusResult["has_bare_repo"].(bool); ok && hasBare {
			fmt.Printf("  has_bare_repo: true\n")
		}
		if isDir, ok := statusResult["is_dir"].(bool); ok {
			fmt.Printf("  is_dir: %v\n", isDir)
		}
	}
	return nil
}

func duCommand() *cli.Command {
	return aliasCommand("du",
		"Show workspace disk usage breakdown",
		`Show disk usage for workspace subdirectories: .bare/ (git objects),
each worktree, .shared/ (virtualenvs, build caches), and .cache/
(cross-project caches). Helps identify where disk space went.`,
		"bureau workspace du <alias> [flags]",
		"command/workspace/du",
		cli.ReadOnly(),
		runDu)
}

func runDu(alias string, serverName ref.ServerName, jsonOutput *cli.JSONOutput) error {
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

	result, err := command.Execute(ctx, command.SendParams{
		Session:   session,
		RoomID:    roomID,
		Command:   "workspace.du",
		Workspace: alias,
	})
	if err != nil {
		return err
	}
	if err := result.Err(); err != nil {
		return err
	}

	if done, err := jsonOutput.EmitJSON(json.RawMessage(result.ResultData)); done {
		return err
	}

	var duResult map[string]any
	if err := result.UnmarshalResult(&duResult); err != nil {
		return cli.Internal("parsing du result: %v", err)
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	workspaceName, _ := duResult["workspace"].(string)
	size, _ := duResult["size"].(string)
	fmt.Fprintf(writer, "WORKSPACE\tSIZE\n")
	fmt.Fprintf(writer, "%s\t%s\n", workspaceName, size)
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

// worktreeAddParams holds the parameters for the workspace worktree add command.
type worktreeAddParams struct {
	cli.JSONOutput
	Branch     string `json:"branch"      flag:"branch"      desc:"git branch or commit to check out (empty for detached HEAD)"`
	Wait       bool   `json:"wait"        flag:"wait"        desc:"wait for the setup pipeline to complete" default:"true"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
}

// worktreeAddResult is the JSON output for workspace worktree add.
type worktreeAddResult struct {
	Alias         string       `json:"alias"                      desc:"worktree alias"`
	Workspace     string       `json:"workspace"                  desc:"parent workspace alias"`
	Subpath       string       `json:"subpath"                    desc:"worktree subpath within workspace"`
	Branch        string       `json:"branch,omitempty"           desc:"git branch or commit checked out"`
	PrincipalName string       `json:"principal_name,omitempty"   desc:"executor principal name"`
	TicketID      ref.TicketID `json:"ticket_id,omitempty"        desc:"pipeline ticket ID"`
	Conclusion    string       `json:"conclusion,omitempty"       desc:"pipeline conclusion (when --wait)"`
}

func worktreeAddCommand() *cli.Command {
	var params worktreeAddParams

	return &cli.Command{
		Name:    "add",
		Summary: "Add a git worktree to a workspace",
		Description: `Create a new git worktree in a workspace project. The alias extends
the parent workspace alias — for workspace "iree", creating worktree
"iree/feature/amdgpu" adds a worktree at the "feature/amdgpu" subpath.

The daemon spawns a pipeline executor to perform the git operations
(worktree creation, submodule init, project-level setup scripts).
By default, the command waits for the pipeline to complete, printing
step progress. Use --no-wait to return immediately after acceptance.`,
		Usage:          "bureau workspace worktree add <alias> [flags]",
		Params:         func() any { return &params },
		Output:         func() any { return &worktreeAddResult{} },
		RequiredGrants: []string{"command/workspace/worktree/add"},
		Annotations:    cli.Create(),
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("worktree alias is required\n\nUsage: bureau workspace worktree add <alias>")
			}
			if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}
			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return fmt.Errorf("invalid --server-name: %w", err)
			}
			return runWorktreeAdd(args[0], params.Branch, params.Wait, serverName, &params.JSONOutput)
		},
	}
}

func runWorktreeAdd(alias, branch string, wait bool, serverName ref.ServerName, jsonOutput *cli.JSONOutput) error {
	if err := principal.ValidateLocalpart(alias); err != nil {
		return cli.Validation("invalid worktree alias: %w", err)
	}

	ctx, cancel, session, err := cli.ConnectOperator()
	if err != nil {
		return err
	}
	defer cancel()
	defer session.Close()

	// Worktree operations route through the parent workspace room.
	// Worktrees don't get their own rooms — they're filesystem
	// artifacts, not organizational boundaries.
	workspaceRoomID, workspaceState, workspaceAlias, err := findParentWorkspace(ctx, session, alias, serverName)
	if err != nil {
		return err
	}

	if workspaceState.Status != "active" {
		return cli.Conflict("workspace %s is in status %q (must be \"active\" to add worktrees)", workspaceAlias, workspaceState.Status)
	}

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

	future, err := command.Send(ctx, command.SendParams{
		Session:    session,
		RoomID:     workspaceRoomID,
		Command:    "workspace.worktree.add",
		Workspace:  workspaceAlias,
		Parameters: parameters,
	})
	if err != nil {
		return err
	}
	defer future.Discard()

	result, err := future.Wait(ctx)
	if err != nil {
		return err
	}
	if err := result.Err(); err != nil {
		return err
	}

	// When --wait is set and the daemon returned a ticket, watch
	// the pipeline until it completes. Use a 5-minute context —
	// worktree setup pipelines run git clone + submodule init which
	// can take a while for large repos.
	conclusion := ""
	if wait && !result.TicketID.IsZero() {
		fmt.Fprintf(os.Stderr, "Worktree add accepted for %s, waiting for pipeline...\n", alias)

		watchCtx, watchCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer watchCancel()

		final, watchError := command.WatchTicket(watchCtx, command.WatchTicketParams{
			Session:    session,
			RoomID:     result.TicketRoom,
			TicketID:   result.TicketID,
			OnProgress: command.StepProgressWriter(os.Stderr),
		})
		if watchError != nil {
			return watchError
		}
		if final.Pipeline != nil {
			conclusion = final.Pipeline.Conclusion
		}
	}

	outputResult := worktreeAddResult{
		Alias:         alias,
		Workspace:     workspaceAlias,
		Subpath:       worktreeSubpath,
		Branch:        branch,
		PrincipalName: result.Principal,
		TicketID:      result.TicketID,
		Conclusion:    conclusion,
	}
	if done, err := jsonOutput.EmitJSON(outputResult); done {
		return err
	}

	if conclusion != "" {
		// Waited for completion — print the outcome.
		fmt.Fprintf(os.Stderr, "Worktree %s %s\n", alias, conclusion)
	} else {
		// Fire-and-forget — print acceptance info with follow-up hint.
		fmt.Fprintf(os.Stderr, "Worktree add accepted for %s\n", alias)
		fmt.Fprintf(os.Stderr, "  workspace: %s\n", workspaceAlias)
		fmt.Fprintf(os.Stderr, "  subpath:   %s\n", worktreeSubpath)
		if branch != "" {
			fmt.Fprintf(os.Stderr, "  branch:    %s\n", branch)
		}
		result.WriteAcceptedHint(os.Stderr)
	}

	if conclusion != "" && conclusion != "success" {
		return &cli.ExitError{Code: 1}
	}
	return nil
}

// worktreeRemoveParams holds the parameters for the workspace worktree remove command.
type worktreeRemoveParams struct {
	cli.JSONOutput
	Mode       string `json:"mode"        flag:"mode"        desc:"removal mode: archive (preserve uncommitted work) or delete" default:"archive"`
	Wait       bool   `json:"wait"        flag:"wait"        desc:"wait for the removal pipeline to complete" default:"true"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
}

// worktreeRemoveResult is the JSON output for workspace worktree remove.
type worktreeRemoveResult struct {
	Alias         string       `json:"alias"                      desc:"worktree alias"`
	Workspace     string       `json:"workspace"                  desc:"parent workspace alias"`
	Subpath       string       `json:"subpath"                    desc:"worktree subpath within workspace"`
	Mode          string       `json:"mode"                       desc:"removal mode (archive or delete)"`
	PrincipalName string       `json:"principal_name,omitempty"   desc:"executor principal name"`
	TicketID      ref.TicketID `json:"ticket_id,omitempty"        desc:"pipeline ticket ID"`
	Conclusion    string       `json:"conclusion,omitempty"       desc:"pipeline conclusion (when --wait)"`
}

func worktreeRemoveCommand() *cli.Command {
	var params worktreeRemoveParams

	return &cli.Command{
		Name:    "remove",
		Summary: "Remove a git worktree from a workspace",
		Description: `Remove a git worktree from a workspace project. In archive mode
(the default), any uncommitted changes are committed to a timestamped
archive branch before removal, preserving work-in-progress. In delete
mode, the worktree is removed without preserving changes.

The daemon spawns a pipeline executor to perform the removal. By
default, the command waits for the pipeline to complete. Use --no-wait
to return immediately after acceptance.`,
		Usage:          "bureau workspace worktree remove <alias> [flags]",
		Params:         func() any { return &params },
		Output:         func() any { return &worktreeRemoveResult{} },
		RequiredGrants: []string{"command/workspace/worktree/remove"},
		Annotations:    cli.Destructive(),
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("worktree alias is required\n\nUsage: bureau workspace worktree remove <alias>")
			}
			if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}
			if params.Mode != "archive" && params.Mode != "delete" {
				return cli.Validation("--mode must be \"archive\" or \"delete\", got %q", params.Mode)
			}
			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return fmt.Errorf("invalid --server-name: %w", err)
			}
			return runWorktreeRemove(args[0], params.Mode, params.Wait, serverName, &params.JSONOutput)
		},
	}
}

func runWorktreeRemove(alias, mode string, wait bool, serverName ref.ServerName, jsonOutput *cli.JSONOutput) error {
	if err := principal.ValidateLocalpart(alias); err != nil {
		return cli.Validation("invalid worktree alias: %w", err)
	}

	ctx, cancel, session, err := cli.ConnectOperator()
	if err != nil {
		return err
	}
	defer cancel()
	defer session.Close()

	workspaceRoomID, workspaceState, workspaceAlias, err := findParentWorkspace(ctx, session, alias, serverName)
	if err != nil {
		return err
	}

	if workspaceState.Status != "active" {
		return cli.Conflict("workspace %s is in status %q (must be \"active\" to remove worktrees)", workspaceAlias, workspaceState.Status)
	}

	worktreeSubpath, err := extractSubpath(alias, workspaceAlias)
	if err != nil {
		return err
	}

	future, err := command.Send(ctx, command.SendParams{
		Session:   session,
		RoomID:    workspaceRoomID,
		Command:   "workspace.worktree.remove",
		Workspace: workspaceAlias,
		Parameters: map[string]any{
			"path": worktreeSubpath,
			"mode": mode,
		},
	})
	if err != nil {
		return err
	}
	defer future.Discard()

	result, err := future.Wait(ctx)
	if err != nil {
		return err
	}
	if err := result.Err(); err != nil {
		return err
	}

	// When --wait is set and the daemon returned a ticket, watch
	// the pipeline until it completes.
	conclusion := ""
	if wait && !result.TicketID.IsZero() {
		fmt.Fprintf(os.Stderr, "Worktree remove accepted for %s, waiting for pipeline...\n", alias)

		watchCtx, watchCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer watchCancel()

		final, watchError := command.WatchTicket(watchCtx, command.WatchTicketParams{
			Session:    session,
			RoomID:     result.TicketRoom,
			TicketID:   result.TicketID,
			OnProgress: command.StepProgressWriter(os.Stderr),
		})
		if watchError != nil {
			return watchError
		}
		if final.Pipeline != nil {
			conclusion = final.Pipeline.Conclusion
		}
	}

	outputResult := worktreeRemoveResult{
		Alias:         alias,
		Workspace:     workspaceAlias,
		Subpath:       worktreeSubpath,
		Mode:          mode,
		PrincipalName: result.Principal,
		TicketID:      result.TicketID,
		Conclusion:    conclusion,
	}
	if done, err := jsonOutput.EmitJSON(outputResult); done {
		return err
	}

	if conclusion != "" {
		fmt.Fprintf(os.Stderr, "Worktree %s removed (%s)\n", alias, conclusion)
	} else {
		fmt.Fprintf(os.Stderr, "Worktree remove accepted for %s (mode=%s)\n", alias, mode)
		fmt.Fprintf(os.Stderr, "  workspace: %s\n", workspaceAlias)
		fmt.Fprintf(os.Stderr, "  subpath:   %s\n", worktreeSubpath)
		result.WriteAcceptedHint(os.Stderr)
	}

	if conclusion != "" && conclusion != "success" {
		return &cli.ExitError{Code: 1}
	}
	return nil
}

func fetchCommand() *cli.Command {
	return aliasCommand("fetch",
		"Fetch latest changes for a workspace",
		`Run git fetch on the workspace's bare object store. Uses flock
coordination to prevent concurrent fetch conflicts when multiple
agents share the same .bare/ directory.

This is a remote command — the daemon on the hosting machine
executes it. Fetch can take minutes for large repos, so the poll
timeout is extended to 5 minutes.`,
		"bureau workspace fetch <alias> [flags]",
		"command/workspace/fetch",
		cli.Idempotent(),
		runFetch)
}

func runFetch(alias string, serverName ref.ServerName, jsonOutput *cli.JSONOutput) error {
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

	fmt.Fprintf(os.Stderr, "Fetching %s (this may take a while for large repos)...\n", alias)

	// Fetch can take minutes for large repos. ConnectOperator gives
	// a 30s context which is too short for waiting on the result, so
	// create a longer context for the command execution. The /sync
	// long-poll uses 30s server-side holds internally, so even a
	// 5-minute wait makes at most ~10 HTTP round-trips.
	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer fetchCancel()

	result, err := command.Execute(fetchCtx, command.SendParams{
		Session:   session,
		RoomID:    roomID,
		Command:   "workspace.fetch",
		Workspace: alias,
	})
	if err != nil {
		return err
	}
	if err := result.Err(); err != nil {
		return err
	}

	if done, err := jsonOutput.EmitJSON(json.RawMessage(result.ResultData)); done {
		return err
	}

	var fetchResult map[string]any
	if err := result.UnmarshalResult(&fetchResult); err != nil {
		return cli.Internal("parsing fetch result: %v", err)
	}

	output, _ := fetchResult["output"].(string)
	if output != "" {
		fmt.Println(output)
	}
	fmt.Fprintf(os.Stderr, "Fetch complete (%dms)\n", result.DurationMS)
	return nil
}

func worktreeListCommand() *cli.Command {
	return aliasCommand("list",
		"List git worktrees in a workspace",
		`List all git worktrees in a workspace's .bare directory. Shows the
raw git worktree list output including paths and branch information.`,
		"bureau workspace worktree list <alias> [flags]",
		"command/workspace/worktree/list",
		cli.ReadOnly(),
		runWorktreeList)
}

func runWorktreeList(alias string, serverName ref.ServerName, jsonOutput *cli.JSONOutput) error {
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

	result, err := command.Execute(ctx, command.SendParams{
		Session:   session,
		RoomID:    roomID,
		Command:   "workspace.worktree.list",
		Workspace: alias,
	})
	if err != nil {
		return err
	}
	if err := result.Err(); err != nil {
		return err
	}

	if done, err := jsonOutput.EmitJSON(json.RawMessage(result.ResultData)); done {
		return err
	}

	var listResult map[string]any
	if err := result.UnmarshalResult(&listResult); err != nil {
		return cli.Internal("parsing worktree list result: %v", err)
	}

	worktrees, _ := listResult["worktrees"].([]any)
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
