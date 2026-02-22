// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

func TestPipelineLocalpart(t *testing.T) {
	t.Parallel()

	d := &Daemon{}

	// First call produces pipeline/1.
	first := d.pipelineLocalpart()
	if first != "pipeline/1" {
		t.Errorf("first pipelineLocalpart() = %q, want %q", first, "pipeline/1")
	}

	// Second call produces pipeline/2 (monotonically increasing).
	second := d.pipelineLocalpart()
	if second != "pipeline/2" {
		t.Errorf("second pipelineLocalpart() = %q, want %q", second, "pipeline/2")
	}

	// Uniqueness: every call produces a different localpart.
	if first == second {
		t.Errorf("pipelineLocalpart should produce unique values, got %q twice", first)
	}
}

func TestWorktreeLocalpart(t *testing.T) {
	t.Parallel()

	d := &Daemon{}

	// Worktree localparts use the same counter as pipelines.
	first := d.worktreeLocalpart()
	if first != "worktree/1" {
		t.Errorf("first worktreeLocalpart() = %q, want %q", first, "worktree/1")
	}

	second := d.worktreeLocalpart()
	if second != "worktree/2" {
		t.Errorf("second worktreeLocalpart() = %q, want %q", second, "worktree/2")
	}
}

func TestEphemeralCounterSharedAcrossTypes(t *testing.T) {
	t.Parallel()

	d := &Daemon{}

	pipeline := d.pipelineLocalpart()
	worktree := d.worktreeLocalpart()
	pipeline2 := d.pipelineLocalpart()

	if pipeline != "pipeline/1" {
		t.Errorf("pipeline = %q, want %q", pipeline, "pipeline/1")
	}
	if worktree != "worktree/2" {
		t.Errorf("worktree = %q, want %q", worktree, "worktree/2")
	}
	if pipeline2 != "pipeline/3" {
		t.Errorf("pipeline2 = %q, want %q", pipeline2, "pipeline/3")
	}
}

