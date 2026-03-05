// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestCompileInterceptors_Valid(t *testing.T) {
	t.Parallel()
	definitions := []schema.ResponseInterceptor{
		{
			Method:      "POST",
			PathPattern: `^/repos/([^/]+)/([^/]+)/pulls$`,
			EventType:   "m.bureau.forge_attribution",
			EventContent: map[string]any{
				"agent":       "${agent}",
				"entity_type": "pull_request",
			},
		},
	}
	compiled, err := compileInterceptors(definitions)
	if err != nil {
		t.Fatalf("compileInterceptors() error: %v", err)
	}
	if len(compiled) != 1 {
		t.Fatalf("expected 1 interceptor, got %d", len(compiled))
	}
	if compiled[0].method != "POST" {
		t.Errorf("method = %q, want POST", compiled[0].method)
	}
	if compiled[0].statusMin != 200 || compiled[0].statusMax != 299 {
		t.Errorf("status range = %d-%d, want 200-299", compiled[0].statusMin, compiled[0].statusMax)
	}
}

func TestCompileInterceptors_CustomStatusRange(t *testing.T) {
	t.Parallel()
	definitions := []schema.ResponseInterceptor{
		{
			PathPattern:  `^/test$`,
			StatusMin:    201,
			StatusMax:    201,
			EventType:    "m.test",
			EventContent: map[string]any{"ok": true},
		},
	}
	compiled, err := compileInterceptors(definitions)
	if err != nil {
		t.Fatalf("compileInterceptors() error: %v", err)
	}
	if compiled[0].statusMin != 201 || compiled[0].statusMax != 201 {
		t.Errorf("status range = %d-%d, want 201-201", compiled[0].statusMin, compiled[0].statusMax)
	}
}

func TestCompileInterceptors_MissingPathPattern(t *testing.T) {
	t.Parallel()
	_, err := compileInterceptors([]schema.ResponseInterceptor{
		{EventType: "m.test", EventContent: map[string]any{"ok": true}},
	})
	if err == nil {
		t.Fatal("expected error for missing path_pattern")
	}
}

func TestCompileInterceptors_MissingEventType(t *testing.T) {
	t.Parallel()
	_, err := compileInterceptors([]schema.ResponseInterceptor{
		{PathPattern: `^/test$`, EventContent: map[string]any{"ok": true}},
	})
	if err == nil {
		t.Fatal("expected error for missing event_type")
	}
}

func TestCompileInterceptors_MissingEventContent(t *testing.T) {
	t.Parallel()
	_, err := compileInterceptors([]schema.ResponseInterceptor{
		{PathPattern: `^/test$`, EventType: "m.test"},
	})
	if err == nil {
		t.Fatal("expected error for missing event_content")
	}
}

func TestCompileInterceptors_InvalidRegex(t *testing.T) {
	t.Parallel()
	_, err := compileInterceptors([]schema.ResponseInterceptor{
		{PathPattern: `^/test[`, EventType: "m.test", EventContent: map[string]any{"ok": true}},
	})
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

func TestCompileInterceptors_InvalidStatusRange(t *testing.T) {
	t.Parallel()
	_, err := compileInterceptors([]schema.ResponseInterceptor{
		{PathPattern: `^/test$`, StatusMin: 300, StatusMax: 200, EventType: "m.test", EventContent: map[string]any{"ok": true}},
	})
	if err == nil {
		t.Fatal("expected error for status_min > status_max")
	}
}

func TestCompileInterceptors_Empty(t *testing.T) {
	t.Parallel()
	compiled, err := compileInterceptors(nil)
	if err != nil {
		t.Fatalf("compileInterceptors(nil) error: %v", err)
	}
	if compiled != nil {
		t.Errorf("expected nil, got %v", compiled)
	}
}

func TestCompiledInterceptor_Match_MethodAndPath(t *testing.T) {
	t.Parallel()
	definitions := []schema.ResponseInterceptor{
		{
			Method:       "POST",
			PathPattern:  `^/repos/([^/]+)/([^/]+)/pulls$`,
			EventType:    "m.test",
			EventContent: map[string]any{"ok": true},
		},
	}
	compiled, err := compileInterceptors(definitions)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}
	interceptor := compiled[0]

	// Match: correct method and path.
	captures, ok := interceptor.match("POST", "/repos/org/repo/pulls", 201)
	if !ok {
		t.Fatal("expected match")
	}
	if len(captures) != 3 || captures[1] != "org" || captures[2] != "repo" {
		t.Errorf("captures = %v, want [full, org, repo]", captures)
	}

	// No match: wrong method.
	_, ok = interceptor.match("GET", "/repos/org/repo/pulls", 200)
	if ok {
		t.Fatal("GET should not match POST-only interceptor")
	}

	// No match: wrong path.
	_, ok = interceptor.match("POST", "/repos/org/repo/comments", 201)
	if ok {
		t.Fatal("comments path should not match")
	}

	// No match: wrong status.
	_, ok = interceptor.match("POST", "/repos/org/repo/pulls", 422)
	if ok {
		t.Fatal("422 status should not match 200-299 range")
	}
}

