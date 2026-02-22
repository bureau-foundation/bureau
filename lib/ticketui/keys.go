// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import "github.com/charmbracelet/bubbles/key"

// KeyMap defines all key bindings for the ticket viewer TUI.
type KeyMap struct {
	// Navigation (context-sensitive: list movement or detail scrolling
	// depending on current focus).
	Up       key.Binding
	Down     key.Binding
	Left     key.Binding // List: collapse epic / go to parent header.
	Right    key.Binding // List: expand epic / enter first child.
	PageUp   key.Binding
	PageDown key.Binding
	Home     key.Binding
	End      key.Binding

	// Focus switching.
	FocusToggle key.Binding

	// Splitter resize.
	SplitGrow   key.Binding // Grow list pane (push detail right).
	SplitShrink key.Binding // Shrink list pane (push detail left).

	// Tab switching.
	TabReady     key.Binding
	TabBlocked   key.Binding
	TabAll       key.Binding
	TabPipelines key.Binding

	// Navigation history.
	NavigateBack key.Binding // Go back to the previous ticket.

	// Filter.
	FilterActivate key.Binding // Enter filter mode.
	FilterClear    key.Binding // Clear filter and exit filter mode.

	// Mutations.
	Assign key.Binding // Open the assignee dropdown (detail pane).

	// Detail search (active when the detail pane has a search query).
	SearchNext     key.Binding // Jump to next match in detail search.
	SearchPrevious key.Binding // Jump to previous match in detail search.

	Quit key.Binding
}

// DefaultKeyMap is the built-in key binding set. Vim-style navigation
// (j/k) alongside standard arrow keys and page up/down.
var DefaultKeyMap = KeyMap{
	Up: key.NewBinding(
		key.WithKeys("k", "up"),
		key.WithHelp("k/↑", "up"),
	),
	Down: key.NewBinding(
		key.WithKeys("j", "down"),
		key.WithHelp("j/↓", "down"),
	),
	Left: key.NewBinding(
		key.WithKeys("h", "left"),
		key.WithHelp("h/←", "collapse"),
	),
	Right: key.NewBinding(
		key.WithKeys("l", "right"),
		key.WithHelp("l/→", "expand"),
	),
	PageUp: key.NewBinding(
		key.WithKeys("ctrl+u", "pgup"),
		key.WithHelp("C-u", "page up"),
	),
	PageDown: key.NewBinding(
		key.WithKeys("ctrl+d", "pgdown"),
		key.WithHelp("C-d", "page down"),
	),
	Home: key.NewBinding(
		key.WithKeys("g", "home"),
		key.WithHelp("g", "top"),
	),
	End: key.NewBinding(
		key.WithKeys("G", "end"),
		key.WithHelp("G", "bottom"),
	),
	FocusToggle: key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("Tab", "switch pane"),
	),
	SplitGrow: key.NewBinding(
		key.WithKeys("]"),
		key.WithHelp("]", "grow list"),
	),
	SplitShrink: key.NewBinding(
		key.WithKeys("["),
		key.WithHelp("[", "shrink list"),
	),
	TabReady: key.NewBinding(
		key.WithKeys("1"),
		key.WithHelp("1", "ready"),
	),
	TabBlocked: key.NewBinding(
		key.WithKeys("2"),
		key.WithHelp("2", "blocked"),
	),
	TabAll: key.NewBinding(
		key.WithKeys("3"),
		key.WithHelp("3", "all"),
	),
	TabPipelines: key.NewBinding(
		key.WithKeys("4"),
		key.WithHelp("4", "pipelines"),
	),
	NavigateBack: key.NewBinding(
		key.WithKeys("backspace"),
		key.WithHelp("BS", "back"),
	),
	Assign: key.NewBinding(
		key.WithKeys("a"),
		key.WithHelp("a", "assign"),
	),
	FilterActivate: key.NewBinding(
		key.WithKeys("/"),
		key.WithHelp("/", "filter"),
	),
	FilterClear: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("Esc", "clear filter"),
	),
	SearchNext: key.NewBinding(
		key.WithKeys("n"),
		key.WithHelp("n", "next match"),
	),
	SearchPrevious: key.NewBinding(
		key.WithKeys("N"),
		key.WithHelp("N", "prev match"),
	),
	Quit: key.NewBinding(
		key.WithKeys("q", "ctrl+c"),
		key.WithHelp("q", "quit"),
	),
}
