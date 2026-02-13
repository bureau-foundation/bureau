// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPipelineContentRoundTrip(t *testing.T) {
	original := PipelineContent{
		Description: "Clone repository and prepare project workspace",
		Variables: map[string]PipelineVariable{
			"REPOSITORY": {
				Description: "Git clone URL",
				Required:    true,
			},
			"BRANCH": {
				Description: "Git branch to check out",
				Default:     "main",
			},
			"PROJECT": {
				Description: "Project name",
				Required:    true,
			},
		},
		Steps: []PipelineStep{
			{
				Name: "create-project-directory",
				Run:  "mkdir -p /workspace/${PROJECT}",
			},
			{
				Name:    "clone-repository",
				Run:     "git clone --bare ${REPOSITORY} /workspace/${PROJECT}/.bare",
				Check:   "test -d /workspace/${PROJECT}/.bare/objects",
				When:    "test -n '${REPOSITORY}'",
				Timeout: "10m",
			},
			{
				Name:     "run-project-init",
				Run:      "/workspace/${PROJECT}/.bureau/pipeline/init",
				Optional: true,
				Env:      map[string]string{"INIT_MODE": "full"},
			},
			{
				Name: "publish-active",
				Publish: &PipelinePublish{
					EventType: "m.bureau.workspace",
					Room:      "${WORKSPACE_ROOM_ID}",
					Content: map[string]any{
						"status":     "active",
						"project":    "${PROJECT}",
						"machine":    "${MACHINE}",
						"updated_at": "2026-02-10T12:00:00Z",
					},
				},
			},
		},
		Log: &PipelineLog{
			Room: "${WORKSPACE_ROOM_ID}",
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
	assertField(t, raw, "description", "Clone repository and prepare project workspace")

	// Verify variables.
	variables, ok := raw["variables"].(map[string]any)
	if !ok {
		t.Fatal("variables field missing or wrong type")
	}
	if len(variables) != 3 {
		t.Fatalf("variables count = %d, want 3", len(variables))
	}
	repoVar, ok := variables["REPOSITORY"].(map[string]any)
	if !ok {
		t.Fatal("variables[REPOSITORY] missing or wrong type")
	}
	assertField(t, repoVar, "description", "Git clone URL")
	assertField(t, repoVar, "required", true)

	branchVar, ok := variables["BRANCH"].(map[string]any)
	if !ok {
		t.Fatal("variables[BRANCH] missing or wrong type")
	}
	assertField(t, branchVar, "default", "main")

	// Verify steps array.
	steps, ok := raw["steps"].([]any)
	if !ok {
		t.Fatal("steps field missing or wrong type")
	}
	if len(steps) != 4 {
		t.Fatalf("steps count = %d, want 4", len(steps))
	}

	// First step: simple run.
	firstStep := steps[0].(map[string]any)
	assertField(t, firstStep, "name", "create-project-directory")
	assertField(t, firstStep, "run", "mkdir -p /workspace/${PROJECT}")

	// Second step: run with check, when, timeout.
	secondStep := steps[1].(map[string]any)
	assertField(t, secondStep, "name", "clone-repository")
	assertField(t, secondStep, "check", "test -d /workspace/${PROJECT}/.bare/objects")
	assertField(t, secondStep, "when", "test -n '${REPOSITORY}'")
	assertField(t, secondStep, "timeout", "10m")

	// Third step: optional with env.
	thirdStep := steps[2].(map[string]any)
	assertField(t, thirdStep, "optional", true)
	stepEnv, ok := thirdStep["env"].(map[string]any)
	if !ok {
		t.Fatal("step env field missing or wrong type")
	}
	assertField(t, stepEnv, "INIT_MODE", "full")

	// Fourth step: publish.
	fourthStep := steps[3].(map[string]any)
	assertField(t, fourthStep, "name", "publish-active")
	publish, ok := fourthStep["publish"].(map[string]any)
	if !ok {
		t.Fatal("publish field missing or wrong type")
	}
	assertField(t, publish, "event_type", "m.bureau.workspace")
	assertField(t, publish, "room", "${WORKSPACE_ROOM_ID}")
	publishContent, ok := publish["content"].(map[string]any)
	if !ok {
		t.Fatal("publish content field missing or wrong type")
	}
	assertField(t, publishContent, "status", "active")
	assertField(t, publishContent, "project", "${PROJECT}")
	assertField(t, publishContent, "machine", "${MACHINE}")

	// Verify log.
	logField, ok := raw["log"].(map[string]any)
	if !ok {
		t.Fatal("log field missing or wrong type")
	}
	assertField(t, logField, "room", "${WORKSPACE_ROOM_ID}")

	// Round-trip back to struct.
	var decoded PipelineContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Description != original.Description {
		t.Errorf("Description: got %q, want %q", decoded.Description, original.Description)
	}
	if len(decoded.Variables) != 3 {
		t.Fatalf("Variables count = %d, want 3", len(decoded.Variables))
	}
	if !decoded.Variables["REPOSITORY"].Required {
		t.Error("Variables[REPOSITORY].Required should be true")
	}
	if decoded.Variables["BRANCH"].Default != "main" {
		t.Errorf("Variables[BRANCH].Default: got %q, want %q", decoded.Variables["BRANCH"].Default, "main")
	}
	if len(decoded.Steps) != 4 {
		t.Fatalf("Steps count = %d, want 4", len(decoded.Steps))
	}
	if decoded.Steps[0].Run != "mkdir -p /workspace/${PROJECT}" {
		t.Errorf("Steps[0].Run: got %q, want %q", decoded.Steps[0].Run, "mkdir -p /workspace/${PROJECT}")
	}
	if decoded.Steps[1].Check != "test -d /workspace/${PROJECT}/.bare/objects" {
		t.Errorf("Steps[1].Check: got %q, want %q", decoded.Steps[1].Check, "test -d /workspace/${PROJECT}/.bare/objects")
	}
	if decoded.Steps[2].Optional != true {
		t.Error("Steps[2].Optional should be true")
	}
	if decoded.Steps[3].Publish == nil {
		t.Fatal("Steps[3].Publish should not be nil after round-trip")
	}
	if decoded.Steps[3].Publish.EventType != "m.bureau.workspace" {
		t.Errorf("Steps[3].Publish.EventType: got %q, want %q",
			decoded.Steps[3].Publish.EventType, "m.bureau.workspace")
	}
	if decoded.Log == nil {
		t.Fatal("Log should not be nil after round-trip")
	}
	if decoded.Log.Room != "${WORKSPACE_ROOM_ID}" {
		t.Errorf("Log.Room: got %q, want %q", decoded.Log.Room, "${WORKSPACE_ROOM_ID}")
	}
}

func TestPipelineContentMinimal(t *testing.T) {
	// Pipeline with only the required field (steps) — all optional
	// fields should be omitted from the wire format.
	pipeline := PipelineContent{
		Steps: []PipelineStep{
			{Name: "hello", Run: "echo hello"},
		},
	}

	data, err := json.Marshal(pipeline)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"description", "variables", "log"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty/nil", field)
		}
	}

	// Steps should be present.
	steps, ok := raw["steps"].([]any)
	if !ok {
		t.Fatal("steps field missing or wrong type")
	}
	if len(steps) != 1 {
		t.Fatalf("steps count = %d, want 1", len(steps))
	}
}

