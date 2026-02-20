// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

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
	// roomAliases maps full room alias (e.g., schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"))
	// to room ID (e.g., "!template:test").
	roomAliases map[string]string

	// stateEvents maps "roomID\x00eventType\x00stateKey" to raw JSON content.
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

// handler returns an http.Handler that serves the mock Matrix endpoints.
func (state *templateTestState) handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Use RawPath for correct percent-encoded slash handling.
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
	// Path: /_matrix/client/v3/directory/room/{alias}
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
	// Path: /_matrix/client/v3/rooms/{roomId}/state/{eventType}/{stateKey}
	// All segments are percent-encoded.
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

	// Split eventType/stateKey — the state key may contain slashes.
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

// newTemplateTestSession creates a messaging session pointing at the mock server.
func newTemplateTestSession(t *testing.T, state *templateTestState) *messaging.DirectSession {
	t.Helper()
	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken("@daemon:test.local", "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

const testServerName = "test.local"

func TestResolveTemplateSimple(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), "!template:test")
	state.setTemplate("!template:test", "base", schema.TemplateContent{
		Description: "Base sandbox template",
		Command:     []string{"/bin/bash"},
		Filesystem: []schema.TemplateMount{
			{Source: "/usr", Dest: "/usr", Mode: "ro"},
			{Source: "/bin", Dest: "/bin", Mode: "ro"},
		},
		Namespaces: &schema.TemplateNamespaces{PID: true, Net: true},
		Security:   &schema.TemplateSecurity{NoNewPrivs: true, DieWithParent: true},
		EnvironmentVariables: map[string]string{
			"PATH": "/usr/bin:/bin",
			"HOME": "/workspace",
		},
		CreateDirs: []string{"/tmp", "/run/bureau"},
	})

	session := newTemplateTestSession(t, state)
	ctx := context.Background()

	template, err := resolveTemplate(ctx, session, "bureau/template:base", testServerName)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	if template.Description != "Base sandbox template" {
		t.Errorf("Description = %q, want %q", template.Description, "Base sandbox template")
	}
	if len(template.Command) != 1 || template.Command[0] != "/bin/bash" {
		t.Errorf("Command = %v, want [/bin/bash]", template.Command)
	}
	if len(template.Filesystem) != 2 {
		t.Fatalf("Filesystem count = %d, want 2", len(template.Filesystem))
	}
	if template.Namespaces == nil || !template.Namespaces.PID {
		t.Error("Namespaces.PID should be true")
	}
	if len(template.Inherits) != 0 {
		t.Errorf("Inherits should be empty after resolution, got %v", template.Inherits)
	}
}

