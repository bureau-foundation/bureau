// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

// Layout describes the tmux session structure for an observation target.
// This is the Go representation of the m.bureau.layout Matrix state event
// content. It is used both for reading live tmux state and for applying
// desired state from Matrix.
type Layout struct {
	// Prefix is the tmux prefix key for this session (e.g., "C-a").
	// Empty means use the Bureau default (Ctrl-a).
	Prefix string `json:"prefix,omitempty"`

	// Windows is the ordered list of tmux windows in the session.
	Windows []Window `json:"windows"`
}

// Window describes a single tmux window containing one or more panes.
type Window struct {
	// Name is the tmux window name (displayed in the status bar).
	Name string `json:"name"`

	// Panes is the ordered list of panes in this window. The first pane
	// is the root; subsequent panes are splits from it.
	Panes []Pane `json:"panes"`
}

// Pane describes a single pane within a tmux window. Exactly one of
// Observe, Command, or Role must be set.
type Pane struct {
	// Observe is the principal localpart to observe in this pane
	// (e.g., "iree/amdgpu/pm"). When set, the pane runs bureau-observe
	// connected to this principal. Used in channel/room layouts for
	// composite views.
	Observe string `json:"observe,omitempty"`

	// Command is a shell command to run in this pane. Used for local
	// tooling (beads-tui, dashboards, shells) in composite layouts.
	Command string `json:"command,omitempty"`

	// Role identifies the pane's purpose in a principal's own layout
	// (e.g., "agent", "shell"). The launcher resolves roles to concrete
	// commands based on the agent template. Used in principal layouts.
	Role string `json:"role,omitempty"`

	// ObserveMembers dynamically populates panes from room membership.
	// When set, the daemon creates one pane per room member matching
	// the filter. Used in channel layouts for auto-scaling views.
	ObserveMembers *MemberFilter `json:"observe_members,omitempty"`

	// Split is the split direction from the previous pane: "horizontal"
	// or "vertical". Empty for the first pane in a window (which is not
	// a split).
	Split string `json:"split,omitempty"`

	// Size is the pane size as a percentage of the available space
	// (1-99). Empty or zero means tmux divides evenly.
	Size int `json:"size,omitempty"`
}

// MemberFilter selects room members for dynamic pane creation.
type MemberFilter struct {
	// Role filters members by their Bureau role (from the principal's
	// identity metadata). Empty means all members.
	Role string `json:"role,omitempty"`
}

// ReadTmuxLayout reads the current layout of a tmux session and returns
// it as a Layout struct. Queries tmux for window list, pane arrangement,
// and running commands.
//
// serverSocket is the tmux server socket path (the -S flag).
// sessionName is the tmux session to read (e.g., "bureau/iree/amdgpu/pm").
func ReadTmuxLayout(serverSocket, sessionName string) (*Layout, error) {
	// Implementation in bd-2sx (layout types and tmux conversion bead).
	panic("observe.ReadTmuxLayout not yet implemented")
}

// ApplyLayout creates or modifies a tmux session to match the given
// layout. Creates windows and panes as needed, sets split sizes, and
// starts commands. Existing panes running matching commands are left
// untouched; extra panes are closed; missing panes are created.
//
// serverSocket is the tmux server socket path (the -S flag).
// sessionName is the tmux session to modify (e.g., "bureau/iree/amdgpu/pm").
// If the session does not exist, it is created.
func ApplyLayout(serverSocket, sessionName string, layout *Layout) error {
	// Implementation in bd-2sx (layout types and tmux conversion bead).
	panic("observe.ApplyLayout not yet implemented")
}