func TestPipelineStepRunOnly(t *testing.T) {
	// A step with all run-related fields set; publish and interactive
	// should be omitted.
	step := PipelineStep{
		Name:     "full-run-step",
		Run:      "some-command --flag",
		Check:    "test -f /expected/file",
		When:     "test -n '${CONDITION}'",
		Optional: true,
		Timeout:  "30s",
		Env:      map[string]string{"KEY": "value"},
	}

	data, err := json.Marshal(step)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Run-related fields should be present.
	assertField(t, raw, "name", "full-run-step")
	assertField(t, raw, "run", "some-command --flag")
	assertField(t, raw, "check", "test -f /expected/file")
	assertField(t, raw, "when", "test -n '${CONDITION}'")
	assertField(t, raw, "optional", true)
	assertField(t, raw, "timeout", "30s")

	env, ok := raw["env"].(map[string]any)
	if !ok {
		t.Fatal("env field missing or wrong type")
	}
	assertField(t, env, "KEY", "value")

	// Publish, interactive, and grace_period should be absent.
	for _, field := range []string{"publish", "interactive", "grace_period"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted on a run-only step without it set", field)
		}
	}
}

func TestPipelineStepPublishOnly(t *testing.T) {
	// A step with only publish set; run-related fields should be omitted.
	step := PipelineStep{
		Name: "publish-state",
		Publish: &PipelinePublish{
			EventType: "m.bureau.workspace",
			Room:      "!abc123:bureau.local",
			StateKey:  "custom-key",
			Content: map[string]any{
				"status": "active",
			},
		},
	}

	data, err := json.Marshal(step)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Publish should be present with all fields.
	publish, ok := raw["publish"].(map[string]any)
	if !ok {
		t.Fatal("publish field missing or wrong type")
	}
	assertField(t, publish, "event_type", "m.bureau.workspace")
	assertField(t, publish, "room", "!abc123:bureau.local")
	assertField(t, publish, "state_key", "custom-key")
	content, ok := publish["content"].(map[string]any)
	if !ok {
		t.Fatal("publish content field missing or wrong type")
	}
	assertField(t, content, "status", "active")

	// Run-related fields should be absent.
	for _, field := range []string{"run", "check", "when", "optional", "timeout", "grace_period", "env", "interactive"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted on a publish-only step", field)
		}
	}
}

