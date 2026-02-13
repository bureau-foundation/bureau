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
		{"machine_info", EventTypeMachineInfo, "m.bureau.machine_info"},
		{"machine_status", EventTypeMachineStatus, "m.bureau.machine_status"},
		{"machine_config", EventTypeMachineConfig, "m.bureau.machine_config"},
		{"credentials", EventTypeCredentials, "m.bureau.credentials"},
		{"service", EventTypeService, "m.bureau.service"},
		{"layout", EventTypeLayout, "m.bureau.layout"},
		{"template", EventTypeTemplate, "m.bureau.template"},
		{"project", EventTypeProject, "m.bureau.project"},
		{"workspace", EventTypeWorkspace, "m.bureau.workspace"},
		{"pipeline", EventTypePipeline, "m.bureau.pipeline"},
		{"pipeline_result", EventTypePipelineResult, "m.bureau.pipeline_result"},
		{"room_service", EventTypeRoomService, "m.bureau.room_service"},
		{"ticket", EventTypeTicket, "m.bureau.ticket"},
		{"ticket_config", EventTypeTicketConfig, "m.bureau.ticket_config"},
		{"artifact_scope", EventTypeArtifactScope, "m.bureau.artifact_scope"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.constant != test.want {
				t.Errorf("%s = %q, want %q", test.name, test.constant, test.want)
			}
		})
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
						ObserveMembers: &LayoutMemberFilter{Labels: map[string]string{"role": "agent"}},
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
	labels, ok := observeMembers["labels"].(map[string]any)
	if !ok {
		t.Fatal("observe_members.labels field missing or wrong type")
	}
	assertField(t, labels, "role", "agent")

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
	if decodedPane.ObserveMembers.Labels["role"] != "agent" {
		t.Errorf("ObserveMembers.Labels[role]: got %q, want %q", decodedPane.ObserveMembers.Labels["role"], "agent")
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

func TestMsgTypeConstants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		constant string
		want     string
	}{
		{"command", MsgTypeCommand, "m.bureau.command"},
		{"command_result", MsgTypeCommandResult, "m.bureau.command_result"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.constant != test.want {
				t.Errorf("%s = %q, want %q", test.name, test.constant, test.want)
			}
		})
	}
}

func TestMatrixEventTypeConstants(t *testing.T) {
	t.Parallel()
	// These are standard Matrix spec event type strings. They are wire-format
	// identifiers defined by the Matrix protocol and must never change.
	tests := []struct {
		name     string
		constant string
		want     string
	}{
		{"message", MatrixEventTypeMessage, "m.room.message"},
		{"power_levels", MatrixEventTypePowerLevels, "m.room.power_levels"},
		{"join_rules", MatrixEventTypeJoinRules, "m.room.join_rules"},
		{"name", MatrixEventTypeName, "m.room.name"},
		{"topic", MatrixEventTypeTopic, "m.room.topic"},
		{"space_child", MatrixEventTypeSpaceChild, "m.space.child"},
		{"canonical_alias", MatrixEventTypeCanonicalAlias, "m.room.canonical_alias"},
		{"encryption", MatrixEventTypeEncryption, "m.room.encryption"},
		{"server_acl", MatrixEventTypeServerACL, "m.room.server_acl"},
		{"tombstone", MatrixEventTypeTombstone, "m.room.tombstone"},
		{"avatar", MatrixEventTypeAvatar, "m.room.avatar"},
		{"history_visibility", MatrixEventTypeHistoryVisibility, "m.room.history_visibility"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.constant != test.want {
				t.Errorf("%s = %q, want %q", test.name, test.constant, test.want)
			}
		})
	}
}

func TestPowerLevelConstants(t *testing.T) {
	t.Parallel()
	if PowerLevelReadOnly != 0 {
		t.Errorf("PowerLevelReadOnly = %d, want 0", PowerLevelReadOnly)
	}
	if PowerLevelOperator != 50 {
		t.Errorf("PowerLevelOperator = %d, want 50", PowerLevelOperator)
	}
	if PowerLevelAdmin != 100 {
		t.Errorf("PowerLevelAdmin = %d, want 100", PowerLevelAdmin)
	}
}

