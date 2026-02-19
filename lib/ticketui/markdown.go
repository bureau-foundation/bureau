// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2/quick"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// markdownParserInstance is initialized once and reused. The parser
// configuration (extensions, options) never changes and the goldmark
// Parser is safe to share — actual parsing creates per-call state via
// Parse(reader).
var (
	markdownParserInstance goldmark.Markdown
	markdownParserOnce     sync.Once
)

func getMarkdownParser() goldmark.Markdown {
	markdownParserOnce.Do(func() {
		markdownParserInstance = goldmark.New(
			goldmark.WithExtensions(
				extension.GFM,
				extension.DefinitionList,
			),
		)
	})
	return markdownParserInstance
}

// renderTerminalMarkdown parses markdown text and renders it as styled
// terminal output. Soft line breaks (single newlines within paragraphs)
// become spaces so hard-wrapped source text reflows correctly at any
// terminal width. Code blocks, lists, and other structural elements
// preserve their formatting.
func renderTerminalMarkdown(input string, theme Theme, width int) string {
	if input == "" {
		return ""
	}
	source := []byte(input)
	reader := text.NewReader(source)
	document := getMarkdownParser().Parser().Parse(reader)

	// Force ANSI256 color profile: this output is always for terminal
	// display (bubbletea TUI), so we bypass auto-detection which would
	// produce uncolored output in test environments with no TTY.
	// SetColorProfile is required because lipgloss.Renderer.ColorProfile()
	// ignores the termenv.Output profile and re-detects from the
	// environment unless explicitColorProfile is set.
	lipRenderer := lipgloss.NewRenderer(os.Stderr, termenv.WithProfile(termenv.ANSI256))
	lipRenderer.SetColorProfile(termenv.ANSI256)

	renderer := &markdownRenderer{
		source:      source,
		theme:       theme,
		width:       width,
		lipRenderer: lipRenderer,
	}
	ast.Walk(document, renderer.walk)

	return strings.TrimRight(renderer.output.String(), "\n")
}

// markdownRenderer walks a goldmark AST and produces styled terminal
// text. It uses a direct ast.Walk rather than goldmark's renderer
// interface because terminal rendering needs accumulate-then-wrap
// semantics: paragraph inline content collects in a buffer and gets
// word-wrapped as a unit when the paragraph closes. Goldmark's
// streaming NodeRendererFunc callbacks don't fit this pattern without
// the intermediate-buffer gymnastics glamour uses.
type markdownRenderer struct {
	source []byte
	theme  Theme
	width  int

	// Final rendered output.
	output strings.Builder

	// Inline accumulator: collects styled text fragments within a
	// paragraph, heading, or other inline-containing block. Flushed
	// with word-wrap when the containing block closes.
	inline strings.Builder

	// Prefix stack for nested block containers (blockquotes, lists).
	prefixStack     []prefixLevel
	linePrefix      string // Concatenation of all prefix texts.
	linePrefixWidth int    // Sum of all visible prefix widths.

	// Pending bullet: replaces linePrefix for the very next emitted
	// line, then clears. Used for list item bullets/numbers.
	pendingBullet string

	// Inline style counters: incremented by Emphasis/Strikethrough
	// entering, decremented on leaving. Text nodes read these to
	// determine the current style. Counters (not booleans) handle
	// nested emphasis correctly.
	boldCount          int
	italicCount        int
	strikethroughCount int

	// List nesting state.
	listStack []listState

	// lipgloss renderer with forced color profile for ANSI output.
	lipRenderer *lipgloss.Renderer

	// Tracks trailing newlines at end of output for blank line management.
	trailingNewlines int
}

type prefixLevel struct {
	text  string
	width int
}

type listState struct {
	ordered bool
	counter int
	tight   bool
}

// newStyle creates a lipgloss style using the renderer's forced color
// profile, ensuring ANSI output regardless of terminal detection.
func (renderer *markdownRenderer) newStyle() lipgloss.Style {
	return renderer.lipRenderer.NewStyle()
}