func TestResolveTemplateSingleInheritance(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), "!template:test")

	// Base template: provides filesystem, namespaces, security.
	state.setTemplate("!template:test", "base", schema.TemplateContent{
		Description: "Base template",
		Filesystem: []schema.TemplateMount{
			{Source: "/usr", Dest: "/usr", Mode: "ro"},
			{Source: "/bin", Dest: "/bin", Mode: "ro"},
			{Type: "tmpfs", Dest: "/tmp", Options: "size=64M"},
		},
		Namespaces: &schema.TemplateNamespaces{PID: true, Net: true, IPC: true},
		Security:   &schema.TemplateSecurity{NoNewPrivs: true, DieWithParent: true},
		EnvironmentVariables: map[string]string{
			"PATH":           "/usr/bin:/bin",
			"HOME":           "/workspace",
			"BUREAU_SANDBOX": "1",
		},
		CreateDirs: []string{"/tmp", "/run/bureau"},
	})

	// Child template: inherits from base, adds command, env, workspace mount.
	state.setTemplate("!template:test", "llm-agent", schema.TemplateContent{
		Description: "LLM agent template",
		Inherits:    []string{"bureau/template:base"},
		Command:     []string{"/usr/local/bin/claude", "--agent"},
		Environment: "/nix/store/abc123-agent-env",
		Filesystem: []schema.TemplateMount{
			{Source: "${WORKSPACE_ROOT}/${PROJECT}", Dest: "/workspace", Mode: "rw"},
		},
		EnvironmentVariables: map[string]string{
			"PATH": "/usr/local/bin:/usr/bin:/bin", // Override parent PATH
		},
		Roles: map[string][]string{
			"agent": {"/usr/local/bin/claude", "--agent"},
			"shell": {"/bin/bash"},
		},
		RequiredCredentials: []string{"ANTHROPIC_API_KEY"},
	})

	session := newTemplateTestSession(t, state)
	ctx := context.Background()

	template, err := resolveTemplate(ctx, session, "bureau/template:llm-agent", testServerName)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	// Child description should override parent.
	if template.Description != "LLM agent template" {
		t.Errorf("Description = %q, want %q", template.Description, "LLM agent template")
	}

	// Child command should replace parent (parent had none).
	if len(template.Command) != 2 || template.Command[0] != "/usr/local/bin/claude" {
		t.Errorf("Command = %v, want [/usr/local/bin/claude --agent]", template.Command)
	}

	// Child environment should be set.
	if template.Environment != "/nix/store/abc123-agent-env" {
		t.Errorf("Environment = %q, want /nix/store/abc123-agent-env", template.Environment)
	}

	// Filesystem: parent mounts (/usr, /bin, /tmp) + child mount (/workspace).
	if len(template.Filesystem) != 4 {
		t.Fatalf("Filesystem count = %d, want 4", len(template.Filesystem))
	}

	// Environment variables should be merged, child PATH wins.
	if template.EnvironmentVariables["PATH"] != "/usr/local/bin:/usr/bin:/bin" {
		t.Errorf("PATH = %q, want /usr/local/bin:/usr/bin:/bin", template.EnvironmentVariables["PATH"])
	}
	// Parent HOME preserved.
	if template.EnvironmentVariables["HOME"] != "/workspace" {
		t.Errorf("HOME = %q, want /workspace", template.EnvironmentVariables["HOME"])
	}
	// Parent BUREAU_SANDBOX preserved.
	if template.EnvironmentVariables["BUREAU_SANDBOX"] != "1" {
		t.Errorf("BUREAU_SANDBOX = %q, want 1", template.EnvironmentVariables["BUREAU_SANDBOX"])
	}

	// Namespaces should inherit from parent (child had none).
	if template.Namespaces == nil || !template.Namespaces.IPC {
		t.Error("Namespaces.IPC should be inherited from parent")
	}

	// Roles from child should be present.
	if len(template.Roles) != 2 {
		t.Fatalf("Roles count = %d, want 2", len(template.Roles))
	}

	// CreateDirs should be parent's (child had none).
	if len(template.CreateDirs) != 2 {
		t.Fatalf("CreateDirs count = %d, want 2", len(template.CreateDirs))
	}

	// RequiredCredentials from child.
	if len(template.RequiredCredentials) != 1 || template.RequiredCredentials[0] != "ANTHROPIC_API_KEY" {
		t.Errorf("RequiredCredentials = %v, want [ANTHROPIC_API_KEY]", template.RequiredCredentials)
	}
}