func TestPipelineStepOmitsEmptyFields(t *testing.T) {
	// Minimal step (name + run only) — all optional fields should be
	// omitted from the wire format.
	step := PipelineStep{
		Name: "simple",
		Run:  "echo done",
	}

	data, err := json.Marshal(step)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "name", "simple")
	assertField(t, raw, "run", "echo done")

	for _, field := range []string{"check", "when", "optional", "publish", "timeout", "grace_period", "env", "interactive"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when zero-value", field)
		}
	}
}

func TestPipelineStepInteractive(t *testing.T) {
	step := PipelineStep{
		Name:        "guided-setup",
		Run:         "claude --prompt 'Set up the workspace'",
		Interactive: true,
		Timeout:     "30m",
	}

	data, err := json.Marshal(step)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "interactive", true)
	assertField(t, raw, "timeout", "30m")

	var decoded PipelineStep
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !decoded.Interactive {
		t.Error("Interactive should be true after round-trip")
	}
}

func TestPipelineStepGracePeriod(t *testing.T) {
	step := PipelineStep{
		Name:        "db-migration",
		Run:         "python manage.py migrate",
		Timeout:     "10m",
		GracePeriod: "30s",
	}

	data, err := json.Marshal(step)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "grace_period", "30s")
	assertField(t, raw, "timeout", "10m")

	var decoded PipelineStep
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.GracePeriod != "30s" {
		t.Errorf("GracePeriod = %q after round-trip, want %q", decoded.GracePeriod, "30s")
	}
}

func TestPipelinePublishOmitsEmptyStateKey(t *testing.T) {
	publish := PipelinePublish{
		EventType: "m.bureau.workspace",
		Room:      "${WORKSPACE_ROOM_ID}",
		Content:   map[string]any{"status": "ready"},
	}

	data, err := json.Marshal(publish)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "event_type", "m.bureau.workspace")
	assertField(t, raw, "room", "${WORKSPACE_ROOM_ID}")
	if _, exists := raw["state_key"]; exists {
		t.Error("state_key should be omitted when empty")
	}
}

func TestPipelineVariableOmitsEmptyFields(t *testing.T) {
	// A variable with only Required set should omit description and default.
	variable := PipelineVariable{
		Required: true,
	}

	data, err := json.Marshal(variable)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "required", true)
	for _, field := range []string{"description", "default"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty", field)
		}
	}
}