// currentWidth returns the available content width after accounting
// for all nesting prefixes. Clamped to a minimum of 10 to prevent
// degenerate wrapping.
func (renderer *markdownRenderer) currentWidth() int {
	width := renderer.width - renderer.linePrefixWidth
	if width < 10 {
		width = 10
	}
	return width
}

func (renderer *markdownRenderer) pushPrefix(prefixText string, visibleWidth int) {
	renderer.prefixStack = append(renderer.prefixStack, prefixLevel{
		text:  prefixText,
		width: visibleWidth,
	})
	renderer.linePrefix += prefixText
	renderer.linePrefixWidth += visibleWidth
}

func (renderer *markdownRenderer) popPrefix() {
	if len(renderer.prefixStack) == 0 {
		return
	}
	top := renderer.prefixStack[len(renderer.prefixStack)-1]
	renderer.prefixStack = renderer.prefixStack[:len(renderer.prefixStack)-1]
	renderer.linePrefix = renderer.linePrefix[:len(renderer.linePrefix)-len(top.text)]
	renderer.linePrefixWidth -= top.width
}

func (renderer *markdownRenderer) inTightList() bool {
	if len(renderer.listStack) == 0 {
		return false
	}
	return renderer.listStack[len(renderer.listStack)-1].tight
}

// writeOutput appends text to the output buffer, tracking trailing
// newlines for blank line management.
func (renderer *markdownRenderer) writeOutput(s string) {
	if s == "" {
		return
	}
	renderer.output.WriteString(s)

	// Count trailing newlines in the new text.
	newTrailing := 0
	entirelyNewlines := true
	for index := len(s) - 1; index >= 0; index-- {
		if s[index] == '\n' {
			newTrailing++
		} else {
			entirelyNewlines = false
			break
		}
	}

	// If the entire string is newlines, they extend the existing
	// trailing count. Otherwise, the new text's trailing newlines
	// replace the count (any non-newline character resets it).
	if entirelyNewlines {
		renderer.trailingNewlines += newTrailing
	} else {
		renderer.trailingNewlines = newTrailing
	}
}

func (renderer *markdownRenderer) ensureNewline() {
	if renderer.trailingNewlines < 1 {
		renderer.writeOutput("\n")
	}
}

func (renderer *markdownRenderer) ensureBlankLine() {
	for renderer.trailingNewlines < 2 {
		renderer.writeOutput("\n")
	}
}

// consumeLinePrefix returns the prefix for the current line. If a
// pending bullet is set, returns and clears it (used for the first
// line of a list item). Otherwise returns the regular line prefix.
func (renderer *markdownRenderer) consumeLinePrefix() string {
	if renderer.pendingBullet != "" {
		bullet := renderer.pendingBullet
		renderer.pendingBullet = ""
		return bullet
	}
	return renderer.linePrefix
}

// applyPrefixes prepends the appropriate line prefix to each line of
// text. The first line uses the pending bullet (if set), subsequent
// lines use the regular line prefix.
func (renderer *markdownRenderer) applyPrefixes(content string) string {
	lines := strings.Split(content, "\n")
	var result strings.Builder
	for index, line := range lines {
		if index == 0 {
			result.WriteString(renderer.consumeLinePrefix())
		} else {
			result.WriteString(renderer.linePrefix)
		}
		result.WriteString(line)
		if index < len(lines)-1 {
			result.WriteString("\n")
		}
	}
	return result.String()
}

// flushInline word-wraps the accumulated inline content to the current
// width, applies line prefixes, and returns the result. Resets the
// inline buffer.
func (renderer *markdownRenderer) flushInline() string {
	content := renderer.inline.String()
	renderer.inline.Reset()
	if content == "" {
		return ""
	}

	width := renderer.currentWidth()
	content = ansi.Wrap(content, width, " ,.;-+|")
	return renderer.applyPrefixes(content)
}