func TestCompiledInterceptor_Match_AnyMethod(t *testing.T) {
	t.Parallel()
	definitions := []schema.ResponseInterceptor{
		{
			PathPattern:  `^/test$`,
			EventType:    "m.test",
			EventContent: map[string]any{"ok": true},
		},
	}
	compiled, err := compileInterceptors(definitions)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}
	interceptor := compiled[0]

	// Any method should match when Method is empty.
	if _, ok := interceptor.match("GET", "/test", 200); !ok {
		t.Error("GET should match when method is empty")
	}
	if _, ok := interceptor.match("POST", "/test", 200); !ok {
		t.Error("POST should match when method is empty")
	}
}

func TestBuildEventContent_Literals(t *testing.T) {
	t.Parallel()
	definitions := []schema.ResponseInterceptor{
		{
			PathPattern: `^/test$`,
			EventType:   "m.test",
			EventContent: map[string]any{
				"provider":    "github",
				"entity_type": "pull_request",
				"count":       42,
				"active":      true,
			},
		},
	}
	compiled, err := compileInterceptors(definitions)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	result, err := compiled[0].buildEventContent([]string{"/test"}, []byte("{}"), "@agent:server")
	if err != nil {
		t.Fatalf("buildEventContent error: %v", err)
	}

	if result["provider"] != "github" {
		t.Errorf("provider = %v, want github", result["provider"])
	}
	if result["entity_type"] != "pull_request" {
		t.Errorf("entity_type = %v, want pull_request", result["entity_type"])
	}
	// Non-string values pass through unchanged.
	if result["count"] != 42 {
		t.Errorf("count = %v, want 42", result["count"])
	}
	if result["active"] != true {
		t.Errorf("active = %v, want true", result["active"])
	}
}

func TestBuildEventContent_AgentPlaceholders(t *testing.T) {
	t.Parallel()
	definitions := []schema.ResponseInterceptor{
		{
			PathPattern: `^/test$`,
			EventType:   "m.test",
			EventContent: map[string]any{
				"agent":         "${agent}",
				"agent_display": "${agent_display}",
			},
		},
	}
	compiled, err := compileInterceptors(definitions)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	result, err := compiled[0].buildEventContent(
		[]string{"/test"},
		[]byte("{}"),
		"@bureau/fleet/prod/agent/coder:bureau.local",
	)
	if err != nil {
		t.Fatalf("buildEventContent error: %v", err)
	}

	if result["agent"] != "@bureau/fleet/prod/agent/coder:bureau.local" {
		t.Errorf("agent = %v", result["agent"])
	}
	if result["agent_display"] != "coder" {
		t.Errorf("agent_display = %v, want coder", result["agent_display"])
	}
}

func TestBuildEventContent_PathCaptures(t *testing.T) {
	t.Parallel()
	definitions := []schema.ResponseInterceptor{
		{
			PathPattern: `^/repos/([^/]+)/([^/]+)/pulls$`,
			EventType:   "m.test",
			EventContent: map[string]any{
				"owner": "${path.1}",
				"repo":  "${path.1}/${path.2}",
			},
		},
	}
	compiled, err := compileInterceptors(definitions)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	captures := []string{"/repos/my-org/my-repo/pulls", "my-org", "my-repo"}
	result, err := compiled[0].buildEventContent(captures, []byte("{}"), "@agent:server")
	if err != nil {
		t.Fatalf("buildEventContent error: %v", err)
	}

	if result["owner"] != "my-org" {
		t.Errorf("owner = %v, want my-org", result["owner"])
	}
	if result["repo"] != "my-org/my-repo" {
		t.Errorf("repo = %v, want my-org/my-repo", result["repo"])
	}
}

