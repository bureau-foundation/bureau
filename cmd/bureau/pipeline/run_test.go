// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestRunPipeline(t *testing.T) {
	state := newPipelineTestState()
	state.mu.Lock()
	state.roomAliases["#bureau/fleet/prod/machine/workstation:test.local"] = "!config-ws:test"
	// Auto-generate an accepted command_result for every command.
	state.autoCommandResult = func(requestID, roomID string) map[string]any {
		return map[string]any{
			"status":    "accepted",
			"ticket_id": "pip-test1234",
			"room":      "!project:test",
		}
	}
	state.mu.Unlock()
	startTestServer(t, state)

	cmd := runCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--machine", "bureau/fleet/prod/machine/workstation",
		"--room", "!project:test",
		"--param", "PROJECT=iree",
		"--param", "BRANCH=main",
		"--server-name", "test.local",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	if err := cmd.Run(context.Background(), []string{"bureau/pipeline:dev-workspace-init"}, testLogger()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Verify the command message was sent to the config room.
	state.mu.Lock()
	defer state.mu.Unlock()

	if len(state.sentEvents) != 1 {
		t.Fatalf("sentEvents count = %d, want 1", len(state.sentEvents))
	}
	event := state.sentEvents[0]
	if event.RoomID != "!config-ws:test" {
		t.Errorf("RoomID = %q, want %q", event.RoomID, "!config-ws:test")
	}
	if event.Type != "m.room.message" {
		t.Errorf("Type = %q, want %q", event.Type, "m.room.message")
	}

	// Decode the command message and verify its fields.
	var command schema.CommandMessage
	if err := json.Unmarshal(event.Body, &command); err != nil {
		t.Fatalf("unmarshal command: %v", err)
	}
	if command.MsgType != schema.MsgTypeCommand {
		t.Errorf("MsgType = %q, want %q", command.MsgType, schema.MsgTypeCommand)
	}
	if command.Command != "pipeline.execute" {
		t.Errorf("Command = %q, want %q", command.Command, "pipeline.execute")
	}
	if len(command.RequestID) != 32 {
		t.Errorf("RequestID length = %d, want 32", len(command.RequestID))
	}
	if pipeline, ok := command.Parameters["pipeline"].(string); !ok || pipeline != "bureau/pipeline:dev-workspace-init" {
		t.Errorf("Parameters[pipeline] = %v, want %q", command.Parameters["pipeline"], "bureau/pipeline:dev-workspace-init")
	}
	if room, ok := command.Parameters["room"].(string); !ok || room != "!project:test" {
		t.Errorf("Parameters[room] = %v, want %q", command.Parameters["room"], "!project:test")
	}
	if project, ok := command.Parameters["PROJECT"].(string); !ok || project != "iree" {
		t.Errorf("Parameters[PROJECT] = %v, want %q", command.Parameters["PROJECT"], "iree")
	}
	if branch, ok := command.Parameters["BRANCH"].(string); !ok || branch != "main" {
		t.Errorf("Parameters[BRANCH] = %v, want %q", command.Parameters["BRANCH"], "main")
	}
}

func TestRunMissingMachine(t *testing.T) {
	t.Parallel()

	cmd := runCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--room", "!project:test",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run(context.Background(), []string{"bureau/pipeline:test"}, testLogger())
	if err == nil {
		t.Fatal("expected error for missing --machine")
	}
	if !strings.Contains(err.Error(), "--machine is required") {
		t.Errorf("error %q should mention --machine", err.Error())
	}
}

func TestRunMissingRoom(t *testing.T) {
	t.Parallel()

	cmd := runCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--machine", "bureau/fleet/prod/machine/workstation",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run(context.Background(), []string{"bureau/pipeline:test"}, testLogger())
	if err == nil {
		t.Fatal("expected error for missing --room")
	}
	if !strings.Contains(err.Error(), "--room is required") {
		t.Errorf("error %q should mention --room", err.Error())
	}
}

