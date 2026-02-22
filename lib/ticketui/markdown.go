// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import "github.com/bureau-foundation/bureau/lib/tui"

// renderTerminalMarkdown delegates to the shared TUI library.
func renderTerminalMarkdown(input string, theme Theme, width int) string {
	return tui.RenderTerminalMarkdown(input, theme, width)
}

// stripHTMLTags delegates to the shared TUI library.
func stripHTMLTags(html string) string {
	return tui.StripHTMLTags(html)
}
