// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forgesub

import (
	"regexp"
	"strings"
)

// forgeMentionPattern matches @username in markdown text after code
// blocks and inline code have been stripped. The username must be
// preceded by whitespace or start-of-string (to avoid matching email
// addresses like user@example.com) and follow GitHub/Forgejo username
// rules: alphanumeric + hyphens, 1-39 characters, cannot start or
// end with a hyphen.
var forgeMentionPattern = regexp.MustCompile(`(?:^|[\s(,;!?])@([a-zA-Z0-9](?:[a-zA-Z0-9-]{0,37}[a-zA-Z0-9])?)`)

// ExtractMentions extracts unique @usernames from markdown text.
// Strips fenced code blocks (``` delimiters) and inline code spans
// (` delimiters) before matching to avoid false positives from
// code examples.
//
// Returns deduplicated usernames in the order they first appear,
// lowercased (forge usernames are case-insensitive).
func ExtractMentions(markdown string) []string {
	stripped := stripCode(markdown)

	matches := forgeMentionPattern.FindAllStringSubmatch(stripped, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[string]struct{})
	var usernames []string
	for _, match := range matches {
		username := strings.ToLower(match[1])
		if _, duplicate := seen[username]; duplicate {
			continue
		}
		seen[username] = struct{}{}
		usernames = append(usernames, username)
	}

	return usernames
}

// stripCode removes fenced code blocks and inline code spans from
// markdown text. Fenced blocks are delimited by lines starting with
// ``` (with optional language tag). Inline code is delimited by
// backtick pairs.
//
// This is intentionally not a full markdown parser. It handles the
// common cases that produce false-positive @mention matches:
// code examples containing @variable, @decorator, etc.
func stripCode(text string) string {
	// First pass: remove fenced code blocks (``` ... ```).
	// Process line by line to handle the line-start requirement.
	var result strings.Builder
	result.Grow(len(text))

	lines := strings.Split(text, "\n")
	inFencedBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFencedBlock = !inFencedBlock
			continue
		}
		if inFencedBlock {
			continue
		}
		result.WriteString(line)
		result.WriteByte('\n')
	}

	// Second pass: remove inline code spans (` ... `).
	withoutFenced := result.String()
	return stripInlineCode(withoutFenced)
}

// stripInlineCode removes inline code spans delimited by backticks.
// Handles single and double backtick delimiters (“ `code` “ and
// ``` “code“ ```).
func stripInlineCode(text string) string {
	var result strings.Builder
	result.Grow(len(text))

	i := 0
	for i < len(text) {
		if text[i] != '`' {
			result.WriteByte(text[i])
			i++
			continue
		}

		// Count opening backticks.
		backtickCount := 0
		for i < len(text) && text[i] == '`' {
			backtickCount++
			i++
		}

		// Find matching closing backticks.
		closingIndex := strings.Index(text[i:], strings.Repeat("`", backtickCount))
		if closingIndex == -1 {
			// No closing backticks — treat as literal text.
			result.WriteString(strings.Repeat("`", backtickCount))
			continue
		}

		// Skip the inline code content and closing backticks.
		i += closingIndex + backtickCount
	}

	return result.String()
}