func TestPipelineLogOmittedWhenNil(t *testing.T) {
	pipeline := PipelineContent{
		Steps: []PipelineStep{
			{Name: "test", Run: "echo test"},
		},
	}

	data, err := json.Marshal(pipeline)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["log"]; exists {
		t.Error("log should be omitted when nil")
	}
}

func TestPipelineRoomPowerLevels(t *testing.T) {
	adminUserID := "@bureau-admin:bureau.local"
	levels := PipelineRoomPowerLevels(adminUserID)

	// Admin should have power level 100.
	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if users[adminUserID] != 100 {
		t.Errorf("admin power level = %v, want 100", users[adminUserID])
	}
	if len(users) != 1 {
		t.Errorf("users map should have exactly 1 entry, got %d", len(users))
	}

	// Default user power level should be 0 (other members can read).
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	// Pipeline events require power level 100 (admin-only writes).
	events, ok := levels["events"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}
	if events[EventTypePipeline] != 100 {
		t.Errorf("%s power level = %v, want 100", EventTypePipeline, events[EventTypePipeline])
	}

	// Room metadata events from AdminProtectedEvents should all be PL 100.
	for _, eventType := range []string{
		"m.room.encryption", "m.room.server_acl",
		"m.room.tombstone", "m.space.child",
	} {
		if events[eventType] != 100 {
			t.Errorf("%s power level = %v, want 100", eventType, events[eventType])
		}
	}

	// Default event power level should be 100 (admin-only room).
	if levels["events_default"] != 100 {
		t.Errorf("events_default = %v, want 100", levels["events_default"])
	}

	// Administrative actions all require power level 100.
	for _, field := range []string{"state_default", "ban", "kick", "invite", "redact"} {
		if levels[field] != 100 {
			t.Errorf("%s = %v, want 100", field, levels[field])
		}
	}

	// Invite is 100 (unlike config/workspace rooms where machine can
	// invite at PL 50 — pipeline rooms have no machine tier).
	if levels["invite"] != 100 {
		t.Errorf("invite = %v, want 100", levels["invite"])
	}
}

// --- PipelineResultContent tests ---

func validPipelineResultContent() PipelineResultContent {
	return PipelineResultContent{
		Version:     1,
		PipelineRef: "dev-workspace-init",
		Conclusion:  "success",
		StartedAt:   "2026-02-12T10:00:00Z",
		CompletedAt: "2026-02-12T10:01:30Z",
		DurationMS:  90000,
		StepCount:   3,
		StepResults: []PipelineStepResult{
			{Name: "clone-repo", Status: "ok", DurationMS: 30000},
			{Name: "install-deps", Status: "ok", DurationMS: 45000},
			{Name: "publish-ready", Status: "ok", DurationMS: 200},
		},
		LogEventID: "$abc123:bureau.local",
	}
}

func TestPipelineResultContentRoundTrip(t *testing.T) {
	original := validPipelineResultContent()
	original.Extra = map[string]json.RawMessage{
		"trigger_event": json.RawMessage(`"$trigger123:bureau.local"`),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify JSON field names match the wire format.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}

	requiredFields := []string{
		"version", "pipeline_ref", "conclusion",
		"started_at", "completed_at", "duration_ms",
		"step_count", "step_results", "log_event_id", "extra",
	}
	for _, field := range requiredFields {
		if _, exists := raw[field]; !exists {
			t.Errorf("JSON missing field %q", field)
		}
	}

	// Verify step result wire format.
	stepResults, ok := raw["step_results"].([]any)
	if !ok {
		t.Fatal("step_results is not an array")
	}
	if len(stepResults) != 3 {
		t.Fatalf("step_results length = %d, want 3", len(stepResults))
	}
	firstStep, ok := stepResults[0].(map[string]any)
	if !ok {
		t.Fatal("step_results[0] is not an object")
	}
	for _, field := range []string{"name", "status", "duration_ms"} {
		if _, exists := firstStep[field]; !exists {
			t.Errorf("step_results[0] missing field %q", field)
		}
	}

	// Round-trip.
	var decoded PipelineResultContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if !reflect.DeepEqual(original, decoded) {
		t.Errorf("round-trip mismatch:\n  original: %+v\n  decoded:  %+v", original, decoded)
	}
}