func TestReadPipelineResultFile(t *testing.T) {
	t.Parallel()

	t.Run("complete pipeline", func(t *testing.T) {
		t.Parallel()
		directory := t.TempDir()
		path := filepath.Join(directory, "result.jsonl")

		lines := []string{
			`{"type":"start","pipeline":"dev-init","step_count":2,"timestamp":"2026-02-10T12:00:00Z"}`,
			`{"type":"step","index":0,"name":"fetch","status":"ok","duration_ms":1200}`,
			`{"type":"step","index":1,"name":"setup","status":"ok","duration_ms":800}`,
			`{"type":"complete","status":"ok","duration_ms":2000,"log_event_id":"$abc123"}`,
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
			t.Fatal(err)
		}

		entries, err := readPipelineResultFile(path)
		if err != nil {
			t.Fatalf("readPipelineResultFile: %v", err)
		}

		if len(entries) != 4 {
			t.Fatalf("expected 4 entries, got %d", len(entries))
		}

		// Verify start entry.
		if entries[0].Type != "start" || entries[0].Pipeline != "dev-init" || entries[0].StepCount != 2 {
			t.Errorf("start entry: %+v", entries[0])
		}

		// Verify step entries.
		if entries[1].Type != "step" || entries[1].Name != "fetch" || entries[1].Status != "ok" {
			t.Errorf("step 0: %+v", entries[1])
		}
		if entries[2].Type != "step" || entries[2].Name != "setup" || entries[2].DurationMS != 800 {
			t.Errorf("step 1: %+v", entries[2])
		}

		// Verify complete entry.
		if entries[3].Type != "complete" || entries[3].LogEventID != ref.MustParseEventID("$abc123") {
			t.Errorf("complete entry: %+v", entries[3])
		}
	})

	t.Run("failed pipeline", func(t *testing.T) {
		t.Parallel()
		directory := t.TempDir()
		path := filepath.Join(directory, "result.jsonl")

		lines := []string{
			`{"type":"start","pipeline":"deploy","step_count":3,"timestamp":"2026-02-10T12:00:00Z"}`,
			`{"type":"step","index":0,"name":"build","status":"ok","duration_ms":5000}`,
			`{"type":"step","index":1,"name":"test","status":"failed","duration_ms":3000,"error":"exit code 1"}`,
			`{"type":"failed","status":"failed","error":"step failed: exit code 1","failed_step":"test","duration_ms":8000,"log_event_id":"$xyz"}`,
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
			t.Fatal(err)
		}

		entries, err := readPipelineResultFile(path)
		if err != nil {
			t.Fatalf("readPipelineResultFile: %v", err)
		}

		if len(entries) != 4 {
			t.Fatalf("expected 4 entries, got %d", len(entries))
		}

		// Verify the failed step has the error.
		if entries[2].Status != "failed" || entries[2].Error != "exit code 1" {
			t.Errorf("failed step: %+v", entries[2])
		}

		// Verify the terminal failed entry.
		if entries[3].Type != "failed" || entries[3].FailedStep != "test" {
			t.Errorf("terminal entry: %+v", entries[3])
		}
	})

	t.Run("empty file", func(t *testing.T) {
		t.Parallel()
		directory := t.TempDir()
		path := filepath.Join(directory, "result.jsonl")

		if err := os.WriteFile(path, nil, 0644); err != nil {
			t.Fatal(err)
		}

		entries, err := readPipelineResultFile(path)
		if err != nil {
			t.Fatalf("readPipelineResultFile: %v", err)
		}

		if len(entries) != 0 {
			t.Errorf("expected 0 entries for empty file, got %d", len(entries))
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		t.Parallel()
		_, err := readPipelineResultFile("/nonexistent/result.jsonl")
		if err == nil {
			t.Fatal("expected error for nonexistent file")
		}
	})

	t.Run("partial write (crash mid-pipeline)", func(t *testing.T) {
		t.Parallel()
		directory := t.TempDir()
		path := filepath.Join(directory, "result.jsonl")

		// Only start + one step, no terminal entry.
		lines := []string{
			`{"type":"start","pipeline":"crashed","step_count":5,"timestamp":"2026-02-10T12:00:00Z"}`,
			`{"type":"step","index":0,"name":"step-one","status":"ok","duration_ms":100}`,
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
			t.Fatal(err)
		}

		entries, err := readPipelineResultFile(path)
		if err != nil {
			t.Fatalf("readPipelineResultFile: %v", err)
		}

		if len(entries) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(entries))
		}
	})
}