func TestResolveTemplateMultiLevelInheritance(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), "!template:test")
	state.setRoomAlias("#iree/template:test.local", "!iree-template:test")

	// Level 0 (root): base
	state.setTemplate("!template:test", "base", schema.TemplateContent{
		Filesystem: []schema.TemplateMount{
			{Source: "/usr", Dest: "/usr", Mode: "ro"},
			{Source: "/bin", Dest: "/bin", Mode: "ro"},
		},
		Namespaces: &schema.TemplateNamespaces{PID: true, Net: true},
		Security:   &schema.TemplateSecurity{NoNewPrivs: true},
		EnvironmentVariables: map[string]string{
			"BUREAU_SANDBOX": "1",
		},
		CreateDirs: []string{"/tmp"},
	})

	// Level 1: llm-agent inherits base
	state.setTemplate("!template:test", "llm-agent", schema.TemplateContent{
		Inherits:    []string{"bureau/template:base"},
		Command:     []string{"/usr/local/bin/claude", "--agent"},
		Environment: "/nix/store/abc-agent-env",
		EnvironmentVariables: map[string]string{
			"PATH": "/usr/local/bin:/usr/bin:/bin",
		},
		CreateDirs: []string{"/run/bureau"},
	})

	// Level 2: amdgpu-developer inherits llm-agent (cross-room!)
	state.setTemplate("!iree-template:test", "amdgpu-developer", schema.TemplateContent{
		Description: "AMDGPU developer agent",
		Inherits:    []string{"bureau/template:llm-agent"},
		Filesystem: []schema.TemplateMount{
			{Source: "/dev/kfd", Dest: "/dev/kfd", Mode: "rw"},
		},
		EnvironmentVariables: map[string]string{
			"HSA_OVERRIDE_GFX_VERSION": "11.0.0",
		},
		RequiredCredentials: []string{"ANTHROPIC_API_KEY"},
		DefaultPayload: map[string]any{
			"project": "iree/amdgpu",
		},
	})

	session := newTemplateTestSession(t, state)
	ctx := context.Background()

	template, err := resolveTemplate(ctx, session, "iree/template:amdgpu-developer", testServerName)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	// Level 2 description.
	if template.Description != "AMDGPU developer agent" {
		t.Errorf("Description = %q, want %q", template.Description, "AMDGPU developer agent")
	}

	// Level 1 command (level 2 didn't override).
	if len(template.Command) != 2 || template.Command[0] != "/usr/local/bin/claude" {
		t.Errorf("Command = %v, want [/usr/local/bin/claude --agent]", template.Command)
	}

	// Level 1 environment path.
	if template.Environment != "/nix/store/abc-agent-env" {
		t.Errorf("Environment = %q, want /nix/store/abc-agent-env", template.Environment)
	}

	// Filesystem: level 0 (/usr, /bin) + level 2 (/dev/kfd) = 3 mounts.
	if len(template.Filesystem) != 3 {
		t.Fatalf("Filesystem count = %d, want 3", len(template.Filesystem))
	}

	// Environment variables: all three levels merged.
	if template.EnvironmentVariables["BUREAU_SANDBOX"] != "1" {
		t.Error("BUREAU_SANDBOX should be inherited from level 0")
	}
	if template.EnvironmentVariables["PATH"] != "/usr/local/bin:/usr/bin:/bin" {
		t.Error("PATH should be from level 1")
	}
	if template.EnvironmentVariables["HSA_OVERRIDE_GFX_VERSION"] != "11.0.0" {
		t.Error("HSA_OVERRIDE_GFX_VERSION should be from level 2")
	}

	// Level 0 namespaces and security inherited through.
	if template.Namespaces == nil || !template.Namespaces.PID {
		t.Error("Namespaces should be inherited from level 0")
	}
	if template.Security == nil || !template.Security.NoNewPrivs {
		t.Error("Security should be inherited from level 0")
	}

	// CreateDirs merged from level 0 + level 1 (deduplicated).
	if len(template.CreateDirs) != 2 {
		t.Fatalf("CreateDirs count = %d, want 2 (/tmp + /run/bureau)", len(template.CreateDirs))
	}
}

func TestResolveTemplateCycleDetection(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), "!template:test")

	// A inherits B, B inherits A.
	state.setTemplate("!template:test", "a", schema.TemplateContent{
		Inherits: []string{"bureau/template:b"},
		Command:  []string{"/bin/a"},
	})
	state.setTemplate("!template:test", "b", schema.TemplateContent{
		Inherits: []string{"bureau/template:a"},
		Command:  []string{"/bin/b"},
	})

	session := newTemplateTestSession(t, state)
	ctx := context.Background()

	_, err := resolveTemplate(ctx, session, "bureau/template:a", testServerName)
	if err == nil {
		t.Fatal("expected cycle detection error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention cycle, got: %v", err)
	}
}

func TestResolveTemplateMissingParent(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), "!template:test")

	state.setTemplate("!template:test", "child", schema.TemplateContent{
		Inherits: []string{"bureau/template:nonexistent"},
		Command:  []string{"/bin/child"},
	})

	session := newTemplateTestSession(t, state)
	ctx := context.Background()

	_, err := resolveTemplate(ctx, session, "bureau/template:child", testServerName)
	if err == nil {
		t.Fatal("expected error for missing parent template")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should mention the missing template name, got: %v", err)
	}
}

func TestResolveTemplateNotFound(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	state.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), "!template:test")
	// Room exists but template doesn't.

	session := newTemplateTestSession(t, state)
	ctx := context.Background()

	_, err := resolveTemplate(ctx, session, "bureau/template:ghost", testServerName)
	if err == nil {
		t.Fatal("expected error for missing template")
	}
}

func TestResolveTemplateMissingRoom(t *testing.T) {
	t.Parallel()

	state := newTemplateTestState()
	// No room aliases set.

	session := newTemplateTestSession(t, state)
	ctx := context.Background()

	_, err := resolveTemplate(ctx, session, "nonexistent/room:template", testServerName)
	if err == nil {
		t.Fatal("expected error for missing room alias")
	}
}