func TestRunBadPipelineRef(t *testing.T) {
	t.Parallel()

	cmd := runCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--machine", "machine/workstation",
		"--room", "!project:test",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run(context.Background(), []string{"no-colon-here"}, testLogger())
	if err == nil {
		t.Fatal("expected error for bad pipeline reference")
	}
	if !strings.Contains(err.Error(), "invalid pipeline reference") {
		t.Errorf("error %q should mention invalid pipeline reference", err.Error())
	}
}

func TestRunBadParamFormat(t *testing.T) {
	state := newPipelineTestState()
	state.mu.Lock()
	state.roomAliases["#bureau/fleet/prod/machine/workstation:test.local"] = "!config-ws:test"
	state.mu.Unlock()
	startTestServer(t, state)

	cmd := runCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--machine", "bureau/fleet/prod/machine/workstation",
		"--room", "!project:test",
		"--param", "no-equals-sign",
		"--server-name", "test.local",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run(context.Background(), []string{"bureau/pipeline:test"}, testLogger())
	if err == nil {
		t.Fatal("expected error for bad param format")
	}
	if !strings.Contains(err.Error(), "expected key=value") {
		t.Errorf("error %q should mention key=value format", err.Error())
	}
}

func TestRunEmptyParamKey(t *testing.T) {
	state := newPipelineTestState()
	state.mu.Lock()
	state.roomAliases["#bureau/fleet/prod/machine/workstation:test.local"] = "!config-ws:test"
	state.mu.Unlock()
	startTestServer(t, state)

	cmd := runCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--machine", "bureau/fleet/prod/machine/workstation",
		"--room", "!project:test",
		"--param", "=value-without-key",
		"--server-name", "test.local",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run(context.Background(), []string{"bureau/pipeline:test"}, testLogger())
	if err == nil {
		t.Fatal("expected error for empty param key")
	}
	if !strings.Contains(err.Error(), "empty key") {
		t.Errorf("error %q should mention empty key", err.Error())
	}
}

func TestRunInvalidMachineName(t *testing.T) {
	t.Parallel()

	cmd := runCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--machine", "../escape/attempt",
		"--room", "!project:test",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run(context.Background(), []string{"bureau/pipeline:test"}, testLogger())
	if err == nil {
		t.Fatal("expected error for invalid machine name")
	}
	if !strings.Contains(err.Error(), "invalid machine name") {
		t.Errorf("error %q should mention invalid machine name", err.Error())
	}
}

func TestRunInvalidRoomID(t *testing.T) {
	t.Parallel()

	cmd := runCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--machine", "bureau/fleet/prod/machine/workstation",
		"--room", "not-a-room-id",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run(context.Background(), []string{"bureau/pipeline:test"}, testLogger())
	if err == nil {
		t.Fatal("expected error for invalid room ID")
	}
	if !strings.Contains(err.Error(), "invalid --room") {
		t.Errorf("error %q should mention invalid --room", err.Error())
	}
}

func TestRunConfigRoomNotFound(t *testing.T) {
	state := newPipelineTestState()
	// No rooms registered — config room resolution will fail.
	startTestServer(t, state)

	cmd := runCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--machine", "bureau/fleet/prod/machine/workstation",
		"--room", "!project:test",
		"--server-name", "test.local",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run(context.Background(), []string{"bureau/pipeline:test"}, testLogger())
	if err == nil {
		t.Fatal("expected error for nonexistent config room")
	}
	if !strings.Contains(err.Error(), "resolving config room") {
		t.Errorf("error %q should mention config room resolution", err.Error())
	}
}

func TestRunNoArgs(t *testing.T) {
	t.Parallel()

	cmd := runCommand()
	if err := cmd.FlagSet().Parse([]string{
		"--machine", "bureau/fleet/prod/machine/workstation",
		"--room", "!project:test",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run(context.Background(), []string{}, testLogger())
	if err == nil {
		t.Fatal("expected error for no args")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q should contain usage hint", err.Error())
	}
}