// styledText applies the current inline style (bold, italic,
// strikethrough) to a text string.
func (renderer *markdownRenderer) styledText(content string) string {
	style := renderer.newStyle().Foreground(renderer.theme.NormalText)
	if renderer.boldCount > 0 {
		style = style.Bold(true)
	}
	if renderer.italicCount > 0 {
		style = style.Italic(true)
	}
	if renderer.strikethroughCount > 0 {
		style = style.Strikethrough(true)
	}
	return style.Render(content)
}

// renderInlineContent walks a node's children to collect inline
// content into a string. Saves and restores the inline buffer and
// style state so the caller's context is unaffected.
func (renderer *markdownRenderer) renderInlineContent(node ast.Node) string {
	savedInline := renderer.inline.String()
	savedBold := renderer.boldCount
	savedItalic := renderer.italicCount
	savedStrikethrough := renderer.strikethroughCount

	renderer.inline.Reset()
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		ast.Walk(child, renderer.walk)
	}
	result := renderer.inline.String()

	renderer.inline.Reset()
	renderer.inline.WriteString(savedInline)
	renderer.boldCount = savedBold
	renderer.italicCount = savedItalic
	renderer.strikethroughCount = savedStrikethrough

	return result
}

// highlightCode uses Chroma to syntax-highlight code. Returns
// ANSI-styled text on success, or FaintText-styled plain text on
// failure (unknown language, Chroma error).
func (renderer *markdownRenderer) highlightCode(code, language string) string {
	if language == "" {
		return renderer.newStyle().Foreground(renderer.theme.FaintText).Render(code)
	}
	var buffer strings.Builder
	err := quick.Highlight(&buffer, code, language, "terminal256", "monokai")
	if err != nil {
		return renderer.newStyle().Foreground(renderer.theme.FaintText).Render(code)
	}
	return buffer.String()
}

// --- AST walk dispatcher ---

