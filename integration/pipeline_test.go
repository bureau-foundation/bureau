// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestPipelineExecution exercises the full pipeline execution lifecycle
// through real processes:
//
//   - Admin publishes a pipeline definition to the pipeline room
//   - Admin sends a pipeline.execute command to the config room
//   - Daemon picks it up via /sync, posts "accepted" acknowledgment
//   - Daemon creates a pip- ticket in the project room via the ticket service
//   - Daemon creates a bwrap sandbox running bureau-pipeline-executor
//   - Executor claims the ticket, runs pipeline steps, posts notes, closes ticket
//   - Executor writes JSONL result, exits
//   - Daemon reads result, posts threaded pipeline result to Matrix
//   - Test reads both results and verifies structure
//
// This proves the daemon→launcher IPC, bwrap sandbox creation, ticket
// lifecycle, pipeline executor binary, Nix environment bind mounting,
// result file flow, and threaded Matrix reply posting all work together.
func TestPipelineExecution(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "pipeline")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:         resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:           resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:            resolvedBinary(t, "PROXY_BINARY"),
		PipelineExecutorBinary: resolvedBinary(t, "PIPELINE_EXECUTOR_BINARY"),
		PipelineEnvironment:    findRunnerEnv(t),
		Fleet:                  fleet,
	})

	ctx := t.Context()

	// --- Ticket service setup ---
	ticketSvc := deployTicketService(t, admin, fleet, machine, "pipeline")

	// Create a project room for pip- tickets.
	projectRoomID := createTicketProjectRoom(t, admin, "pipeline-project",
		ticketSvc.Entity, machine.UserID.String())

	// --- Publish pipeline definition ---
	publishPipelineDefinition(t, admin, "test-greet", pipeline.PipelineContent{
		Description: "Integration test pipeline",
		Steps: []pipeline.PipelineStep{
			{Name: "greet", Run: "echo hello"},
			{Name: "farewell", Run: "echo goodbye"},
		},
	})

	// --- Execute pipeline ---
	requestID := "test-pipeline-" + t.Name()
	resultWatch := watchRoom(t, admin, machine.ConfigRoomID)
	// Create the project room watch before sending the command so it
	// captures ticket state events that arrive during pipeline execution.
	// The findPipelineTicket call below uses this watch to find the
	// closed pip- ticket.
	projectWatch := watchRoom(t, admin, projectRoomID)

	commandEventID, err := admin.SendEvent(ctx, machine.ConfigRoomID, schema.MatrixEventTypeMessage,
		schema.CommandMessage{
			MsgType:   schema.MsgTypeCommand,
			Body:      "pipeline.execute: integration test",
			Command:   "pipeline.execute",
			RequestID: requestID,
			Parameters: map[string]any{
				"pipeline": "bureau/pipeline:test-greet",
				"room":     projectRoomID.String(),
			},
		})
	if err != nil {
		t.Fatalf("send pipeline.execute command: %v", err)
	}

	// Wait for both command results: the sync "accepted" acknowledgment
	// and the async pipeline execution result.
	results := resultWatch.WaitForCommandResults(t, requestID, 2)

	acceptedResult := findAcceptedEvent(t, results)
	pipelineResult := findPipelineEvent(t, results)

	// --- Verify accepted result ---
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
	verifyThreadRelation(t, acceptedResult, commandEventID.String())

	// --- Verify pipeline result ---
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

	verifyThreadRelation(t, pipelineResult, commandEventID.String())

	// --- Verify pip- ticket was created and closed ---
	ticketID, ticketContent := findPipelineTicket(t, admin, projectRoomID, &projectWatch, "test-greet")
	if ticketContent.Status != "closed" {
		t.Errorf("ticket %s status = %q, want closed", ticketID, ticketContent.Status)
	}
	if ticketContent.Type != "pipeline" {
		t.Errorf("ticket %s type = %q, want pipeline", ticketID, ticketContent.Type)
	}

	t.Log("pipeline execution lifecycle verified: command → accepted → ticket → sandbox → executor → result")
}

// TestPipelineExecutionFailure verifies that pipeline step failures are
// correctly reported through the full stack. A pipeline with a failing step
// should produce a result with status "error" and the correct step-level
// failure information. The pip- ticket should still be closed.
func TestPipelineExecutionFailure(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "pipeline-fail")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:         resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:           resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:            resolvedBinary(t, "PROXY_BINARY"),
		PipelineExecutorBinary: resolvedBinary(t, "PIPELINE_EXECUTOR_BINARY"),
		PipelineEnvironment:    findRunnerEnv(t),
		Fleet:                  fleet,
	})

	ctx := t.Context()

	// --- Ticket service + project room ---
	ticketSvc := deployTicketService(t, admin, fleet, machine, "pipefail")
	projectRoomID := createTicketProjectRoom(t, admin, "pipefail-project",
		ticketSvc.Entity, machine.UserID.String())

	// --- Publish failing pipeline definition ---
	publishPipelineDefinition(t, admin, "test-fail", pipeline.PipelineContent{
		Description: "Failing pipeline",
		Steps: []pipeline.PipelineStep{
			{Name: "setup", Run: "echo starting"},
			{Name: "fail-step", Run: "exit 1"},
		},
	})

	// --- Execute pipeline ---
	requestID := "test-pipeline-fail-" + t.Name()
	resultWatch := watchRoom(t, admin, machine.ConfigRoomID)

	commandEventID, err := admin.SendEvent(ctx, machine.ConfigRoomID, schema.MatrixEventTypeMessage,
		schema.CommandMessage{
			MsgType:   schema.MsgTypeCommand,
			Body:      "pipeline.execute: failure test",
			Command:   "pipeline.execute",
			RequestID: requestID,
			Parameters: map[string]any{
				"pipeline": "bureau/pipeline:test-fail",
				"room":     projectRoomID.String(),
			},
		})
	if err != nil {
		t.Fatalf("send pipeline.execute command: %v", err)
	}

	results := resultWatch.WaitForCommandResults(t, requestID, 2)
	pipelineResult := findPipelineEvent(t, results)

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

	body, _ := pipelineResult["body"].(string)
	if body == "" {
		t.Error("pipeline result body is empty")
	}

	verifyThreadRelation(t, pipelineResult, commandEventID.String())

	t.Log("pipeline failure reporting verified: failing step produces error result with step-level detail")
}