func TestBuildPipelineExecutorSpec(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.pipelineExecutorBinary = "/nix/store/abc-executor/bin/bureau-pipeline-executor"
	daemon.pipelineEnvironment = "/nix/store/xyz-runner-env"
	daemon.workspaceRoot = "/var/bureau/workspace"

	command := schema.CommandMessage{
		Command:   "pipeline.execute",
		RequestID: "req-42",
		Parameters: map[string]any{
			"pipeline": "bureau/pipeline:dev-workspace-init",
			"project":  "my-project",
		},
	}

	spec := daemon.buildPipelineExecutorSpec(
		"bureau/pipeline:dev-workspace-init",
		"/tmp/result-abc/result.jsonl",
		"", "",
		command,
	)

	// Verify command.
	if len(spec.Command) != 2 {
		t.Fatalf("expected 2 command elements, got %d: %v", len(spec.Command), spec.Command)
	}
	if spec.Command[0] != daemon.pipelineExecutorBinary {
		t.Errorf("command[0] = %q, want %q", spec.Command[0], daemon.pipelineExecutorBinary)
	}
	if spec.Command[1] != "bureau/pipeline:dev-workspace-init" {
		t.Errorf("command[1] = %q, want pipeline ref", spec.Command[1])
	}

	// Verify environment variables.
	if spec.EnvironmentVariables["BUREAU_SANDBOX"] != "1" {
		t.Errorf("BUREAU_SANDBOX = %q, want '1'", spec.EnvironmentVariables["BUREAU_SANDBOX"])
	}
	if spec.EnvironmentVariables["BUREAU_RESULT_PATH"] != "/run/bureau/result.jsonl" {
		t.Errorf("BUREAU_RESULT_PATH = %q", spec.EnvironmentVariables["BUREAU_RESULT_PATH"])
	}

	// Verify filesystem mounts by destination rather than by index.
	// Order is not semantically meaningful and shifts when mounts are added.
	findMount := func(dest string) *schema.TemplateMount {
		for index := range spec.Filesystem {
			if spec.Filesystem[index].Dest == dest {
				return &spec.Filesystem[index]
			}
		}
		return nil
	}

	// Executor binary mount (bind-mounted at its host path so bwrap can
	// find it regardless of where it lives on the host).
	executorMount := findMount(daemon.pipelineExecutorBinary)
	if executorMount == nil {
		t.Error("executor binary mount not found")
	} else {
		if executorMount.Source != daemon.pipelineExecutorBinary {
			t.Errorf("executor mount source = %q, want %q", executorMount.Source, daemon.pipelineExecutorBinary)
		}
		if executorMount.Mode != "ro" {
			t.Errorf("executor mount mode = %q, want 'ro'", executorMount.Mode)
		}
	}

	// Result file mount.
	resultMount := findMount("/run/bureau/result.jsonl")
	if resultMount == nil {
		t.Error("result file mount not found")
	} else {
		if resultMount.Source != "/tmp/result-abc/result.jsonl" {
			t.Errorf("result mount source = %q", resultMount.Source)
		}
		if resultMount.Mode != "rw" {
			t.Errorf("result mount mode = %q, want 'rw'", resultMount.Mode)
		}
	}

	// Workspace root mount (canonical /workspace path used by pipeline JSONC).
	workspaceMount := findMount("/workspace")
	if workspaceMount == nil {
		t.Error("workspace root mount not found")
	} else {
		if workspaceMount.Source != "/var/bureau/workspace" {
			t.Errorf("workspace mount source = %q, want %q", workspaceMount.Source, "/var/bureau/workspace")
		}
		if workspaceMount.Mode != "rw" {
			t.Errorf("workspace mount mode = %q, want 'rw'", workspaceMount.Mode)
		}
	}

	// Verify namespaces.
	if spec.Namespaces == nil || !spec.Namespaces.PID {
		t.Error("expected PID namespace isolation")
	}

	// Verify security.
	if spec.Security == nil {
		t.Fatal("expected security config")
	}
	if !spec.Security.NewSession || !spec.Security.DieWithParent || !spec.Security.NoNewPrivs {
		t.Errorf("security = %+v, want all true", spec.Security)
	}

	// Verify environment path.
	if spec.EnvironmentPath != daemon.pipelineEnvironment {
		t.Errorf("EnvironmentPath = %q, want %q", spec.EnvironmentPath, daemon.pipelineEnvironment)
	}

	// Verify payload.
	if spec.Payload == nil {
		t.Fatal("expected payload from command parameters")
	}
	if spec.Payload["project"] != "my-project" {
		t.Errorf("payload[project] = %v, want 'my-project'", spec.Payload["project"])
	}
}

func TestBuildPipelineExecutorSpec_NoEnvironment(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.pipelineExecutorBinary = "/usr/bin/executor"
	daemon.pipelineEnvironment = "" // No Nix environment.
	daemon.workspaceRoot = "/var/bureau/workspace"

	spec := daemon.buildPipelineExecutorSpec("test", "/tmp/result.jsonl",
		"", "", schema.CommandMessage{})

	if spec.EnvironmentPath != "" {
		t.Errorf("EnvironmentPath should be empty, got %q", spec.EnvironmentPath)
	}
}

func TestBuildPipelineExecutorSpec_NoRef(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.pipelineExecutorBinary = "/usr/bin/executor"
	daemon.workspaceRoot = "/var/bureau/workspace"

	// When pipelineRef is empty, the executor resolves via payload.
	spec := daemon.buildPipelineExecutorSpec("", "/tmp/result.jsonl",
		"", "", schema.CommandMessage{
			Parameters: map[string]any{
				"pipeline_ref": "bureau/pipeline:inline-test",
			},
		})

	// Command should have only the binary, no pipeline ref argument.
	if len(spec.Command) != 1 {
		t.Fatalf("expected 1 command element (no ref arg), got %d: %v",
			len(spec.Command), spec.Command)
	}

	// Pipeline ref should be in the payload.
	if spec.Payload["pipeline_ref"] != "bureau/pipeline:inline-test" {
		t.Errorf("payload missing pipeline_ref")
	}
}

