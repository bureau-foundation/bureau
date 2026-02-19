// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// stripped renders markdown and returns ANSI-stripped visible text.
func stripped(input string, width int) string {
	return ansi.Strip(renderTerminalMarkdown(input, DefaultTheme, width))
}

// raw renders markdown and returns the raw ANSI-styled output.
func raw(input string, width int) string {
	return renderTerminalMarkdown(input, DefaultTheme, width)
}

func TestRenderMarkdownEmpty(t *testing.T) {
	result := renderTerminalMarkdown("", DefaultTheme, 80)
	if result != "" {
		t.Errorf("expected empty string for empty input, got %q", result)
	}
}

func TestRenderMarkdownParagraphReflow(t *testing.T) {
	// Source text hard-wrapped at ~40 columns.
	input := "This is a paragraph that was\nwritten at a narrow width with\nhard line breaks embedded in it."
	// Joined text is ~91 chars, so use width 120 to verify soft
	// breaks become spaces without word-wrap interference.
	result := stripped(input, 120)

	if strings.Contains(result, "\n") {
		t.Errorf("expected no newlines at width=120, got:\n%s", result)
	}
	if !strings.Contains(result, "was written at") {
		t.Errorf("expected soft break converted to space, got:\n%s", result)
	}
}

func TestRenderMarkdownParagraphReflowNarrow(t *testing.T) {
	input := "This is a paragraph that should be wrapped at the target width."
	result := stripped(input, 30)

	// At width 30, the text should wrap but not have unnecessary breaks.
	lines := strings.Split(result, "\n")
	for _, line := range lines {
		if len(line) > 30 {
			t.Errorf("line exceeds width 30: %q (len=%d)", line, len(line))
		}
	}
}

func TestRenderMarkdownHardLineBreak(t *testing.T) {
	// Two trailing spaces create a hard line break in CommonMark.
	input := "Line one  \nLine two"
	result := stripped(input, 80)

	if !strings.Contains(result, "Line one\nLine two") {
		t.Errorf("expected hard line break preserved, got:\n%s", result)
	}
}

func TestRenderMarkdownHeading(t *testing.T) {
	input := "# Heading One\n\n## Heading Two\n\n### Heading Three"
	result := stripped(input, 80)

	if !strings.Contains(result, "Heading One") {
		t.Error("missing heading 1 text")
	}
	if !strings.Contains(result, "Heading Two") {
		t.Error("missing heading 2 text")
	}
	if !strings.Contains(result, "Heading Three") {
		t.Error("missing heading 3 text")
	}

	// Headings should produce ANSI bold.
	rawResult := raw(input, 80)
	if rawResult == result {
		t.Error("expected ANSI styling in heading output")
	}
}

func TestRenderMarkdownEmphasis(t *testing.T) {
	input := "This is *italic* and **bold** text."
	result := stripped(input, 80)

	if !strings.Contains(result, "italic") {
		t.Error("missing italic text")
	}
	if !strings.Contains(result, "bold") {
		t.Error("missing bold text")
	}

	// Should have ANSI escapes for styling.
	rawResult := raw(input, 80)
	if rawResult == result {
		t.Error("expected ANSI styling in emphasis output")
	}
}

func TestRenderMarkdownBoldItalic(t *testing.T) {
	input := "***bold and italic***"
	result := stripped(input, 80)

	if !strings.Contains(result, "bold and italic") {
		t.Errorf("expected combined bold+italic text, got:\n%s", result)
	}
}

func TestRenderMarkdownCodeSpan(t *testing.T) {
	input := "Use the `foo()` function."
	result := stripped(input, 80)

	if !strings.Contains(result, "foo()") {
		t.Error("missing code span text")
	}
}

func TestRenderMarkdownFencedCodeBlock(t *testing.T) {
	input := "Text before.\n\n```go\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n```\n\nText after."
	result := stripped(input, 80)

	// Code block content should be preserved exactly (no reflow).
	if !strings.Contains(result, "func main()") {
		t.Error("missing code block content")
	}
	if !strings.Contains(result, "fmt.Println") {
		t.Error("missing code block content")
	}
	if !strings.Contains(result, "Text before.") {
		t.Error("missing text before code block")
	}
	if !strings.Contains(result, "Text after.") {
		t.Error("missing text after code block")
	}
}

