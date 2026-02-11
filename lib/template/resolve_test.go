// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// templateTestState holds state for a mock Matrix server used by template
// resolution tests. It supports room alias resolution and state event
// fetching — the two operations needed for template resolution.
type templateTestState struct {
	roomAliases map[string]string
	stateEvents map[string]json.RawMessage
}

func newTemplateTestState() *templateTestState {
	return &templateTestState{
		roomAliases: make(map[string]string),
		stateEvents: make(map[string]json.RawMessage),
	}
}

func (state *templateTestState) setRoomAlias(alias, roomID string) {
	state.roomAliases[alias] = roomID
}

func (state *templateTestState) setTemplate(roomID, templateName string, content schema.TemplateContent) {
	data, err := json.Marshal(content)
	if err != nil {
		panic(fmt.Sprintf("marshaling template content: %v", err))
	}
	key := roomID + "\x00" + schema.EventTypeTemplate + "\x00" + templateName
	state.stateEvents[key] = data
}

func (state *templateTestState) handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.RawPath
		if path == "" {
			path = request.URL.Path
		}

		switch {
		case strings.HasPrefix(path, "/_matrix/client/v3/directory/room/"):
			state.handleResolveAlias(writer, path)
		case strings.Contains(path, "/state/"):
			state.handleGetStateEvent(writer, path)
		default:
			http.Error(writer, fmt.Sprintf(`{"errcode":"M_UNRECOGNIZED","error":"unknown path: %s"}`, path), http.StatusNotFound)
		}
	})
}

func (state *templateTestState) handleResolveAlias(writer http.ResponseWriter, path string) {
	encoded := strings.TrimPrefix(path, "/_matrix/client/v3/directory/room/")
	alias, err := url.PathUnescape(encoded)
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad alias encoding"}`, http.StatusBadRequest)
		return
	}

	roomID, exists := state.roomAliases[alias]
	if !exists {
		writer.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(writer, `{"errcode":"M_NOT_FOUND","error":"room alias %q not found"}`, alias)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(writer, `{"room_id":"%s"}`, roomID)
}

func (state *templateTestState) handleGetStateEvent(writer http.ResponseWriter, path string) {
	trimmed := strings.TrimPrefix(path, "/_matrix/client/v3/rooms/")
	parts := strings.SplitN(trimmed, "/state/", 2)
	if len(parts) != 2 {
		http.Error(writer, `{"errcode":"M_UNRECOGNIZED","error":"bad state path"}`, http.StatusBadRequest)
		return
	}

	roomID, err := url.PathUnescape(parts[0])
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad roomId encoding"}`, http.StatusBadRequest)
		return
	}

	eventAndKey := parts[1]
	slashIndex := strings.Index(eventAndKey, "/")
	if slashIndex < 0 {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"missing state key"}`, http.StatusBadRequest)
		return
	}
	eventType, err := url.PathUnescape(eventAndKey[:slashIndex])
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad eventType encoding"}`, http.StatusBadRequest)
		return
	}
	stateKey, err := url.PathUnescape(eventAndKey[slashIndex+1:])
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad stateKey encoding"}`, http.StatusBadRequest)
		return
	}

	key := roomID + "\x00" + eventType + "\x00" + stateKey
	content, exists := state.stateEvents[key]
	if !exists {
		writer.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(writer, `{"errcode":"M_NOT_FOUND","error":"state event not found: %s/%s in %s"}`, eventType, stateKey, roomID)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Write(content)
}

