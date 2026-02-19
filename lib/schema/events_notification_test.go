// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNotificationMsgTypeConstants(t *testing.T) {
	t.Parallel()
	// Verify all notification msgtype constants use the m.bureau.* namespace
	// and match expected wire-format strings.
	tests := []struct {
		name     string
		constant string
		want     string
	}{
		{"ServiceDirectoryUpdated", MsgTypeServiceDirectoryUpdated, "m.bureau.service_directory_updated"},
		{"GrantsUpdated", MsgTypeGrantsUpdated, "m.bureau.grants_updated"},
		{"PayloadUpdated", MsgTypePayloadUpdated, "m.bureau.payload_updated"},
		{"PrincipalAdopted", MsgTypePrincipalAdopted, "m.bureau.principal_adopted"},
		{"SandboxExited", MsgTypeSandboxExited, "m.bureau.sandbox_exited"},
		{"CredentialsRotated", MsgTypeCredentialsRotated, "m.bureau.credentials_rotated"},
		{"ProxyCrash", MsgTypeProxyCrash, "m.bureau.proxy_crash"},
		{"HealthCheck", MsgTypeHealthCheck, "m.bureau.health_check"},
		{"DaemonSelfUpdate", MsgTypeDaemonSelfUpdate, "m.bureau.daemon_self_update"},
		{"NixPrefetchFailed", MsgTypeNixPrefetchFailed, "m.bureau.nix_prefetch_failed"},
		{"PrincipalStartFailed", MsgTypePrincipalStartFailed, "m.bureau.principal_start_failed"},
		{"PrincipalRestarted", MsgTypePrincipalRestarted, "m.bureau.principal_restarted"},
		{"BureauVersionUpdate", MsgTypeBureauVersionUpdate, "m.bureau.bureau_version_update"},
	}
	for _, tt := range tests {
		if tt.constant != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.constant, tt.want)
		}
	}
}

func TestServiceDirectoryUpdatedMessageRoundTrip(t *testing.T) {
	t.Parallel()
	original := NewServiceDirectoryUpdatedMessage(
		[]string{"service/stt/test", "service/tts/test"},
		[]string{"service/old/gone"},
		[]string{"service/vis/updated"},
	)

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypeServiceDirectoryUpdated)

	var decoded ServiceDirectoryUpdatedMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded.Added, original.Added) {
		t.Errorf("Added = %v, want %v", decoded.Added, original.Added)
	}
	if !reflect.DeepEqual(decoded.Removed, original.Removed) {
		t.Errorf("Removed = %v, want %v", decoded.Removed, original.Removed)
	}
	if !reflect.DeepEqual(decoded.Updated, original.Updated) {
		t.Errorf("Updated = %v, want %v", decoded.Updated, original.Updated)
	}
}

func TestServiceDirectoryUpdatedMessageMinimal(t *testing.T) {
	t.Parallel()
	original := NewServiceDirectoryUpdatedMessage([]string{"service/only/added"}, nil, nil)

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	for _, field := range []string{"removed", "updated"} {
		if _, exists := raw[field]; exists {
			t.Errorf("field %q should be omitted when nil", field)
		}
	}
}

func TestGrantsUpdatedMessageRoundTrip(t *testing.T) {
	t.Parallel()
	original := NewGrantsUpdatedMessage("agent/vis-hr", 3)

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypeGrantsUpdated)
	assertField(t, raw, "principal", "agent/vis-hr")
	// JSON numbers are float64.
	assertField(t, raw, "grant_count", float64(3))

	var decoded GrantsUpdatedMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Principal != "agent/vis-hr" {
		t.Errorf("Principal = %q, want %q", decoded.Principal, "agent/vis-hr")
	}
	if decoded.GrantCount != 3 {
		t.Errorf("GrantCount = %d, want 3", decoded.GrantCount)
	}
}