func TestHandlePipelineExecute_NoBinary(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	const roomID = "!workspace:test"
	harness.matrixState.setStateEvent(roomID, schema.MatrixEventTypePowerLevels, "", map[string]any{
		"users": map[string]any{
			"@admin:bureau.local": float64(100),
		},
	})

	// daemon has no pipelineExecutorBinary set.
	event := buildPipelineCommandEvent(ref.MustParseEventID("$pipe1"), ref.MustParseUserID("@admin:bureau.local"),
		"bureau/pipeline:test", "req-1")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, mustRoomID(roomID), []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(messages))
	}

	message := messages[0]
	if message.Content["status"] != "error" {
		t.Errorf("status = %v, want 'error'", message.Content["status"])
	}
	errorText, _ := message.Content["error"].(string)
	if !strings.Contains(errorText, "pipeline-executor-binary") {
		t.Errorf("error should mention missing binary, got: %q", errorText)
	}
}

func TestHandlePipelineExecute_MissingPipeline(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	// Create a temporary "binary" that exists and is executable.
	binaryDir := t.TempDir()
	binaryPath := filepath.Join(binaryDir, "executor")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	harness.daemon.pipelineExecutorBinary = binaryPath

	const roomID = "!workspace:test"
	harness.matrixState.setStateEvent(roomID, schema.MatrixEventTypePowerLevels, "", map[string]any{
		"users": map[string]any{
			"@admin:bureau.local": float64(100),
		},
	})

	// Command has no pipeline, pipeline_ref, or pipeline_inline parameter.
	event := messaging.Event{
		EventID: ref.MustParseEventID("$pipe2"),
		Type:    schema.MatrixEventTypeMessage,
		Sender:  ref.MustParseUserID("@admin:bureau.local"),
		Content: map[string]any{
			"msgtype":    schema.MsgTypeCommand,
			"body":       "pipeline.execute",
			"command":    "pipeline.execute",
			"parameters": map[string]any{},
		},
	}

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, mustRoomID(roomID), []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(messages))
	}

	message := messages[0]
	if message.Content["status"] != "error" {
		t.Errorf("status = %v, want 'error'", message.Content["status"])
	}
	errorText, _ := message.Content["error"].(string)
	if !strings.Contains(errorText, "pipeline") {
		t.Errorf("error should mention missing pipeline, got: %q", errorText)
	}
}

func TestHandlePipelineExecute_Accepted(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	// Create a temporary "binary" that exists and is executable.
	binaryDir := t.TempDir()
	binaryPath := filepath.Join(binaryDir, "executor")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	harness.daemon.pipelineExecutorBinary = binaryPath

	// Use a pre-cancelled context for shutdownCtx. The handler starts
	// an async goroutine that would race against the synchronous result
	// — the cancelled context makes executePipeline exit immediately,
	// so only the synchronous "accepted" result is captured.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	harness.daemon.shutdownCtx = cancelledCtx

	const roomID = "!workspace:test"
	harness.matrixState.setStateEvent(roomID, schema.MatrixEventTypePowerLevels, "", map[string]any{
		"users": map[string]any{
			"@admin:bureau.local": float64(100),
		},
	})

	event := buildPipelineCommandEvent(ref.MustParseEventID("$pipe3"), ref.MustParseUserID("@admin:bureau.local"),
		"bureau/pipeline:dev-init", "req-accepted")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, mustRoomID(roomID), []messaging.Event{event})

	// The synchronous result should be "accepted".
	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected exactly 1 sent message, got %d", len(messages))
	}

	message := messages[0]
	if message.Content["status"] != "success" {
		t.Errorf("status = %v, want 'success' (accepted)", message.Content["status"])
	}

	result, ok := message.Content["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", message.Content["result"])
	}
	if result["status"] != "accepted" {
		t.Errorf("result.status = %v, want 'accepted'", result["status"])
	}
	principalLocalpart, _ := result["principal"].(string)
	if !strings.HasPrefix(principalLocalpart, "pipeline/") {
		t.Errorf("result.principal = %q, want prefix 'pipeline/'", principalLocalpart)
	}
}

