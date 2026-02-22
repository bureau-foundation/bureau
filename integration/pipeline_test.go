// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"os"
	"testing"

	"github.com/bureau-foundation/bureau/lib/command"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
)

// TestPipelineExecution exercises the full pipeline execution lifecycle
// through real processes:
//
//   - Admin sends a pipeline.execute command via command.Send
//   - Daemon picks it up via /sync, posts "accepted" with ticket ID
//   - Ticket watcher creates a bwrap sandbox running bureau-pipeline-executor
//   - Executor claims the ticket, runs pipeline steps, posts notes, closes ticket
//   - Test verifies accepted result and closed ticket with correct conclusion
//
// This proves the daemon->launcher IPC, bwrap sandbox creation, ticket-driven
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

	// --- Execute pipeline via production code path ---
	future, err := command.Send(ctx, command.SendParams{
		Session: admin,
		RoomID:  machine.ConfigRoomID,
		Command: "pipeline.execute",
		Parameters: map[string]any{
			"pipeline": "bureau/pipeline:test-greet",
			"room":     projectRoomID.String(),
		},
	})
	if err != nil {
		t.Fatalf("send pipeline.execute: %v", err)
	}
	defer future.Discard()

	result, err := future.Wait(ctx)
	if err != nil {
		t.Fatalf("wait for accepted result: %v", err)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("pipeline.execute failed: %v", err)
	}
	if result.TicketID.IsZero() {
		t.Fatal("accepted result has empty ticket_id")
	}
	t.Logf("pipeline.execute accepted, ticket: %s in %s", result.TicketID, result.TicketRoom)

	// --- Watch ticket to completion ---
	final, err := command.WatchTicket(ctx, command.WatchTicketParams{
		Session:  admin,
		RoomID:   result.TicketRoom,
		TicketID: result.TicketID,
	})
	if err != nil {
		t.Fatalf("watching pipeline ticket: %v", err)
	}
	if final.Status != "closed" {
		t.Errorf("ticket status = %q, want closed", final.Status)
	}
	if final.Type != "pipeline" {
		t.Errorf("ticket type = %q, want pipeline", final.Type)
	}
	if final.Pipeline == nil {
		t.Fatal("ticket has nil pipeline content")
	}
	if final.Pipeline.Conclusion != "success" {
		t.Errorf("pipeline conclusion = %q, want success", final.Pipeline.Conclusion)
	}

	t.Log("pipeline execution lifecycle verified: command -> accepted -> ticket -> sandbox -> executor -> ticket closed")
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

	// --- Execute pipeline via production code path ---
	future, err := command.Send(ctx, command.SendParams{
		Session: admin,
		RoomID:  machine.ConfigRoomID,
		Command: "pipeline.execute",
		Parameters: map[string]any{
			"pipeline": "bureau/pipeline:test-fail",
			"room":     projectRoomID.String(),
		},
	})
	if err != nil {
		t.Fatalf("send pipeline.execute: %v", err)
	}
	defer future.Discard()

	result, err := future.Wait(ctx)
	if err != nil {
		t.Fatalf("wait for accepted result: %v", err)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("pipeline.execute failed: %v", err)
	}
	if result.TicketID.IsZero() {
		t.Fatal("accepted result has empty ticket_id")
	}
	t.Logf("pipeline.execute accepted, ticket: %s", result.TicketID)

	// --- Watch ticket to completion — expect failure ---
	final, err := command.WatchTicket(ctx, command.WatchTicketParams{
		Session:  admin,
		RoomID:   result.TicketRoom,
		TicketID: result.TicketID,
	})
	if err != nil {
		t.Fatalf("watching pipeline ticket: %v", err)
	}
	if final.Status != "closed" {
		t.Errorf("ticket status = %q, want closed", final.Status)
	}
	if final.Pipeline == nil {
		t.Fatal("ticket has nil pipeline content")
	}
	if final.Pipeline.Conclusion != "failure" {
		t.Errorf("pipeline conclusion = %q, want failure", final.Pipeline.Conclusion)
	}

	t.Log("pipeline failure reporting verified: failing step produces closed ticket with failure conclusion")
}

// TestPipelineParameterPropagation verifies that command parameters flow
// through the entire chain: command message -> daemon -> pip- ticket
// variables -> executor -> pipeline variable resolution -> shell execution.
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

	// --- Execute pipeline with variable via production code path ---
	future, err := command.Send(ctx, command.SendParams{
		Session: admin,
		RoomID:  machine.ConfigRoomID,
		Command: "pipeline.execute",
		Parameters: map[string]any{
			"pipeline": "bureau/pipeline:test-params",
			"room":     projectRoomID.String(),
			// This top-level parameter becomes a ticket variable.
			// The executor reads it from TicketContent.Pipeline.Variables,
			// and ResolveVariables makes it available as ${PROJECT}.
			"PROJECT": "iree",
		},
	})
	if err != nil {
		t.Fatalf("send pipeline.execute: %v", err)
	}
	defer future.Discard()

	result, err := future.Wait(ctx)
	if err != nil {
		t.Fatalf("wait for accepted result: %v", err)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("pipeline.execute failed: %v", err)
	}
	if result.TicketID.IsZero() {
		t.Fatal("accepted result has empty ticket_id")
	}
	t.Logf("pipeline.execute accepted, ticket: %s", result.TicketID)

	// --- Watch ticket — success proves variable propagation ---
	final, err := command.WatchTicket(ctx, command.WatchTicketParams{
		Session:  admin,
		RoomID:   result.TicketRoom,
		TicketID: result.TicketID,
	})
	if err != nil {
		t.Fatalf("watching pipeline ticket: %v", err)
	}
	if final.Status != "closed" {
		t.Errorf("ticket status = %q, want closed", final.Status)
	}
	if final.Pipeline == nil {
		t.Fatal("ticket has nil pipeline content")
	}
	if final.Pipeline.Conclusion != "success" {
		t.Errorf("pipeline conclusion = %q, want success (PROJECT variable not propagated)",
			final.Pipeline.Conclusion)
	}

	t.Log("parameter propagation verified: command parameters flow through daemon -> ticket -> executor -> shell")
}
