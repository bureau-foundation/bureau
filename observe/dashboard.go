// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import "fmt"

// Dashboard creates a local tmux session for a composite observation
// view. It reads the layout (typically from a Matrix room state event)
// and creates a local tmux session with windows and panes: each
// "observe" pane runs a bureau-observe process connected to the target
// principal, each "command" pane runs the specified local command.
//
// serverSocket is the local tmux server socket path for the dashboard
// session. This can be the user's default tmux server (dashboards are
// local, not Bureau-managed sessions).
//
// sessionName is the local tmux session name to create
// (e.g., "observe/iree/amdgpu/general").
//
// daemonSocket is the path to the daemon's observation socket, passed
// to bureau-observe processes spawned in observe panes.
//
// Dashboard blocks until the local tmux session is created and all
// panes are started. It does not wait for the session to end.
func Dashboard(serverSocket, sessionName, daemonSocket string, layout *Layout) error {
	if layout == nil {
		return fmt.Errorf("layout is nil")
	}
	if len(layout.Windows) == 0 {
		return fmt.Errorf("layout has no windows")
	}
	if daemonSocket == "" {
		return fmt.Errorf("daemon socket path is empty")
	}

	// Resolve the layout into concrete commands. Observe panes become
	// bureau observe invocations with the daemon socket; command panes
	// pass through unchanged; role panes become informational shells.
	resolved := resolveLayout(layout, daemonSocket)

	return ApplyLayout(serverSocket, sessionName, resolved)
}

// resolveLayout creates a copy of the layout with all pane types
// resolved into concrete Command strings suitable for tmux pane
// execution. The original layout is not modified.
//
// Resolution rules:
//   - Observe panes → `bureau observe <principal> --socket <daemonSocket>`
//   - Command panes → unchanged (command string passes through)
//   - Role panes → informational echo (the launcher resolves roles to
//     concrete commands; in a dashboard context, we show what role this
//     pane represents)
//   - ObserveMembers panes → skipped (the daemon expands these
//     dynamically; Dashboard receives an already-expanded layout)
//   - Empty panes (no type set) → default shell
func resolveLayout(layout *Layout, daemonSocket string) *Layout {
	resolved := &Layout{
		Prefix:  layout.Prefix,
		Windows: make([]Window, len(layout.Windows)),
	}

	for windowIndex, window := range layout.Windows {
		resolvedWindow := Window{
			Name:  window.Name,
			Panes: make([]Pane, len(window.Panes)),
		}

		for paneIndex, pane := range window.Panes {
			resolvedPane := Pane{
				Split: pane.Split,
				Size:  pane.Size,
			}

			switch {
			case pane.Observe != "":
				resolvedPane.Command = fmt.Sprintf(
					"bureau observe %s --socket %s",
					pane.Observe, daemonSocket)
			case pane.Command != "":
				resolvedPane.Command = pane.Command
			case pane.Role != "":
				// In a dashboard, role panes show their identity.
				// The launcher would resolve this to a real command,
				// but dashboards are observer-side composite views
				// where role information is display-only.
				resolvedPane.Command = fmt.Sprintf(
					"echo '[bureau] role: %s' && exec cat",
					pane.Role)
			}
			// Empty panes (no type set) get no command, which means
			// tmux will start the default shell.

			resolvedWindow.Panes[paneIndex] = resolvedPane
		}

		resolved.Windows[windowIndex] = resolvedWindow
	}

	return resolved
}