func TestResolveInstanceConfigAllOverrides(t *testing.T) {
	t.Parallel()

	template := &schema.TemplateContent{
		Command:     []string{"/usr/local/bin/claude", "--agent"},
		Environment: "/nix/store/abc-agent-env",
		Filesystem: []schema.TemplateMount{
			{Source: "/usr", Dest: "/usr", Mode: "ro"},
		},
		Namespaces: &schema.TemplateNamespaces{PID: true},
		Security:   &schema.TemplateSecurity{NoNewPrivs: true},
		EnvironmentVariables: map[string]string{
			"PATH":           "/usr/local/bin:/usr/bin:/bin",
			"BUREAU_SANDBOX": "1",
		},
		Roles: map[string][]string{
			"agent": {"/usr/local/bin/claude", "--agent"},
		},
		CreateDirs: []string{"/tmp"},
		DefaultPayload: map[string]any{
			"model":      "claude-sonnet-4-5-20250929",
			"max_tokens": float64(4096),
		},
	}

	_, fleet := testMachineSetup(t, "test", "test.local")

	assignment := &schema.PrincipalAssignment{
		Principal:           testEntity(t, fleet, "iree/amdgpu/pm"),
		Template:            "bureau/template:llm-agent",
		CommandOverride:     []string{"/usr/local/bin/custom-agent", "--gpu"},
		EnvironmentOverride: "/nix/store/xyz-custom-env",
		ExtraEnvironmentVariables: map[string]string{
			"MODEL_NAME": "claude-opus-4-6",
			"PATH":       "/custom/bin:/usr/local/bin:/usr/bin:/bin", // Overrides template PATH
		},
		Payload: map[string]any{
			"project":    "iree/amdgpu",
			"max_tokens": float64(8192), // Overrides template default
		},
	}

	spec := resolveInstanceConfig(template, assignment)

	// CommandOverride should replace template command.
	if len(spec.Command) != 2 || spec.Command[0] != "/usr/local/bin/custom-agent" {
		t.Errorf("Command = %v, want [/usr/local/bin/custom-agent --gpu]", spec.Command)
	}

	// EnvironmentOverride should replace template environment path.
	if spec.EnvironmentPath != "/nix/store/xyz-custom-env" {
		t.Errorf("EnvironmentPath = %q, want /nix/store/xyz-custom-env", spec.EnvironmentPath)
	}

	// Extra env vars merged (PATH overridden, MODEL_NAME added, BUREAU_SANDBOX preserved).
	if spec.EnvironmentVariables["PATH"] != "/custom/bin:/usr/local/bin:/usr/bin:/bin" {
		t.Errorf("PATH = %q, want /custom/bin:/usr/local/bin:/usr/bin:/bin", spec.EnvironmentVariables["PATH"])
	}
	if spec.EnvironmentVariables["MODEL_NAME"] != "claude-opus-4-6" {
		t.Errorf("MODEL_NAME = %q, want claude-opus-4-6", spec.EnvironmentVariables["MODEL_NAME"])
	}
	if spec.EnvironmentVariables["BUREAU_SANDBOX"] != "1" {
		t.Errorf("BUREAU_SANDBOX should be preserved from template")
	}

	// Payload: template defaults + instance overrides (instance wins).
	if spec.Payload["project"] != "iree/amdgpu" {
		t.Errorf("Payload[project] = %v, want iree/amdgpu", spec.Payload["project"])
	}
	if spec.Payload["max_tokens"] != float64(8192) {
		t.Errorf("Payload[max_tokens] = %v, want 8192", spec.Payload["max_tokens"])
	}
	if spec.Payload["model"] != "claude-sonnet-4-5-20250929" {
		t.Errorf("Payload[model] = %v, want claude-sonnet-4-5-20250929 (from template default)", spec.Payload["model"])
	}

	// Other fields passed through.
	if len(spec.Filesystem) != 1 {
		t.Fatalf("Filesystem count = %d, want 1", len(spec.Filesystem))
	}
	if spec.Namespaces == nil || !spec.Namespaces.PID {
		t.Error("Namespaces.PID should be true")
	}
	if len(spec.Roles) != 1 {
		t.Fatalf("Roles count = %d, want 1", len(spec.Roles))
	}
	if len(spec.CreateDirs) != 1 {
		t.Fatalf("CreateDirs count = %d, want 1", len(spec.CreateDirs))
	}
}

