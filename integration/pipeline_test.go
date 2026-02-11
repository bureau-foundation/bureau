// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// TestPipelineExecution exercises the full pipeline execution lifecycle
// through real processes:
//
//   - Admin sends a pipeline.execute command to the config room
//   - Daemon picks it up via /sync, posts "accepted" acknowledgment
//   - Daemon creates a bwrap sandbox running bureau-pipeline-executor
//   - Executor runs pipeline steps (via /bin/sh -c from the Nix runner-env)
//   - Executor writes JSONL result, exits
//   - Daemon reads result, posts threaded pipeline result to Matrix
//   - Test reads both results and verifies structure
//
// This proves the daemon→launcher IPC, bwrap sandbox creation, pipeline
// executor binary, Nix environment bind mounting, result file flow, and
// threaded Matrix reply posting all work together.
func TestPipelineExecution(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	machine := newTestMachine(t, "machine/pipeline")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:         resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:           resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:            resolvedBinary(t, "PROXY_BINARY"),
		PipelineExecutorBinary: resolvedBinary(t, "PIPELINE_EXECUTOR_BINARY"),
		PipelineEnvironment:    findRunnerEnv(t),
	})

	ctx := t.Context()

	// Send a pipeline.execute command to the config room. The admin has
	// PL 100 (set by the daemon during room creation), satisfying the
	// PowerLevelAdmin requirement for pipeline.execute.
	requestID := "test-pipeline-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	commandEventID, err := admin.SendEvent(ctx, machine.ConfigRoomID, "m.room.message",
		schema.CommandMessage{
			MsgType:   schema.MsgTypeCommand,
			Body:      "pipeline.execute: integration test",
			Command:   "pipeline.execute",
			RequestID: requestID,
			Parameters: map[string]any{
				"pipeline_inline": map[string]any{
					"description": "Integration test pipeline",
					"steps": []map[string]any{
						{"name": "greet", "run": "echo hello"},
						{"name": "farewell", "run": "echo goodbye"},
					},
				},
			},
		})
	if err != nil {
		t.Fatalf("send pipeline.execute command: %v", err)
	}

	// Wait for both command results: the sync "accepted" acknowledgment
	// and the async pipeline execution result. The pipeline creates a
	// bwrap sandbox, runs two echo commands, and posts the result — give
	// it enough time for the full lifecycle.
	results := waitForCommandResults(t, admin, machine.ConfigRoomID, requestID, 2, 60*time.Second)

	// Classify results by their content. The "accepted" result comes from
	// postCommandResult (has result.status == "accepted"), the pipeline
	// result comes from postPipelineResult (has exit_code).
	var acceptedResult, pipelineResult map[string]any
	for _, event := range results {
		content := event.Content

		// Check if this is the pipeline result (has exit_code).
		if _, hasExitCode := content["exit_code"]; hasExitCode {
			pipelineResult = content
			continue
		}

		// Check if this is the accepted result.
		resultField, _ := content["result"].(map[string]any)
		if resultField != nil {
			status, _ := resultField["status"].(string)
			if status == "accepted" {
				acceptedResult = content
				continue
			}
		}
	}

	// --- Verify accepted result ---
	if acceptedResult == nil {
		t.Fatal("accepted result not found in command results")
	}
	if status, _ := acceptedResult["status"].(string); status != "success" {
		t.Errorf("accepted result status = %q, want %q", status, "success")
	}
	resultMap, _ := acceptedResult["result"].(map[string]any)
	if resultMap == nil {
		t.Fatal("accepted result has nil result field")
	}
	principalName, _ := resultMap["principal"].(string)
	if principalName == "" {
		t.Error("accepted result has empty principal")
	}
	// Verify the result threads to the command event.
	verifyThreadRelation(t, acceptedResult, commandEventID)

	// --- Verify pipeline result ---
	if pipelineResult == nil {
		t.Fatal("pipeline result not found in command results")
	}
	if status, _ := pipelineResult["status"].(string); status != "success" {
		body, _ := pipelineResult["body"].(string)
		t.Errorf("pipeline result status = %q, want %q (body: %s)", status, "success", body)
	}
	exitCode, _ := pipelineResult["exit_code"].(float64)
	if exitCode != 0 {
		t.Errorf("pipeline result exit_code = %v, want 0", exitCode)
	}
	durationMilliseconds, _ := pipelineResult["duration_ms"].(float64)
	if durationMilliseconds < 0 {
		t.Errorf("pipeline result duration_ms = %v, want >= 0", durationMilliseconds)
	}

	// Verify steps.
	steps, _ := pipelineResult["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("pipeline result steps count = %d, want 2", len(steps))
	}
	for index, expectedName := range []string{"greet", "farewell"} {
		step, _ := steps[index].(map[string]any)
		if step == nil {
			t.Errorf("step %d is nil", index)
			continue
		}
		name, _ := step["name"].(string)
		if name != expectedName {
			t.Errorf("step %d name = %q, want %q", index, name, expectedName)
		}
		stepStatus, _ := step["status"].(string)
		if stepStatus != "ok" {
			t.Errorf("step %d status = %q, want %q", index, stepStatus, "ok")
		}
	}

	// Verify the pipeline result threads to the command event.
	verifyThreadRelation(t, pipelineResult, commandEventID)

	t.Log("pipeline execution lifecycle verified: command → accepted → sandbox → executor → result")
}

