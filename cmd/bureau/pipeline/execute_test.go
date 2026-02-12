// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestExecutePipeline(t *testing.T) {

	state := newPipelineTestState()
	// Register the config room that the execute command resolves.
	state.mu.Lock()
	state.roomAliases["#bureau/config/machine/workstation:test.local"] = "!config-ws:test"
	state.mu.Unlock()
	startTestServer(t, state)

	cmd := executeCommand()
	if err := cmd.Flags().Parse([]string{
		"--machine", "machine/workstation",
		"--param", "PROJECT=iree",
		"--param", "BRANCH=main",
		"--server-name", "test.local",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	if err := cmd.Run([]string{"bureau/pipeline:dev-workspace-init"}); err != nil {
		t.Fatalf("execute: %v", err)
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
	if project, ok := command.Parameters["PROJECT"].(string); !ok || project != "iree" {
		t.Errorf("Parameters[PROJECT] = %v, want %q", command.Parameters["PROJECT"], "iree")
	}
	if branch, ok := command.Parameters["BRANCH"].(string); !ok || branch != "main" {
		t.Errorf("Parameters[BRANCH] = %v, want %q", command.Parameters["BRANCH"], "main")
	}
}

func TestExecuteMissingMachine(t *testing.T) {
	t.Parallel()

	cmd := executeCommand()
	err := cmd.Run([]string{"bureau/pipeline:test"})
	if err == nil {
		t.Fatal("expected error for missing --machine")
	}
	if !strings.Contains(err.Error(), "--machine is required") {
		t.Errorf("error %q should mention --machine", err.Error())
	}
}

func TestExecuteBadPipelineRef(t *testing.T) {
	t.Parallel()

	cmd := executeCommand()
	if err := cmd.Flags().Parse([]string{"--machine", "machine/workstation"}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run([]string{"no-colon-here"})
	if err == nil {
		t.Fatal("expected error for bad pipeline reference")
	}
	if !strings.Contains(err.Error(), "parsing pipeline reference") {
		t.Errorf("error %q should mention parsing", err.Error())
	}
}

func TestExecuteBadParamFormat(t *testing.T) {

	state := newPipelineTestState()
	state.mu.Lock()
	state.roomAliases["#bureau/config/machine/workstation:test.local"] = "!config-ws:test"
	state.mu.Unlock()
	startTestServer(t, state)

	cmd := executeCommand()
	if err := cmd.Flags().Parse([]string{
		"--machine", "machine/workstation",
		"--param", "no-equals-sign",
		"--server-name", "test.local",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run([]string{"bureau/pipeline:test"})
	if err == nil {
		t.Fatal("expected error for bad param format")
	}
	if !strings.Contains(err.Error(), "expected key=value") {
		t.Errorf("error %q should mention key=value format", err.Error())
	}
}

func TestExecuteEmptyParamKey(t *testing.T) {

	state := newPipelineTestState()
	state.mu.Lock()
	state.roomAliases["#bureau/config/machine/workstation:test.local"] = "!config-ws:test"
	state.mu.Unlock()
	startTestServer(t, state)

	cmd := executeCommand()
	if err := cmd.Flags().Parse([]string{
		"--machine", "machine/workstation",
		"--param", "=value-without-key",
		"--server-name", "test.local",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run([]string{"bureau/pipeline:test"})
	if err == nil {
		t.Fatal("expected error for empty param key")
	}
	if !strings.Contains(err.Error(), "empty key") {
		t.Errorf("error %q should mention empty key", err.Error())
	}
}

func TestExecuteInvalidMachineName(t *testing.T) {
	t.Parallel()

	cmd := executeCommand()
	if err := cmd.Flags().Parse([]string{"--machine", "../escape/attempt"}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run([]string{"bureau/pipeline:test"})
	if err == nil {
		t.Fatal("expected error for invalid machine name")
	}
	if !strings.Contains(err.Error(), "invalid machine name") {
		t.Errorf("error %q should mention invalid machine name", err.Error())
	}
}

func TestExecuteConfigRoomNotFound(t *testing.T) {

	state := newPipelineTestState()
	// No rooms registered — config room resolution will fail.
	startTestServer(t, state)

	cmd := executeCommand()
	if err := cmd.Flags().Parse([]string{
		"--machine", "machine/workstation",
		"--server-name", "test.local",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run([]string{"bureau/pipeline:test"})
	if err == nil {
		t.Fatal("expected error for nonexistent config room")
	}
	if !strings.Contains(err.Error(), "resolving config room") {
		t.Errorf("error %q should mention config room resolution", err.Error())
	}
}

func TestExecuteNoArgs(t *testing.T) {
	t.Parallel()

	cmd := executeCommand()
	err := cmd.Run([]string{})
	if err == nil {
		t.Fatal("expected error for no args")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q should contain usage hint", err.Error())
	}
}
