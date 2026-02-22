// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// testEntity constructs a ref.Entity for test use. Panics on failure.
func testEntity(t *testing.T, userID string) ref.Entity {
	t.Helper()
	entity, err := ref.ParseEntityUserID(userID)
	if err != nil {
		t.Fatalf("testEntity(%q): %v", userID, err)
	}
	return entity
}

func TestTemplateContentRoundTrip(t *testing.T) {
	original := TemplateContent{
		Description: "GPU-accelerated LLM agent with IREE runtime",
		Inherits:    []string{"bureau/template:base"},
		Command:     []string{"/usr/local/bin/claude", "--agent", "--no-tty"},
		Environment: "/nix/store/abc123-bureau-agent-env",
		EnvironmentVariables: map[string]string{
			"PATH":           "/workspace/bin:/usr/local/bin:/usr/bin:/bin",
			"HOME":           "/workspace",
			"BUREAU_SANDBOX": "1",
		},
		Filesystem: []TemplateMount{
			{Source: "${WORKSPACE_ROOT}/${PROJECT}", Dest: "/workspace", Mode: "rw"},
			{Type: "tmpfs", Dest: "/tmp", Options: "size=64M"},
			{Source: "/nix", Dest: "/nix", Mode: "ro", Optional: true},
		},
		Namespaces: &TemplateNamespaces{
			PID: true,
			Net: true,
			IPC: true,
			UTS: true,
		},
		Resources: &TemplateResources{
			CPUShares:     1024,
			MemoryLimitMB: 8192,
			PidsLimit:     512,
		},
		Security: &TemplateSecurity{
			NewSession:    true,
			DieWithParent: true,
			NoNewPrivs:    true,
		},
		CreateDirs:          []string{"/tmp", "/var/tmp", "/run/bureau"},
		Roles:               map[string][]string{"agent": {"/usr/local/bin/claude", "--agent"}, "shell": {"/bin/bash"}},
		RequiredCredentials: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"},
		DefaultPayload: map[string]any{
			"model":      "claude-sonnet-4-5-20250929",
			"max_tokens": float64(4096),
		},
		ProxyServices: map[string]TemplateProxyService{
			"anthropic": {
				Upstream:      "https://api.anthropic.com",
				InjectHeaders: map[string]string{"x-api-key": "ANTHROPIC_API_KEY"},
				StripHeaders:  []string{"x-api-key", "authorization"},
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify JSON field names match the wire format.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "description", "GPU-accelerated LLM agent with IREE runtime")
	// Verify inherits is a JSON array.
	inheritsRaw, ok := raw["inherits"].([]any)
	if !ok {
		t.Fatal("inherits field missing or wrong type (expected array)")
	}
	if len(inheritsRaw) != 1 || inheritsRaw[0] != "bureau/template:base" {
		t.Errorf("inherits = %v, want [bureau/template:base]", inheritsRaw)
	}
	assertField(t, raw, "environment", "/nix/store/abc123-bureau-agent-env")

	// Verify command is an array.
	command, ok := raw["command"].([]any)
	if !ok {
		t.Fatal("command field missing or wrong type")
	}
	if len(command) != 3 || command[0] != "/usr/local/bin/claude" {
		t.Errorf("command = %v, want [/usr/local/bin/claude --agent --no-tty]", command)
	}

	// Verify filesystem is an array with correct structure.
	filesystem, ok := raw["filesystem"].([]any)
	if !ok {
		t.Fatal("filesystem field missing or wrong type")
	}
	if len(filesystem) != 3 {
		t.Fatalf("filesystem count = %d, want 3", len(filesystem))
	}
	firstMount := filesystem[0].(map[string]any)
	assertField(t, firstMount, "source", "${WORKSPACE_ROOT}/${PROJECT}")
	assertField(t, firstMount, "dest", "/workspace")
	assertField(t, firstMount, "mode", "rw")

	// Verify tmpfs mount has type but no source.
	tmpfsMount := filesystem[1].(map[string]any)
	assertField(t, tmpfsMount, "type", "tmpfs")
	assertField(t, tmpfsMount, "dest", "/tmp")
	if _, exists := tmpfsMount["source"]; exists {
		t.Error("tmpfs mount should not have source")
	}

	// Verify optional mount.
	nixMount := filesystem[2].(map[string]any)
	assertField(t, nixMount, "optional", true)

	// Verify namespaces.
	namespaces, ok := raw["namespaces"].(map[string]any)
	if !ok {
		t.Fatal("namespaces field missing or wrong type")
	}
	assertField(t, namespaces, "pid", true)
	assertField(t, namespaces, "net", true)

	// Verify resources.
	resources, ok := raw["resources"].(map[string]any)
	if !ok {
		t.Fatal("resources field missing or wrong type")
	}
	assertField(t, resources, "cpu_shares", float64(1024))
	assertField(t, resources, "memory_limit_mb", float64(8192))

	// Verify security.
	security, ok := raw["security"].(map[string]any)
	if !ok {
		t.Fatal("security field missing or wrong type")
	}
	assertField(t, security, "new_session", true)
	assertField(t, security, "no_new_privs", true)

	// Verify roles.
	roles, ok := raw["roles"].(map[string]any)
	if !ok {
		t.Fatal("roles field missing or wrong type")
	}
	agentRole, ok := roles["agent"].([]any)
	if !ok {
		t.Fatal("roles.agent missing or wrong type")
	}
	if len(agentRole) != 2 || agentRole[0] != "/usr/local/bin/claude" {
		t.Errorf("roles.agent = %v, want [/usr/local/bin/claude --agent]", agentRole)
	}

	// Verify required_credentials.
	requiredCredentials, ok := raw["required_credentials"].([]any)
	if !ok {
		t.Fatal("required_credentials field missing or wrong type")
	}
	if len(requiredCredentials) != 2 {
		t.Fatalf("required_credentials count = %d, want 2", len(requiredCredentials))
	}

	// Verify default_payload.
	defaultPayload, ok := raw["default_payload"].(map[string]any)
	if !ok {
		t.Fatal("default_payload field missing or wrong type")
	}
	assertField(t, defaultPayload, "model", "claude-sonnet-4-5-20250929")
	assertField(t, defaultPayload, "max_tokens", float64(4096))

	// Verify proxy_services.
	proxyServices, ok := raw["proxy_services"].(map[string]any)
	if !ok {
		t.Fatal("proxy_services field missing or wrong type")
	}
	anthropic, ok := proxyServices["anthropic"].(map[string]any)
	if !ok {
		t.Fatal("proxy_services.anthropic missing or wrong type")
	}
	assertField(t, anthropic, "upstream", "https://api.anthropic.com")
	injectHeaders, ok := anthropic["inject_headers"].(map[string]any)
	if !ok {
		t.Fatal("proxy_services.anthropic.inject_headers missing or wrong type")
	}
	assertField(t, injectHeaders, "x-api-key", "ANTHROPIC_API_KEY")

	// Round-trip back to struct.
	var decoded TemplateContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Description != original.Description {
		t.Errorf("Description: got %q, want %q", decoded.Description, original.Description)
	}
	if len(decoded.Inherits) != 1 || decoded.Inherits[0] != "bureau/template:base" {
		t.Errorf("Inherits: got %v, want [bureau/template:base]", decoded.Inherits)
	}
	if decoded.Environment != original.Environment {
		t.Errorf("Environment: got %q, want %q", decoded.Environment, original.Environment)
	}
	if len(decoded.Command) != 3 {
		t.Fatalf("Command count = %d, want 3", len(decoded.Command))
	}
	if decoded.Command[0] != "/usr/local/bin/claude" {
		t.Errorf("Command[0]: got %q, want %q", decoded.Command[0], "/usr/local/bin/claude")
	}
	if len(decoded.Filesystem) != 3 {
		t.Fatalf("Filesystem count = %d, want 3", len(decoded.Filesystem))
	}
	if decoded.Filesystem[0].Source != "${WORKSPACE_ROOT}/${PROJECT}" || decoded.Filesystem[0].Dest != "/workspace" {
		t.Errorf("Filesystem[0]: got source=%q dest=%q, want source=${WORKSPACE_ROOT}/${PROJECT} dest=/workspace",
			decoded.Filesystem[0].Source, decoded.Filesystem[0].Dest)
	}
	if decoded.Filesystem[2].Optional != true {
		t.Error("Filesystem[2].Optional should be true")
	}
	if decoded.Namespaces == nil || !decoded.Namespaces.PID || !decoded.Namespaces.Net {
		t.Error("Namespaces should have PID and Net set")
	}
	if decoded.Resources == nil || decoded.Resources.CPUShares != 1024 {
		t.Error("Resources.CPUShares should be 1024")
	}
	if decoded.Security == nil || !decoded.Security.NoNewPrivs {
		t.Error("Security.NoNewPrivs should be true")
	}
	if len(decoded.Roles) != 2 {
		t.Fatalf("Roles count = %d, want 2", len(decoded.Roles))
	}
	if len(decoded.Roles["agent"]) != 2 || decoded.Roles["agent"][0] != "/usr/local/bin/claude" {
		t.Errorf("Roles[agent] = %v, want [/usr/local/bin/claude --agent]", decoded.Roles["agent"])
	}
	if len(decoded.ProxyServices) != 1 {
		t.Fatalf("ProxyServices count = %d, want 1", len(decoded.ProxyServices))
	}
	decodedAnthropic, ok := decoded.ProxyServices["anthropic"]
	if !ok {
		t.Fatal("ProxyServices missing \"anthropic\" key")
	}
	if decodedAnthropic.Upstream != "https://api.anthropic.com" {
		t.Errorf("ProxyServices[anthropic].Upstream: got %q, want %q",
			decodedAnthropic.Upstream, "https://api.anthropic.com")
	}
	if decodedAnthropic.InjectHeaders["x-api-key"] != "ANTHROPIC_API_KEY" {
		t.Errorf("ProxyServices[anthropic].InjectHeaders[x-api-key]: got %q, want %q",
			decodedAnthropic.InjectHeaders["x-api-key"], "ANTHROPIC_API_KEY")
	}
	if len(decodedAnthropic.StripHeaders) != 2 || decodedAnthropic.StripHeaders[0] != "x-api-key" {
		t.Errorf("ProxyServices[anthropic].StripHeaders: got %v, want [x-api-key authorization]",
			decodedAnthropic.StripHeaders)
	}
}

func TestTemplateContentOmitsEmptyFields(t *testing.T) {
	// A minimal template with only required structure should omit all
	// optional fields from the JSON wire format.
	template := TemplateContent{
		Command: []string{"/bin/bash"},
	}

	data, err := json.Marshal(template)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	omittedFields := []string{
		"description", "inherits", "environment", "environment_variables",
		"filesystem", "namespaces", "resources", "security",
		"create_dirs", "roles", "required_credentials", "default_payload",
		"health_check", "proxy_services",
	}
	for _, field := range omittedFields {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty/nil", field)
		}
	}

	// Command should be present.
	if _, exists := raw["command"]; !exists {
		t.Error("command should be present")
	}
}

func TestHealthCheckOnTemplateContent(t *testing.T) {
	original := TemplateContent{
		Command:     []string{"/usr/local/bin/gmail-watcher"},
		Environment: "/nix/store/abc123-gmail-watcher-env",
		HealthCheck: &HealthCheck{
			Endpoint:           "/health",
			IntervalSeconds:    30,
			TimeoutSeconds:     5,
			FailureThreshold:   3,
			GracePeriodSeconds: 60,
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	healthCheck, ok := raw["health_check"].(map[string]any)
	if !ok {
		t.Fatal("health_check field missing or wrong type")
	}
	assertField(t, healthCheck, "endpoint", "/health")
	assertField(t, healthCheck, "interval_seconds", float64(30))
	assertField(t, healthCheck, "timeout_seconds", float64(5))
	assertField(t, healthCheck, "failure_threshold", float64(3))
	assertField(t, healthCheck, "grace_period_seconds", float64(60))

	var decoded TemplateContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.HealthCheck == nil {
		t.Fatal("HealthCheck should not be nil after round-trip")
	}
	if decoded.HealthCheck.Endpoint != "/health" {
		t.Errorf("Endpoint: got %q, want %q", decoded.HealthCheck.Endpoint, "/health")
	}
	if decoded.HealthCheck.IntervalSeconds != 30 {
		t.Errorf("IntervalSeconds: got %d, want 30", decoded.HealthCheck.IntervalSeconds)
	}
	if decoded.HealthCheck.TimeoutSeconds != 5 {
		t.Errorf("TimeoutSeconds: got %d, want 5", decoded.HealthCheck.TimeoutSeconds)
	}
	if decoded.HealthCheck.FailureThreshold != 3 {
		t.Errorf("FailureThreshold: got %d, want 3", decoded.HealthCheck.FailureThreshold)
	}
	if decoded.HealthCheck.GracePeriodSeconds != 60 {
		t.Errorf("GracePeriodSeconds: got %d, want 60", decoded.HealthCheck.GracePeriodSeconds)
	}
}

func TestHealthCheckOmitsZeroOptionalFields(t *testing.T) {
	// A HealthCheck with only required fields should omit the optional
	// fields (timeout, failure threshold, grace period) from the wire
	// format — they default at runtime.
	healthCheck := HealthCheck{
		Endpoint:        "/healthz",
		IntervalSeconds: 15,
	}

	data, err := json.Marshal(healthCheck)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "endpoint", "/healthz")
	assertField(t, raw, "interval_seconds", float64(15))

	for _, field := range []string{"timeout_seconds", "failure_threshold", "grace_period_seconds"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when zero (runtime default)", field)
		}
	}
}

func TestHealthCheckOmittedWhenNilOnTemplate(t *testing.T) {
	// Templates without health monitoring should not include health_check.
	template := TemplateContent{
		Command: []string{"/usr/local/bin/claude", "--agent"},
	}

	data, err := json.Marshal(template)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["health_check"]; exists {
		t.Error("health_check should be omitted when nil")
	}
}

func TestTemplateMountOmitsEmptyFields(t *testing.T) {
	// A bind mount with only dest and mode should omit type, options,
	// source (if empty), and optional.
	mount := TemplateMount{
		Dest: "/workspace",
		Mode: "rw",
	}

	data, err := json.Marshal(mount)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"source", "type", "options", "optional"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty/zero", field)
		}
	}
	assertField(t, raw, "dest", "/workspace")
	assertField(t, raw, "mode", "rw")
}

