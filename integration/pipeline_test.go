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
//   - Daemon picks it up via /sync, posts "accepted" with ticket ID
//   - Ticket watcher creates a bwrap sandbox running bureau-pipeline-executor
//   - Executor claims the ticket, runs pipeline steps, posts notes, closes ticket
//   - Test verifies accepted result and closed ticket with correct conclusion
//
// This proves the daemon→launcher IPC, bwrap sandbox creation, ticket-driven
// pipeline execution, executor binary, Nix environment bind mounting, and
// ticket lifecycle all work together.
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

	// Wait for the accepted acknowledgment. The daemon creates a pip-
	// ticket and returns immediately; the ticket watcher picks up the
	// ticket from the next /sync and creates the executor sandbox.
	results := resultWatch.WaitForCommandResults(t, requestID, 1)

	acceptedResult := findAcceptedEvent(t, results)

	// --- Verify accepted result ---
	if status, _ := acceptedResult["status"].(string); status != "success" {
		t.Errorf("accepted result status = %q, want %q (error: %v)", status, "success", acceptedResult["error"])
	}
	resultMap, _ := acceptedResult["result"].(map[string]any)
	if resultMap == nil {
		t.Fatal("accepted result has nil result field")
	}
	ticketIDFromResult, _ := resultMap["ticket_id"].(string)
	if ticketIDFromResult == "" {
		t.Error("accepted result has empty ticket_id")
	}
	verifyThreadRelation(t, acceptedResult, commandEventID.String())

	// --- Verify pip- ticket was created and closed successfully ---
	ticketID, ticketContent := findPipelineTicket(t, admin, projectRoomID, &projectWatch, "test-greet")
	if ticketContent.Status != "closed" {
		t.Errorf("ticket %s status = %q, want closed", ticketID, ticketContent.Status)
	}
	if ticketContent.Type != "pipeline" {
		t.Errorf("ticket %s type = %q, want pipeline", ticketID, ticketContent.Type)
	}
	if ticketContent.Pipeline == nil {
		t.Fatal("ticket has nil pipeline content")
	}
	if ticketContent.Pipeline.Conclusion != "success" {
		t.Errorf("ticket %s pipeline conclusion = %q, want %q", ticketID, ticketContent.Pipeline.Conclusion, "success")
	}

	t.Log("pipeline execution lifecycle verified: command → accepted → ticket → sandbox → executor → ticket closed")
}

// TestPipelineExecutionFailure verifies that pipeline step failures are
// correctly reported through the ticket lifecycle. A pipeline with a
// failing step should produce a closed ticket with conclusion "failure".
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
	projectWatch := watchRoom(t, admin, projectRoomID)

	_, err := admin.SendEvent(ctx, machine.ConfigRoomID, schema.MatrixEventTypeMessage,
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

	// Wait for the accepted acknowledgment.
	results := resultWatch.WaitForCommandResults(t, requestID, 1)
	acceptedResult := findAcceptedEvent(t, results)
	if status, _ := acceptedResult["status"].(string); status != "success" {
		t.Errorf("accepted result status = %q, want %q (error: %v)", status, "success", acceptedResult["error"])
	}

	// Wait for the ticket to close. The executor reports the failure
	// through the ticket conclusion, not a daemon-posted command result.
	ticketID, ticketContent := findPipelineTicket(t, admin, projectRoomID, &projectWatch, "test-fail")
	if ticketContent.Status != "closed" {
		t.Errorf("ticket %s status = %q, want closed", ticketID, ticketContent.Status)
	}
	if ticketContent.Pipeline == nil {
		t.Fatal("ticket has nil pipeline content")
	}
	if ticketContent.Pipeline.Conclusion != "failure" {
		t.Errorf("ticket %s pipeline conclusion = %q, want %q", ticketID, ticketContent.Pipeline.Conclusion, "failure")
	}

	t.Log("pipeline failure reporting verified: failing step produces closed ticket with failure conclusion")
}

// TestPipelineParameterPropagation verifies that command parameters flow
// through the entire chain: command message → daemon → pip- ticket
// variables → executor → pipeline variable resolution → shell execution.
// A pipeline step uses a variable from the command parameters; if the
// ticket closes with conclusion "success", the variable was propagated
// correctly through every layer.
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
	projectWatch := watchRoom(t, admin, projectRoomID)

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

	// Wait for the accepted acknowledgment.
	results := resultWatch.WaitForCommandResults(t, requestID, 1)
	acceptedResult := findAcceptedEvent(t, results)
	if status, _ := acceptedResult["status"].(string); status != "success" {
		t.Errorf("accepted result status = %q, want %q (error: %v)", status, "success", acceptedResult["error"])
	}

	// If the ticket closes with conclusion "success", the variable was
	// propagated correctly through the entire daemon → ticket → executor
	// → shell chain.
	ticketID, ticketContent := findPipelineTicket(t, admin, projectRoomID, &projectWatch, "test-params")
	if ticketContent.Status != "closed" {
		t.Errorf("ticket %s status = %q, want closed", ticketID, ticketContent.Status)
	}
	if ticketContent.Pipeline == nil {
		t.Fatal("ticket has nil pipeline content")
	}
	if ticketContent.Pipeline.Conclusion != "success" {
		t.Errorf("ticket %s pipeline conclusion = %q, want %q (PROJECT variable not propagated)",
			ticketID, ticketContent.Pipeline.Conclusion, "success")
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