// TestPipelineExecutionFailure verifies that pipeline step failures are
// correctly reported through the full stack. A pipeline with a failing step
// should produce a result with status "error" and the correct step-level
// failure information.
func TestPipelineExecutionFailure(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	machine := newTestMachine(t, "machine/pipeline-fail")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:         resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:           resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:            resolvedBinary(t, "PROXY_BINARY"),
		PipelineExecutorBinary: resolvedBinary(t, "PIPELINE_EXECUTOR_BINARY"),
		PipelineEnvironment:    findRunnerEnv(t),
	})

	ctx := t.Context()

	requestID := "test-pipeline-fail-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	commandEventID, err := admin.SendEvent(ctx, machine.ConfigRoomID, "m.room.message",
		schema.CommandMessage{
			MsgType:   schema.MsgTypeCommand,
			Body:      "pipeline.execute: failure test",
			Command:   "pipeline.execute",
			RequestID: requestID,
			Parameters: map[string]any{
				"pipeline_inline": map[string]any{
					"description": "Failing pipeline",
					"steps": []map[string]any{
						{"name": "setup", "run": "echo starting"},
						{"name": "fail-step", "run": "exit 1"},
					},
				},
			},
		})
	if err != nil {
		t.Fatalf("send pipeline.execute command: %v", err)
	}

	results := waitForCommandResults(t, admin, machine.ConfigRoomID, requestID, 2, 60*time.Second)

	// Find the pipeline result (the one with exit_code).
	var pipelineResult map[string]any
	for _, event := range results {
		if _, hasExitCode := event.Content["exit_code"]; hasExitCode {
			pipelineResult = event.Content
			break
		}
	}

	if pipelineResult == nil {
		t.Fatal("pipeline result not found in command results")
	}

	// The executor exits non-zero when a step fails, and the daemon
	// reports this as an error status.
	if status, _ := pipelineResult["status"].(string); status != "error" {
		t.Errorf("pipeline result status = %q, want %q", status, "error")
	}
	exitCode, _ := pipelineResult["exit_code"].(float64)
	if exitCode == 0 {
		t.Error("pipeline result exit_code = 0, want non-zero for a failing pipeline")
	}

	// Verify step details. The first step should succeed, the second
	// should fail.
	steps, _ := pipelineResult["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("pipeline result steps count = %d, want 2", len(steps))
	}

	setupStep, _ := steps[0].(map[string]any)
	if setupStep != nil {
		name, _ := setupStep["name"].(string)
		if name != "setup" {
			t.Errorf("step 0 name = %q, want %q", name, "setup")
		}
		stepStatus, _ := setupStep["status"].(string)
		if stepStatus != "ok" {
			t.Errorf("step 0 status = %q, want %q", stepStatus, "ok")
		}
	}

	failStep, _ := steps[1].(map[string]any)
	if failStep != nil {
		name, _ := failStep["name"].(string)
		if name != "fail-step" {
			t.Errorf("step 1 name = %q, want %q", name, "fail-step")
		}
		stepStatus, _ := failStep["status"].(string)
		if stepStatus != "failed" {
			t.Errorf("step 1 status = %q, want %q", stepStatus, "failed")
		}
	}

	// The body should mention the failing step.
	body, _ := pipelineResult["body"].(string)
	if body == "" {
		t.Error("pipeline result body is empty")
	}

	verifyThreadRelation(t, pipelineResult, commandEventID)

	t.Log("pipeline failure reporting verified: failing step produces error result with step-level detail")
}