func TestPrincipalAssignmentOverrides(t *testing.T) {
	// A PrincipalAssignment with all override fields set should
	// round-trip correctly through JSON.
	original := PrincipalAssignment{
		Principal:           testEntity(t, "@bureau/fleet/test/agent/pm:bureau.local"),
		Template:            "iree/template:amdgpu-developer",
		AutoStart:           true,
		CommandOverride:     []string{"/usr/local/bin/custom-agent", "--mode=gpu"},
		EnvironmentOverride: "/nix/store/xyz789-custom-env",
		ExtraEnvironmentVariables: map[string]string{
			"MODEL_NAME": "claude-opus-4-6",
			"BATCH_SIZE": "32",
		},
		Payload: map[string]any{
			"project":    "iree/amdgpu",
			"max_tokens": float64(8192),
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Verify override wire format field names.
	commandOverride, ok := raw["command_override"].([]any)
	if !ok {
		t.Fatal("command_override field missing or wrong type")
	}
	if len(commandOverride) != 2 || commandOverride[0] != "/usr/local/bin/custom-agent" {
		t.Errorf("command_override = %v, want [/usr/local/bin/custom-agent --mode=gpu]", commandOverride)
	}

	assertField(t, raw, "environment_override", "/nix/store/xyz789-custom-env")

	extraEnvVars, ok := raw["extra_environment_variables"].(map[string]any)
	if !ok {
		t.Fatal("extra_environment_variables field missing or wrong type")
	}
	assertField(t, extraEnvVars, "MODEL_NAME", "claude-opus-4-6")
	assertField(t, extraEnvVars, "BATCH_SIZE", "32")

	payload, ok := raw["payload"].(map[string]any)
	if !ok {
		t.Fatal("payload field missing or wrong type")
	}
	assertField(t, payload, "project", "iree/amdgpu")
	assertField(t, payload, "max_tokens", float64(8192))

	// Round-trip.
	var decoded PrincipalAssignment
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Template != "iree/template:amdgpu-developer" {
		t.Errorf("Template: got %q, want %q", decoded.Template, "iree/template:amdgpu-developer")
	}
	if len(decoded.CommandOverride) != 2 || decoded.CommandOverride[0] != "/usr/local/bin/custom-agent" {
		t.Errorf("CommandOverride: got %v, want [/usr/local/bin/custom-agent --mode=gpu]", decoded.CommandOverride)
	}
	if decoded.EnvironmentOverride != "/nix/store/xyz789-custom-env" {
		t.Errorf("EnvironmentOverride: got %q, want %q", decoded.EnvironmentOverride, "/nix/store/xyz789-custom-env")
	}
	if decoded.ExtraEnvironmentVariables["MODEL_NAME"] != "claude-opus-4-6" {
		t.Errorf("ExtraEnvironmentVariables[MODEL_NAME]: got %q, want %q",
			decoded.ExtraEnvironmentVariables["MODEL_NAME"], "claude-opus-4-6")
	}
}

func TestPrincipalAssignmentOmitsEmptyOverrides(t *testing.T) {
	// A PrincipalAssignment without override fields should not include
	// them in the wire format (backward compatibility).
	assignment := PrincipalAssignment{
		Principal: testEntity(t, "@bureau/fleet/test/service/stt/whisper:bureau.local"),
		Template:  "bureau/template:whisper-stt",
		AutoStart: true,
	}

	data, err := json.Marshal(assignment)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	overrideFields := []string{
		"command_override", "environment_override",
		"extra_environment_variables", "payload",
	}
	for _, field := range overrideFields {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty/nil", field)
		}
	}

	// Existing fields should still be present. The principal is serialized
	// as the full Matrix user ID via Entity.MarshalText.
	assertField(t, raw, "principal", "@bureau/fleet/test/service/stt/whisper:bureau.local")
	assertField(t, raw, "template", "bureau/template:whisper-stt")
	assertField(t, raw, "auto_start", true)
}

func TestStartConditionOnPrincipalAssignment(t *testing.T) {
	original := PrincipalAssignment{
		Principal: testEntity(t, "@bureau/fleet/test/agent/pm:bureau.local"),
		Template:  "iree/template:llm-agent",
		AutoStart: true,
		StartCondition: &StartCondition{
			EventType:    "m.bureau.workspace",
			StateKey:     "",
			RoomAlias:    ref.MustParseRoomAlias("#iree/amdgpu/inference:bureau.local"),
			ContentMatch: ContentMatch{"status": Eq("active")},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	startCondition, ok := raw["start_condition"].(map[string]any)
	if !ok {
		t.Fatal("start_condition field missing or wrong type")
	}
	assertField(t, startCondition, "event_type", "m.bureau.workspace")
	assertField(t, startCondition, "state_key", "")
	assertField(t, startCondition, "room_alias", "#iree/amdgpu/inference:bureau.local")

	contentMatch, ok := startCondition["content_match"].(map[string]any)
	if !ok {
		t.Fatal("content_match field missing or wrong type")
	}
	assertField(t, contentMatch, "status", "active")

	var decoded PrincipalAssignment
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.StartCondition == nil {
		t.Fatal("StartCondition should not be nil after round-trip")
	}
	if decoded.StartCondition.EventType != "m.bureau.workspace" {
		t.Errorf("StartCondition.EventType: got %q, want %q",
			decoded.StartCondition.EventType, "m.bureau.workspace")
	}
	if decoded.StartCondition.StateKey != "" {
		t.Errorf("StartCondition.StateKey: got %q, want empty", decoded.StartCondition.StateKey)
	}
	if decoded.StartCondition.RoomAlias != ref.MustParseRoomAlias("#iree/amdgpu/inference:bureau.local") {
		t.Errorf("StartCondition.RoomAlias: got %s, want %s",
			decoded.StartCondition.RoomAlias, "#iree/amdgpu/inference:bureau.local")
	}
	statusMatch, ok := decoded.StartCondition.ContentMatch["status"]
	if !ok {
		t.Fatal("StartCondition.ContentMatch missing \"status\" key")
	}
	if statusMatch.StringValue() != "active" {
		t.Errorf("StartCondition.ContentMatch[status]: got %q, want %q",
			statusMatch.StringValue(), "active")
	}
}

func TestStartConditionOmittedWhenNil(t *testing.T) {
	assignment := PrincipalAssignment{
		Principal: testEntity(t, "@bureau/fleet/test/service/stt/whisper:bureau.local"),
		Template:  "bureau/template:whisper-stt",
		AutoStart: true,
	}

	data, err := json.Marshal(assignment)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["start_condition"]; exists {
		t.Error("start_condition should be omitted when nil")
	}
}

func TestStartConditionOmitsEmptyRoomAlias(t *testing.T) {
	// When RoomAlias is empty (check principal's own config room),
	// it should be omitted from the wire format.
	condition := StartCondition{
		EventType: "m.bureau.workspace",
		StateKey:  "",
	}

	data, err := json.Marshal(condition)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "event_type", "m.bureau.workspace")
	assertField(t, raw, "state_key", "")
	// ref.RoomAlias implements TextMarshaler, so omitempty serializes
	// the zero value as "" rather than omitting the field entirely.
	if value, exists := raw["room_alias"]; exists && value != "" {
		t.Errorf("room_alias should be empty string for zero value, got %v", value)
	}
	if _, exists := raw["content_match"]; exists {
		t.Error("content_match should be omitted when nil")
	}
}

func TestStartConditionContentMatchRoundTrip(t *testing.T) {
	condition := StartCondition{
		EventType:    "m.bureau.workspace",
		StateKey:     "",
		RoomAlias:    ref.MustParseRoomAlias("#iree/amdgpu/inference:bureau.local"),
		ContentMatch: ContentMatch{"status": Eq("active")},
	}

	data, err := json.Marshal(condition)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded StartCondition
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.ContentMatch) != 1 {
		t.Fatalf("ContentMatch length = %d, want 1", len(decoded.ContentMatch))
	}
	statusMatch, ok := decoded.ContentMatch["status"]
	if !ok {
		t.Fatal("ContentMatch missing \"status\" key")
	}
	if statusMatch.StringValue() != "active" {
		t.Errorf("ContentMatch[status] = %q, want %q", statusMatch.StringValue(), "active")
	}
}

func TestStartConditionMultipleContentMatch(t *testing.T) {
	// ContentMatch with multiple keys — all must match.
	condition := StartCondition{
		EventType: "m.bureau.service",
		StateKey:  "stt/whisper",
		ContentMatch: ContentMatch{
			"status": Eq("healthy"),
			"stage":  Eq("canary"),
		},
	}

	data, err := json.Marshal(condition)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	contentMatch, ok := raw["content_match"].(map[string]any)
	if !ok {
		t.Fatal("content_match field missing or wrong type")
	}
	assertField(t, contentMatch, "status", "healthy")
	assertField(t, contentMatch, "stage", "canary")
}