func TestPayloadUpdatedMessageRoundTrip(t *testing.T) {
	t.Parallel()
	original := NewPayloadUpdatedMessage("agent/payload-test")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypePayloadUpdated)
	assertField(t, raw, "principal", "agent/payload-test")

	var decoded PayloadUpdatedMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Principal != original.Principal {
		t.Errorf("Principal = %q, want %q", decoded.Principal, original.Principal)
	}
}

func TestPrincipalAdoptedMessageRoundTrip(t *testing.T) {
	t.Parallel()
	original := NewPrincipalAdoptedMessage("agent/adopted-test")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypePrincipalAdopted)
	assertField(t, raw, "principal", "agent/adopted-test")

	var decoded PrincipalAdoptedMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Principal != original.Principal {
		t.Errorf("Principal = %q, want %q", decoded.Principal, original.Principal)
	}
}

func TestSandboxExitedMessageRoundTrip(t *testing.T) {
	t.Parallel()
	original := NewSandboxExitedMessage("agent/test", 1, "signal: killed", "error: file not found\npanic: runtime error")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypeSandboxExited)
	assertField(t, raw, "principal", "agent/test")
	assertField(t, raw, "exit_code", float64(1))
	assertField(t, raw, "exit_description", "signal: killed")

	var decoded SandboxExitedMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", decoded.ExitCode)
	}
	if decoded.CapturedOutput != original.CapturedOutput {
		t.Errorf("CapturedOutput mismatch")
	}
}

func TestSandboxExitedMessageNormalExit(t *testing.T) {
	t.Parallel()
	original := NewSandboxExitedMessage("agent/test", 0, "", "")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	// Optional fields omitted on normal exit.
	for _, field := range []string{"exit_description", "captured_output"} {
		if _, exists := raw[field]; exists {
			t.Errorf("field %q should be omitted for normal exit", field)
		}
	}
}

func TestCredentialsRotatedMessageRoundTrip(t *testing.T) {
	t.Parallel()
	original := NewCredentialsRotatedMessage("agent/cred-test", "failed", "sandbox creation timed out")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypeCredentialsRotated)
	assertField(t, raw, "status", "failed")
	assertField(t, raw, "error", "sandbox creation timed out")

	var decoded CredentialsRotatedMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Status != "failed" {
		t.Errorf("Status = %q, want %q", decoded.Status, "failed")
	}
}

func TestCredentialsRotatedMessageRestartingOmitsError(t *testing.T) {
	t.Parallel()
	original := NewCredentialsRotatedMessage("agent/cred-test", "restarting", "")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["error"]; exists {
		t.Error("error field should be omitted when empty")
	}
}

func TestProxyCrashMessageRoundTrip(t *testing.T) {
	t.Parallel()
	original := NewProxyCrashMessage("agent/proxy-test", "detected", 137, "")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypeProxyCrash)
	assertField(t, raw, "status", "detected")
	assertField(t, raw, "exit_code", float64(137))

	var decoded ProxyCrashMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.ExitCode != 137 {
		t.Errorf("ExitCode = %d, want 137", decoded.ExitCode)
	}
}

func TestProxyCrashMessageRecoveredOmitsExitCode(t *testing.T) {
	t.Parallel()
	original := NewProxyCrashMessage("agent/proxy-test", "recovered", 0, "")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	for _, field := range []string{"exit_code", "error"} {
		if _, exists := raw[field]; exists {
			t.Errorf("field %q should be omitted for recovered proxy", field)
		}
	}
}

func TestHealthCheckMessageRoundTrip(t *testing.T) {
	t.Parallel()
	original := NewHealthCheckMessage("agent/health-test", "rollback_failed", "cannot read credentials: permission denied")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypeHealthCheck)
	assertField(t, raw, "outcome", "rollback_failed")

	var decoded HealthCheckMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Outcome != "rollback_failed" {
		t.Errorf("Outcome = %q, want %q", decoded.Outcome, "rollback_failed")
	}
}

func TestHealthCheckMessageDestroyedOmitsError(t *testing.T) {
	t.Parallel()
	original := NewHealthCheckMessage("agent/health-test", "destroyed", "")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["error"]; exists {
		t.Error("error field should be omitted when empty")
	}
}