func (renderer *markdownRenderer) walk(node ast.Node, entering bool) (ast.WalkStatus, error) {
	switch node.Kind() {

	// Block nodes.
	case ast.KindDocument:
		// No action needed on entering or leaving.

	case ast.KindParagraph, ast.KindTextBlock:
		if entering {
			renderer.inline.Reset()
		} else {
			flushed := renderer.flushInline()
			if flushed != "" {
				renderer.writeOutput(flushed)
				renderer.ensureNewline()
				if !renderer.inTightList() {
					renderer.ensureBlankLine()
				}
			}
		}

	case ast.KindHeading:
		if entering {
			renderer.inline.Reset()
		} else {
			renderer.leaveHeading(node.(*ast.Heading))
		}

	case ast.KindFencedCodeBlock:
		if entering {
			renderer.renderFencedCodeBlock(node.(*ast.FencedCodeBlock))
			return ast.WalkSkipChildren, nil
		}

	case ast.KindCodeBlock:
		if entering {
			renderer.renderCodeBlock(node.(*ast.CodeBlock))
			return ast.WalkSkipChildren, nil
		}

	case ast.KindBlockquote:
		if entering {
			renderer.pushPrefix("│ ", 2)
		} else {
			renderer.popPrefix()
			renderer.ensureBlankLine()
		}

	case ast.KindList:
		if entering {
			renderer.enterList(node.(*ast.List))
		} else {
			renderer.leaveList()
		}

	case ast.KindListItem:
		if entering {
			renderer.enterListItem()
		} else {
			renderer.leaveListItem()
		}

	case ast.KindThematicBreak:
		if entering {
			renderer.renderThematicBreak()
		}

	case ast.KindHTMLBlock:
		if entering {
			renderer.renderHTMLBlock(node.(*ast.HTMLBlock))
			return ast.WalkSkipChildren, nil
		}

	// Inline nodes.
	case ast.KindText:
		if entering {
			renderer.handleText(node.(*ast.Text))
		}

	case ast.KindString:
		if entering {
			str := node.(*ast.String)
			renderer.inline.WriteString(renderer.styledText(string(str.Value)))
		}

	case ast.KindEmphasis:
		renderer.handleEmphasis(node.(*ast.Emphasis), entering)

	case ast.KindCodeSpan:
		if entering {
			renderer.renderCodeSpan(node)
			return ast.WalkSkipChildren, nil
		}

	case ast.KindLink:
		if entering {
			renderer.renderLink(node.(*ast.Link))
			return ast.WalkSkipChildren, nil
		}

	case ast.KindAutoLink:
		if entering {
			renderer.renderAutoLink(node.(*ast.AutoLink))
		}

	case ast.KindImage:
		if entering {
			renderer.renderImage(node.(*ast.Image))
			return ast.WalkSkipChildren, nil
		}

	case ast.KindRawHTML:
		if entering {
			renderer.renderRawHTML(node.(*ast.RawHTML))
		}

	// GFM extension nodes.
	case extast.KindStrikethrough:
		if entering {
			renderer.strikethroughCount++
		} else {
			renderer.strikethroughCount--
		}

	case extast.KindTable:
		if entering {
			renderer.renderTable(node)
			return ast.WalkSkipChildren, nil
		}

	case extast.KindTaskCheckBox:
		if entering {
			checkbox := node.(*extast.TaskCheckBox)
			if checkbox.IsChecked {
				checkStyle := renderer.newStyle().Foreground(renderer.theme.StatusClosed)
				renderer.inline.WriteString(checkStyle.Render("[x]") + " ")
			} else {
				renderer.inline.WriteString(renderer.styledText("[ ] "))
			}
		}

	// Definition list extension nodes.
	case extast.KindDefinitionList:
		// Container — no special handling needed.

	case extast.KindDefinitionTerm:
		if entering {
			renderer.inline.Reset()
		} else {
			// Render term as bold text. Strip existing inline styling
			// since the definition term has its own bold style.
			content := ansi.Strip(renderer.inline.String())
			renderer.inline.Reset()
			if content != "" {
				bold := renderer.newStyle().
					Foreground(renderer.theme.NormalText).
					Bold(true)
				flushed := renderer.applyPrefixes(bold.Render(content))
				renderer.writeOutput(flushed)
				renderer.ensureNewline()
			}
		}

	case extast.KindDefinitionDescription:
		if entering {
			renderer.pushPrefix("  ", 2)
		} else {
			renderer.popPrefix()
		}
	}

	return ast.WalkContinue, nil
}

// --- Block-level handlers ---

func (renderer *markdownRenderer) leaveHeading(heading *ast.Heading) {
	// Strip existing inline styling — heading has its own style that
	// should replace the default NormalText applied by styledText().
	content := ansi.Strip(renderer.inline.String())
	renderer.inline.Reset()
	if content == "" {
		return
	}

	style := renderer.newStyle().Bold(true)
	if heading.Level <= 2 {
		style = style.Foreground(renderer.theme.HeaderForeground)
	} else {
		style = style.Foreground(renderer.theme.NormalText)
	}

	wrapped := ansi.Wrap(style.Render(content), renderer.currentWidth(), " ,.;-+|")
	flushed := renderer.applyPrefixes(wrapped)
	renderer.ensureBlankLine()
	renderer.writeOutput(flushed)
	renderer.ensureNewline()
	renderer.ensureBlankLine()
}

func (renderer *markdownRenderer) renderFencedCodeBlock(node *ast.FencedCodeBlock) {
	language := string(node.Language(renderer.source))
	var code strings.Builder
	lines := node.Lines()
	for index := 0; index < lines.Len(); index++ {
		segment := lines.At(index)
		code.Write(segment.Value(renderer.source))
	}

	highlighted := renderer.highlightCode(code.String(), language)
	renderer.ensureBlankLine()
	codeLines := strings.Split(strings.TrimRight(highlighted, "\n"), "\n")
	for _, line := range codeLines {
		renderer.writeOutput(renderer.consumeLinePrefix() + line)
		renderer.ensureNewline()
	}
	renderer.ensureBlankLine()
}

