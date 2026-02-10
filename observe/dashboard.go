// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

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
	// Implementation in a future dashboard bead.
	panic("observe.Dashboard not yet implemented")
}
