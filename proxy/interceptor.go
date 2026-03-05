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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// interceptorTransactionCounter generates unique transaction IDs for
// Matrix event publishing from response interceptors.
var interceptorTransactionCounter atomic.Int64

// compiledInterceptor is a response interceptor with its regex compiled
// and event content template parsed. Created from a schema.ResponseInterceptor
// at service registration time.
type compiledInterceptor struct {
	method    string
	pattern   *regexp.Regexp
	statusMin int
	statusMax int
	eventType string
	// eventContent is the template: string values may contain ${...}
	// placeholders, non-string values pass through unchanged.
	eventContent map[string]any
}

// compileInterceptors validates and compiles a slice of response
// interceptor definitions. Returns an error if any definition is
// invalid (missing required fields, bad regex).
func compileInterceptors(definitions []schema.ResponseInterceptor) ([]compiledInterceptor, error) {
	if len(definitions) == 0 {
		return nil, nil
	}

	result := make([]compiledInterceptor, 0, len(definitions))
	for i, definition := range definitions {
		if definition.PathPattern == "" {
			return nil, fmt.Errorf("interceptor %d: path_pattern is required", i)
		}
		if definition.EventType == "" {
			return nil, fmt.Errorf("interceptor %d: event_type is required", i)
		}
		if len(definition.EventContent) == 0 {
			return nil, fmt.Errorf("interceptor %d: event_content is required", i)
		}

		compiled, err := regexp.Compile(definition.PathPattern)
		if err != nil {
			return nil, fmt.Errorf("interceptor %d: invalid path_pattern %q: %w", i, definition.PathPattern, err)
		}

		statusMin := definition.StatusMin
		statusMax := definition.StatusMax
		if statusMin == 0 && statusMax == 0 {
			statusMin = 200
			statusMax = 299
		}
		if statusMin > statusMax {
			return nil, fmt.Errorf("interceptor %d: status_min (%d) > status_max (%d)", i, statusMin, statusMax)
		}

		result = append(result, compiledInterceptor{
			method:       definition.Method,
			pattern:      compiled,
			statusMin:    statusMin,
			statusMax:    statusMax,
			eventType:    definition.EventType,
			eventContent: definition.EventContent,
		})
	}
	return result, nil
}

// match checks if an HTTP response matches this interceptor's pattern.
// Returns the path regex capture groups (index 0 is the full match)
// and true, or nil and false.
func (c *compiledInterceptor) match(method, path string, statusCode int) ([]string, bool) {
	if c.method != "" && method != c.method {
		return nil, false
	}
	if statusCode < c.statusMin || statusCode > c.statusMax {
		return nil, false
	}
	captures := c.pattern.FindStringSubmatch(path)
	if captures == nil {
		return nil, false
	}
	return captures, true
}

// buildEventContent evaluates the event content template against the
// provided captures, response body, and agent identity. Returns the
// resolved event content as a JSON-serializable map.
func (c *compiledInterceptor) buildEventContent(
	captures []string,
	responseBody []byte,
	agentUserID string,
) (map[string]any, error) {
	// Parse response body lazily — only if any placeholder references it.
	var responseJSON map[string]any
	var responseParseError error
	var responseParsed bool

	parseResponse := func() (map[string]any, error) {
		if responseParsed {
			return responseJSON, responseParseError
		}
		responseParsed = true
		if err := json.Unmarshal(responseBody, &responseJSON); err != nil {
			responseParseError = fmt.Errorf("parsing response JSON: %w", err)
		}
		return responseJSON, responseParseError
	}

	agentDisplay := extractAgentDisplay(agentUserID)

	result := make(map[string]any, len(c.eventContent))
	for key, value := range c.eventContent {
		stringValue, isString := value.(string)
		if !isString {
			// Non-string values (numbers, bools, nested objects) pass through.
			result[key] = value
			continue
		}

		resolved, err := resolveTemplate(stringValue, captures, parseResponse, agentUserID, agentDisplay)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", key, err)
		}
		result[key] = resolved
	}
	return result, nil
}

// resolveTemplate evaluates a single template string. If the string is
// exactly one ${response.field} placeholder, the JSON type is preserved.
// Otherwise, all placeholders are resolved to strings and concatenated.
func resolveTemplate(
	template string,
	captures []string,
	parseResponse func() (map[string]any, error),
	agentUserID, agentDisplay string,
) (any, error) {
	// Fast path: no placeholders.
	if !strings.Contains(template, "${") {
		return template, nil
	}

	// Check if the entire value is a single placeholder — if so,
	// preserve the original type for response fields.
	if isSinglePlaceholder(template) {
		name := template[2 : len(template)-1] // strip ${ and }
		return resolvePlaceholder(name, captures, parseResponse, agentUserID, agentDisplay, true)
	}

	// Mixed template: resolve all placeholders to strings.
	var builder strings.Builder
	remaining := template
	for {
		start := strings.Index(remaining, "${")
		if start == -1 {
			builder.WriteString(remaining)
			break
		}
		builder.WriteString(remaining[:start])

		end := strings.Index(remaining[start:], "}")
		if end == -1 {
			// Unclosed placeholder — treat as literal.
			builder.WriteString(remaining[start:])
			break
		}
		end += start // adjust to absolute position

		name := remaining[start+2 : end]
		value, err := resolvePlaceholder(name, captures, parseResponse, agentUserID, agentDisplay, false)
		if err != nil {
			return nil, err
		}
		builder.WriteString(fmt.Sprintf("%v", value))
		remaining = remaining[end+1:]
	}
	return builder.String(), nil
}

