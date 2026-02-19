// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"reflect"
	"testing"
)

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

	// Optional error/pipeline fields should be omitted. Note: duration_ms
	// is always present (0 means sub-millisecond), so it is not in this list.
	for _, field := range []string{"error", "exit_code", "steps", "log_event_id", "principal"} {
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