func TestHandlePipelineExecute_AuthorizationRequired(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	// Create a temporary "binary" that exists and is executable.
	binaryDir := t.TempDir()
	binaryPath := filepath.Join(binaryDir, "executor")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	harness.daemon.pipelineExecutorBinary = binaryPath

	const roomID = "!workspace:test"
	// Sender has PL 50, pipeline.execute requires PL 100 (admin).
	harness.matrixState.setStateEvent(roomID, schema.MatrixEventTypePowerLevels, "", map[string]any{
		"users": map[string]any{
			"@operator:bureau.local": float64(50),
		},
	})

	event := buildPipelineCommandEvent(ref.MustParseEventID("$pipe4"), ref.MustParseUserID("@operator:bureau.local"),
		"bureau/pipeline:test", "req-denied")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, mustRoomID(roomID), []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(messages))
	}

	message := messages[0]
	if message.Content["status"] != "error" {
		t.Errorf("status = %v, want 'error'", message.Content["status"])
	}
	errorText, _ := message.Content["error"].(string)
	if !strings.Contains(errorText, "authorization denied") {
		t.Errorf("error should mention authorization, got: %q", errorText)
	}
}

func TestHandlePipelineExecute_ViaPayloadRef(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	binaryDir := t.TempDir()
	binaryPath := filepath.Join(binaryDir, "executor")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	harness.daemon.pipelineExecutorBinary = binaryPath

	// Use a pre-cancelled context so the async goroutine exits
	// immediately without racing against the synchronous result.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	harness.daemon.shutdownCtx = cancelledCtx

	const roomID = "!workspace:test"
	harness.matrixState.setStateEvent(roomID, schema.MatrixEventTypePowerLevels, "", map[string]any{
		"users": map[string]any{
			"@admin:bureau.local": float64(100),
		},
	})

	// No "pipeline" parameter, but pipeline_ref is present.
	event := messaging.Event{
		EventID: ref.MustParseEventID("$pipe5"),
		Type:    schema.MatrixEventTypeMessage,
		Sender:  ref.MustParseUserID("@admin:bureau.local"),
		Content: map[string]any{
			"msgtype": schema.MsgTypeCommand,
			"body":    "pipeline.execute",
			"command": "pipeline.execute",
			"parameters": map[string]any{
				"pipeline_ref": "bureau/pipeline:via-payload",
			},
		},
	}

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, mustRoomID(roomID), []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected exactly 1 sent message, got %d", len(messages))
	}

	result, ok := messages[0].Content["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", messages[0].Content["result"])
	}
	if result["status"] != "accepted" {
		t.Errorf("result.status = %v, want 'accepted'", result["status"])
	}
}

// buildPipelineCommandEvent creates a messaging.Event for a
// pipeline.execute command with a pipeline ref parameter.
func buildPipelineCommandEvent(eventID ref.EventID, sender ref.UserID, pipelineRef, requestID string) messaging.Event {
	content := map[string]any{
		"msgtype": schema.MsgTypeCommand,
		"body":    "pipeline.execute " + pipelineRef,
		"command": "pipeline.execute",
		"parameters": map[string]any{
			"pipeline": pipelineRef,
		},
	}
	if requestID != "" {
		content["request_id"] = requestID
	}

	return messaging.Event{
		EventID: eventID,
		Type:    schema.MatrixEventTypeMessage,
		Sender:  sender,
		Content: content,
	}
}