func TestBuildEventContent_ResponseFields(t *testing.T) {
	t.Parallel()
	definitions := []schema.ResponseInterceptor{
		{
			PathPattern: `^/test$`,
			EventType:   "m.test",
			EventContent: map[string]any{
				"number": "${response.number}",
				"title":  "${response.title}",
				"sha":    "${response.sha}",
			},
		},
	}
	compiled, err := compileInterceptors(definitions)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	body := `{"number": 42, "title": "Add feature", "sha": "abc123"}`
	result, err := compiled[0].buildEventContent([]string{"/test"}, []byte(body), "@agent:server")
	if err != nil {
		t.Fatalf("buildEventContent error: %v", err)
	}

	// Single placeholder for a number field preserves the JSON type.
	if number, ok := result["number"].(float64); !ok || number != 42 {
		t.Errorf("number = %v (%T), want 42 (float64)", result["number"], result["number"])
	}
	if result["title"] != "Add feature" {
		t.Errorf("title = %v, want 'Add feature'", result["title"])
	}
	if result["sha"] != "abc123" {
		t.Errorf("sha = %v, want abc123", result["sha"])
	}
}

func TestBuildEventContent_MissingResponseField(t *testing.T) {
	t.Parallel()
	definitions := []schema.ResponseInterceptor{
		{
			PathPattern: `^/test$`,
			EventType:   "m.test",
			EventContent: map[string]any{
				"value": "${response.missing}",
			},
		},
	}
	compiled, err := compileInterceptors(definitions)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	result, err := compiled[0].buildEventContent([]string{"/test"}, []byte("{}"), "@agent:server")
	if err != nil {
		t.Fatalf("buildEventContent error: %v", err)
	}

	// Single placeholder for missing field → nil (JSON null).
	if result["value"] != nil {
		t.Errorf("value = %v, want nil", result["value"])
	}
}

func TestBuildEventContent_OutOfRangeCapture(t *testing.T) {
	t.Parallel()
	definitions := []schema.ResponseInterceptor{
		{
			PathPattern: `^/test$`,
			EventType:   "m.test",
			EventContent: map[string]any{
				"value": "${path.5}",
			},
		},
	}
	compiled, err := compileInterceptors(definitions)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	result, err := compiled[0].buildEventContent([]string{"/test"}, []byte("{}"), "@agent:server")
	if err != nil {
		t.Fatalf("buildEventContent error: %v", err)
	}

	// Out of range capture → empty string.
	if result["value"] != "" {
		t.Errorf("value = %v, want empty string", result["value"])
	}
}

func TestBuildEventContent_UnknownPlaceholder(t *testing.T) {
	t.Parallel()
	definitions := []schema.ResponseInterceptor{
		{
			PathPattern: `^/test$`,
			EventType:   "m.test",
			EventContent: map[string]any{
				"value": "${unknown}",
			},
		},
	}
	compiled, err := compileInterceptors(definitions)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	_, err = compiled[0].buildEventContent([]string{"/test"}, []byte("{}"), "@agent:server")
	if err == nil {
		t.Fatal("expected error for unknown placeholder")
	}
}

