// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package context

import "github.com/bureau-foundation/bureau/lib/llm"

// turnGroup identifies a contiguous slice of messages that form an
// atomic eviction unit. A turn group starts with a user message
// containing text content (a "real" user prompt, not tool results)
// and includes all subsequent messages until the next such user
// message.
//
// Examples of a single turn group:
//   - user(text) → assistant(text)
//   - user(text) → assistant(tool_use) → user(tool_results) → assistant(text)
//   - user(text) → assistant(tool_use) → user(tool_results) → assistant(tool_use) → user(tool_results) → assistant(text)
type turnGroup struct {
	startIndex int // inclusive index into the message slice
	endIndex   int // exclusive index into the message slice
}

// identifyTurnGroups partitions a message slice into turn groups.
// Each group starts at a user-role message containing at least one
// ContentText block. User-role messages containing only
// ContentToolResult blocks are continuations of the preceding group,
// not new group boundaries.
//
// Returns nil if messages is empty.
func identifyTurnGroups(messages []llm.Message) []turnGroup {
	var groups []turnGroup
	currentStart := -1

	for i, message := range messages {
		if message.Role == llm.RoleUser && messageHasTextContent(message) {
			if currentStart >= 0 {
				groups = append(groups, turnGroup{
					startIndex: currentStart,
					endIndex:   i,
				})
			}
			currentStart = i
		}
	}

	// Close the final group.
	if currentStart >= 0 {
		groups = append(groups, turnGroup{
			startIndex: currentStart,
			endIndex:   len(messages),
		})
	}

	return groups
}

// messageHasTextContent reports whether a message contains at least
// one ContentText block. Used to distinguish user prompts (which
// start new turn groups) from tool result messages (which continue
// existing groups).
func messageHasTextContent(message llm.Message) bool {
	for _, block := range message.Content {
		if block.Type == llm.ContentText {
			return true
		}
	}
	return false
}

// messageCharCount returns the total character count across all
// content blocks in a message, plus a fixed overhead for the message
// structure (role marker, JSON framing). Used by [CharEstimator] for
// token estimation.
func messageCharCount(message llm.Message) int {
	count := 0
	for _, block := range message.Content {
		switch block.Type {
		case llm.ContentText:
			count += len(block.Text)
		case llm.ContentToolUse:
			if block.ToolUse != nil {
				count += len(block.ToolUse.Name)
				count += len(block.ToolUse.Input)
			}
		case llm.ContentToolResult:
			if block.ToolResult != nil {
				count += len(block.ToolResult.Content)
				count += len(block.ToolResult.ToolUseID)
			}
		}
	}
	// Fixed cost per message for role markers and JSON structure
	// overhead (~20 chars for {"role":"user","content":[...]}).
	count += 20
	return count
}

// messagesCharCount returns the total character count across all
// messages in a slice.
func messagesCharCount(messages []llm.Message) int {
	total := 0
	for i := range messages {
		total += messageCharCount(messages[i])
	}
	return total
}