func TestDaemonSelfUpdateMessageRoundTrip(t *testing.T) {
	t.Parallel()
	original := NewDaemonSelfUpdateMessage("/nix/store/old-daemon", "/nix/store/new-daemon", "succeeded", "")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypeDaemonSelfUpdate)
	assertField(t, raw, "previous_binary", "/nix/store/old-daemon")
	assertField(t, raw, "new_binary", "/nix/store/new-daemon")
	assertField(t, raw, "status", "succeeded")

	if _, exists := raw["error"]; exists {
		t.Error("error field should be omitted on success")
	}

	var decoded DaemonSelfUpdateMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.PreviousBinary != original.PreviousBinary {
		t.Errorf("PreviousBinary = %q, want %q", decoded.PreviousBinary, original.PreviousBinary)
	}
	if decoded.NewBinary != original.NewBinary {
		t.Errorf("NewBinary = %q, want %q", decoded.NewBinary, original.NewBinary)
	}
}

func TestNixPrefetchFailedMessageRoundTrip(t *testing.T) {
	t.Parallel()
	original := NewNixPrefetchFailedMessage("agent/nix-test", "/nix/store/abc-env", "connection refused")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypeNixPrefetchFailed)
	assertField(t, raw, "principal", "agent/nix-test")
	assertField(t, raw, "store_path", "/nix/store/abc-env")
	assertField(t, raw, "error", "connection refused")

	var decoded NixPrefetchFailedMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.StorePath != original.StorePath {
		t.Errorf("StorePath = %q, want %q", decoded.StorePath, original.StorePath)
	}
}

func TestPrincipalStartFailedMessageRoundTrip(t *testing.T) {
	t.Parallel()
	original := NewPrincipalStartFailedMessage("agent/start-test", "failed to mint service tokens: token expired")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypePrincipalStartFailed)
	assertField(t, raw, "principal", "agent/start-test")

	var decoded PrincipalStartFailedMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Error != original.Error {
		t.Errorf("Error = %q, want %q", decoded.Error, original.Error)
	}
}

func TestPrincipalRestartedMessageRoundTrip(t *testing.T) {
	t.Parallel()
	original := NewPrincipalRestartedMessage("agent/restart-test", "bureau/template:my-agent")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypePrincipalRestarted)
	assertField(t, raw, "principal", "agent/restart-test")
	assertField(t, raw, "template", "bureau/template:my-agent")

	var decoded PrincipalRestartedMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Template != original.Template {
		t.Errorf("Template = %q, want %q", decoded.Template, original.Template)
	}
}

func TestBureauVersionUpdatePrefetchFailedRoundTrip(t *testing.T) {
	t.Parallel()
	original := NewBureauVersionPrefetchFailedMessage("store path not found")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypeBureauVersionUpdate)
	assertField(t, raw, "status", "prefetch_failed")
	assertField(t, raw, "error", "store path not found")

	// Boolean fields omitted when false.
	for _, field := range []string{"proxy_changed", "launcher_changed"} {
		if _, exists := raw[field]; exists {
			t.Errorf("field %q should be omitted for prefetch failure", field)
		}
	}
}

func TestBureauVersionUpdateReconciledRoundTrip(t *testing.T) {
	t.Parallel()
	original := NewBureauVersionReconciledMessage(true, false)

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypeBureauVersionUpdate)
	assertField(t, raw, "status", "reconciled")
	assertField(t, raw, "proxy_changed", true)

	// Error and launcher_changed omitted.
	for _, field := range []string{"error", "launcher_changed"} {
		if _, exists := raw[field]; exists {
			t.Errorf("field %q should be omitted when zero/false", field)
		}
	}

	var decoded BureauVersionUpdateMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !decoded.ProxyChanged {
		t.Error("ProxyChanged = false, want true")
	}
	if decoded.LauncherChanged {
		t.Error("LauncherChanged = true, want false")
	}
}
