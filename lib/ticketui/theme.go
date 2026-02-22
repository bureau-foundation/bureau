// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import "github.com/bureau-foundation/bureau/lib/tui"

// Re-export theme types from the shared TUI library so that
// existing code within this package can refer to them unqualified.

// Theme defines the color palette and visual properties for the TUI.
type Theme = tui.Theme

// DefaultTheme is the built-in dark-terminal color scheme.
var DefaultTheme = tui.DefaultTheme