func TestPipelineResultContentOmitsEmptyOptionals(t *testing.T) {
	content := PipelineResultContent{
		Version:     1,
		PipelineRef: "dev-init",
		Conclusion:  "success",
		StartedAt:   "2026-02-12T10:00:00Z",
		CompletedAt: "2026-02-12T10:01:00Z",
		DurationMS:  60000,
		StepCount:   1,
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Optional fields with zero values should be omitted.
	for _, field := range []string{"step_results", "failed_step", "error_message", "log_event_id", "extra"} {
		if _, exists := raw[field]; exists {
			t.Errorf("expected field %q to be omitted, but it is present", field)
		}
	}
}

func TestPipelineResultContentFailure(t *testing.T) {
	content := PipelineResultContent{
		Version:     1,
		PipelineRef: "ci-pipeline",
		Conclusion:  "failure",
		StartedAt:   "2026-02-12T10:00:00Z",
		CompletedAt: "2026-02-12T10:00:45Z",
		DurationMS:  45000,
		StepCount:   3,
		StepResults: []PipelineStepResult{
			{Name: "build", Status: "ok", DurationMS: 30000},
			{Name: "test", Status: "failed", DurationMS: 15000, Error: "exit code 1"},
		},
		FailedStep:   "test",
		ErrorMessage: "exit code 1",
		LogEventID:   "$log:bureau.local",
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if raw["failed_step"] != "test" {
		t.Errorf("failed_step = %v, want %q", raw["failed_step"], "test")
	}
	if raw["error_message"] != "exit code 1" {
		t.Errorf("error_message = %v, want %q", raw["error_message"], "exit code 1")
	}

	var decoded PipelineResultContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.FailedStep != "test" {
		t.Errorf("decoded.FailedStep = %q, want %q", decoded.FailedStep, "test")
	}
}

func TestPipelineResultContentExtraRoundTrip(t *testing.T) {
	content := validPipelineResultContent()
	content.Extra = map[string]json.RawMessage{
		"custom_metric": json.RawMessage(`42`),
		"build_info":    json.RawMessage(`{"commit":"abc123"}`),
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded PipelineResultContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if string(decoded.Extra["custom_metric"]) != "42" {
		t.Errorf("Extra[custom_metric] = %s, want 42", decoded.Extra["custom_metric"])
	}
	if string(decoded.Extra["build_info"]) != `{"commit":"abc123"}` {
		t.Errorf("Extra[build_info] = %s, want {\"commit\":\"abc123\"}", decoded.Extra["build_info"])
	}
}

func TestPipelineResultContentForwardCompatibility(t *testing.T) {
	// Simulate a newer version with unknown fields. Readers should
	// successfully unmarshal and access known fields without error.
	futureJSON := `{
		"version": 2,
		"pipeline_ref": "dev-init",
		"conclusion": "success",
		"started_at": "2026-02-12T10:00:00Z",
		"completed_at": "2026-02-12T10:01:00Z",
		"duration_ms": 60000,
		"step_count": 1,
		"future_field": "something new",
		"extra": {"custom": true}
	}`

	var content PipelineResultContent
	if err := json.Unmarshal([]byte(futureJSON), &content); err != nil {
		t.Fatalf("Unmarshal future content: %v", err)
	}

	// Known fields are correctly populated.
	if content.Version != 2 {
		t.Errorf("Version = %d, want 2", content.Version)
	}
	if content.PipelineRef != "dev-init" {
		t.Errorf("PipelineRef = %q, want %q", content.PipelineRef, "dev-init")
	}
	if content.Conclusion != "success" {
		t.Errorf("Conclusion = %q, want %q", content.Conclusion, "success")
	}

	// CanModify should refuse modification (version > current).
	if err := content.CanModify(); err == nil {
		t.Error("CanModify() = nil for future version, want error")
	}
}

func TestPipelineResultContentValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*PipelineResultContent)
		wantErr string
	}{
		{
			name:    "valid",
			modify:  func(p *PipelineResultContent) {},
			wantErr: "",
		},
		{
			name:    "version_zero",
			modify:  func(p *PipelineResultContent) { p.Version = 0 },
			wantErr: "version must be >= 1",
		},
		{
			name:    "version_negative",
			modify:  func(p *PipelineResultContent) { p.Version = -1 },
			wantErr: "version must be >= 1",
		},
		{
			name:    "pipeline_ref_empty",
			modify:  func(p *PipelineResultContent) { p.PipelineRef = "" },
			wantErr: "pipeline_ref is required",
		},
		{
			name:    "conclusion_empty",
			modify:  func(p *PipelineResultContent) { p.Conclusion = "" },
			wantErr: "conclusion is required",
		},
		{
			name:    "conclusion_unknown",
			modify:  func(p *PipelineResultContent) { p.Conclusion = "cancelled" },
			wantErr: `unknown conclusion "cancelled"`,
		},
		{
			name:    "conclusion_success",
			modify:  func(p *PipelineResultContent) { p.Conclusion = "success" },
			wantErr: "",
		},
		{
			name:    "conclusion_failure",
			modify:  func(p *PipelineResultContent) { p.Conclusion = "failure" },
			wantErr: "",
		},
		{
			name:    "conclusion_aborted",
			modify:  func(p *PipelineResultContent) { p.Conclusion = "aborted" },
			wantErr: "",
		},
		{
			name:    "started_at_empty",
			modify:  func(p *PipelineResultContent) { p.StartedAt = "" },
			wantErr: "started_at is required",
		},
		{
			name:    "completed_at_empty",
			modify:  func(p *PipelineResultContent) { p.CompletedAt = "" },
			wantErr: "completed_at is required",
		},
		{
			name:    "step_count_zero",
			modify:  func(p *PipelineResultContent) { p.StepCount = 0 },
			wantErr: "step_count must be >= 1",
		},
		{
			name: "step_result_invalid",
			modify: func(p *PipelineResultContent) {
				p.StepResults = []PipelineStepResult{
					{Name: "", Status: "ok"},
				}
			},
			wantErr: "step_results[0]: step result: name is required",
		},
		{
			name: "step_result_invalid_status",
			modify: func(p *PipelineResultContent) {
				p.StepResults = []PipelineStepResult{
					{Name: "build", Status: "unknown"},
				}
			},
			wantErr: `step_results[0]: step result: unknown status "unknown"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := validPipelineResultContent()
			test.modify(&content)
			err := content.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestPipelineResultContentCanModify(t *testing.T) {
	tests := []struct {
		name    string
		version int
		wantErr bool
	}{
		{"current_version", PipelineResultContentVersion, false},
		{"older_version", PipelineResultContentVersion - 1, false},
		{"newer_version", PipelineResultContentVersion + 1, true},
		{"far_future_version", PipelineResultContentVersion + 100, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := validPipelineResultContent()
			content.Version = test.version
			err := content.CanModify()
			if test.wantErr {
				if err == nil {
					t.Fatal("CanModify() = nil, want error")
				}
				message := err.Error()
				if !strings.Contains(message, "upgrade") {
					t.Errorf("error should mention upgrade: %q", message)
				}
			} else {
				if err != nil {
					t.Errorf("CanModify() = %v, want nil", err)
				}
			}
		})
	}
}

func TestPipelineStepResultValidate(t *testing.T) {
	tests := []struct {
		name    string
		step    PipelineStepResult
		wantErr string
	}{
		{
			name:    "valid_ok",
			step:    PipelineStepResult{Name: "build", Status: "ok", DurationMS: 1000},
			wantErr: "",
		},
		{
			name:    "valid_failed",
			step:    PipelineStepResult{Name: "test", Status: "failed", DurationMS: 500, Error: "exit 1"},
			wantErr: "",
		},
		{
			name:    "valid_failed_optional",
			step:    PipelineStepResult{Name: "lint", Status: "failed (optional)", DurationMS: 200},
			wantErr: "",
		},
		{
			name:    "valid_skipped",
			step:    PipelineStepResult{Name: "deploy", Status: "skipped"},
			wantErr: "",
		},
		{
			name:    "valid_aborted",
			step:    PipelineStepResult{Name: "check", Status: "aborted"},
			wantErr: "",
		},
		{
			name:    "name_empty",
			step:    PipelineStepResult{Name: "", Status: "ok"},
			wantErr: "name is required",
		},
		{
			name:    "status_empty",
			step:    PipelineStepResult{Name: "build", Status: ""},
			wantErr: "status is required",
		},
		{
			name:    "status_unknown",
			step:    PipelineStepResult{Name: "build", Status: "timeout"},
			wantErr: `unknown status "timeout"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.step.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestPipelineAssertStateInStep(t *testing.T) {
	content := PipelineContent{
		Steps: []PipelineStep{
			{
				Name: "verify-teardown",
				AssertState: &PipelineAssertState{
					Room:       "!room:bureau.local",
					EventType:  "m.bureau.workspace",
					Field:      "status",
					Equals:     "teardown",
					OnMismatch: "abort",
					Message:    "workspace is no longer in teardown",
				},
			},
			{
				Name: "assert-not-removing",
				AssertState: &PipelineAssertState{
					Room:      "!room:bureau.local",
					EventType: "m.bureau.worktree",
					StateKey:  "feature/amdgpu",
					Field:     "status",
					NotIn:     []string{"removing", "archived", "removed"},
				},
			},
		},
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded PipelineContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Steps) != 2 {
		t.Fatalf("Steps count = %d, want 2", len(decoded.Steps))
	}

	step0 := decoded.Steps[0]
	if step0.AssertState == nil {
		t.Fatal("Steps[0].AssertState is nil")
	}
	if step0.AssertState.Equals != "teardown" {
		t.Errorf("AssertState.Equals = %q, want %q", step0.AssertState.Equals, "teardown")
	}
	if step0.AssertState.OnMismatch != "abort" {
		t.Errorf("AssertState.OnMismatch = %q, want %q", step0.AssertState.OnMismatch, "abort")
	}

	step1 := decoded.Steps[1]
	if step1.AssertState == nil {
		t.Fatal("Steps[1].AssertState is nil")
	}
	if step1.AssertState.StateKey != "feature/amdgpu" {
		t.Errorf("AssertState.StateKey = %q, want %q", step1.AssertState.StateKey, "feature/amdgpu")
	}
	if len(step1.AssertState.NotIn) != 3 {
		t.Fatalf("AssertState.NotIn length = %d, want 3", len(step1.AssertState.NotIn))
	}
}

func TestPipelineOnFailure(t *testing.T) {
	content := PipelineContent{
		Steps: []PipelineStep{
			{Name: "do-work", Run: "echo hello"},
		},
		OnFailure: []PipelineStep{
			{
				Name: "publish-failed",
				Publish: &PipelinePublish{
					EventType: "m.bureau.worktree",
					Room:      "!room:bureau.local",
					StateKey:  "feature/amdgpu",
					Content:   map[string]any{"status": "failed"},
				},
			},
		},
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded PipelineContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.OnFailure) != 1 {
		t.Fatalf("OnFailure count = %d, want 1", len(decoded.OnFailure))
	}
	if decoded.OnFailure[0].Name != "publish-failed" {
		t.Errorf("OnFailure[0].Name = %q, want %q", decoded.OnFailure[0].Name, "publish-failed")
	}
	if decoded.OnFailure[0].Publish == nil {
		t.Fatal("OnFailure[0].Publish is nil")
	}
	if decoded.OnFailure[0].Publish.EventType != "m.bureau.worktree" {
		t.Errorf("OnFailure[0].Publish.EventType = %q, want %q",
			decoded.OnFailure[0].Publish.EventType, "m.bureau.worktree")
	}
}