func (renderer *markdownRenderer) renderCodeBlock(node *ast.CodeBlock) {
	var code strings.Builder
	lines := node.Lines()
	for index := 0; index < lines.Len(); index++ {
		segment := lines.At(index)
		code.Write(segment.Value(renderer.source))
	}

	faint := renderer.newStyle().Foreground(renderer.theme.FaintText)
	renderer.ensureBlankLine()
	codeLines := strings.Split(strings.TrimRight(code.String(), "\n"), "\n")
	for _, line := range codeLines {
		renderer.writeOutput(renderer.consumeLinePrefix() + faint.Render(line))
		renderer.ensureNewline()
	}
	renderer.ensureBlankLine()
}

func (renderer *markdownRenderer) enterList(list *ast.List) {
	startNumber := 0
	if list.IsOrdered() {
		startNumber = list.Start
	}
	renderer.listStack = append(renderer.listStack, listState{
		ordered: list.IsOrdered(),
		counter: startNumber,
		tight:   list.IsTight,
	})
}

func (renderer *markdownRenderer) leaveList() {
	if len(renderer.listStack) > 0 {
		renderer.listStack = renderer.listStack[:len(renderer.listStack)-1]
	}
	if !renderer.inTightList() {
		renderer.ensureBlankLine()
	}
}

func (renderer *markdownRenderer) enterListItem() {
	if len(renderer.listStack) == 0 {
		return
	}
	top := &renderer.listStack[len(renderer.listStack)-1]

	var bullet string
	if top.ordered {
		bullet = fmt.Sprintf("%d. ", top.counter)
		top.counter++
	} else {
		bullet = "- "
	}

	bulletWidth := len(bullet) // ASCII-only, so byte length == visual width.
	continuation := strings.Repeat(" ", bulletWidth)

	// The pending bullet includes the current linePrefix so it
	// replaces the entire prefix for the first line of this item.
	renderer.pendingBullet = renderer.linePrefix + bullet
	renderer.pushPrefix(continuation, bulletWidth)
}

func (renderer *markdownRenderer) leaveListItem() {
	renderer.popPrefix()
	if !renderer.inTightList() {
		renderer.ensureBlankLine()
	} else {
		renderer.ensureNewline()
	}
}

func (renderer *markdownRenderer) renderThematicBreak() {
	width := renderer.currentWidth()
	rule := strings.Repeat("─", width)
	ruleStyle := renderer.newStyle().Foreground(renderer.theme.BorderColor)
	renderer.ensureBlankLine()
	renderer.writeOutput(renderer.applyPrefixes(ruleStyle.Render(rule)))
	renderer.ensureNewline()
	renderer.ensureBlankLine()
}

func (renderer *markdownRenderer) renderHTMLBlock(node *ast.HTMLBlock) {
	var html strings.Builder
	lines := node.Lines()
	for index := 0; index < lines.Len(); index++ {
		segment := lines.At(index)
		html.Write(segment.Value(renderer.source))
	}
	stripped := strings.TrimSpace(stripHTMLTags(html.String()))
	if stripped != "" {
		faint := renderer.newStyle().Foreground(renderer.theme.FaintText)
		renderer.writeOutput(renderer.applyPrefixes(faint.Render(stripped)))
		renderer.ensureNewline()
		renderer.ensureBlankLine()
	}
}

// --- Inline handlers ---

func (renderer *markdownRenderer) handleText(node *ast.Text) {
	segment := node.Segment
	value := string(segment.Value(renderer.source))
	renderer.inline.WriteString(renderer.styledText(value))

	if node.SoftLineBreak() {
		// The key reflow fix: soft line breaks become spaces so
		// hard-wrapped source text reflows at any terminal width.
		renderer.inline.WriteString(" ")
	}
	if node.HardLineBreak() {
		renderer.inline.WriteString("\n")
	}
}

