// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observation

// LayoutContent is the content of an EventTypeLayout state event. It describes
// the tmux session structure for an observation target: windows, panes, and
// what each pane shows.
//
// This is the Matrix wire-format representation. The observe package defines a
// parallel runtime type (observe.Layout). The two have the same shape but are
// separate types: the schema package defines persistence, the observe package
// defines behavior. Conversion between them happens in the daemon.
type LayoutContent struct {
	// Prefix is the tmux prefix key for this session (e.g., "C-a").
	// Empty means use the Bureau default from deploy/tmux/bureau.conf.
	Prefix string `json:"prefix,omitempty"`

	// Windows is the ordered list of tmux windows in the session.
	Windows []LayoutWindow `json:"windows"`

	// SourceMachine is the full Matrix user ID of the machine that
	// published this layout event (e.g., "@machine/workstation:bureau.local").
	// Used for loop prevention: when a daemon receives a layout event
	// that it published itself, it skips applying it to avoid an
	// infinite sync loop.
	SourceMachine string `json:"source_machine,omitempty"`

	// SealedMetadata is reserved for future use. When populated, it
	// will contain an age-encrypted blob with sensitive runtime state
	// (environment variables, checkpoint references, etc.) that should
	// not be stored in plaintext. The structural layout above remains
	// in plaintext for routing and display.
	SealedMetadata string `json:"sealed_metadata,omitempty"`
}

// LayoutWindow describes a single tmux window containing one or more panes.
type LayoutWindow struct {
	// Name is the tmux window name (displayed in the status bar).
	Name string `json:"name"`

	// Panes is the ordered list of panes in this window. The first pane
	// is the root; subsequent panes are splits from it.
	Panes []LayoutPane `json:"panes"`
}

// LayoutPane describes a single pane within a tmux window. Exactly one of
// Observe, Command, Role, or ObserveMembers should be set to determine what
// the pane shows.
type LayoutPane struct {
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
	ObserveMembers *LayoutMemberFilter `json:"observe_members,omitempty"`

	// Split is the split direction from the previous pane: "horizontal"
	// or "vertical". Empty for the first pane in a window (which is not
	// a split).
	Split string `json:"split,omitempty"`

	// Size is the pane size as a percentage of the available space
	// (1-99). Zero means tmux divides evenly.
	Size int `json:"size,omitempty"`
}

// LayoutMemberFilter selects room members for dynamic pane creation in
// channel layouts with ObserveMembers.
type LayoutMemberFilter struct {
	// Labels filters members whose PrincipalAssignment labels contain all
	// specified key-value pairs (subset match). An empty or nil map means
	// all members pass the filter. Example: {"role": "agent"} matches any
	// principal labeled role=agent; {"role": "agent", "team": "iree"}
	// matches only principals with both labels set to those values.
	Labels map[string]string `json:"labels,omitempty"`
}