func TestResolveInstanceConfigNoOverrides(t *testing.T) {
	t.Parallel()

	template := &schema.TemplateContent{
		Command:     []string{"/bin/bash"},
		Environment: "/nix/store/abc-env",
		EnvironmentVariables: map[string]string{
			"PATH": "/usr/bin:/bin",
		},
		DefaultPayload: map[string]any{
			"model": "default-model",
		},
	}

	_, fleet := testMachineSetup(t, "test", "test.local")

	assignment := &schema.PrincipalAssignment{
		Principal: testEntity(t, fleet, "test/agent"),
		Template:  "bureau/template:bash",
	}

	spec := resolveInstanceConfig(template, assignment)

	// Template values should pass through unchanged.
	if len(spec.Command) != 1 || spec.Command[0] != "/bin/bash" {
		t.Errorf("Command = %v, want [/bin/bash]", spec.Command)
	}
	if spec.EnvironmentPath != "/nix/store/abc-env" {
		t.Errorf("EnvironmentPath = %q, want /nix/store/abc-env", spec.EnvironmentPath)
	}
	if spec.EnvironmentVariables["PATH"] != "/usr/bin:/bin" {
		t.Errorf("PATH = %q, want /usr/bin:/bin", spec.EnvironmentVariables["PATH"])
	}
	// Payload should carry template defaults when no instance payload.
	if spec.Payload["model"] != "default-model" {
		t.Errorf("Payload[model] = %v, want default-model", spec.Payload["model"])
	}
}

func TestResolveInstanceConfigCarriesProxyServices(t *testing.T) {
	t.Parallel()

	template := &schema.TemplateContent{
		Command: []string{"/usr/local/bin/claude"},
		ProxyServices: map[string]schema.TemplateProxyService{
			"anthropic": {
				Upstream:      "https://api.anthropic.com",
				InjectHeaders: map[string]string{"x-api-key": "ANTHROPIC_API_KEY"},
				StripHeaders:  []string{"x-api-key", "authorization"},
			},
			"openai": {
				Upstream:      "https://api.openai.com",
				InjectHeaders: map[string]string{"Authorization": "OPENAI_BEARER"},
			},
		},
	}

	_, fleet := testMachineSetup(t, "test", "test.local")

	assignment := &schema.PrincipalAssignment{
		Principal: testEntity(t, fleet, "test/claude-agent"),
		Template:  "bureau/template:claude",
	}

	spec := resolveInstanceConfig(template, assignment)

	if len(spec.ProxyServices) != 2 {
		t.Fatalf("ProxyServices count = %d, want 2", len(spec.ProxyServices))
	}
	anthropic, ok := spec.ProxyServices["anthropic"]
	if !ok {
		t.Fatal("ProxyServices missing \"anthropic\" key")
	}
	if anthropic.Upstream != "https://api.anthropic.com" {
		t.Errorf("anthropic.Upstream = %q, want %q", anthropic.Upstream, "https://api.anthropic.com")
	}
	if anthropic.InjectHeaders["x-api-key"] != "ANTHROPIC_API_KEY" {
		t.Errorf("anthropic.InjectHeaders[x-api-key] = %q, want %q",
			anthropic.InjectHeaders["x-api-key"], "ANTHROPIC_API_KEY")
	}
	if len(anthropic.StripHeaders) != 2 {
		t.Errorf("anthropic.StripHeaders length = %d, want 2", len(anthropic.StripHeaders))
	}
	openai, ok := spec.ProxyServices["openai"]
	if !ok {
		t.Fatal("ProxyServices missing \"openai\" key")
	}
	if openai.Upstream != "https://api.openai.com" {
		t.Errorf("openai.Upstream = %q, want %q", openai.Upstream, "https://api.openai.com")
	}
}

func TestResolveInstanceConfigDoesNotMutateTemplate(t *testing.T) {
	t.Parallel()

	template := &schema.TemplateContent{
		EnvironmentVariables: map[string]string{
			"PATH": "/usr/bin:/bin",
		},
	}

	assignment := &schema.PrincipalAssignment{
		ExtraEnvironmentVariables: map[string]string{
			"NEW_VAR": "new-value",
		},
	}

	_ = resolveInstanceConfig(template, assignment)

	// The template's EnvironmentVariables should not have been mutated.
	if _, exists := template.EnvironmentVariables["NEW_VAR"]; exists {
		t.Error("resolveInstanceConfig should not mutate the template's EnvironmentVariables")
	}
}
