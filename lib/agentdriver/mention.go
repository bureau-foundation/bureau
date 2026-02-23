// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agentdriver

import (
	"encoding/json"
	"regexp"
	"slices"
)

// mentionPattern matches Matrix user IDs in message body text.
// Matrix user IDs are @localpart:server where:
//   - localpart: [a-z0-9._=/-]+
//   - server: hostname that starts and ends with alphanumeric, with dots
//     and hyphens allowed in the middle, plus an optional :port suffix
//
// Requiring the server name to end with an alphanumeric character prevents
// trailing punctuation from being absorbed — "ask @agent:bureau.local."
// correctly extracts "@agent:bureau.local" without the sentence-ending
// period.
//
// The pattern requires whitespace or start-of-string before the @ to
// prevent matching email addresses mid-word.
var mentionPattern = regexp.MustCompile(
	`(?:^|[\s])(@[a-z0-9._=/-]+:[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]+)?)(?:$|[\s,.\!\?\)\]])`,
)

// extractMentions scans a message body for Matrix user ID mentions
// (e.g., "@agent/worker:bureau.local") and returns a deduplicated list
// of matched user ID strings.
func extractMentions(body string) []string {
	matches := mentionPattern.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}

	var mentions []string
	for _, match := range matches {
		userID := match[1]
		if !slices.Contains(mentions, userID) {
			mentions = append(mentions, userID)
		}
	}
	return mentions
}

// extractEventMentions collects all user IDs mentioned in a Matrix event,
// combining structured m.mentions.user_ids with body @-pattern matches.
// Returns a deduplicated list.
func extractEventMentions(content map[string]any, body string) []string {
	var mentions []string

	// Structured m.mentions from Matrix spec.
	if mentionsField, ok := content["m.mentions"]; ok {
		raw, err := json.Marshal(mentionsField)
		if err == nil {
			var structured struct {
				UserIDs []string `json:"user_ids"`
			}
			if json.Unmarshal(raw, &structured) == nil {
				mentions = append(mentions, structured.UserIDs...)
			}
		}
	}

	// Body @-pattern mentions.
	bodyMentions := extractMentions(body)
	for _, mention := range bodyMentions {
		if !slices.Contains(mentions, mention) {
			mentions = append(mentions, mention)
		}
	}

	return mentions
}