// TestPipelineParameterPropagation verifies that command parameters flow
// through the entire chain: command message → daemon → SandboxSpec.Payload
// → launcher (payload.json) → executor → pipeline variable resolution →
// shell execution. A pipeline step uses a variable from the command
// parameters; if the step succeeds, the variable was propagated correctly.
func TestPipelineParameterPropagation(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	machine := newTestMachine(t, "machine/pipeline-params")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:         resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:           resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:            resolvedBinary(t, "PROXY_BINARY"),
		PipelineExecutorBinary: resolvedBinary(t, "PIPELINE_EXECUTOR_BINARY"),
		PipelineEnvironment:    findRunnerEnv(t),
	})

	ctx := t.Context()

	requestID := "test-pipeline-params-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	_, err := admin.SendEvent(ctx, machine.ConfigRoomID, "m.room.message",
		schema.CommandMessage{
			MsgType:   schema.MsgTypeCommand,
			Body:      "pipeline.execute: parameter propagation test",
			Command:   "pipeline.execute",
			RequestID: requestID,
			Parameters: map[string]any{
				"pipeline_inline": map[string]any{
					"description": "Parameter propagation test",
					"variables": map[string]any{
						"PROJECT": map[string]any{
							"description": "project name",
							"required":    true,
						},
					},
					"steps": []map[string]any{
						// test(1) returns 0 (success) if the string
						// comparison is true. If PROJECT was not
						// propagated or resolved, this fails.
						{"name": "verify-project", "run": "test \"${PROJECT}\" = \"iree\""},
					},
				},
				// This top-level parameter becomes a payload variable.
				// The executor reads it from payload.json, and
				// ResolveVariables makes it available as ${PROJECT}.
				"PROJECT": "iree",
			},
		})
	if err != nil {
		t.Fatalf("send pipeline.execute command: %v", err)
	}

	results := waitForCommandResults(t, admin, machine.ConfigRoomID, requestID, 2, 60*time.Second)

	// Find the pipeline result.
	var pipelineResult map[string]any
	for _, event := range results {
		if _, hasExitCode := event.Content["exit_code"]; hasExitCode {
			pipelineResult = event.Content
			break
		}
	}

	if pipelineResult == nil {
		t.Fatal("pipeline result not found in command results")
	}

	// If the step succeeded (exit 0), the variable was propagated correctly
	// through the entire daemon → launcher → executor → shell chain.
	if status, _ := pipelineResult["status"].(string); status != "success" {
		body, _ := pipelineResult["body"].(string)
		exitCode, _ := pipelineResult["exit_code"].(float64)
		t.Errorf("pipeline result status = %q, want %q (exit_code=%v, body=%s)",
			status, "success", exitCode, body)
	}

	exitCode, _ := pipelineResult["exit_code"].(float64)
	if exitCode != 0 {
		t.Errorf("pipeline result exit_code = %v, want 0 (variable propagation failed)", exitCode)
	}

	steps, _ := pipelineResult["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("pipeline result steps count = %d, want 1", len(steps))
	}
	step, _ := steps[0].(map[string]any)
	if step != nil {
		name, _ := step["name"].(string)
		if name != "verify-project" {
			t.Errorf("step 0 name = %q, want %q", name, "verify-project")
		}
		stepStatus, _ := step["status"].(string)
		if stepStatus != "ok" {
			t.Errorf("step 0 status = %q, want %q (PROJECT variable not propagated)", stepStatus, "ok")
		}
	}

	t.Log("parameter propagation verified: command parameters flow through daemon → launcher → executor → shell")
}

// --- Pipeline test helpers ---

// verifyThreadRelation checks that an event content's m.relates_to field
// threads to the expected event ID. Pipeline results and command
// acknowledgments are posted as threaded replies to the original command.
func verifyThreadRelation(t *testing.T, content map[string]any, expectedEventID string) {
	t.Helper()

	relatesTo, _ := content["m.relates_to"].(map[string]any)
	if relatesTo == nil {
		t.Error("event content missing m.relates_to")
		return
	}

	relType, _ := relatesTo["rel_type"].(string)
	if relType != "m.thread" {
		t.Errorf("m.relates_to.rel_type = %q, want %q", relType, "m.thread")
	}

	eventID, _ := relatesTo["event_id"].(string)
	if eventID != expectedEventID {
		t.Errorf("m.relates_to.event_id = %q, want %q", eventID, expectedEventID)
	}
}