func TestRenderMarkdownFencedCodeBlockWithHighlighting(t *testing.T) {
	input := "```go\npackage main\n```"
	rawResult := raw(input, 80)

	// Chroma should produce ANSI escape sequences for Go syntax.
	if !strings.Contains(rawResult, "\x1b[") {
		t.Error("expected ANSI escapes from syntax highlighting")
	}
}

func TestRenderMarkdownFencedCodeBlockNoLanguage(t *testing.T) {
	input := "```\nplain code\n```"
	result := stripped(input, 80)

	if !strings.Contains(result, "plain code") {
		t.Errorf("missing code block content, got:\n%s", result)
	}
}

func TestRenderMarkdownFencedCodeBlockNotReflowed(t *testing.T) {
	// Code block lines should NOT be reflowed regardless of width.
	input := "```\nshort\nlines\nhere\n```"
	result := stripped(input, 80)

	if !strings.Contains(result, "short\nlines\nhere") {
		t.Errorf("expected code block lines preserved, got:\n%s", result)
	}
}

func TestRenderMarkdownBlockquote(t *testing.T) {
	input := "> This is a quoted paragraph."
	result := stripped(input, 80)

	if !strings.Contains(result, "│") {
		t.Errorf("expected blockquote prefix, got:\n%s", result)
	}
	if !strings.Contains(result, "This is a quoted paragraph.") {
		t.Error("missing blockquote content")
	}
}

func TestRenderMarkdownBlockquoteReflow(t *testing.T) {
	input := "> This is a long quoted paragraph that\n> was written at a narrow width with\n> hard line breaks."
	result := stripped(input, 80)

	// Soft breaks within the blockquote should be reflowed.
	lines := strings.Split(result, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "│") {
			t.Errorf("expected blockquote prefix on every line, got: %q", line)
		}
	}
}

func TestRenderMarkdownUnorderedList(t *testing.T) {
	input := "- Item one\n- Item two\n- Item three"
	result := stripped(input, 80)

	if !strings.Contains(result, "- Item one") {
		t.Errorf("missing list item, got:\n%s", result)
	}
	if !strings.Contains(result, "- Item two") {
		t.Error("missing list item")
	}
	if !strings.Contains(result, "- Item three") {
		t.Error("missing list item")
	}
}

func TestRenderMarkdownOrderedList(t *testing.T) {
	input := "1. First\n2. Second\n3. Third"
	result := stripped(input, 80)

	if !strings.Contains(result, "1. First") {
		t.Errorf("missing ordered list item, got:\n%s", result)
	}
	if !strings.Contains(result, "2. Second") {
		t.Error("missing ordered list item")
	}
	if !strings.Contains(result, "3. Third") {
		t.Error("missing ordered list item")
	}
}

func TestRenderMarkdownNestedList(t *testing.T) {
	input := "- Outer\n  - Inner\n- Outer two"
	result := stripped(input, 80)

	if !strings.Contains(result, "Outer") {
		t.Error("missing outer list item")
	}
	if !strings.Contains(result, "Inner") {
		t.Error("missing inner list item")
	}
	// Inner item should be indented more than outer.
	lines := strings.Split(result, "\n")
	var outerIndent, innerIndent int
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if strings.Contains(line, "Inner") {
			innerIndent = indent
		}
		if strings.Contains(line, "Outer") && !strings.Contains(line, "two") {
			outerIndent = indent
		}
	}
	if innerIndent <= outerIndent {
		t.Errorf("expected inner list to be more indented: outer=%d, inner=%d",
			outerIndent, innerIndent)
	}
}

func TestRenderMarkdownTaskCheckbox(t *testing.T) {
	input := "- [x] Done task\n- [ ] Pending task"
	result := stripped(input, 80)

	if !strings.Contains(result, "[x]") {
		t.Errorf("missing checked checkbox, got:\n%s", result)
	}
	if !strings.Contains(result, "[ ]") {
		t.Error("missing unchecked checkbox")
	}
	if !strings.Contains(result, "Done task") {
		t.Error("missing checkbox label")
	}
}

func TestRenderMarkdownStrikethrough(t *testing.T) {
	input := "This is ~~deleted~~ text."
	result := stripped(input, 80)

	if !strings.Contains(result, "deleted") {
		t.Error("missing strikethrough text")
	}

	rawResult := raw(input, 80)
	if rawResult == result {
		t.Error("expected ANSI styling for strikethrough")
	}
}