func (renderer *markdownRenderer) handleEmphasis(node *ast.Emphasis, entering bool) {
	if node.Level >= 2 {
		if entering {
			renderer.boldCount++
		} else {
			renderer.boldCount--
		}
	} else {
		if entering {
			renderer.italicCount++
		} else {
			renderer.italicCount--
		}
	}
}

func (renderer *markdownRenderer) renderCodeSpan(node ast.Node) {
	// Collect code text from children, joining segments.
	var code strings.Builder
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		if textNode, ok := child.(*ast.Text); ok {
			segment := textNode.Segment
			code.Write(segment.Value(renderer.source))
		} else if strNode, ok := child.(*ast.String); ok {
			code.Write(strNode.Value)
		}
	}
	codeStyle := renderer.newStyle().Foreground(renderer.theme.FaintText)
	renderer.inline.WriteString(codeStyle.Render(code.String()))
}

func (renderer *markdownRenderer) renderLink(node *ast.Link) {
	// renderInlineContent already applies inline styling (bold, italic, etc.)
	// to the link text, so we write it directly without double-styling.
	displayText := renderer.renderInlineContent(node)
	url := string(node.Destination)

	renderer.inline.WriteString(displayText)
	if url != "" {
		urlStyle := renderer.newStyle().Foreground(renderer.theme.FaintText)
		renderer.inline.WriteString(" " + urlStyle.Render("("+url+")"))
	}
}

func (renderer *markdownRenderer) renderAutoLink(node *ast.AutoLink) {
	url := string(node.URL(renderer.source))
	urlStyle := renderer.newStyle().Foreground(renderer.theme.FaintText)
	renderer.inline.WriteString(urlStyle.Render(url))
}

func (renderer *markdownRenderer) renderImage(node *ast.Image) {
	altText := renderer.renderInlineContent(node)
	url := string(node.Destination)
	faint := renderer.newStyle().Foreground(renderer.theme.FaintText)
	renderer.inline.WriteString(faint.Render("[" + altText + "]"))
	if url != "" {
		renderer.inline.WriteString(" " + faint.Render("("+url+")"))
	}
}

func (renderer *markdownRenderer) renderRawHTML(node *ast.RawHTML) {
	var html strings.Builder
	for index := 0; index < node.Segments.Len(); index++ {
		segment := node.Segments.At(index)
		html.Write(segment.Value(renderer.source))
	}
	stripped := stripHTMLTags(html.String())
	if stripped != "" {
		faint := renderer.newStyle().Foreground(renderer.theme.FaintText)
		renderer.inline.WriteString(faint.Render(stripped))
	}
}

// --- Table rendering ---