func TestBuildEventContent_InvalidResponseJSON(t *testing.T) {
	t.Parallel()
	definitions := []schema.ResponseInterceptor{
		{
			PathPattern: `^/test$`,
			EventType:   "m.test",
			EventContent: map[string]any{
				"value": "${response.field}",
			},
		},
	}
	compiled, err := compileInterceptors(definitions)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	_, err = compiled[0].buildEventContent([]string{"/test"}, []byte("not json"), "@agent:server")
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

// TestBuildEventContent_ForgeAttribution_PullRequest verifies the full
// ForgeAttribution event content can be produced for a GitHub PR creation,
// matching the schema from lib/schema/forge/attribution.go.
func TestBuildEventContent_ForgeAttribution_PullRequest(t *testing.T) {
	t.Parallel()
	definitions := []schema.ResponseInterceptor{
		{
			Method:      "POST",
			PathPattern: `^/repos/([^/]+)/([^/]+)/pulls$`,
			EventType:   "m.bureau.forge_attribution",
			EventContent: map[string]any{
				"agent":         "${agent}",
				"provider":      "github",
				"repo":          "${path.1}/${path.2}",
				"entity_type":   "pull_request",
				"entity_number": "${response.number}",
				"entity_url":    "${response.html_url}",
				"title":         "${response.title}",
			},
		},
	}
	compiled, err := compileInterceptors(definitions)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	captures, ok := compiled[0].match("POST", "/repos/org/repo/pulls", 201)
	if !ok {
		t.Fatal("expected match")
	}

	body := `{"number": 42, "title": "Add feature", "html_url": "https://github.com/org/repo/pull/42"}`
	result, err := compiled[0].buildEventContent(
		captures,
		[]byte(body),
		"@bureau/fleet/prod/agent/coder:bureau.local",
	)
	if err != nil {
		t.Fatalf("buildEventContent error: %v", err)
	}

	if result["agent"] != "@bureau/fleet/prod/agent/coder:bureau.local" {
		t.Errorf("agent = %v", result["agent"])
	}
	if result["provider"] != "github" {
		t.Errorf("provider = %v", result["provider"])
	}
	if result["repo"] != "org/repo" {
		t.Errorf("repo = %v, want org/repo", result["repo"])
	}
	if result["entity_type"] != "pull_request" {
		t.Errorf("entity_type = %v", result["entity_type"])
	}
	if number, ok := result["entity_number"].(float64); !ok || number != 42 {
		t.Errorf("entity_number = %v (%T), want 42", result["entity_number"], result["entity_number"])
	}
	if result["entity_url"] != "https://github.com/org/repo/pull/42" {
		t.Errorf("entity_url = %v", result["entity_url"])
	}
	if result["title"] != "Add feature" {
		t.Errorf("title = %v", result["title"])
	}
}

// TestBuildEventContent_ForgeAttribution_Commit verifies commit attribution.
func TestBuildEventContent_ForgeAttribution_Commit(t *testing.T) {
	t.Parallel()
	definitions := []schema.ResponseInterceptor{
		{
			Method:      "POST",
			PathPattern: `^/repos/([^/]+)/([^/]+)/git/commits$`,
			EventType:   "m.bureau.forge_attribution",
			EventContent: map[string]any{
				"agent":       "${agent}",
				"provider":    "github",
				"repo":        "${path.1}/${path.2}",
				"entity_type": "commit",
				"entity_sha":  "${response.sha}",
				"title":       "${response.message}",
			},
		},
	}
	compiled, err := compileInterceptors(definitions)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	captures, ok := compiled[0].match("POST", "/repos/org/repo/git/commits", 201)
	if !ok {
		t.Fatal("expected match")
	}

	body := `{"sha": "abc123def456789", "message": "Initial commit\n\nMore details"}`
	result, err := compiled[0].buildEventContent(captures, []byte(body), "@agent:server")
	if err != nil {
		t.Fatalf("buildEventContent error: %v", err)
	}

	if result["entity_sha"] != "abc123def456789" {
		t.Errorf("entity_sha = %v", result["entity_sha"])
	}
	// The full message is preserved — first-line extraction is the consumer's job.
	if result["title"] != "Initial commit\n\nMore details" {
		t.Errorf("title = %v", result["title"])
	}
}

func TestExtractAgentDisplay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected string
	}{
		{"@bureau/fleet/prod/agent/coder:bureau.local", "coder"},
		{"@simple:server", "simple"},
		{"@a/b/c:server", "c"},
		{"not-a-matrix-id", "not-a-matrix-id"},
		{"@:server", "@:server"}, // colonIndex < 2
	}
	for _, test := range tests {
		result := extractAgentDisplay(test.input)
		if result != test.expected {
			t.Errorf("extractAgentDisplay(%q) = %q, want %q", test.input, result, test.expected)
		}
	}
}

func TestIsSinglePlaceholder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected bool
	}{
		{"${agent}", true},
		{"${response.number}", true},
		{"${path.1}", true},
		{"${path.1}/${path.2}", false}, // mixed
		{"literal", false},
		{"prefix${agent}", false},
		{"${agent}suffix", false},
		{"${a}${b}", false}, // two placeholders
		{"${}", true},       // edge case: empty name, still structurally single
	}
	for _, test := range tests {
		result := isSinglePlaceholder(test.input)
		if result != test.expected {
			t.Errorf("isSinglePlaceholder(%q) = %v, want %v", test.input, result, test.expected)
		}
	}
}