func TestRenderMarkdownLink(t *testing.T) {
	input := "See [the docs](https://example.com) for details."
	result := stripped(input, 80)

	if !strings.Contains(result, "the docs") {
		t.Error("missing link text")
	}
	if !strings.Contains(result, "(https://example.com)") {
		t.Errorf("missing link URL, got:\n%s", result)
	}
}

func TestRenderMarkdownAutoLink(t *testing.T) {
	input := "Visit https://example.com for info."
	result := stripped(input, 80)

	if !strings.Contains(result, "https://example.com") {
		t.Errorf("missing autolink URL, got:\n%s", result)
	}
}

func TestRenderMarkdownImage(t *testing.T) {
	input := "![alt text](https://example.com/image.png)"
	result := stripped(input, 80)

	if !strings.Contains(result, "[alt text]") {
		t.Errorf("missing image alt text, got:\n%s", result)
	}
	if !strings.Contains(result, "(https://example.com/image.png)") {
		t.Error("missing image URL")
	}
}

func TestRenderMarkdownThematicBreak(t *testing.T) {
	input := "Before.\n\n---\n\nAfter."
	result := stripped(input, 40)

	if !strings.Contains(result, "Before.") {
		t.Error("missing text before break")
	}
	if !strings.Contains(result, "After.") {
		t.Error("missing text after break")
	}
	if !strings.Contains(result, "───") {
		t.Errorf("expected horizontal rule, got:\n%s", result)
	}
}

func TestRenderMarkdownTable(t *testing.T) {
	input := "| Name | Age |\n|------|-----|\n| Alice | 30 |\n| Bob | 25 |"
	result := stripped(input, 80)

	if !strings.Contains(result, "Name") {
		t.Errorf("missing table header, got:\n%s", result)
	}
	if !strings.Contains(result, "Alice") {
		t.Error("missing table cell")
	}
	if !strings.Contains(result, "Bob") {
		t.Error("missing table cell")
	}
	if !strings.Contains(result, "───") {
		t.Error("missing table header separator")
	}
}

func TestRenderMarkdownDefinitionList(t *testing.T) {
	input := "Term\n:   Description of the term."
	result := stripped(input, 80)

	if !strings.Contains(result, "Term") {
		t.Errorf("missing definition term, got:\n%s", result)
	}
	if !strings.Contains(result, "Description of the term.") {
		t.Errorf("missing definition description, got:\n%s", result)
	}
}

func TestRenderMarkdownSearchHighlightCompatibility(t *testing.T) {
	// Verify that the renderer's ANSI output works with the search
	// highlighting system, which uses ansi.DecodeSequence to walk
	// through styled text.
	input := "This is **bold** and `code` text."
	rawResult := raw(input, 80)

	// highlightSearchMatches should work on the rendered output.
	highlighted, matches := highlightSearchMatches(rawResult, "bold", -1, DefaultTheme)

	if len(matches) == 0 {
		t.Error("expected at least one match for 'bold' in rendered markdown")
	}
	if highlighted == rawResult {
		t.Error("expected highlighting to modify the output")
	}
}

func TestRenderMarkdownMultipleParagraphs(t *testing.T) {
	input := "First paragraph.\n\nSecond paragraph."
	result := stripped(input, 80)

	if !strings.Contains(result, "First paragraph.") {
		t.Error("missing first paragraph")
	}
	if !strings.Contains(result, "Second paragraph.") {
		t.Error("missing second paragraph")
	}
	// Should have a blank line between paragraphs.
	if !strings.Contains(result, "\n\n") {
		t.Error("expected blank line between paragraphs")
	}
}

func TestRenderMarkdownListItemReflow(t *testing.T) {
	// Long list item text with soft breaks should reflow.
	input := "- This is a long list item that\n  was written at a narrow width."
	result := stripped(input, 80)

	// At width 80, the list item text should reflow to fewer lines.
	if !strings.Contains(result, "long list item that was written") {
		t.Errorf("expected list item text reflowed, got:\n%s", result)
	}
}

func TestStripHTMLTags(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"<p>hello</p>", "hello"},
		{"no tags", "no tags"},
		{"<b>bold</b> and <i>italic</i>", "bold and italic"},
		{"<br/>", ""},
		{"", ""},
	}
	for _, test := range tests {
		result := stripHTMLTags(test.input)
		if result != test.expected {
			t.Errorf("stripHTMLTags(%q) = %q, want %q", test.input, result, test.expected)
		}
	}
}