// isSinglePlaceholder returns true if the string is exactly "${...}"
// with no other content.
func isSinglePlaceholder(value string) bool {
	if !strings.HasPrefix(value, "${") || !strings.HasSuffix(value, "}") {
		return false
	}
	// Ensure there's no nested ${ or extra }.
	inner := value[2 : len(value)-1]
	return !strings.Contains(inner, "${") && !strings.Contains(inner, "}")
}

// resolvePlaceholder resolves a single placeholder name to its value.
// When preserveType is true and the source is a JSON response field,
// the original JSON type is preserved.
func resolvePlaceholder(
	name string,
	captures []string,
	parseResponse func() (map[string]any, error),
	agentUserID, agentDisplay string,
	preserveType bool,
) (any, error) {
	switch {
	case name == "agent":
		return agentUserID, nil

	case name == "agent_display":
		return agentDisplay, nil

	case strings.HasPrefix(name, "path."):
		indexString := name[5:]
		index, err := strconv.Atoi(indexString)
		if err != nil {
			return nil, fmt.Errorf("invalid path capture index %q", indexString)
		}
		if index < 1 || index >= len(captures) {
			return "", nil // Out of range — empty string, not an error.
		}
		return captures[index], nil

	case strings.HasPrefix(name, "response."):
		fieldName := name[9:]
		responseJSON, err := parseResponse()
		if err != nil {
			return nil, err
		}
		value, exists := responseJSON[fieldName]
		if !exists {
			if preserveType {
				return nil, nil // Missing field → JSON null.
			}
			return "", nil // Missing field in mixed string → empty.
		}
		if preserveType {
			return value, nil
		}
		return fmt.Sprintf("%v", value), nil

	default:
		return nil, fmt.Errorf("unknown placeholder ${%s}", name)
	}
}

// extractAgentDisplay extracts a human-readable display name from a
// Matrix user ID. For "@bureau/fleet/prod/agent/coder:server", returns
// "coder" (the last segment of the localpart).
func extractAgentDisplay(userID string) string {
	if !strings.HasPrefix(userID, "@") {
		return userID
	}
	colonIndex := strings.Index(userID, ":")
	if colonIndex < 2 {
		return userID
	}
	localpart := userID[1:colonIndex]
	parts := strings.Split(localpart, "/")
	return parts[len(parts)-1]
}

// interceptorContext holds the handler-level state needed by interceptor
// goroutines. Captured once under the handler's read lock before
// spawning the goroutine.
type interceptorContext struct {
	matrixService *HTTPService
	agentUserID   string
	workspaceRoom ref.RoomID
}

// runInterceptors checks all interceptors against the response and
// publishes Matrix events for any matches. Runs asynchronously — the
// caller is responsible for spawning in a goroutine.
func runInterceptors(
	interceptors []compiledInterceptor,
	context interceptorContext,
	method, path string,
	statusCode int,
	responseBody []byte,
	logger interface {
		Info(string, ...any)
		Warn(string, ...any)
	},
) {
	if context.workspaceRoom.IsZero() {
		return
	}
	if context.matrixService == nil || context.agentUserID == "" {
		return
	}

	for i := range interceptors {
		captures, matched := interceptors[i].match(method, path, statusCode)
		if !matched {
			continue
		}

		eventContent, err := interceptors[i].buildEventContent(
			captures, responseBody, context.agentUserID,
		)
		if err != nil {
			logger.Warn("interceptor: failed to build event content",
				"event_type", interceptors[i].eventType,
				"error", err,
			)
			continue
		}

		publishInterceptedEvent(
			context.matrixService,
			interceptors[i].eventType,
			eventContent,
			context.workspaceRoom,
			logger,
		)
	}
}

// publishInterceptedEvent sends a Matrix event to the workspace room.
// Uses the proxy's Matrix HTTP service to forward the PUT request to
// the homeserver with the agent's credentials.
//
// Best-effort: if publishing fails, the error is logged but does not
// propagate to the caller.
func publishInterceptedEvent(
	matrixService *HTTPService,
	eventType string,
	content map[string]any,
	workspaceRoom ref.RoomID,
	logger interface {
		Info(string, ...any)
		Warn(string, ...any)
	},
) {
	roomID := workspaceRoom.String()

	contentJSON, err := json.Marshal(content)
	if err != nil {
		logger.Warn("interceptor: failed to marshal event content",
			"event_type", eventType,
			"error", err,
		)
		return
	}

	transactionID := fmt.Sprintf("intercept_%d_%d",
		time.Now().UnixMilli(), //nolint:realclock // transaction ID uniqueness only
		interceptorTransactionCounter.Add(1))

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
		logger.Warn("interceptor: failed to publish event",
			"event_type", eventType,
			"room", roomID,
			"error", err,
		)
		return
	}
	response.Body.Close()

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		logger.Info("interceptor: event published",
			"event_type", eventType,
			"room", roomID,
		)
	} else {
		logger.Warn("interceptor: homeserver rejected event",
			"event_type", eventType,
			"room", roomID,
			"status", response.StatusCode,
		)
	}
}
