// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

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
	return &cli.Command{
		Name:    "destroy",
		Summary: "Tear down a workspace",
		Description: `Tear down a workspace on the hosting machine. Executes the
dev-workspace-deinit pipeline, which checks for uncommitted changes,
archives the data (with --mode archive, the default), or removes
everything (with --mode delete). The workspace state event is updated
to "archived" or "removed" to signal that the workspace is no longer
active.

The Matrix room is preserved by default — its message history remains
accessible. Use "bureau matrix room leave" separately to remove the
room.`,
		Run: func(args []string) error {
			return cli.ErrNotImplemented("workspace destroy")
		},
	}
}

func listCommand() *cli.Command {
	return &cli.Command{
		Name:    "list",
		Summary: "List workspaces across the fleet",
		Description: `Query Matrix for rooms with workspace project configuration. Shows
the workspace alias, project, repository, and status.

This is a Matrix-only query — works from any machine without needing
access to the workspace filesystem.`,
		Run: runList,
	}
}

// workspaceInfo holds the display data for a single workspace, extracted
// from the Matrix room's state events.
type workspaceInfo struct {
	Alias      string
	Project    string
	Repository string
	Status     string
}

func runList(args []string) error {
	operatorSession, err := cli.LoadSession()
	if err != nil {
		return err
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: operatorSession.Homeserver,
	})
	if err != nil {
		return fmt.Errorf("creating Matrix client: %w", err)
	}

	session, err := client.SessionFromToken(operatorSession.UserID, operatorSession.AccessToken)
	if err != nil {
		return fmt.Errorf("creating Matrix session: %w", err)
	}
	defer session.Close()

	ctx := context.Background()

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

func statusCommand() *cli.Command {
	return &cli.Command{
		Name:    "status",
		Summary: "Show detailed workspace status",
		Description: `Query the hosting machine for detailed workspace status: disk usage,
git status per worktree, running principals, and last activity.

This is a remote command — the daemon on the hosting machine executes
it and replies via Matrix.`,
		Run: func(args []string) error {
			return cli.ErrNotImplemented("workspace status")
		},
	}
}

func duCommand() *cli.Command {
	return &cli.Command{
		Name:    "du",
		Summary: "Show workspace disk usage breakdown",
		Description: `Show disk usage for workspace subdirectories: .bare/ (git objects),
each worktree, .shared/ (virtualenvs, build caches), and .cache/
(cross-project caches). Helps identify where disk space went.`,
		Run: func(args []string) error {
			return cli.ErrNotImplemented("workspace du")
		},
	}
}

func worktreeCommand() *cli.Command {
	return &cli.Command{
		Name:    "worktree",
		Summary: "Manage git worktrees within a workspace",
		Description: `Add or remove git worktrees in a workspace project. Adding a worktree
creates both the git worktree on the hosting machine and a Matrix
room for the new workspace path.`,
		Subcommands: []*cli.Command{
			worktreeAddCommand(),
			worktreeRemoveCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Add a worktree for a feature branch",
				Command:     "bureau workspace worktree add iree/amdgpu/pm --branch feature/amdgpu-pm",
			},
			{
				Description: "Remove a worktree (checks for uncommitted changes)",
				Command:     "bureau workspace worktree remove iree/amdgpu/pm",
			},
		},
	}
}

func worktreeAddCommand() *cli.Command {
	return &cli.Command{
		Name:    "add",
		Summary: "Add a git worktree to a workspace",
		Description: `Create a new git worktree in a workspace project. Creates the
worktree on the hosting machine (via the daemon) and the
corresponding Matrix room.`,
		Run: func(args []string) error {
			return cli.ErrNotImplemented("workspace worktree add")
		},
	}
}

func worktreeRemoveCommand() *cli.Command {
	return &cli.Command{
		Name:    "remove",
		Summary: "Remove a git worktree from a workspace",
		Description: `Remove a git worktree from a workspace project. Checks for
uncommitted changes before removing. Also removes the Matrix room
if it has no other purpose.`,
		Run: func(args []string) error {
			return cli.ErrNotImplemented("workspace worktree remove")
		},
	}
}

func fetchCommand() *cli.Command {
	return &cli.Command{
		Name:    "fetch",
		Summary: "Fetch latest changes for a workspace",
		Description: `Run git fetch on the workspace's bare object store. Uses flock
coordination to prevent concurrent fetch conflicts when multiple
agents share the same .bare/ directory.

This is a remote command — the daemon on the hosting machine
executes it.`,
		Run: func(args []string) error {
			return cli.ErrNotImplemented("workspace fetch")
		},
	}
}