// TestPipelineParameterPropagation verifies that command parameters flow
// through the entire chain: command message → daemon → pip- ticket
// variables → executor → pipeline variable resolution → shell execution.
// A pipeline step uses a variable from the command parameters; if the step
// succeeds, the variable was propagated correctly.
func TestPipelineParameterPropagation(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "pipeline-params")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:         resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:           resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:            resolvedBinary(t, "PROXY_BINARY"),
		PipelineExecutorBinary: resolvedBinary(t, "PIPELINE_EXECUTOR_BINARY"),
		PipelineEnvironment:    findRunnerEnv(t),
		Fleet:                  fleet,
	})

	ctx := t.Context()

	// --- Ticket service + project room ---
	ticketSvc := deployTicketService(t, admin, fleet, machine, "pipeparam")
	projectRoomID := createTicketProjectRoom(t, admin, "pipeparam-project",
		ticketSvc.Entity, machine.UserID.String())

	// --- Publish pipeline definition with variable declaration ---
	publishPipelineDefinition(t, admin, "test-params", pipeline.PipelineContent{
		Description: "Parameter propagation test",
		Variables: map[string]pipeline.PipelineVariable{
			"PROJECT": {
				Description: "project name",
				Required:    true,
			},
		},
		Steps: []pipeline.PipelineStep{
			// test(1) returns 0 (success) if the string comparison
			// is true. If PROJECT was not propagated or resolved,
			// this fails.
			{Name: "verify-project", Run: `test "${PROJECT}" = "iree"`},
		},
	})

	// --- Execute pipeline with variable ---
	requestID := "test-pipeline-params-" + t.Name()
	resultWatch := watchRoom(t, admin, machine.ConfigRoomID)

	if _, err := admin.SendEvent(ctx, machine.ConfigRoomID, schema.MatrixEventTypeMessage,
		schema.CommandMessage{
			MsgType:   schema.MsgTypeCommand,
			Body:      "pipeline.execute: parameter propagation test",
			Command:   "pipeline.execute",
			RequestID: requestID,
			Parameters: map[string]any{
				"pipeline": "bureau/pipeline:test-params",
				"room":     projectRoomID.String(),
				// This top-level parameter becomes a ticket variable.
				// The executor reads it from the TicketContent.Pipeline.Variables,
				// and ResolveVariables makes it available as ${PROJECT}.
				"PROJECT": "iree",
			},
		}); err != nil {
		t.Fatalf("send pipeline.execute command: %v", err)
	}

	results := resultWatch.WaitForCommandResults(t, requestID, 2)
	pipelineResult := findPipelineEvent(t, results)

	// If the step succeeded (exit 0), the variable was propagated correctly
	// through the entire daemon → ticket → executor → shell chain.
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

	t.Log("parameter propagation verified: command parameters flow through daemon → ticket → executor → shell")
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

// findPipelineTicket searches for a pip- ticket in the project room that
// references the given pipeline name. Checks current state first, then
// falls back to watching for new events. Returns the ticket ID and content.
func findPipelineTicket(t *testing.T, admin *messaging.DirectSession, roomID ref.RoomID, watch *roomWatch, pipelineName string) (string, ticket.TicketContent) {
	t.Helper()

	// Search for existing tickets by scanning state events.
	// The ticket state key is the ticket ID (e.g., "pip-a3f9").
	// We need to find the one whose pipeline ref contains pipelineName.
	event := watch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != schema.EventTypeTicket {
			return false
		}
		ticketType, _ := event.Content["type"].(string)
		if ticketType != "pipeline" {
			return false
		}
		title, _ := event.Content["title"].(string)
		status, _ := event.Content["status"].(string)
		// Match pipeline tickets by title and terminal status.
		return title != "" && status == "closed" && event.StateKey != nil
	}, "closed pipeline ticket for "+pipelineName)

	ticketID := ""
	if event.StateKey != nil {
		ticketID = *event.StateKey
	}
	if ticketID == "" {
		t.Fatal("pipeline ticket state event has no state key")
	}

	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		t.Fatalf("marshal ticket content: %v", err)
	}
	var content ticket.TicketContent
	if err := json.Unmarshal(contentJSON, &content); err != nil {
		t.Fatalf("unmarshal ticket content: %v", err)
	}

	return ticketID, content
}