func newTestSession(t *testing.T, state *templateTestState) *messaging.Session {
	t.Helper()
	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken("@test:test.local", "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

const testServerName = "test.local"

func TestFetchSimple(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")
	state.setTemplate("!template:test", "base", schema.TemplateContent{
		Description: "Base template",
		Command:     []string{"/bin/bash"},
	})

	session := newTestSession(t, state)
	ctx := context.Background()

	ref, err := schema.ParseTemplateRef("bureau/template:base")
	if err != nil {
		t.Fatalf("ParseTemplateRef: %v", err)
	}

	template, err := Fetch(ctx, session, ref, testServerName)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if template.Description != "Base template" {
		t.Errorf("Description = %q, want %q", template.Description, "Base template")
	}
	if len(template.Command) != 1 || template.Command[0] != "/bin/bash" {
		t.Errorf("Command = %v, want [/bin/bash]", template.Command)
	}
}

func TestResolveSimple(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")
	state.setTemplate("!template:test", "base", schema.TemplateContent{
		Description: "Base sandbox template",
		Command:     []string{"/bin/bash"},
		Filesystem: []schema.TemplateMount{
			{Source: "/usr", Dest: "/usr", Mode: "ro"},
			{Source: "/bin", Dest: "/bin", Mode: "ro"},
		},
		Namespaces: &schema.TemplateNamespaces{PID: true, Net: true},
		Security:   &schema.TemplateSecurity{NoNewPrivs: true, DieWithParent: true},
	})

	session := newTestSession(t, state)
	ctx := context.Background()

	template, err := Resolve(ctx, session, "bureau/template:base", testServerName)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if template.Description != "Base sandbox template" {
		t.Errorf("Description = %q, want %q", template.Description, "Base sandbox template")
	}
	if template.Inherits != "" {
		t.Errorf("Inherits should be cleared after resolution, got %q", template.Inherits)
	}
}

func TestResolveSingleInheritance(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")

	state.setTemplate("!template:test", "base", schema.TemplateContent{
		Description: "Base template",
		Filesystem: []schema.TemplateMount{
			{Source: "/usr", Dest: "/usr", Mode: "ro"},
		},
		Namespaces:           &schema.TemplateNamespaces{PID: true, Net: true},
		EnvironmentVariables: map[string]string{"PATH": "/usr/bin:/bin"},
		CreateDirs:           []string{"/tmp"},
	})

	state.setTemplate("!template:test", "child", schema.TemplateContent{
		Description: "Child template",
		Inherits:    "bureau/template:base",
		Command:     []string{"/usr/local/bin/agent"},
		EnvironmentVariables: map[string]string{
			"PATH":  "/usr/local/bin:/usr/bin:/bin",
			"AGENT": "true",
		},
		Roles:               map[string][]string{"agent": {"/usr/local/bin/agent"}},
		RequiredCredentials: []string{"API_KEY"},
	})

	session := newTestSession(t, state)
	ctx := context.Background()

	template, err := Resolve(ctx, session, "bureau/template:child", testServerName)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if template.Description != "Child template" {
		t.Errorf("Description = %q, want %q", template.Description, "Child template")
	}
	if len(template.Command) != 1 || template.Command[0] != "/usr/local/bin/agent" {
		t.Errorf("Command = %v, want [/usr/local/bin/agent]", template.Command)
	}
	if template.Namespaces == nil || !template.Namespaces.PID {
		t.Error("Namespaces.PID should be inherited from parent")
	}
	if template.EnvironmentVariables["PATH"] != "/usr/local/bin:/usr/bin:/bin" {
		t.Errorf("PATH = %q, want child override", template.EnvironmentVariables["PATH"])
	}
	if template.EnvironmentVariables["AGENT"] != "true" {
		t.Error("AGENT env var should be present from child")
	}
	if len(template.Filesystem) != 1 {
		t.Errorf("Filesystem count = %d, want 1 (parent mount)", len(template.Filesystem))
	}
	if len(template.CreateDirs) != 1 {
		t.Errorf("CreateDirs count = %d, want 1 (parent dirs)", len(template.CreateDirs))
	}
	if len(template.RequiredCredentials) != 1 || template.RequiredCredentials[0] != "API_KEY" {
		t.Errorf("RequiredCredentials = %v, want [API_KEY]", template.RequiredCredentials)
	}
}

func TestResolveCrossRoomInheritance(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")
	state.setRoomAlias("#project/template:test.local", "!project-template:test")

	state.setTemplate("!template:test", "base", schema.TemplateContent{
		Command:    []string{"/bin/bash"},
		Namespaces: &schema.TemplateNamespaces{PID: true},
	})

	state.setTemplate("!project-template:test", "custom", schema.TemplateContent{
		Inherits:    "bureau/template:base",
		Description: "Cross-room child",
		DefaultPayload: map[string]any{
			"project": "test-project",
		},
	})

	session := newTestSession(t, state)
	ctx := context.Background()

	template, err := Resolve(ctx, session, "project/template:custom", testServerName)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if template.Description != "Cross-room child" {
		t.Errorf("Description = %q, want %q", template.Description, "Cross-room child")
	}
	if len(template.Command) != 1 || template.Command[0] != "/bin/bash" {
		t.Errorf("Command = %v, want inherited [/bin/bash]", template.Command)
	}
	if template.DefaultPayload["project"] != "test-project" {
		t.Errorf("DefaultPayload[project] = %v, want test-project", template.DefaultPayload["project"])
	}
}

func TestResolveCycleDetection(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")

	state.setTemplate("!template:test", "a", schema.TemplateContent{
		Inherits: "bureau/template:b",
		Command:  []string{"/bin/a"},
	})
	state.setTemplate("!template:test", "b", schema.TemplateContent{
		Inherits: "bureau/template:a",
		Command:  []string{"/bin/b"},
	})

	session := newTestSession(t, state)
	ctx := context.Background()

	_, err := Resolve(ctx, session, "bureau/template:a", testServerName)
	if err == nil {
		t.Fatal("expected cycle detection error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention cycle, got: %v", err)
	}
}

func TestResolveMissingParent(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")

	state.setTemplate("!template:test", "child", schema.TemplateContent{
		Inherits: "bureau/template:nonexistent",
		Command:  []string{"/bin/child"},
	})

	session := newTestSession(t, state)
	ctx := context.Background()

	_, err := Resolve(ctx, session, "bureau/template:child", testServerName)
	if err == nil {
		t.Fatal("expected error for missing parent template")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should mention the missing template name, got: %v", err)
	}
}

func TestMergeMountDeduplication(t *testing.T) {
	t.Parallel()

	parent := &schema.TemplateContent{
		Filesystem: []schema.TemplateMount{
			{Source: "/usr", Dest: "/usr", Mode: "ro"},
			{Type: "tmpfs", Dest: "/tmp", Options: "size=64M"},
		},
	}

	child := &schema.TemplateContent{
		Filesystem: []schema.TemplateMount{
			{Type: "tmpfs", Dest: "/tmp", Options: "size=128M"},
			{Source: "${WORKTREE}", Dest: "/workspace", Mode: "rw"},
		},
	}

	result := Merge(parent, child)

	// Should have 3 mounts: /usr (parent), /tmp (child override), /workspace (child new).
	if len(result.Filesystem) != 3 {
		t.Fatalf("Filesystem count = %d, want 3", len(result.Filesystem))
	}

	if result.Filesystem[0].Dest != "/usr" {
		t.Errorf("Filesystem[0].Dest = %q, want /usr", result.Filesystem[0].Dest)
	}

	found := false
	for _, mount := range result.Filesystem {
		if mount.Dest == "/tmp" {
			found = true
			if mount.Options != "size=128M" {
				t.Errorf("/tmp mount Options = %q, want size=128M (child should override parent)", mount.Options)
			}
		}
	}
	if !found {
		t.Error("/tmp mount not found in result")
	}
}

func TestMergeStringSlicesDeduplication(t *testing.T) {
	t.Parallel()

	parent := []string{"/tmp", "/var/tmp", "/run/bureau"}
	child := []string{"/run/bureau", "/workspace/.cache"}

	result := mergeStringSlices(parent, child)

	if len(result) != 4 {
		t.Fatalf("result count = %d, want 4, got %v", len(result), result)
	}
}

func TestMergeAnyMaps(t *testing.T) {
	t.Parallel()

	parent := map[string]any{"model": "default", "tokens": float64(4096)}
	child := map[string]any{"tokens": float64(8192), "project": "test"}

	result := MergeAnyMaps(parent, child)

	if result["model"] != "default" {
		t.Errorf("model = %v, want default (from parent)", result["model"])
	}
	if result["tokens"] != float64(8192) {
		t.Errorf("tokens = %v, want 8192 (child wins)", result["tokens"])
	}
	if result["project"] != "test" {
		t.Errorf("project = %v, want test (from child)", result["project"])
	}
}

func TestMergeAnyMapsNilInputs(t *testing.T) {
	t.Parallel()

	if result := MergeAnyMaps(nil, nil); result != nil {
		t.Errorf("MergeAnyMaps(nil, nil) = %v, want nil", result)
	}

	child := map[string]any{"key": "value"}
	if result := MergeAnyMaps(nil, child); result["key"] != "value" {
		t.Errorf("MergeAnyMaps(nil, child) should return child values")
	}

	parent := map[string]any{"key": "value"}
	if result := MergeAnyMaps(parent, nil); result["key"] != "value" {
		t.Errorf("MergeAnyMaps(parent, nil) should return parent values")
	}
}

func TestMergeScalarOverride(t *testing.T) {
	t.Parallel()

	parent := &schema.TemplateContent{
		Description: "parent",
		Command:     []string{"/bin/parent"},
		Environment: "/nix/store/parent-env",
	}

	child := &schema.TemplateContent{
		Description: "child",
		Command:     []string{"/bin/child"},
		Environment: "/nix/store/child-env",
	}

	result := Merge(parent, child)

	if result.Description != "child" {
		t.Errorf("Description = %q, want child override", result.Description)
	}
	if len(result.Command) != 1 || result.Command[0] != "/bin/child" {
		t.Errorf("Command = %v, want child override", result.Command)
	}
	if result.Environment != "/nix/store/child-env" {
		t.Errorf("Environment = %q, want child override", result.Environment)
	}
}

func TestMergePointerOverride(t *testing.T) {
	t.Parallel()

	parent := &schema.TemplateContent{
		Namespaces: &schema.TemplateNamespaces{PID: true, Net: true},
		Resources:  &schema.TemplateResources{CPUShares: 1024},
		Security:   &schema.TemplateSecurity{NoNewPrivs: true},
	}

	child := &schema.TemplateContent{
		Namespaces: &schema.TemplateNamespaces{PID: true, Net: false, IPC: true},
	}

	result := Merge(parent, child)

	// Child namespaces should fully replace parent.
	if result.Namespaces.Net {
		t.Error("Namespaces.Net should be false (child override)")
	}
	if !result.Namespaces.IPC {
		t.Error("Namespaces.IPC should be true (child override)")
	}

	// Resources and Security should be inherited from parent (child had nil).
	if result.Resources == nil || result.Resources.CPUShares != 1024 {
		t.Error("Resources should be inherited from parent")
	}
	if result.Security == nil || !result.Security.NoNewPrivs {
		t.Error("Security should be inherited from parent")
	}
}

func TestMergeHealthCheck(t *testing.T) {
	t.Parallel()

	parentHealthCheck := &schema.HealthCheck{
		Endpoint:         "/health",
		IntervalSeconds:  10,
		FailureThreshold: 3,
	}
	childHealthCheck := &schema.HealthCheck{
		Endpoint:           "/api/v1/status",
		IntervalSeconds:    30,
		TimeoutSeconds:     10,
		FailureThreshold:   5,
		GracePeriodSeconds: 60,
	}

	t.Run("parent only", func(t *testing.T) {
		parent := &schema.TemplateContent{HealthCheck: parentHealthCheck}
		child := &schema.TemplateContent{}

		result := Merge(parent, child)
		if result.HealthCheck == nil {
			t.Fatal("HealthCheck should be inherited from parent")
		}
		if result.HealthCheck.Endpoint != "/health" {
			t.Errorf("Endpoint = %q, want /health", result.HealthCheck.Endpoint)
		}
		if result.HealthCheck.IntervalSeconds != 10 {
			t.Errorf("IntervalSeconds = %d, want 10", result.HealthCheck.IntervalSeconds)
		}
	})

	t.Run("child overrides parent", func(t *testing.T) {
		parent := &schema.TemplateContent{HealthCheck: parentHealthCheck}
		child := &schema.TemplateContent{HealthCheck: childHealthCheck}

		result := Merge(parent, child)
		if result.HealthCheck == nil {
			t.Fatal("HealthCheck should be present")
		}
		if result.HealthCheck.Endpoint != "/api/v1/status" {
			t.Errorf("Endpoint = %q, want /api/v1/status (child override)", result.HealthCheck.Endpoint)
		}
		if result.HealthCheck.IntervalSeconds != 30 {
			t.Errorf("IntervalSeconds = %d, want 30 (child override)", result.HealthCheck.IntervalSeconds)
		}
		if result.HealthCheck.GracePeriodSeconds != 60 {
			t.Errorf("GracePeriodSeconds = %d, want 60 (child override)", result.HealthCheck.GracePeriodSeconds)
		}
	})

	t.Run("neither", func(t *testing.T) {
		parent := &schema.TemplateContent{}
		child := &schema.TemplateContent{}

		result := Merge(parent, child)
		if result.HealthCheck != nil {
			t.Errorf("HealthCheck should be nil when neither parent nor child has one")
		}
	})
}

func TestMergeClearsInherits(t *testing.T) {
	t.Parallel()

	parent := &schema.TemplateContent{
		Inherits: "some/room:grandparent",
		Command:  []string{"/bin/parent"},
	}

	child := &schema.TemplateContent{
		Inherits: "some/room:parent",
	}

	result := Merge(parent, child)

	if result.Inherits != "" {
		t.Errorf("Inherits should be cleared after merge, got %q", result.Inherits)
	}
}