func TestCommandMessageRoundTrip(t *testing.T) {
	t.Parallel()
	original := CommandMessage{
		MsgType:       MsgTypeCommand,
		Body:          "workspace status iree/amdgpu/inference",
		Command:       "workspace.status",
		Workspace:     "iree/amdgpu/inference",
		RequestID:     "req-a7f3",
		SenderMachine: "machine/workstation",
		Parameters: map[string]any{
			"verbose": true,
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify JSON field names match the wire format (snake_case).
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypeCommand)
	assertField(t, raw, "body", "workspace status iree/amdgpu/inference")
	assertField(t, raw, "command", "workspace.status")
	assertField(t, raw, "workspace", "iree/amdgpu/inference")
	assertField(t, raw, "request_id", "req-a7f3")
	assertField(t, raw, "sender_machine", "machine/workstation")

	// Verify parameters round-trip.
	params, ok := raw["parameters"].(map[string]any)
	if !ok {
		t.Fatal("parameters missing or wrong type")
	}
	assertField(t, params, "verbose", true)

	var decoded CommandMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.MsgType != original.MsgType ||
		decoded.Body != original.Body ||
		decoded.Command != original.Command ||
		decoded.Workspace != original.Workspace ||
		decoded.RequestID != original.RequestID ||
		decoded.SenderMachine != original.SenderMachine {
		t.Errorf("round-trip string field mismatch: got %+v", decoded)
	}
	if len(decoded.Parameters) != len(original.Parameters) {
		t.Errorf("parameters length = %d, want %d", len(decoded.Parameters), len(original.Parameters))
	}
}

func TestCommandMessageMinimal(t *testing.T) {
	t.Parallel()
	original := CommandMessage{
		MsgType: MsgTypeCommand,
		Body:    "workspace list",
		Command: "workspace.list",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Required fields present.
	assertField(t, raw, "msgtype", MsgTypeCommand)
	assertField(t, raw, "command", "workspace.list")

	// Optional fields omitted from JSON.
	for _, field := range []string{"workspace", "request_id", "sender_machine", "parameters"} {
		if _, exists := raw[field]; exists {
			t.Errorf("field %q should be omitted from minimal CommandMessage", field)
		}
	}

	var decoded CommandMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestCommandResultMessageSuccessRoundTrip(t *testing.T) {
	t.Parallel()
	resultPayload := json.RawMessage(`{"workspace":"iree/amdgpu/inference","exists":true}`)
	original := CommandResultMessage{
		MsgType:   MsgTypeCommandResult,
		Body:      "workspace.status: success",
		Status:    "success",
		Result:    resultPayload,
		RequestID: "req-a7f3",
		RelatesTo: NewThreadRelation("$command-event-id"),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypeCommandResult)
	assertField(t, raw, "status", "success")
	assertField(t, raw, "request_id", "req-a7f3")

	// Optional error/pipeline fields should be omitted.
	for _, field := range []string{"error", "exit_code", "duration_ms", "steps", "log_event_id", "principal"} {
		if _, exists := raw[field]; exists {
			t.Errorf("field %q should be omitted from success result", field)
		}
	}

	// Thread relation present.
	relatesTo, ok := raw["m.relates_to"].(map[string]any)
	if !ok {
		t.Fatal("m.relates_to missing or wrong type")
	}
	assertField(t, relatesTo, "rel_type", "m.thread")
	assertField(t, relatesTo, "event_id", "$command-event-id")

	var decoded CommandResultMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Status != "success" {
		t.Errorf("Status = %q, want %q", decoded.Status, "success")
	}
	if decoded.RequestID != "req-a7f3" {
		t.Errorf("RequestID = %q, want %q", decoded.RequestID, "req-a7f3")
	}
	if string(decoded.Result) != string(resultPayload) {
		t.Errorf("Result = %s, want %s", decoded.Result, resultPayload)
	}
}

func TestCommandResultMessageErrorRoundTrip(t *testing.T) {
	t.Parallel()
	original := CommandResultMessage{
		MsgType:   MsgTypeCommandResult,
		Body:      "workspace.status: error: not found",
		Status:    "error",
		Error:     "workspace not found",
		RequestID: "req-b4c1",
		RelatesTo: NewThreadRelation("$cmd-event"),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "status", "error")
	assertField(t, raw, "error", "workspace not found")

	// Success-only fields should be absent.
	if _, exists := raw["result"]; exists {
		t.Error("result should be omitted from error response")
	}

	var decoded CommandResultMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Error != "workspace not found" {
		t.Errorf("Error = %q, want %q", decoded.Error, "workspace not found")
	}
}

func TestCommandResultMessagePipelineRoundTrip(t *testing.T) {
	t.Parallel()
	exitCode := 0
	original := CommandResultMessage{
		MsgType:    MsgTypeCommandResult,
		Body:       "pipeline.execute: success (exit 0)",
		Status:     "success",
		ExitCode:   &exitCode,
		DurationMS: 12345,
		Steps: []PipelineStepResult{
			{Name: "build", Status: "success", DurationMS: 5000},
			{Name: "test", Status: "success", DurationMS: 7345},
		},
		LogEventID: "$log-thread-root",
		RequestID:  "req-pipe-1",
		RelatesTo:  NewThreadRelation("$pipeline-cmd"),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "exit_code", float64(0))
	assertField(t, raw, "duration_ms", float64(12345))
	assertField(t, raw, "log_event_id", "$log-thread-root")

	steps, ok := raw["steps"].([]any)
	if !ok {
		t.Fatal("steps missing or wrong type")
	}
	if len(steps) != 2 {
		t.Fatalf("steps count = %d, want 2", len(steps))
	}

	var decoded CommandResultMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.ExitCode == nil || *decoded.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", decoded.ExitCode)
	}
	if len(decoded.Steps) != 2 {
		t.Fatalf("Steps count = %d, want 2", len(decoded.Steps))
	}
	if decoded.Steps[0].Name != "build" {
		t.Errorf("Steps[0].Name = %q, want %q", decoded.Steps[0].Name, "build")
	}
}

func TestCommandResultMessageAccepted(t *testing.T) {
	t.Parallel()
	original := CommandResultMessage{
		MsgType:   MsgTypeCommandResult,
		Body:      "pipeline.execute: accepted",
		Status:    "accepted",
		Principal: "pipeline/build-runner",
		RequestID: "req-pipe-1",
		RelatesTo: NewThreadRelation("$pipeline-cmd"),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "status", "accepted")
	assertField(t, raw, "principal", "pipeline/build-runner")

	var decoded CommandResultMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Principal != "pipeline/build-runner" {
		t.Errorf("Principal = %q, want %q", decoded.Principal, "pipeline/build-runner")
	}
}

func TestNewThreadRelation(t *testing.T) {
	t.Parallel()
	relation := NewThreadRelation("$test-event-id")

	if relation.RelType != "m.thread" {
		t.Errorf("RelType = %q, want %q", relation.RelType, "m.thread")
	}
	if relation.EventID != "$test-event-id" {
		t.Errorf("EventID = %q, want %q", relation.EventID, "$test-event-id")
	}
	if !relation.IsFallingBack {
		t.Error("IsFallingBack should be true")
	}
	if relation.InReplyTo == nil {
		t.Fatal("InReplyTo should not be nil")
	}
	if relation.InReplyTo.EventID != "$test-event-id" {
		t.Errorf("InReplyTo.EventID = %q, want %q", relation.InReplyTo.EventID, "$test-event-id")
	}

	// Verify JSON serialization uses the correct Matrix field names.
	data, err := json.Marshal(relation)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	assertField(t, raw, "rel_type", "m.thread")
	assertField(t, raw, "is_falling_back", true)
	inReplyTo, ok := raw["m.in_reply_to"].(map[string]any)
	if !ok {
		t.Fatal("m.in_reply_to missing or wrong type")
	}
	assertField(t, inReplyTo, "event_id", "$test-event-id")
}

func TestAdminProtectedEventsUsesConstants(t *testing.T) {
	t.Parallel()
	events := AdminProtectedEvents()

	// Every entry should map to power level 100.
	expectedTypes := []string{
		MatrixEventTypeAvatar,
		MatrixEventTypeCanonicalAlias,
		MatrixEventTypeEncryption,
		MatrixEventTypeHistoryVisibility,
		MatrixEventTypeJoinRules,
		MatrixEventTypeName,
		MatrixEventTypePowerLevels,
		MatrixEventTypeServerACL,
		MatrixEventTypeTombstone,
		MatrixEventTypeTopic,
		MatrixEventTypeSpaceChild,
	}
	for _, eventType := range expectedTypes {
		powerLevel, exists := events[eventType]
		if !exists {
			t.Errorf("AdminProtectedEvents missing %q", eventType)
			continue
		}
		if powerLevel != 100 {
			t.Errorf("AdminProtectedEvents[%q] = %v, want 100", eventType, powerLevel)
		}
	}
	if len(events) != len(expectedTypes) {
		t.Errorf("AdminProtectedEvents has %d entries, want %d", len(events), len(expectedTypes))
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

func TestVersionConstants(t *testing.T) {
	// Verify version constants are positive. A zero version
	// constant would mean CanModify can never reject anything.
	if ArtifactScopeVersion < 1 {
		t.Errorf("ArtifactScopeVersion = %d, must be >= 1", ArtifactScopeVersion)
	}
	if CredentialsVersion < 1 {
		t.Errorf("CredentialsVersion = %d, must be >= 1", CredentialsVersion)
	}
	if TicketContentVersion < 1 {
		t.Errorf("TicketContentVersion = %d, must be >= 1", TicketContentVersion)
	}
	if TicketConfigVersion < 1 {
		t.Errorf("TicketConfigVersion = %d, must be >= 1", TicketConfigVersion)
	}
}
