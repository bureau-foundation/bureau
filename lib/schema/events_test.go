// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestEventTypeConstants(t *testing.T) {
	// Verify the event type strings match the Matrix convention (m.bureau.*).
	// These are wire-format identifiers that must never change without a
	// coordinated migration.
	tests := []struct {
		name     string
		constant string
		want     string
	}{
		{"machine_key", EventTypeMachineKey, "m.bureau.machine_key"},
		{"machine_status", EventTypeMachineStatus, "m.bureau.machine_status"},
		{"machine_config", EventTypeMachineConfig, "m.bureau.machine_config"},
		{"credentials", EventTypeCredentials, "m.bureau.credentials"},
		{"service", EventTypeService, "m.bureau.service"},
		{"layout", EventTypeLayout, "m.bureau.layout"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.constant != test.want {
				t.Errorf("%s = %q, want %q", test.name, test.constant, test.want)
			}
		})
	}
}

func TestMachineKeyRoundTrip(t *testing.T) {
	original := MachineKey{
		Algorithm: "age-x25519",
		PublicKey: "age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqs3290gq",
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
	assertField(t, raw, "algorithm", "age-x25519")
	assertField(t, raw, "public_key", original.PublicKey)

	var decoded MachineKey
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestMachineStatusRoundTrip(t *testing.T) {
	original := MachineStatus{
		Principal:             "@machine/workstation:bureau.local",
		CPUPercent:            42.5,
		MemoryUsedGB:          12.3,
		GPUUtilizationPercent: 87.0,
		Sandboxes:             SandboxCounts{Running: 5, Idle: 2},
		UptimeSeconds:         86400,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "principal", "@machine/workstation:bureau.local")
	assertField(t, raw, "cpu_percent", 42.5)
	assertField(t, raw, "memory_used_gb", 12.3)
	assertField(t, raw, "gpu_utilization_percent", 87.0)
	assertField(t, raw, "uptime_seconds", float64(86400))

	var decoded MachineStatus
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestMachineStatusOmitsZeroGPU(t *testing.T) {
	status := MachineStatus{
		Principal:     "@machine/pi-kitchen:bureau.local",
		CPUPercent:    15.0,
		MemoryUsedGB:  0.8,
		Sandboxes:     SandboxCounts{Running: 1, Idle: 0},
		UptimeSeconds: 3600,
		// GPUUtilizationPercent deliberately zero.
	}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["gpu_utilization_percent"]; exists {
		t.Error("gpu_utilization_percent should be omitted when zero")
	}
}

func TestMachineConfigRoundTrip(t *testing.T) {
	original := MachineConfig{
		Principals: []PrincipalAssignment{
			{
				Localpart:         "iree/amdgpu/pm",
				Template:          "llm-agent",
				AutoStart:         true,
				ServiceVisibility: []string{"service/stt/*", "service/embedding/**"},
			},
			{
				Localpart: "service/stt/whisper",
				Template:  "whisper-stt",
				AutoStart: true,
			},
			{
				Localpart: "iree/amdgpu/codegen",
				Template:  "llm-agent",
				AutoStart: false,
			},
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
	principals, ok := raw["principals"].([]any)
	if !ok {
		t.Fatal("principals field missing or wrong type")
	}
	if len(principals) != 3 {
		t.Fatalf("principals count = %d, want 3", len(principals))
	}

	first := principals[0].(map[string]any)
	assertField(t, first, "localpart", "iree/amdgpu/pm")
	assertField(t, first, "template", "llm-agent")
	assertField(t, first, "auto_start", true)

	// Verify service_visibility appears in the wire format.
	visibility, ok := first["service_visibility"].([]any)
	if !ok {
		t.Fatal("service_visibility field missing or wrong type")
	}
	if len(visibility) != 2 {
		t.Fatalf("service_visibility count = %d, want 2", len(visibility))
	}
	if visibility[0] != "service/stt/*" || visibility[1] != "service/embedding/**" {
		t.Errorf("service_visibility = %v, want [service/stt/* service/embedding/**]", visibility)
	}

	// Verify service_visibility is omitted when nil.
	second := principals[1].(map[string]any)
	if _, exists := second["service_visibility"]; exists {
		t.Error("service_visibility should be omitted when nil")
	}

	var decoded MachineConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(decoded.Principals) != len(original.Principals) {
		t.Fatalf("round-trip principal count = %d, want %d", len(decoded.Principals), len(original.Principals))
	}
	for i := range original.Principals {
		if !reflect.DeepEqual(decoded.Principals[i], original.Principals[i]) {
			t.Errorf("principal[%d]: got %+v, want %+v", i, decoded.Principals[i], original.Principals[i])
		}
	}
}

func TestObservePolicyRoundTrip(t *testing.T) {
	original := ObservePolicy{
		AllowedObservers:   []string{"bureau-admin", "iree/**"},
		ReadWriteObservers: []string{"bureau-admin"},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	allowedObservers, ok := raw["allowed_observers"].([]any)
	if !ok {
		t.Fatal("allowed_observers field missing or wrong type")
	}
	if len(allowedObservers) != 2 {
		t.Fatalf("allowed_observers count = %d, want 2", len(allowedObservers))
	}
	if allowedObservers[0] != "bureau-admin" || allowedObservers[1] != "iree/**" {
		t.Errorf("allowed_observers = %v, want [bureau-admin iree/**]", allowedObservers)
	}

	readWriteObservers, ok := raw["readwrite_observers"].([]any)
	if !ok {
		t.Fatal("readwrite_observers field missing or wrong type")
	}
	if len(readWriteObservers) != 1 || readWriteObservers[0] != "bureau-admin" {
		t.Errorf("readwrite_observers = %v, want [bureau-admin]", readWriteObservers)
	}

	var decoded ObservePolicy
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(decoded.AllowedObservers) != 2 {
		t.Fatalf("AllowedObservers count = %d, want 2", len(decoded.AllowedObservers))
	}
	if decoded.AllowedObservers[0] != "bureau-admin" || decoded.AllowedObservers[1] != "iree/**" {
		t.Errorf("AllowedObservers = %v, want [bureau-admin iree/**]", decoded.AllowedObservers)
	}
	if len(decoded.ReadWriteObservers) != 1 || decoded.ReadWriteObservers[0] != "bureau-admin" {
		t.Errorf("ReadWriteObservers = %v, want [bureau-admin]", decoded.ReadWriteObservers)
	}
}

func TestObservePolicyOmitsEmptyFields(t *testing.T) {
	policy := ObservePolicy{}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	for _, field := range []string{"allowed_observers", "readwrite_observers"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty", field)
		}
	}
}

func TestObservePolicyDefaultDeny(t *testing.T) {
	// A nil ObservePolicy and an empty ObservePolicy both mean
	// "no observation allowed". Verify the zero value has no patterns.
	var policy ObservePolicy
	if len(policy.AllowedObservers) != 0 {
		t.Errorf("zero-value AllowedObservers should be empty, got %v", policy.AllowedObservers)
	}
	if len(policy.ReadWriteObservers) != 0 {
		t.Errorf("zero-value ReadWriteObservers should be empty, got %v", policy.ReadWriteObservers)
	}
}

func TestMachineConfigWithObservePolicy(t *testing.T) {
	// A MachineConfig with both DefaultObservePolicy and per-principal
	// ObservePolicy should round-trip correctly.
	config := MachineConfig{
		Principals: []PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "llm-agent",
				AutoStart: true,
				ObservePolicy: &ObservePolicy{
					AllowedObservers:   []string{"bureau-admin", "iree/**"},
					ReadWriteObservers: []string{"bureau-admin"},
				},
			},
			{
				Localpart: "service/stt/whisper",
				Template:  "whisper-stt",
				AutoStart: true,
				// No per-principal policy — falls back to DefaultObservePolicy.
			},
		},
		DefaultObservePolicy: &ObservePolicy{
			AllowedObservers: []string{"bureau-admin"},
		},
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded MachineConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Check DefaultObservePolicy.
	if decoded.DefaultObservePolicy == nil {
		t.Fatal("DefaultObservePolicy should not be nil after round-trip")
	}
	if len(decoded.DefaultObservePolicy.AllowedObservers) != 1 ||
		decoded.DefaultObservePolicy.AllowedObservers[0] != "bureau-admin" {
		t.Errorf("DefaultObservePolicy.AllowedObservers = %v, want [bureau-admin]",
			decoded.DefaultObservePolicy.AllowedObservers)
	}

	// Check first principal's per-principal policy.
	if decoded.Principals[0].ObservePolicy == nil {
		t.Fatal("Principals[0].ObservePolicy should not be nil after round-trip")
	}
	if len(decoded.Principals[0].ObservePolicy.AllowedObservers) != 2 {
		t.Fatalf("Principals[0].ObservePolicy.AllowedObservers count = %d, want 2",
			len(decoded.Principals[0].ObservePolicy.AllowedObservers))
	}
	if len(decoded.Principals[0].ObservePolicy.ReadWriteObservers) != 1 {
		t.Fatalf("Principals[0].ObservePolicy.ReadWriteObservers count = %d, want 1",
			len(decoded.Principals[0].ObservePolicy.ReadWriteObservers))
	}

	// Check second principal has no per-principal policy.
	if decoded.Principals[1].ObservePolicy != nil {
		t.Errorf("Principals[1].ObservePolicy should be nil, got %+v",
			decoded.Principals[1].ObservePolicy)
	}

	// Verify the wire format has the right structure.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["default_observe_policy"]; !exists {
		t.Error("default_observe_policy should be present in JSON")
	}
	principals := raw["principals"].([]any)
	firstPrincipal := principals[0].(map[string]any)
	if _, exists := firstPrincipal["observe_policy"]; !exists {
		t.Error("first principal should have observe_policy in JSON")
	}
	secondPrincipal := principals[1].(map[string]any)
	if _, exists := secondPrincipal["observe_policy"]; exists {
		t.Error("second principal should not have observe_policy in JSON")
	}
}

func TestCredentialsRoundTrip(t *testing.T) {
	original := Credentials{
		Version:   1,
		Principal: "@iree/amdgpu/pm:bureau.local",
		EncryptedFor: []string{
			"@machine/workstation:bureau.local",
			"yubikey:operator-escrow",
		},
		Keys:          []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"},
		Ciphertext:    "YWdlLWVuY3J5cHRpb24ub3JnL3YxCi0+IFgyNTUxOSA...",
		ProvisionedBy: "@bureau/operator:bureau.local",
		ProvisionedAt: "2026-02-09T18:30:00Z",
		Signature:     "base64signature==",
		ExpiresAt:     "2026-08-09T18:30:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "version", float64(1))
	assertField(t, raw, "principal", "@iree/amdgpu/pm:bureau.local")
	assertField(t, raw, "ciphertext", original.Ciphertext)
	assertField(t, raw, "provisioned_by", "@bureau/operator:bureau.local")
	assertField(t, raw, "provisioned_at", "2026-02-09T18:30:00Z")
	assertField(t, raw, "signature", "base64signature==")
	assertField(t, raw, "expires_at", "2026-08-09T18:30:00Z")

	var decoded Credentials
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Version != original.Version {
		t.Errorf("Version: got %d, want %d", decoded.Version, original.Version)
	}
	if decoded.Principal != original.Principal {
		t.Errorf("Principal: got %q, want %q", decoded.Principal, original.Principal)
	}
	if decoded.Ciphertext != original.Ciphertext {
		t.Errorf("Ciphertext: got %q, want %q", decoded.Ciphertext, original.Ciphertext)
	}
}

func TestCredentialsOmitsEmptyExpiry(t *testing.T) {
	credentials := Credentials{
		Version:       1,
		Principal:     "@iree/amdgpu/pm:bureau.local",
		EncryptedFor:  []string{"@machine/workstation:bureau.local"},
		Keys:          []string{"OPENAI_API_KEY"},
		Ciphertext:    "encrypted",
		ProvisionedBy: "@bureau/operator:bureau.local",
		ProvisionedAt: "2026-02-09T18:30:00Z",
		Signature:     "sig",
		// ExpiresAt deliberately empty.
	}

	data, err := json.Marshal(credentials)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["expires_at"]; exists {
		t.Error("expires_at should be omitted when empty")
	}
}

func TestServiceRoundTrip(t *testing.T) {
	original := Service{
		Principal:    "@service/stt/whisper:bureau.local",
		Machine:      "@machine/cloud-gpu-1:bureau.local",
		Capabilities: []string{"streaming", "speaker-diarization"},
		Protocol:     "http",
		Description:  "Whisper Large V3 streaming STT",
		Metadata: map[string]any{
			"languages":     []any{"en", "es", "ja"},
			"model_version": "large-v3",
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
	assertField(t, raw, "principal", "@service/stt/whisper:bureau.local")
	assertField(t, raw, "machine", "@machine/cloud-gpu-1:bureau.local")
	assertField(t, raw, "protocol", "http")
	assertField(t, raw, "description", "Whisper Large V3 streaming STT")

	var decoded Service
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Principal != original.Principal {
		t.Errorf("Principal: got %q, want %q", decoded.Principal, original.Principal)
	}
	if decoded.Machine != original.Machine {
		t.Errorf("Machine: got %q, want %q", decoded.Machine, original.Machine)
	}
	if decoded.Protocol != original.Protocol {
		t.Errorf("Protocol: got %q, want %q", decoded.Protocol, original.Protocol)
	}
	if len(decoded.Capabilities) != 2 {
		t.Fatalf("Capabilities count = %d, want 2", len(decoded.Capabilities))
	}
	if decoded.Capabilities[0] != "streaming" || decoded.Capabilities[1] != "speaker-diarization" {
		t.Errorf("Capabilities: got %v, want [streaming speaker-diarization]", decoded.Capabilities)
	}
}

func TestServiceOmitsOptionalFields(t *testing.T) {
	service := Service{
		Principal: "@service/tts/piper:bureau.local",
		Machine:   "@machine/workstation:bureau.local",
		Protocol:  "http",
		// Capabilities, Description, Metadata deliberately empty.
	}

	data, err := json.Marshal(service)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	for _, field := range []string{"capabilities", "description", "metadata"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty", field)
		}
	}
}

func TestConfigRoomPowerLevels(t *testing.T) {
	adminUserID := "@bureau-admin:bureau.local"
	levels := ConfigRoomPowerLevels(adminUserID)

	// Admin should have power level 100.
	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	adminLevel, ok := users[adminUserID]
	if !ok {
		t.Fatalf("admin %q not in users map", adminUserID)
	}
	if adminLevel != 100 {
		t.Errorf("admin power level = %v, want 100", adminLevel)
	}

	// Default user power level should be 0 (machine is read-only).
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	// Bureau config and credentials events require power level 100.
	events, ok := levels["events"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}
	if events[EventTypeMachineConfig] != 100 {
		t.Errorf("%s power level = %v, want 100", EventTypeMachineConfig, events[EventTypeMachineConfig])
	}
	if events[EventTypeCredentials] != 100 {
		t.Errorf("%s power level = %v, want 100", EventTypeCredentials, events[EventTypeCredentials])
	}

	// Layout events are writable by the daemon (power level 0) so it can
	// publish layout state without admin privileges.
	if events[EventTypeLayout] != 0 {
		t.Errorf("%s power level = %v, want 0", EventTypeLayout, events[EventTypeLayout])
	}

	// Default event power level should be 100 (admin-only room).
	if levels["events_default"] != 100 {
		t.Errorf("events_default = %v, want 100", levels["events_default"])
	}

	// All administrative actions require power level 100.
	for _, field := range []string{"state_default", "ban", "kick", "invite", "redact"} {
		if levels[field] != 100 {
			t.Errorf("%s = %v, want 100", field, levels[field])
		}
	}
}

func TestLayoutContentRoundTrip(t *testing.T) {
	// A channel layout with two windows: agents (two observe panes) and
	// tools (a command pane and an observe pane). Exercises all pane modes
	// except ObserveMembers (tested separately).
	original := LayoutContent{
		Prefix: "C-a",
		Windows: []LayoutWindow{
			{
				Name: "agents",
				Panes: []LayoutPane{
					{Observe: "iree/amdgpu/pm", Split: "horizontal", Size: 50},
					{Observe: "iree/amdgpu/codegen", Size: 50},
				},
			},
			{
				Name: "tools",
				Panes: []LayoutPane{
					{Command: "beads-tui --project iree/amdgpu", Split: "horizontal", Size: 30},
					{Observe: "iree/amdgpu/ci-runner", Size: 70},
				},
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify JSON field names match the wire format from OBSERVATION.md.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "prefix", "C-a")
	windows, ok := raw["windows"].([]any)
	if !ok {
		t.Fatal("windows field missing or wrong type")
	}
	if len(windows) != 2 {
		t.Fatalf("windows count = %d, want 2", len(windows))
	}

	agentsWindow := windows[0].(map[string]any)
	assertField(t, agentsWindow, "name", "agents")
	agentsPanes := agentsWindow["panes"].([]any)
	if len(agentsPanes) != 2 {
		t.Fatalf("agents panes count = %d, want 2", len(agentsPanes))
	}
	firstPane := agentsPanes[0].(map[string]any)
	assertField(t, firstPane, "observe", "iree/amdgpu/pm")
	assertField(t, firstPane, "split", "horizontal")
	assertField(t, firstPane, "size", float64(50))

	toolsWindow := windows[1].(map[string]any)
	assertField(t, toolsWindow, "name", "tools")
	toolsPanes := toolsWindow["panes"].([]any)
	firstToolPane := toolsPanes[0].(map[string]any)
	assertField(t, firstToolPane, "command", "beads-tui --project iree/amdgpu")
	assertField(t, firstToolPane, "size", float64(30))

	// Round-trip back to struct.
	var decoded LayoutContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Prefix != original.Prefix {
		t.Errorf("Prefix: got %q, want %q", decoded.Prefix, original.Prefix)
	}
	if len(decoded.Windows) != len(original.Windows) {
		t.Fatalf("windows count: got %d, want %d", len(decoded.Windows), len(original.Windows))
	}
	for windowIndex, window := range original.Windows {
		decodedWindow := decoded.Windows[windowIndex]
		if decodedWindow.Name != window.Name {
			t.Errorf("window[%d].Name: got %q, want %q", windowIndex, decodedWindow.Name, window.Name)
		}
		if len(decodedWindow.Panes) != len(window.Panes) {
			t.Fatalf("window[%d] panes count: got %d, want %d", windowIndex, len(decodedWindow.Panes), len(window.Panes))
		}
		for paneIndex, pane := range window.Panes {
			decodedPane := decodedWindow.Panes[paneIndex]
			if decodedPane.Observe != pane.Observe {
				t.Errorf("window[%d].pane[%d].Observe: got %q, want %q", windowIndex, paneIndex, decodedPane.Observe, pane.Observe)
			}
			if decodedPane.Command != pane.Command {
				t.Errorf("window[%d].pane[%d].Command: got %q, want %q", windowIndex, paneIndex, decodedPane.Command, pane.Command)
			}
			if decodedPane.Split != pane.Split {
				t.Errorf("window[%d].pane[%d].Split: got %q, want %q", windowIndex, paneIndex, decodedPane.Split, pane.Split)
			}
			if decodedPane.Size != pane.Size {
				t.Errorf("window[%d].pane[%d].Size: got %d, want %d", windowIndex, paneIndex, decodedPane.Size, pane.Size)
			}
		}
	}
}

func TestLayoutContentPrincipalLayout(t *testing.T) {
	// A principal layout uses "role" instead of "observe" or "command".
	// The launcher resolves roles to concrete commands.
	original := LayoutContent{
		Windows: []LayoutWindow{
			{
				Name: "main",
				Panes: []LayoutPane{
					{Role: "agent", Split: "horizontal", Size: 65},
					{Role: "shell", Size: 35},
				},
			},
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

	// Prefix should be omitted when empty (uses Bureau default).
	if _, exists := raw["prefix"]; exists {
		t.Error("prefix should be omitted when empty")
	}

	windows := raw["windows"].([]any)
	mainWindow := windows[0].(map[string]any)
	panes := mainWindow["panes"].([]any)
	agentPane := panes[0].(map[string]any)
	assertField(t, agentPane, "role", "agent")
	assertField(t, agentPane, "size", float64(65))

	// Observe and command should not appear in principal layouts.
	if _, exists := agentPane["observe"]; exists {
		t.Error("observe should be omitted when empty")
	}
	if _, exists := agentPane["command"]; exists {
		t.Error("command should be omitted when empty")
	}

	var decoded LayoutContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Prefix != "" {
		t.Errorf("Prefix should be empty, got %q", decoded.Prefix)
	}
	if decoded.Windows[0].Panes[0].Role != "agent" {
		t.Errorf("Role: got %q, want %q", decoded.Windows[0].Panes[0].Role, "agent")
	}
}

func TestLayoutContentObserveMembers(t *testing.T) {
	// Dynamic pane creation from room membership. The daemon expands
	// ObserveMembers into concrete observe panes at runtime.
	original := LayoutContent{
		Windows: []LayoutWindow{
			{
				Name: "team",
				Panes: []LayoutPane{
					{
						ObserveMembers: &LayoutMemberFilter{Role: "agent"},
						Split:          "horizontal",
					},
				},
			},
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

	windows := raw["windows"].([]any)
	panes := windows[0].(map[string]any)["panes"].([]any)
	pane := panes[0].(map[string]any)

	observeMembers, ok := pane["observe_members"].(map[string]any)
	if !ok {
		t.Fatal("observe_members field missing or wrong type")
	}
	assertField(t, observeMembers, "role", "agent")

	// Other pane mode fields should be absent.
	for _, field := range []string{"observe", "command", "role"} {
		if _, exists := pane[field]; exists {
			t.Errorf("%s should be omitted when ObserveMembers is set", field)
		}
	}

	var decoded LayoutContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	decodedPane := decoded.Windows[0].Panes[0]
	if decodedPane.ObserveMembers == nil {
		t.Fatal("ObserveMembers should not be nil after round-trip")
	}
	if decodedPane.ObserveMembers.Role != "agent" {
		t.Errorf("ObserveMembers.Role: got %q, want %q", decodedPane.ObserveMembers.Role, "agent")
	}
}

func TestLayoutContentSourceMachineRoundTrip(t *testing.T) {
	// SourceMachine and SealedMetadata are set by the daemon before
	// publishing; verify they survive JSON serialization.
	original := LayoutContent{
		SourceMachine:  "@machine/workstation:bureau.local",
		SealedMetadata: "age-encrypted-blob-base64",
		Windows: []LayoutWindow{
			{
				Name: "main",
				Panes: []LayoutPane{
					{Role: "agent"},
				},
			},
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
	assertField(t, raw, "source_machine", "@machine/workstation:bureau.local")
	assertField(t, raw, "sealed_metadata", "age-encrypted-blob-base64")

	var decoded LayoutContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.SourceMachine != original.SourceMachine {
		t.Errorf("SourceMachine: got %q, want %q", decoded.SourceMachine, original.SourceMachine)
	}
	if decoded.SealedMetadata != original.SealedMetadata {
		t.Errorf("SealedMetadata: got %q, want %q", decoded.SealedMetadata, original.SealedMetadata)
	}
}

func TestLayoutContentOmitsEmptySourceMachine(t *testing.T) {
	// When SourceMachine and SealedMetadata are empty, they should be
	// omitted from the JSON to keep the wire format clean.
	layout := LayoutContent{
		Windows: []LayoutWindow{
			{Name: "main", Panes: []LayoutPane{{Role: "agent"}}},
		},
	}

	data, err := json.Marshal(layout)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	for _, field := range []string{"source_machine", "sealed_metadata", "prefix"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty", field)
		}
	}
}

func TestLayoutContentOmitsEmptyFields(t *testing.T) {
	// Verify that zero-value optional fields are omitted from JSON.
	layout := LayoutContent{
		Windows: []LayoutWindow{
			{
				Name: "minimal",
				Panes: []LayoutPane{
					{Observe: "test/agent"},
				},
			},
		},
	}

	data, err := json.Marshal(layout)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Top-level prefix should be omitted.
	if _, exists := raw["prefix"]; exists {
		t.Error("prefix should be omitted when empty")
	}

	panes := raw["windows"].([]any)[0].(map[string]any)["panes"].([]any)
	pane := panes[0].(map[string]any)

	for _, field := range []string{"command", "role", "observe_members", "split", "size"} {
		if _, exists := pane[field]; exists {
			t.Errorf("%s should be omitted when zero-value", field)
		}
	}
}

// assertField checks that a JSON object has a field with the expected value.
func assertField(t *testing.T, object map[string]any, key string, want any) {
	t.Helper()
	got, ok := object[key]
	if !ok {
		t.Errorf("field %q missing from JSON", key)
		return
	}
	// JSON numbers are float64, booleans are bool, strings are string.
	if got != want {
		t.Errorf("field %q = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}