func (renderer *markdownRenderer) renderTable(node ast.Node) {
	table := node.(*extast.Table)
	alignments := table.Alignments

	var headerCells []string
	var bodyRows [][]string

	// Walk children manually to collect cell content.
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		switch child.Kind() {
		case extast.KindTableHeader:
			// TableHeader contains TableCell children directly.
			headerCells = renderer.collectTableRow(child)
		case extast.KindTableRow:
			bodyRows = append(bodyRows, renderer.collectTableRow(child))
		}
	}

	columnCount := len(headerCells)
	if columnCount == 0 && len(bodyRows) > 0 {
		columnCount = len(bodyRows[0])
	}
	if columnCount == 0 {
		return
	}

	// Compute column widths from visible content width.
	columnWidths := make([]int, columnCount)
	for index, cell := range headerCells {
		if index < columnCount {
			if width := lipgloss.Width(cell); width > columnWidths[index] {
				columnWidths[index] = width
			}
		}
	}
	for _, row := range bodyRows {
		for index, cell := range row {
			if index < columnCount {
				if width := lipgloss.Width(cell); width > columnWidths[index] {
					columnWidths[index] = width
				}
			}
		}
	}

	// Cap total width to available space. If the table is too wide,
	// proportionally shrink columns.
	separator := "  "
	totalWidth := 0
	for _, width := range columnWidths {
		totalWidth += width
	}
	totalWidth += len(separator) * (columnCount - 1)
	available := renderer.currentWidth()
	if totalWidth > available && columnCount > 0 {
		// Shrink proportionally, minimum 3 chars per column.
		usable := available - len(separator)*(columnCount-1)
		if usable < columnCount*3 {
			usable = columnCount * 3
		}
		for index := range columnWidths {
			columnWidths[index] = (columnWidths[index] * usable) / totalWidth
			if columnWidths[index] < 3 {
				columnWidths[index] = 3
			}
		}
	}

	renderer.ensureBlankLine()

	// Header row.
	if len(headerCells) > 0 {
		bold := renderer.newStyle().Bold(true).Foreground(renderer.theme.NormalText)
		renderer.writeOutput(renderer.consumeLinePrefix() +
			renderer.formatTableRow(headerCells, columnWidths, alignments, bold))
		renderer.ensureNewline()

		// Header separator.
		var separatorParts []string
		for _, width := range columnWidths {
			separatorParts = append(separatorParts, strings.Repeat("─", width))
		}
		borderStyle := renderer.newStyle().Foreground(renderer.theme.BorderColor)
		renderer.writeOutput(renderer.linePrefix +
			borderStyle.Render(strings.Join(separatorParts, separator)))
		renderer.ensureNewline()
	}

	// Body rows.
	for _, row := range bodyRows {
		renderer.writeOutput(renderer.linePrefix +
			renderer.formatTableRow(row, columnWidths, alignments, renderer.newStyle()))
		renderer.ensureNewline()
	}

	renderer.ensureBlankLine()
}

// collectTableRow extracts cell content strings from a TableRow node.
func (renderer *markdownRenderer) collectTableRow(row ast.Node) []string {
	var cells []string
	for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
		if cell.Kind() == extast.KindTableCell {
			content := renderer.renderInlineContent(cell)
			cells = append(cells, content)
		}
	}
	return cells
}

// formatTableRow formats a single table row with padded columns.
func (renderer *markdownRenderer) formatTableRow(
	cells []string,
	columnWidths []int,
	alignments []extast.Alignment,
	baseStyle lipgloss.Style,
) string {
	separator := "  "
	var parts []string
	for index, width := range columnWidths {
		var cell string
		if index < len(cells) {
			cell = cells[index]
		}

		visibleWidth := lipgloss.Width(cell)
		if visibleWidth > width {
			// Truncate cell to fit column width.
			cell = ansi.Truncate(cell, width, "…")
			visibleWidth = lipgloss.Width(cell)
		}

		padding := width - visibleWidth
		if padding < 0 {
			padding = 0
		}

		var alignment extast.Alignment
		if index < len(alignments) {
			alignment = alignments[index]
		}

		switch alignment {
		case extast.AlignRight:
			cell = strings.Repeat(" ", padding) + cell
		case extast.AlignCenter:
			leftPad := padding / 2
			rightPad := padding - leftPad
			cell = strings.Repeat(" ", leftPad) + cell + strings.Repeat(" ", rightPad)
		default: // Left or unset.
			cell = cell + strings.Repeat(" ", padding)
		}
		parts = append(parts, cell)
	}
	return baseStyle.Render(strings.Join(parts, separator))
}

// --- Utilities ---

// stripHTMLTags removes HTML tags from a string, returning only the
// text content. Used for HTMLBlock and RawHTML nodes.
func stripHTMLTags(html string) string {
	var result strings.Builder
	inTag := false
	for _, character := range html {
		if character == '<' {
			inTag = true
			continue
		}
		if character == '>' {
			inTag = false
			continue
		}
		if !inTag {
			result.WriteRune(character)
		}
	}
	return result.String()
}
