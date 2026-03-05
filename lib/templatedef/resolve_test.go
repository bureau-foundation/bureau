// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package templatedef

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// templateTestState holds state for a mock Matrix server used by template
// resolution tests. It supports room alias resolution, individual state
// event fetching, and full room state fetching (with sender metadata).
type templateTestState struct {
	roomAliases map[string]string
	stateEvents map[string]json.RawMessage

	// roomStateEvents stores full room state (event arrays with sender
	// metadata) for GetRoomState responses. Key: roomID.
	roomStateEvents map[string][]templateTestEvent
}

// templateTestEvent represents a state event with sender metadata for
// GetRoomState mock responses.
type templateTestEvent struct {
	Type     string         `json:"type"`
	StateKey string         `json:"state_key"`
	Sender   string         `json:"sender"`
	Content  map[string]any `json:"content"`
}

func newTemplateTestState() *templateTestState {
	return &templateTestState{
		roomAliases:     make(map[string]string),
		stateEvents:     make(map[string]json.RawMessage),
		roomStateEvents: make(map[string][]templateTestEvent),
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
	key := roomID + "\x00" + string(schema.EventTypeTemplate) + "\x00" + templateName
	state.stateEvents[key] = data
}

// setTemplateWithSender stores a template and also registers it in the
// roomStateEvents for GetRoomState responses (which include the sender).
// This is needed for tests that use ResolveWithAuthor / FetchWithSender.
func (state *templateTestState) setTemplateWithSender(roomID, templateName string, content schema.TemplateContent, sender string) {
	// Also set it for individual state event lookups.
	state.setTemplate(roomID, templateName, content)

	// Build the map[string]any content representation for the room state
	// response (which uses map[string]any for Content, not typed structs).
	contentBytes, err := json.Marshal(content)
	if err != nil {
		panic(fmt.Sprintf("marshaling template content: %v", err))
	}
	var contentMap map[string]any
	if err := json.Unmarshal(contentBytes, &contentMap); err != nil {
		panic(fmt.Sprintf("unmarshaling template content to map: %v", err))
	}

	state.roomStateEvents[roomID] = append(state.roomStateEvents[roomID], templateTestEvent{
		Type:     string(schema.EventTypeTemplate),
		StateKey: templateName,
		Sender:   sender,
		Content:  contentMap,
	})
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
		case state.isGetRoomStatePath(path):
			state.handleGetRoomState(writer, path)
		case strings.Contains(path, "/state/"):
			state.handleGetStateEvent(writer, path)
		default:
			http.Error(writer, fmt.Sprintf(`{"errcode":"M_UNRECOGNIZED","error":"unknown path: %s"}`, path), http.StatusNotFound)
		}
	})
}

// isGetRoomStatePath returns true if path is /rooms/{id}/state (without a
// trailing event type), distinguishing it from /rooms/{id}/state/{type}/{key}.
func (state *templateTestState) isGetRoomStatePath(path string) bool {
	const prefix = "/_matrix/client/v3/rooms/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := path[len(prefix):]
	// Split on first "/" after room ID to get the sub-path.
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) < 2 {
		return false
	}
	// Must be exactly "state" with no further segments.
	return parts[1] == "state"
}

// handleGetRoomState returns all state events for a room as a JSON array,
// including sender metadata. Used by GetStateWithSender.
func (state *templateTestState) handleGetRoomState(writer http.ResponseWriter, path string) {
	trimmed := strings.TrimPrefix(path, "/_matrix/client/v3/rooms/")
	parts := strings.SplitN(trimmed, "/", 2)
	roomID, err := url.PathUnescape(parts[0])
	if err != nil {
		http.Error(writer, `{"errcode":"M_INVALID_PARAM","error":"bad roomId encoding"}`, http.StatusBadRequest)
		return
	}

	events, exists := state.roomStateEvents[roomID]
	if !exists || len(events) == 0 {
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte("[]"))
		return
	}

	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(events)
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