// TestPostPipelineResult verifies the structure of posted pipeline results.
func TestPostPipelineResult(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

	t.Run("successful pipeline", func(t *testing.T) {
		entries := []pipelineResultEntry{
			{Type: "start", Pipeline: "test", StepCount: 2},
			{Type: "step", Index: 0, Name: "build", Status: "ok", DurationMS: 1000},
			{Type: "step", Index: 1, Name: "test", Status: "ok", DurationMS: 2000},
			{Type: "complete", Status: "ok", DurationMS: 3000, LogEventID: ref.MustParseEventID("$log1")},
		}

		command := schema.CommandMessage{
			Command:   "pipeline.execute",
			RequestID: "req-success",
		}

		ctx := context.Background()
		harness.daemon.postPipelineResult(ctx, mustRoomID("!room:test"), ref.MustParseEventID("$cmd1"),
			command, fixedTestTime, 0, "", "", entries)

		messages := harness.getSentMessages()
		if len(messages) != 1 {
			t.Fatalf("expected 1 message, got %d", len(messages))
		}

		content := messages[0].Content
		if content["status"] != "success" {
			t.Errorf("status = %v, want 'success'", content["status"])
		}
		if content["log_event_id"] != "$log1" {
			t.Errorf("log_event_id = %v, want '$log1'", content["log_event_id"])
		}
		if content["request_id"] != "req-success" {
			t.Errorf("request_id = %v", content["request_id"])
		}

		steps, ok := content["steps"].([]any)
		if !ok {
			t.Fatalf("steps is not a slice: %T", content["steps"])
		}
		if len(steps) != 2 {
			t.Errorf("expected 2 steps, got %d", len(steps))
		}
	})

	t.Run("failed pipeline", func(t *testing.T) {
		// Clear previous messages.
		harness.sentMessagesMu.Lock()
		harness.sentMessages = nil
		harness.sentMessagesMu.Unlock()

		entries := []pipelineResultEntry{
			{Type: "start", Pipeline: "deploy", StepCount: 2},
			{Type: "step", Index: 0, Name: "build", Status: "ok", DurationMS: 1000},
			{Type: "step", Index: 1, Name: "deploy", Status: "failed", DurationMS: 500, Error: "connection refused"},
			{Type: "failed", Status: "failed", Error: "step failed", FailedStep: "deploy", DurationMS: 1500},
		}

		ctx := context.Background()
		harness.daemon.postPipelineResult(ctx, mustRoomID("!room:test"), ref.MustParseEventID("$cmd2"),
			schema.CommandMessage{Command: "pipeline.execute"},
			fixedTestTime, 1, "exit status 1", "", entries)

		messages := harness.getSentMessages()
		if len(messages) != 1 {
			t.Fatalf("expected 1 message, got %d", len(messages))
		}

		content := messages[0].Content
		if content["status"] != "error" {
			t.Errorf("status = %v, want 'error'", content["status"])
		}
		body, _ := content["body"].(string)
		if !strings.Contains(body, "deploy") {
			t.Errorf("body should mention failed step, got: %q", body)
		}
	})

	t.Run("no result file (killed)", func(t *testing.T) {
		harness.sentMessagesMu.Lock()
		harness.sentMessages = nil
		harness.sentMessagesMu.Unlock()

		ctx := context.Background()
		harness.daemon.postPipelineResult(ctx, mustRoomID("!room:test"), ref.MustParseEventID("$cmd3"),
			schema.CommandMessage{Command: "pipeline.execute"},
			fixedTestTime, 137, "killed by signal 9", "", nil)

		messages := harness.getSentMessages()
		if len(messages) != 1 {
			t.Fatalf("expected 1 message, got %d", len(messages))
		}

		content := messages[0].Content
		if content["status"] != "error" {
			t.Errorf("status = %v, want 'error'", content["status"])
		}
		exitCode, ok := content["exit_code"].(float64)
		if !ok || int(exitCode) != 137 {
			t.Errorf("exit_code = %v, want 137", content["exit_code"])
		}
	})
}

// fixedTestTime is a deterministic time.Time for pipeline tests. The
// postPipelineResult function computes elapsed duration from the start parameter,
// which will yield a near-zero value — fine for verifying structure.
var fixedTestTime = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// TestPipelineResultSerialization verifies that pipelineResultEntry
// can round-trip through JSON (matching the executor's output format).
func TestPipelineResultSerialization(t *testing.T) {
	t.Parallel()

	original := pipelineResultEntry{
		Type:       "step",
		Index:      2,
		Name:       "deploy",
		Status:     "failed",
		DurationMS: 4500,
		Error:      "connection refused",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded pipelineResultEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Type != original.Type || decoded.Name != original.Name ||
		decoded.Status != original.Status || decoded.Error != original.Error {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}
