// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	pipelineschema "github.com/bureau-foundation/bureau/lib/schema/pipeline"
)

func TestPushPipeline(t *testing.T) {

	state := newPipelineTestState()
	state.addPipelineRoom("#bureau/pipeline:test.local", "!pipeline:test", nil)
	startTestServer(t, state)

	path := writePipelineFile(t, `{
  "description": "Deploy pipeline",
  "steps": [
    {"name": "build", "run": "make build"},
    {"name": "deploy", "run": "make deploy"}
  ]
}`)

	cmd := pushCommand()
	if err := cmd.FlagSet().Parse([]string{"--server-name", "test.local"}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	if err := cmd.Run(context.Background(), []string{"bureau/pipeline:deploy", path}, testLogger()); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Verify the state event was sent.
	state.mu.Lock()
	defer state.mu.Unlock()

	if len(state.sentStateEvents) != 1 {
		t.Fatalf("sentStateEvents count = %d, want 1", len(state.sentStateEvents))
	}
	event := state.sentStateEvents[0]
	if event.RoomID != "!pipeline:test" {
		t.Errorf("RoomID = %q, want %q", event.RoomID, "!pipeline:test")
	}
	if event.Type != string(schema.EventTypePipeline) {
		t.Errorf("Type = %q, want %q", event.Type, schema.EventTypePipeline)
	}
	if event.StateKey != "deploy" {
		t.Errorf("StateKey = %q, want %q", event.StateKey, "deploy")
	}

	// Verify the body is valid PipelineContent.
	var content pipelineschema.PipelineContent
	if err := json.Unmarshal(event.Body, &content); err != nil {
		t.Fatalf("unmarshal sent body: %v", err)
	}
	if content.Description != "Deploy pipeline" {
		t.Errorf("Description = %q, want %q", content.Description, "Deploy pipeline")
	}
	if len(content.Steps) != 2 {
		t.Errorf("Steps count = %d, want 2", len(content.Steps))
	}
}

func TestPushPipelineDryRun(t *testing.T) {

	state := newPipelineTestState()
	state.addPipelineRoom("#bureau/pipeline:test.local", "!pipeline:test", nil)
	startTestServer(t, state)

	path := writePipelineFile(t, `{
  "description": "Test pipeline",
  "steps": [{"name": "test", "run": "echo test"}]
}`)

	cmd := pushCommand()
	if err := cmd.FlagSet().Parse([]string{"--server-name", "test.local", "--dry-run"}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	if err := cmd.Run(context.Background(), []string{"bureau/pipeline:test", path}, testLogger()); err != nil {
		t.Fatalf("push --dry-run: %v", err)
	}

	// Verify no state event was sent.
	state.mu.Lock()
	defer state.mu.Unlock()

	if len(state.sentStateEvents) != 0 {
		t.Errorf("sentStateEvents count = %d, want 0 (dry-run should not publish)", len(state.sentStateEvents))
	}
}

func TestPushPipelineValidationFails(t *testing.T) {
	t.Parallel()

	// No Matrix server needed — validation fails before connection.
	path := writePipelineFile(t, `{
  "description": "Invalid: no steps"
}`)

	cmd := pushCommand()
	err := cmd.Run(context.Background(), []string{"bureau/pipeline:bad", path}, testLogger())
	if err == nil {
		t.Fatal("expected error for pipeline with no steps")
	}
	if !strings.Contains(err.Error(), "validation issue") {
		t.Errorf("error %q should mention validation issues", err.Error())
	}
}

func TestPushPipelineBadRef(t *testing.T) {
	t.Parallel()

	path := writePipelineFile(t, `{
  "steps": [{"name": "x", "run": "echo"}]
}`)

	cmd := pushCommand()
	err := cmd.Run(context.Background(), []string{"no-colon-here", path}, testLogger())
	if err == nil {
		t.Fatal("expected error for bad pipeline reference")
	}
	if !strings.Contains(err.Error(), "invalid pipeline reference") {
		t.Errorf("error %q should mention invalid pipeline reference", err.Error())
	}
}

func TestPushPipelineNonexistentFile(t *testing.T) {
	t.Parallel()

	cmd := pushCommand()
	err := cmd.Run(context.Background(), []string{"bureau/pipeline:test", "/nonexistent/file.jsonc"}, testLogger())
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestPushPipelineRoomNotFound(t *testing.T) {

	state := newPipelineTestState()
	// No rooms registered — alias resolution will fail.
	startTestServer(t, state)

	path := writePipelineFile(t, `{
  "description": "Valid pipeline",
  "steps": [{"name": "test", "run": "echo test"}]
}`)

	cmd := pushCommand()
	if err := cmd.FlagSet().Parse([]string{"--server-name", "test.local"}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run(context.Background(), []string{"bureau/pipeline:test", path}, testLogger())
	if err == nil {
		t.Fatal("expected error for nonexistent room")
	}
	if !strings.Contains(err.Error(), "resolving target room") {
		t.Errorf("error %q should mention room resolution", err.Error())
	}
}

func TestPushPipelineNoArgs(t *testing.T) {
	t.Parallel()

	cmd := pushCommand()
	err := cmd.Run(context.Background(), []string{}, testLogger())
	if err == nil {
		t.Fatal("expected error for no args")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q should contain usage hint", err.Error())
	}
}
