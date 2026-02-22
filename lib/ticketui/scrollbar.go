// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import "github.com/bureau-foundation/bureau/lib/tui"

// renderScrollbar delegates to the shared TUI library's scrollbar
// renderer. Kept as a package-level function so existing call sites
// in model.go and detail.go don't need to change.
func renderScrollbar(theme Theme, height, totalItems, visibleItems, scrollOffset int, focused bool) string {
	return tui.RenderScrollbar(theme, height, totalItems, visibleItems, scrollOffset, focused)
}
