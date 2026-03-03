// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema/forge"
)

// entityPattern matches GitHub API paths that create forge entities.
// Capture groups extract the owner and repo segments.
var entityPatterns = []entityPatternDef{
	{
		Method:      "POST",
		Pattern:     regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls$`),
		EntityType:  "pull_request",
		NumberField: "number",
	},
	{
		Method:      "POST",
		Pattern:     regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues$`),
		EntityType:  "issue",
		NumberField: "number",
	},
	{
		Method:     "POST",
		Pattern:    regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/git/commits$`),
		EntityType: "commit",
		SHAField:   "sha",
	},
}

type entityPatternDef struct {
	Method      string
	Pattern     *regexp.Regexp
	EntityType  string
	NumberField string // JSON field name for issue/PR number
	SHAField    string // JSON field name for commit SHA
}

// entityCreationMatch holds the parsed result of a successful entity
// creation API response.
type entityCreationMatch struct {
	Owner      string
	Repo       string
	EntityType string
	Number     int
	SHA        string
	Title      string
	URL        string
}

// attributionTransactionCounter generates unique transaction IDs for
// Matrix event publishing.
var attributionTransactionCounter atomic.Int64

// matchEntityCreation checks if an HTTP request/response pair
// represents a successful entity creation. Returns the match details
// and true, or zero value and false.
func matchEntityCreation(method, path string, statusCode int, responseBody []byte) (entityCreationMatch, bool) {
	if statusCode < 200 || statusCode >= 300 {
		return entityCreationMatch{}, false
	}

	for _, pattern := range entityPatterns {
		if method != pattern.Method {
			continue
		}
		matches := pattern.Pattern.FindStringSubmatch(path)
		if matches == nil {
			continue
		}

		owner := matches[1]
		repo := matches[2]

		var parsed map[string]any
		if err := json.Unmarshal(responseBody, &parsed); err != nil {
			return entityCreationMatch{}, false
		}

		result := entityCreationMatch{
			Owner:      owner,
			Repo:       repo,
			EntityType: pattern.EntityType,
		}

		if pattern.NumberField != "" {
			if number, ok := parsed[pattern.NumberField].(float64); ok {
				result.Number = int(number)
			} else {
				return entityCreationMatch{}, false
			}
		}
		if pattern.SHAField != "" {
			if sha, ok := parsed[pattern.SHAField].(string); ok {
				result.SHA = sha
			} else {
				return entityCreationMatch{}, false
			}
		}

		// Extract optional fields for human-readable output.
		if title, ok := parsed["title"].(string); ok {
			result.Title = title
		}
		if message, ok := parsed["message"].(string); ok && result.Title == "" {
			// Commits use "message" instead of "title".
			firstLine, _, _ := strings.Cut(message, "\n")
			result.Title = firstLine
		}
		if htmlURL, ok := parsed["html_url"].(string); ok {
			result.URL = htmlURL
		}

		return result, true
	}

	return entityCreationMatch{}, false
}

// publishAttribution sends an m.bureau.forge_attribution event to
// the repo's Matrix room. Uses the proxy's existing Matrix HTTP
// service to forward the PUT request to the homeserver with the
// agent's credentials.
//
// This is a best-effort operation: if publishing fails (room not
// found, agent not in room, homeserver down), the API response to
// the agent is not affected. The attribution record is simply not
// created, and on_author won't fire for this entity.
func publishAttribution(
	matrixService *HTTPService,
	agentUserID string,
	match entityCreationMatch,
	repoRooms map[string]string,
	logger interface {
		Info(string, ...any)
		Warn(string, ...any)
	},
) {
	fullRepo := match.Owner + "/" + match.Repo
	roomID, found := repoRooms[fullRepo]
	if !found {
		logger.Warn("attribution: no room mapping for repo",
			"repo", fullRepo,
		)
		return
	}

	// Build the agent's display name from the localpart.
	agentDisplay := agentUserID
	if atIndex := strings.Index(agentUserID, "@"); atIndex == 0 {
		if colonIndex := strings.Index(agentUserID, ":"); colonIndex > 1 {
			localpart := agentUserID[1:colonIndex]
			// Use the last segment of the localpart as the display name.
			parts := strings.Split(localpart, "/")
			agentDisplay = parts[len(parts)-1]
		}
	}

	// Build human-readable body.
	entityLabel := fmt.Sprintf("%s #%d", match.EntityType, match.Number)
	if match.SHA != "" {
		entityLabel = "commit " + match.SHA[:min(12, len(match.SHA))]
	}

	var body, formattedBody string
	if match.URL != "" && match.Title != "" {
		body = fmt.Sprintf("%s created %s: %s\n%s", agentDisplay, entityLabel, match.Title, match.URL)
		formattedBody = fmt.Sprintf("<b>%s</b> created <a href=\"%s\">%s: %s</a>",
			agentDisplay, match.URL, entityLabel, match.Title)
	} else if match.Title != "" {
		body = fmt.Sprintf("%s created %s: %s", agentDisplay, entityLabel, match.Title)
		formattedBody = fmt.Sprintf("<b>%s</b> created %s: %s", agentDisplay, entityLabel, match.Title)
	} else {
		body = fmt.Sprintf("%s created %s on %s", agentDisplay, entityLabel, fullRepo)
		formattedBody = fmt.Sprintf("<b>%s</b> created %s on <code>%s</code>", agentDisplay, entityLabel, fullRepo)
	}

	attribution := forge.ForgeAttribution{
		Agent:         agentUserID,
		Provider:      "github",
		Repo:          fullRepo,
		EntityType:    match.EntityType,
		EntityNumber:  match.Number,
		EntitySHA:     match.SHA,
		EntityURL:     match.URL,
		Title:         match.Title,
		Body:          body,
		FormattedBody: formattedBody,
	}

	contentJSON, err := json.Marshal(attribution)
	if err != nil {
		logger.Warn("attribution: failed to marshal event content",
			"error", err,
		)
		return
	}

	transactionID := fmt.Sprintf("attr_%d_%d",
		time.Now().UnixMilli(), //nolint:realclock // transaction ID uniqueness only
		attributionTransactionCounter.Add(1))

	eventType := string(forge.EventTypeForgeAttribution)
	path := "/_matrix/client/v3/rooms/" +
		url.PathEscape(roomID) + "/send/" +
		url.PathEscape(eventType) + "/" +
		url.PathEscape(transactionID)

	response, err := matrixService.ForwardRequest(
		context.Background(),
		"PUT",
		path,
		bytes.NewReader(contentJSON),
	)
	if err != nil {
		logger.Warn("attribution: failed to publish event",
			"repo", fullRepo,
			"room", roomID,
			"error", err,
		)
		return
	}
	response.Body.Close()

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		logger.Info("attribution published",
			"repo", fullRepo,
			"room", roomID,
			"entity_type", match.EntityType,
			"entity_number", match.Number,
			"entity_sha", match.SHA,
			"agent", agentUserID,
		)
	} else {
		logger.Warn("attribution: homeserver rejected event",
			"repo", fullRepo,
			"room", roomID,
			"status", response.StatusCode,
		)
	}
}
