// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/messaging"
)

// formatTimestamp converts an origin_server_ts (milliseconds since epoch) to a
// human-readable local time string.
func formatTimestamp(originServerTS int64) string {
	t := time.UnixMilli(originServerTS)
	return t.Local().Format("2006-01-02 15:04:05")
}

// formatEventOneLine renders an event as a single human-readable line suitable
// for watch and messages output. The format is:
//
//	TIMESTAMP  SENDER  TYPE  SUMMARY
//
// For m.room.message events, the summary is the message body text.
// For state events (non-nil state_key), the summary is [state_key] + abbreviated content.
// For other events, the summary is abbreviated JSON content.
func formatEventOneLine(event messaging.Event) string {
	timestamp := formatTimestamp(event.OriginServerTS)
	summary := eventSummary(event)
	return fmt.Sprintf("%-19s  %-38s  %-30s  %s", timestamp, event.Sender, event.Type, summary)
}

// eventSummary produces a short human-readable summary of an event's content.
func eventSummary(event messaging.Event) string {
	// m.room.message: show the body text directly.
	if event.Type == "m.room.message" {
		if body, ok := event.Content["body"].(string); ok {
			if len(body) > 120 {
				return body[:117] + "..."
			}
			return body
		}
	}

	// State events: prefix with [state_key].
	if event.StateKey != nil {
		prefix := fmt.Sprintf("[%s] ", *event.StateKey)
		return prefix + abbreviateContent(event.Content, 120-len(prefix))
	}

	// Everything else: abbreviated JSON.
	return abbreviateContent(event.Content, 120)
}

// matchesTypeFilter checks whether an event type matches any of the given
// filter patterns. An empty filter list matches everything. Each filter is
// either an exact match or a prefix match if it ends with "*".
func matchesTypeFilter(eventType string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, filter := range filters {
		if strings.HasSuffix(filter, "*") {
			prefix := strings.TrimSuffix(filter, "*")
			if strings.HasPrefix(eventType, prefix) {
				return true
			}
		} else if eventType == filter {
			return true
		}
	}
	return false
}

// abbreviateContent returns a truncated string representation of event content.
// It serializes the content map to compact JSON and truncates if it exceeds
// maxLength characters.
func abbreviateContent(content map[string]any, maxLength int) string {
	if maxLength <= 0 {
		return ""
	}
	if len(content) == 0 {
		return "{}"
	}
	data, err := json.Marshal(content)
	if err != nil {
		return fmt.Sprintf("<%v>", err)
	}
	s := string(data)
	if len(s) > maxLength {
		if maxLength <= 3 {
			return s[:maxLength]
		}
		return s[:maxLength-3] + "..."
	}
	return s
}
