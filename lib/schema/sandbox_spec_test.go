// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"testing"
)

func TestSandboxSpecRoundTrip(t *testing.T) {
	t.Parallel()

	original := SandboxSpec{
		Command: []string{"/usr/local/bin/claude", "--agent", "--no-tty"},
		Filesystem: []TemplateMount{
			{Source: "/home/agent/worktree", Dest: "/workspace", Mode: MountModeRW},
			{Type: "tmpfs", Dest: "/tmp", Options: "size=64M"},
			{Source: "/nix/store/abc123-bureau-agent-env", Dest: "/usr/local", Mode: MountModeRO},
			{Source: "/nix", Dest: "/nix", Mode: MountModeRO},
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
		EnvironmentVariables: map[string]string{
			"PATH":           "/usr/local/bin:/usr/bin:/bin",
			"HOME":           "/workspace",
			"BUREAU_SANDBOX": "1",
			"MODEL_NAME":     "claude-opus-4-6",
		},
		EnvironmentPath: "/nix/store/abc123-bureau-agent-env",
		Payload: map[string]any{
			"project":    "iree/amdgpu",
			"max_tokens": float64(8192),
		},
		Roles: map[string][]string{
			"agent": {"/usr/local/bin/claude", "--agent"},
			"shell": {"/bin/bash"},
		},
		CreateDirs: []string{"/tmp", "/var/tmp", "/run/bureau"},
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

	// Verify JSON wire format field names.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Command must be present (not omitempty).
	command, ok := raw["command"].([]any)
	if !ok {
		t.Fatal("command field missing or wrong type")
	}
	if len(command) != 3 || command[0] != "/usr/local/bin/claude" {
		t.Errorf("command = %v, want [/usr/local/bin/claude --agent --no-tty]", command)
	}

	assertField(t, raw, "environment_path", "/nix/store/abc123-bureau-agent-env")

	// Verify filesystem structure.
	filesystem, ok := raw["filesystem"].([]any)
	if !ok {
		t.Fatal("filesystem field missing or wrong type")
	}
	if len(filesystem) != 4 {
		t.Fatalf("filesystem count = %d, want 4", len(filesystem))
	}

	// Verify environment_variables.
	environmentVariables, ok := raw["environment_variables"].(map[string]any)
	if !ok {
		t.Fatal("environment_variables field missing or wrong type")
	}
	assertField(t, environmentVariables, "MODEL_NAME", "claude-opus-4-6")

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

	// Verify payload.
	payload, ok := raw["payload"].(map[string]any)
	if !ok {
		t.Fatal("payload field missing or wrong type")
	}
	assertField(t, payload, "project", "iree/amdgpu")

	// Round-trip back to struct.
	var decoded SandboxSpec
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Command) != 3 || decoded.Command[0] != "/usr/local/bin/claude" {
		t.Errorf("Command: got %v, want [/usr/local/bin/claude --agent --no-tty]", decoded.Command)
	}
	if decoded.EnvironmentPath != "/nix/store/abc123-bureau-agent-env" {
		t.Errorf("EnvironmentPath: got %q, want %q", decoded.EnvironmentPath, "/nix/store/abc123-bureau-agent-env")
	}
	if len(decoded.Filesystem) != 4 {
		t.Fatalf("Filesystem count = %d, want 4", len(decoded.Filesystem))
	}
	if decoded.Filesystem[0].Dest != "/workspace" || decoded.Filesystem[0].Mode != MountModeRW {
		t.Errorf("Filesystem[0]: got dest=%q mode=%q, want /workspace rw",
			decoded.Filesystem[0].Dest, decoded.Filesystem[0].Mode)
	}
	if decoded.Namespaces == nil || !decoded.Namespaces.PID {
		t.Error("Namespaces.PID should be true")
	}
	if decoded.Resources == nil || decoded.Resources.MemoryLimitMB != 8192 {
		t.Error("Resources.MemoryLimitMB should be 8192")
	}
	if decoded.Security == nil || !decoded.Security.NoNewPrivs {
		t.Error("Security.NoNewPrivs should be true")
	}
	if decoded.EnvironmentVariables["BUREAU_SANDBOX"] != "1" {
		t.Errorf("EnvironmentVariables[BUREAU_SANDBOX]: got %q, want %q",
			decoded.EnvironmentVariables["BUREAU_SANDBOX"], "1")
	}
	if len(decoded.Roles) != 2 {
		t.Fatalf("Roles count = %d, want 2", len(decoded.Roles))
	}
	if len(decoded.CreateDirs) != 3 {
		t.Fatalf("CreateDirs count = %d, want 3", len(decoded.CreateDirs))
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
}

func TestSandboxSpecOmitsEmptyFields(t *testing.T) {
	t.Parallel()

	// A minimal SandboxSpec with only command should omit all optional fields.
	spec := SandboxSpec{
		Command: []string{"/bin/bash"},
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	omittedFields := []string{
		"filesystem", "namespaces", "resources", "security",
		"environment_variables", "environment_path",
		"payload", "roles", "create_dirs", "proxy_services",
	}
	for _, field := range omittedFields {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty/nil", field)
		}
	}

	// Command must always be present.
	if _, exists := raw["command"]; !exists {
		t.Error("command should be present")
	}
}

func TestSandboxSpecInIPCRequest(t *testing.T) {
	t.Parallel()

	// Simulate a create-sandbox IPC request containing a SandboxSpec.
	// This verifies the SandboxSpec embeds correctly in the JSON envelope
	// that both daemon and launcher use.
	type ipcRequest struct {
		Action               string       `json:"action"`
		Principal            string       `json:"principal,omitempty"`
		EncryptedCredentials string       `json:"encrypted_credentials,omitempty"`
		SandboxSpec          *SandboxSpec `json:"sandbox_spec,omitempty"`
	}

	request := ipcRequest{
		Action:               "create-sandbox",
		Principal:            "iree/amdgpu/pm",
		EncryptedCredentials: "age-encrypted-blob",
		SandboxSpec: &SandboxSpec{
			Command: []string{"/usr/local/bin/claude", "--agent"},
			Filesystem: []TemplateMount{
				{Source: "/home/agent/worktree", Dest: "/workspace", Mode: MountModeRW},
			},
			Namespaces: &TemplateNamespaces{PID: true, Net: true},
			Security:   &TemplateSecurity{NoNewPrivs: true, DieWithParent: true},
			EnvironmentVariables: map[string]string{
				"PATH": "/usr/local/bin:/usr/bin:/bin",
			},
		},
	}

	data, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Verify the envelope fields.
	assertField(t, raw, "action", "create-sandbox")
	assertField(t, raw, "principal", "iree/amdgpu/pm")

	// Verify sandbox_spec is nested correctly.
	sandboxSpec, ok := raw["sandbox_spec"].(map[string]any)
	if !ok {
		t.Fatal("sandbox_spec field missing or wrong type")
	}
	specCommand, ok := sandboxSpec["command"].([]any)
	if !ok {
		t.Fatal("sandbox_spec.command missing or wrong type")
	}
	if len(specCommand) != 2 || specCommand[0] != "/usr/local/bin/claude" {
		t.Errorf("sandbox_spec.command = %v, want [/usr/local/bin/claude --agent]", specCommand)
	}

	// Round-trip the full request.
	var decoded ipcRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.SandboxSpec == nil {
		t.Fatal("SandboxSpec should not be nil after round-trip")
	}
	if len(decoded.SandboxSpec.Command) != 2 {
		t.Fatalf("SandboxSpec.Command count = %d, want 2", len(decoded.SandboxSpec.Command))
	}
	if decoded.SandboxSpec.Namespaces == nil || !decoded.SandboxSpec.Namespaces.PID {
		t.Error("SandboxSpec.Namespaces.PID should be true")
	}

	// Verify nil SandboxSpec is omitted (backward compatibility for
	// existing requests that don't include a spec yet).
	requestWithoutSpec := ipcRequest{
		Action:    "create-sandbox",
		Principal: "service/stt/whisper",
	}
	data, err = json.Marshal(requestWithoutSpec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var rawWithoutSpec map[string]any
	if err := json.Unmarshal(data, &rawWithoutSpec); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := rawWithoutSpec["sandbox_spec"]; exists {
		t.Error("sandbox_spec should be omitted when nil")
	}
}