func newTestSession(t *testing.T, state *templateTestState) *messaging.DirectSession {
	t.Helper()
	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	testUserID, _ := ref.ParseUserID("@test:test.local")
	session, err := client.SessionFromToken(testUserID, "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

var testServerName = ref.MustParseServerName("test.local")

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
			{Source: "/usr", Dest: "/usr", Mode: schema.MountModeRO},
			{Source: "/bin", Dest: "/bin", Mode: schema.MountModeRO},
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
	if len(template.Inherits) != 0 {
		t.Errorf("Inherits should be cleared after resolution, got %v", template.Inherits)
	}
}

func TestResolveSingleInheritance(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")

	state.setTemplate("!template:test", "base", schema.TemplateContent{
		Description: "Base template",
		Filesystem: []schema.TemplateMount{
			{Source: "/usr", Dest: "/usr", Mode: schema.MountModeRO},
		},
		Namespaces:           &schema.TemplateNamespaces{PID: true, Net: true},
		EnvironmentVariables: map[string]string{"PATH": "/usr/bin:/bin"},
		CreateDirs:           []string{"/tmp"},
	})

	state.setTemplate("!template:test", "child", schema.TemplateContent{
		Description: "Child template",
		Inherits:    []string{"bureau/template:base"},
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
		Inherits:    []string{"bureau/template:base"},
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
		Inherits: []string{"bureau/template:b"},
		Command:  []string{"/bin/a"},
	})
	state.setTemplate("!template:test", "b", schema.TemplateContent{
		Inherits: []string{"bureau/template:a"},
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
		Inherits: []string{"bureau/template:nonexistent"},
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
			{Source: "/usr", Dest: "/usr", Mode: schema.MountModeRO},
			{Type: "tmpfs", Dest: "/tmp", Options: "size=64M"},
		},
	}

	child := &schema.TemplateContent{
		Filesystem: []schema.TemplateMount{
			{Type: "tmpfs", Dest: "/tmp", Options: "size=128M"},
			{Source: "${WORKSPACE_ROOT}/${PROJECT}", Dest: "/workspace", Mode: schema.MountModeRW},
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

func TestMergeSecretBindings(t *testing.T) {
	t.Parallel()

	parent := []schema.SecretBinding{
		{Key: "API_KEY", Env: "API_KEY"},
		{Key: "DB_PASSWORD", File: "db-password"},
	}
	child := []schema.SecretBinding{
		// Override parent's API_KEY binding (change from env to file).
		{Key: "API_KEY", File: "api-key.txt"},
		// Add a new binding.
		{Key: "TUNNEL_TOKEN", Env: "TUNNEL_TOKEN"},
	}

	result := mergeSecretBindings(parent, child)

	// Expect 3 bindings: DB_PASSWORD (from parent), API_KEY (child override),
	// TUNNEL_TOKEN (from child).
	if len(result) != 3 {
		t.Fatalf("result count = %d, want 3, got %v", len(result), result)
	}

	// DB_PASSWORD should come first (parent, not overridden).
	if result[0].Key != "DB_PASSWORD" || result[0].File != "db-password" {
		t.Errorf("result[0] = %+v, want DB_PASSWORD file binding from parent", result[0])
	}
	// API_KEY should be the child's version (file, not env).
	if result[1].Key != "API_KEY" || result[1].File != "api-key.txt" || result[1].Env != "" {
		t.Errorf("result[1] = %+v, want API_KEY file binding from child (overriding parent)", result[1])
	}
	// TUNNEL_TOKEN from child.
	if result[2].Key != "TUNNEL_TOKEN" || result[2].Env != "TUNNEL_TOKEN" {
		t.Errorf("result[2] = %+v, want TUNNEL_TOKEN env binding from child", result[2])
	}
}

func TestMergeSecretBindingsNilInputs(t *testing.T) {
	t.Parallel()

	bindings := []schema.SecretBinding{{Key: "TOKEN", Env: "TOKEN"}}

	if result := mergeSecretBindings(nil, nil); result != nil {
		t.Errorf("mergeSecretBindings(nil, nil) = %v, want nil", result)
	}
	if result := mergeSecretBindings(nil, bindings); len(result) != 1 || result[0].Key != "TOKEN" {
		t.Errorf("mergeSecretBindings(nil, child) should return child")
	}
	if result := mergeSecretBindings(bindings, nil); len(result) != 1 || result[0].Key != "TOKEN" {
		t.Errorf("mergeSecretBindings(parent, nil) should return parent")
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

func TestMergeProxyServices(t *testing.T) {
	t.Parallel()

	parent := &schema.TemplateContent{
		Command: []string{"/bin/parent"},
		ProxyServices: map[string]schema.TemplateProxyService{
			"anthropic": {
				Upstream:      "https://api.anthropic.com",
				InjectHeaders: map[string]string{"x-api-key": "ANTHROPIC_API_KEY"},
				StripHeaders:  []string{"x-api-key"},
			},
			"openai": {
				Upstream:      "https://api.openai.com",
				InjectHeaders: map[string]string{"Authorization": "OPENAI_BEARER"},
			},
		},
	}

	child := &schema.TemplateContent{
		// Override anthropic with different upstream, add a new service.
		ProxyServices: map[string]schema.TemplateProxyService{
			"anthropic": {
				Upstream:      "https://api.anthropic.com/v2",
				InjectHeaders: map[string]string{"x-api-key": "ANTHROPIC_API_KEY_V2"},
			},
			"github": {
				Upstream:      "https://api.github.com",
				InjectHeaders: map[string]string{"Authorization": "GITHUB_TOKEN"},
			},
		},
	}

	result := Merge(parent, child)

	if len(result.ProxyServices) != 3 {
		t.Fatalf("ProxyServices count = %d, want 3", len(result.ProxyServices))
	}

	// anthropic should be overridden by child.
	anthropic := result.ProxyServices["anthropic"]
	if anthropic.Upstream != "https://api.anthropic.com/v2" {
		t.Errorf("anthropic.Upstream = %q, want %q", anthropic.Upstream, "https://api.anthropic.com/v2")
	}
	if anthropic.InjectHeaders["x-api-key"] != "ANTHROPIC_API_KEY_V2" {
		t.Errorf("anthropic.InjectHeaders[x-api-key] = %q, want %q",
			anthropic.InjectHeaders["x-api-key"], "ANTHROPIC_API_KEY_V2")
	}

	// openai should be inherited from parent.
	openai := result.ProxyServices["openai"]
	if openai.Upstream != "https://api.openai.com" {
		t.Errorf("openai.Upstream = %q, want %q", openai.Upstream, "https://api.openai.com")
	}

	// github should come from child.
	github := result.ProxyServices["github"]
	if github.Upstream != "https://api.github.com" {
		t.Errorf("github.Upstream = %q, want %q", github.Upstream, "https://api.github.com")
	}
}

func TestMergeProxyServicesNilInputs(t *testing.T) {
	t.Parallel()

	// nil parent + non-nil child = child.
	parent := &schema.TemplateContent{Command: []string{"/bin/parent"}}
	child := &schema.TemplateContent{
		ProxyServices: map[string]schema.TemplateProxyService{
			"anthropic": {Upstream: "https://api.anthropic.com"},
		},
	}
	result := Merge(parent, child)
	if len(result.ProxyServices) != 1 {
		t.Errorf("ProxyServices count = %d, want 1", len(result.ProxyServices))
	}

	// non-nil parent + nil child = parent.
	parent2 := &schema.TemplateContent{
		Command: []string{"/bin/parent"},
		ProxyServices: map[string]schema.TemplateProxyService{
			"openai": {Upstream: "https://api.openai.com"},
		},
	}
	child2 := &schema.TemplateContent{}
	result2 := Merge(parent2, child2)
	if len(result2.ProxyServices) != 1 {
		t.Errorf("ProxyServices count = %d, want 1", len(result2.ProxyServices))
	}

	// nil parent + nil child = nil.
	parent3 := &schema.TemplateContent{Command: []string{"/bin/parent"}}
	child3 := &schema.TemplateContent{}
	result3 := Merge(parent3, child3)
	if result3.ProxyServices != nil {
		t.Errorf("ProxyServices should be nil when both inputs are nil, got %v", result3.ProxyServices)
	}
}

func TestMergeClearsInherits(t *testing.T) {
	t.Parallel()

	parent := &schema.TemplateContent{
		Inherits: []string{"some/room:grandparent"},
		Command:  []string{"/bin/parent"},
	}

	child := &schema.TemplateContent{
		Inherits: []string{"some/room:parent"},
	}

	result := Merge(parent, child)

	if len(result.Inherits) != 0 {
		t.Errorf("Inherits should be cleared after merge, got %v", result.Inherits)
	}
}

func TestResolveMultipleParents(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")

	// Parent A: provides command and namespaces.
	state.setTemplate("!template:test", "parent-a", schema.TemplateContent{
		Description: "Parent A",
		Command:     []string{"/bin/parent-a"},
		Namespaces:  &schema.TemplateNamespaces{PID: true, Net: true},
		EnvironmentVariables: map[string]string{
			"FROM_A": "yes",
			"SHARED": "from-a",
		},
	})

	// Parent B: provides a different command and security settings.
	// Later parent (B) should override earlier parent (A) on conflicts.
	state.setTemplate("!template:test", "parent-b", schema.TemplateContent{
		Description: "Parent B",
		Command:     []string{"/bin/parent-b"},
		Security:    &schema.TemplateSecurity{NoNewPrivs: true},
		EnvironmentVariables: map[string]string{
			"FROM_B": "yes",
			"SHARED": "from-b",
		},
	})

	// Child inherits from both parents, ordered [A, B].
	state.setTemplate("!template:test", "child", schema.TemplateContent{
		Description: "Multi-parent child",
		Inherits:    []string{"bureau/template:parent-a", "bureau/template:parent-b"},
		EnvironmentVariables: map[string]string{
			"FROM_CHILD": "yes",
		},
	})

	session := newTestSession(t, state)
	ctx := context.Background()

	template, err := Resolve(ctx, session, "bureau/template:child", testServerName)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Child description overrides everything.
	if template.Description != "Multi-parent child" {
		t.Errorf("Description = %q, want %q", template.Description, "Multi-parent child")
	}

	// Child has no command, so the last parent's command wins (parent-b).
	if len(template.Command) != 1 || template.Command[0] != "/bin/parent-b" {
		t.Errorf("Command = %v, want [/bin/parent-b] (later parent wins)", template.Command)
	}

	// Namespaces from parent-a should be present (parent-b had nil).
	if template.Namespaces == nil || !template.Namespaces.PID {
		t.Error("Namespaces.PID should be inherited from parent-a")
	}

	// Security from parent-b should be present (parent-a had nil).
	if template.Security == nil || !template.Security.NoNewPrivs {
		t.Error("Security.NoNewPrivs should be inherited from parent-b")
	}

	// Environment: parent-a provides FROM_A, parent-b provides FROM_B,
	// both provide SHARED (parent-b wins), child provides FROM_CHILD.
	if template.EnvironmentVariables["FROM_A"] != "yes" {
		t.Error("FROM_A should be present from parent-a")
	}
	if template.EnvironmentVariables["FROM_B"] != "yes" {
		t.Error("FROM_B should be present from parent-b")
	}
	if template.EnvironmentVariables["FROM_CHILD"] != "yes" {
		t.Error("FROM_CHILD should be present from child")
	}
	if template.EnvironmentVariables["SHARED"] != "from-b" {
		t.Errorf("SHARED = %q, want %q (later parent wins)", template.EnvironmentVariables["SHARED"], "from-b")
	}

	// Inherits should be cleared.
	if len(template.Inherits) != 0 {
		t.Errorf("Inherits should be cleared after resolution, got %v", template.Inherits)
	}
}

func TestResolveMultipleParentsSliceMerge(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")

	// Parent A provides filesystem mounts and required services.
	state.setTemplate("!template:test", "runtime", schema.TemplateContent{
		Description: "Runtime parent",
		Filesystem: []schema.TemplateMount{
			{Source: "/usr", Dest: "/usr", Mode: schema.MountModeRO},
			{Source: "/lib", Dest: "/lib", Mode: schema.MountModeRO},
		},
		RequiredServices:    []string{"proxy"},
		RequiredCredentials: []string{"API_KEY"},
		CreateDirs:          []string{"/run/bureau"},
	})

	// Parent B provides different filesystem mounts and required services.
	state.setTemplate("!template:test", "networking", schema.TemplateContent{
		Description: "Networking parent",
		Filesystem: []schema.TemplateMount{
			{Source: "/etc/resolv.conf", Dest: "/etc/resolv.conf", Mode: schema.MountModeRO},
		},
		RequiredServices:    []string{"bridge"},
		RequiredCredentials: []string{"TLS_CERT"},
		CreateDirs:          []string{"/var/run"},
	})

	// Child inherits from both, adds its own mount.
	state.setTemplate("!template:test", "agent", schema.TemplateContent{
		Description: "Agent template",
		Inherits:    []string{"bureau/template:runtime", "bureau/template:networking"},
		Command:     []string{"/usr/bin/agent"},
		Filesystem: []schema.TemplateMount{
			{Type: "tmpfs", Dest: "/tmp"},
		},
	})

	session := newTestSession(t, state)
	ctx := context.Background()

	template, err := Resolve(ctx, session, "bureau/template:agent", testServerName)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Filesystem: 2 from runtime + 1 from networking + 1 from child = 4.
	if len(template.Filesystem) != 4 {
		t.Errorf("Filesystem count = %d, want 4, mounts: %v", len(template.Filesystem), template.Filesystem)
	}

	// RequiredServices from both parents: proxy, bridge.
	if len(template.RequiredServices) != 2 {
		t.Errorf("RequiredServices = %v, want [proxy bridge]", template.RequiredServices)
	}

	// RequiredCredentials from both parents: API_KEY, TLS_CERT.
	if len(template.RequiredCredentials) != 2 {
		t.Errorf("RequiredCredentials = %v, want [API_KEY TLS_CERT]", template.RequiredCredentials)
	}

	// CreateDirs from both parents: /run/bureau, /var/run.
	if len(template.CreateDirs) != 2 {
		t.Errorf("CreateDirs = %v, want [/run/bureau /var/run]", template.CreateDirs)
	}
}

func TestResolveDiamondInheritance(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")

	// Diamond shape:
	//     base
	//    /    \
	//   A      B
	//    \    /
	//     child

	state.setTemplate("!template:test", "base", schema.TemplateContent{
		Description: "Shared base",
		Command:     []string{"/bin/base"},
		Namespaces:  &schema.TemplateNamespaces{PID: true},
		EnvironmentVariables: map[string]string{
			"BASE": "yes",
		},
		Filesystem: []schema.TemplateMount{
			{Source: "/usr", Dest: "/usr", Mode: schema.MountModeRO},
		},
	})

	state.setTemplate("!template:test", "left", schema.TemplateContent{
		Description: "Left branch",
		Inherits:    []string{"bureau/template:base"},
		EnvironmentVariables: map[string]string{
			"LEFT": "yes",
		},
	})

	state.setTemplate("!template:test", "right", schema.TemplateContent{
		Description: "Right branch",
		Inherits:    []string{"bureau/template:base"},
		Command:     []string{"/bin/right"},
		EnvironmentVariables: map[string]string{
			"RIGHT": "yes",
		},
	})

	state.setTemplate("!template:test", "diamond-child", schema.TemplateContent{
		Description: "Diamond child",
		Inherits:    []string{"bureau/template:left", "bureau/template:right"},
	})

	session := newTestSession(t, state)
	ctx := context.Background()

	// Diamond inheritance should not produce a cycle error.
	template, err := Resolve(ctx, session, "bureau/template:diamond-child", testServerName)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if template.Description != "Diamond child" {
		t.Errorf("Description = %q, want %q", template.Description, "Diamond child")
	}

	// Right branch resolves after left, so right's command wins.
	if len(template.Command) != 1 || template.Command[0] != "/bin/right" {
		t.Errorf("Command = %v, want [/bin/right] (right branch wins)", template.Command)
	}

	// Base environment should be present through both branches.
	if template.EnvironmentVariables["BASE"] != "yes" {
		t.Error("BASE should be inherited through branches")
	}
	if template.EnvironmentVariables["LEFT"] != "yes" {
		t.Error("LEFT should be inherited from left branch")
	}
	if template.EnvironmentVariables["RIGHT"] != "yes" {
		t.Error("RIGHT should be inherited from right branch")
	}

	// Base namespaces should be present.
	if template.Namespaces == nil || !template.Namespaces.PID {
		t.Error("Namespaces.PID should be inherited from base through branches")
	}
}

func TestResolveMultipleParentsCycle(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")

	// Create an actual cycle via multi-parent inheritance:
	// A inherits [B, C], B inherits [A] — direct cycle through multi-parent.
	state.setTemplate("!template:test", "cycle-a", schema.TemplateContent{
		Inherits: []string{"bureau/template:cycle-b", "bureau/template:cycle-c"},
		Command:  []string{"/bin/a"},
	})
	state.setTemplate("!template:test", "cycle-b", schema.TemplateContent{
		Inherits: []string{"bureau/template:cycle-a"},
		Command:  []string{"/bin/b"},
	})
	state.setTemplate("!template:test", "cycle-c", schema.TemplateContent{
		Command: []string{"/bin/c"},
	})

	session := newTestSession(t, state)
	ctx := context.Background()

	_, err := Resolve(ctx, session, "bureau/template:cycle-a", testServerName)
	if err == nil {
		t.Fatal("expected cycle detection error for multi-parent cycle")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention cycle, got: %v", err)
	}
}

func TestResolveSelfInheritance(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")

	// A template that lists itself as a parent.
	state.setTemplate("!template:test", "self-ref", schema.TemplateContent{
		Inherits: []string{"bureau/template:self-ref"},
		Command:  []string{"/bin/self"},
	})

	session := newTestSession(t, state)
	ctx := context.Background()

	_, err := Resolve(ctx, session, "bureau/template:self-ref", testServerName)
	if err == nil {
		t.Fatal("expected cycle detection error for self-referencing template")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention cycle, got: %v", err)
	}
}

func TestMergeCredentialRef(t *testing.T) {
	t.Parallel()

	parent := &schema.TemplateContent{
		CredentialRef: "${FLEET_ROOM}:parent-creds",
	}
	child := &schema.TemplateContent{}

	// Child doesn't set CredentialRef — parent's value should survive.
	result := Merge(parent, child)
	if result.CredentialRef != "${FLEET_ROOM}:parent-creds" {
		t.Errorf("CredentialRef = %q, want parent value", result.CredentialRef)
	}

	// Child overrides CredentialRef.
	child.CredentialRef = "${CONFIG_ROOM}:child-creds"
	result = Merge(parent, child)
	if result.CredentialRef != "${CONFIG_ROOM}:child-creds" {
		t.Errorf("CredentialRef = %q, want child override", result.CredentialRef)
	}
}

func TestMergeAllowedPipelinesIntersection(t *testing.T) {
	t.Parallel()

	parentPipelines := []string{"env-compose", "deploy-prod", "deploy-staging"}
	childPipelines := []string{"env-compose", "deploy-prod"}

	parent := &schema.TemplateContent{
		AllowedPipelines: &parentPipelines,
	}
	child := &schema.TemplateContent{
		AllowedPipelines: &childPipelines,
	}

	// Both set: result is intersection.
	result := Merge(parent, child)
	if result.AllowedPipelines == nil {
		t.Fatal("AllowedPipelines is nil, want intersection")
	}
	got := *result.AllowedPipelines
	if len(got) != 2 {
		t.Fatalf("AllowedPipelines length = %d, want 2", len(got))
	}
	// Intersection should contain env-compose and deploy-prod.
	found := map[string]bool{}
	for _, pipeline := range got {
		found[pipeline] = true
	}
	if !found["env-compose"] || !found["deploy-prod"] {
		t.Errorf("AllowedPipelines = %v, want intersection [env-compose, deploy-prod]", got)
	}
}

func TestMergeAllowedPipelinesNilSemantics(t *testing.T) {
	t.Parallel()

	pipelines := []string{"env-compose"}

	// nil parent, non-nil child: child sets restriction.
	result := Merge(
		&schema.TemplateContent{},
		&schema.TemplateContent{AllowedPipelines: &pipelines},
	)
	if result.AllowedPipelines == nil {
		t.Fatal("AllowedPipelines should be non-nil when child sets it")
	}
	if len(*result.AllowedPipelines) != 1 {
		t.Errorf("AllowedPipelines = %v, want [env-compose]", *result.AllowedPipelines)
	}

	// Non-nil parent, nil child: parent restriction preserved.
	result = Merge(
		&schema.TemplateContent{AllowedPipelines: &pipelines},
		&schema.TemplateContent{},
	)
	if result.AllowedPipelines == nil {
		t.Fatal("AllowedPipelines should be preserved from parent")
	}
	if len(*result.AllowedPipelines) != 1 {
		t.Errorf("AllowedPipelines = %v, want [env-compose]", *result.AllowedPipelines)
	}

	// Both nil: result is nil (permissive).
	result = Merge(
		&schema.TemplateContent{},
		&schema.TemplateContent{},
	)
	if result.AllowedPipelines != nil {
		t.Errorf("AllowedPipelines should be nil when both are nil, got %v", *result.AllowedPipelines)
	}

	// Parent has restriction, child narrows to empty (deny-all).
	empty := []string{}
	result = Merge(
		&schema.TemplateContent{AllowedPipelines: &pipelines},
		&schema.TemplateContent{AllowedPipelines: &empty},
	)
	if result.AllowedPipelines == nil {
		t.Fatal("AllowedPipelines should be non-nil (deny-all)")
	}
	if len(*result.AllowedPipelines) != 0 {
		t.Errorf("AllowedPipelines = %v, want empty (deny-all)", *result.AllowedPipelines)
	}
}

// --- ResolveWithAuthor tests ---

func TestFetchWithSender(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")
	state.setTemplateWithSender("!template:test", "base", schema.TemplateContent{
		Description:   "Base template",
		Command:       []string{"/bin/bash"},
		CredentialRef: "bureau/creds:nix-builder",
	}, "@operator:test.local")

	session := newTestSession(t, state)
	ctx := context.Background()

	templateRef, err := schema.ParseTemplateRef("bureau/template:base")
	if err != nil {
		t.Fatalf("ParseTemplateRef: %v", err)
	}

	result, err := FetchWithSender(ctx, session, templateRef, testServerName)
	if err != nil {
		t.Fatalf("FetchWithSender: %v", err)
	}

	if result.Content.Description != "Base template" {
		t.Errorf("Description = %q, want %q", result.Content.Description, "Base template")
	}
	if result.Sender.String() != "@operator:test.local" {
		t.Errorf("Sender = %q, want %q", result.Sender, "@operator:test.local")
	}
}

func TestResolveWithAuthor_NoCredentialRef(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")
	state.setTemplateWithSender("!template:test", "base", schema.TemplateContent{
		Description: "Base template",
		Command:     []string{"/bin/bash"},
	}, "@operator:test.local")

	session := newTestSession(t, state)
	ctx := context.Background()

	result, err := ResolveWithAuthor(ctx, session, "bureau/template:base", testServerName)
	if err != nil {
		t.Fatalf("ResolveWithAuthor: %v", err)
	}

	if result.Template.Description != "Base template" {
		t.Errorf("Description = %q, want %q", result.Template.Description, "Base template")
	}
	// No CredentialRef → CredentialRefAuthor should be zero.
	if !result.CredentialRefAuthor.IsZero() {
		t.Errorf("CredentialRefAuthor should be zero, got %s", result.CredentialRefAuthor)
	}
}

func TestResolveWithAuthor_ChildCredentialRef(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")

	// Parent has no CredentialRef.
	state.setTemplateWithSender("!template:test", "parent", schema.TemplateContent{
		Description: "Parent",
		Command:     []string{"/bin/bash"},
	}, "@parent-author:test.local")

	// Child sets CredentialRef.
	state.setTemplateWithSender("!template:test", "child", schema.TemplateContent{
		Inherits:      []string{"bureau/template:parent"},
		CredentialRef: "bureau/creds:nix-builder",
	}, "@child-author:test.local")

	session := newTestSession(t, state)
	ctx := context.Background()

	result, err := ResolveWithAuthor(ctx, session, "bureau/template:child", testServerName)
	if err != nil {
		t.Fatalf("ResolveWithAuthor: %v", err)
	}

	if result.Template.CredentialRef != "bureau/creds:nix-builder" {
		t.Errorf("CredentialRef = %q, want %q", result.Template.CredentialRef, "bureau/creds:nix-builder")
	}
	// Child set the CredentialRef, so child's author is the credential ref author.
	if result.CredentialRefAuthor.String() != "@child-author:test.local" {
		t.Errorf("CredentialRefAuthor = %q, want @child-author:test.local", result.CredentialRefAuthor)
	}
}

func TestResolveWithAuthor_ParentCredentialRefInherited(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")

	// Parent sets CredentialRef.
	state.setTemplateWithSender("!template:test", "parent", schema.TemplateContent{
		Description:   "Parent",
		CredentialRef: "bureau/creds:parent-creds",
	}, "@parent-author:test.local")

	// Child does NOT set CredentialRef → inherits from parent.
	state.setTemplateWithSender("!template:test", "child", schema.TemplateContent{
		Inherits: []string{"bureau/template:parent"},
		Command:  []string{"/bin/bash"},
	}, "@child-author:test.local")

	session := newTestSession(t, state)
	ctx := context.Background()

	result, err := ResolveWithAuthor(ctx, session, "bureau/template:child", testServerName)
	if err != nil {
		t.Fatalf("ResolveWithAuthor: %v", err)
	}

	if result.Template.CredentialRef != "bureau/creds:parent-creds" {
		t.Errorf("CredentialRef = %q, want %q", result.Template.CredentialRef, "bureau/creds:parent-creds")
	}
	// Parent set the CredentialRef, so parent's author is the credential ref author.
	if result.CredentialRefAuthor.String() != "@parent-author:test.local" {
		t.Errorf("CredentialRefAuthor = %q, want @parent-author:test.local", result.CredentialRefAuthor)
	}
}

func TestResolveWithAuthor_ChildOverridesParentCredentialRef(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias("#bureau/template:test.local", "!template:test")

	// Parent sets CredentialRef.
	state.setTemplateWithSender("!template:test", "parent", schema.TemplateContent{
		Description:   "Parent",
		CredentialRef: "bureau/creds:parent-creds",
	}, "@parent-author:test.local")

	// Child overrides with a different CredentialRef.
	state.setTemplateWithSender("!template:test", "child", schema.TemplateContent{
		Inherits:      []string{"bureau/template:parent"},
		CredentialRef: "bureau/creds:child-creds",
	}, "@child-author:test.local")

	session := newTestSession(t, state)
	ctx := context.Background()

	result, err := ResolveWithAuthor(ctx, session, "bureau/template:child", testServerName)
	if err != nil {
		t.Fatalf("ResolveWithAuthor: %v", err)
	}

	if result.Template.CredentialRef != "bureau/creds:child-creds" {
		t.Errorf("CredentialRef = %q, want %q", result.Template.CredentialRef, "bureau/creds:child-creds")
	}
	// Child overrides CredentialRef → child's author wins.
	if result.CredentialRefAuthor.String() != "@child-author:test.local" {
		t.Errorf("CredentialRefAuthor = %q, want @child-author:test.local", result.CredentialRefAuthor)
	}
}
