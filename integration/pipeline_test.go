// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	pipelinecmd "github.com/bureau-foundation/bureau/cmd/bureau/pipeline"
	"github.com/bureau-foundation/bureau/lib/command"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/testutil"
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

	// Enable pipeline execution in the project room. Without this,
	// the daemon rejects pipeline.execute commands for this room.
	if err := pipelinecmd.ConfigureRoom(ctx, slog.Default(), admin, projectRoomID, pipelinecmd.ConfigureRoomParams{}); err != nil {
		t.Fatalf("configure pipeline execution: %v", err)
	}

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
	if final.Status != ticket.StatusClosed {
		t.Errorf("ticket status = %q, want %s", final.Status, ticket.StatusClosed)
	}
	if final.Type != ticket.TypePipeline {
		t.Errorf("ticket type = %q, want %s", final.Type, ticket.TypePipeline)
	}
	if final.Pipeline == nil {
		t.Fatal("ticket has nil pipeline content")
	}
	if final.Pipeline.Conclusion != pipeline.ConclusionSuccess {
		t.Errorf("pipeline conclusion = %q, want %s", final.Pipeline.Conclusion, pipeline.ConclusionSuccess)
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

	// Enable pipeline execution in the project room.
	if err := pipelinecmd.ConfigureRoom(ctx, slog.Default(), admin, projectRoomID, pipelinecmd.ConfigureRoomParams{}); err != nil {
		t.Fatalf("configure pipeline execution: %v", err)
	}

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
	if final.Status != ticket.StatusClosed {
		t.Errorf("ticket status = %q, want %s", final.Status, ticket.StatusClosed)
	}
	if final.Pipeline == nil {
		t.Fatal("ticket has nil pipeline content")
	}
	if final.Pipeline.Conclusion != pipeline.ConclusionFailure {
		t.Errorf("pipeline conclusion = %q, want %s", final.Pipeline.Conclusion, pipeline.ConclusionFailure)
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

	// Enable pipeline execution in the project room.
	if err := pipelinecmd.ConfigureRoom(ctx, slog.Default(), admin, projectRoomID, pipelinecmd.ConfigureRoomParams{}); err != nil {
		t.Fatalf("configure pipeline execution: %v", err)
	}

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
	if final.Status != ticket.StatusClosed {
		t.Errorf("ticket status = %q, want %s", final.Status, ticket.StatusClosed)
	}
	if final.Pipeline == nil {
		t.Fatal("ticket has nil pipeline content")
	}
	if final.Pipeline.Conclusion != pipeline.ConclusionSuccess {
		t.Errorf("pipeline conclusion = %q, want %s (PROJECT variable not propagated)",
			final.Pipeline.Conclusion, pipeline.ConclusionSuccess)
	}

	t.Log("parameter propagation verified: command parameters flow through daemon -> ticket -> executor -> shell")
}

// TestPipelineArtifactAttachment exercises the full artifact attachment flow:
//
//   - Pipeline step writes a file to disk
//   - Step output is declared with artifact: true
//   - Executor stores the file in the artifact service (BUREAU_ARTIFACT_SOCKET)
//   - Executor calls add-attachment on the ticket service with the art-* ref
//   - Test reads the closed ticket and verifies the attachment fields
//
// This proves the daemon's artifact socket provisioning for pipeline sandboxes,
// the executor's artifact output capture, the add-attachment ticket service
// action, and the auto-attach wiring between the executor's step loop and the
// ticket service. The attachment appears as a TicketAttachment on the closed
// ticket's state event with the correct ref prefix, label format, and content type.
func TestPipelineArtifactAttachment(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "pipeline-attach")
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

	// --- Artifact service setup ---
	//
	// The daemon discovers the artifact service by its
	// "content-addressed-store" capability in the fleet service directory.
	// When present, the daemon bind-mounts the artifact socket into the
	// pipeline executor's sandbox and sets BUREAU_ARTIFACT_SOCKET +
	// BUREAU_ARTIFACT_TOKEN environment variables.

	artifactBinary := testutil.DataBinary(t, "ARTIFACT_SERVICE_BINARY")
	deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    artifactBinary,
		Name:      "artifact-service",
		Localpart: "service/artifact/pipe-attach",
		Command:   []string{artifactBinary, "--store-dir", "/tmp/artifacts"},
	})

	// --- Ticket service setup ---

	ticketSvc := deployTicketService(t, admin, fleet, machine, "pipe-attach")

	projectRoomID := createTicketProjectRoom(t, admin, "pipe-attach-project",
		ticketSvc.Entity, machine.UserID.String())

	if err := pipelinecmd.ConfigureRoom(ctx, slog.Default(), admin, projectRoomID, pipelinecmd.ConfigureRoomParams{}); err != nil {
		t.Fatalf("configure pipeline execution: %v", err)
	}

	// --- Publish pipeline with artifact-mode output ---
	//
	// The step writes a file, and the output declaration marks it as
	// artifact: true. The executor stores the file in the artifact
	// service, receives an art-* ref, then calls add-attachment on
	// the ticket service with label "gen-data/report" and content type
	// "text/plain".

	artifactOutputDeclaration, err := json.Marshal(pipeline.PipelineStepOutput{
		Path:        "/workspace/report.txt",
		Artifact:    true,
		ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("marshal output declaration: %v", err)
	}

	publishPipelineDefinition(t, admin, "test-artifact-attach", pipeline.PipelineContent{
		Description: "Pipeline with artifact output and ticket attachment",
		Steps: []pipeline.PipelineStep{
			{
				Name: "gen-data",
				Run:  "echo 'integration test artifact content' > /workspace/report.txt",
				Outputs: map[string]json.RawMessage{
					"report": artifactOutputDeclaration,
				},
			},
		},
	})

	// --- Execute pipeline ---

	future, err := command.Send(ctx, command.SendParams{
		Session: admin,
		RoomID:  machine.ConfigRoomID,
		Command: "pipeline.execute",
		Parameters: map[string]any{
			"pipeline": "bureau/pipeline:test-artifact-attach",
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
	if final.Status != ticket.StatusClosed {
		t.Errorf("ticket status = %q, want %s", final.Status, ticket.StatusClosed)
	}
	if final.Pipeline == nil {
		t.Fatal("ticket has nil pipeline content")
	}
	if final.Pipeline.Conclusion != pipeline.ConclusionSuccess {
		t.Errorf("pipeline conclusion = %q, want %s", final.Pipeline.Conclusion, pipeline.ConclusionSuccess)
	}

	// --- Verify ticket attachments ---
	//
	// The executor attaches two things:
	//   - The output capture log (BUREAU_LOG_REF), attached immediately
	//     after claiming the ticket
	//   - Artifact outputs from pipeline steps, attached after each step
	//     with artifact: true output declarations

	ticketContent := readTicketState(t, admin, projectRoomID, result.TicketID.String())

	if len(ticketContent.Attachments) < 2 {
		raw, _ := admin.GetStateEvent(ctx, projectRoomID, schema.EventTypeTicket, result.TicketID.String())
		t.Fatalf("expected at least 2 attachments (log + artifact), got %d; full content: %s",
			len(ticketContent.Attachments), string(raw))
	}

	// Find each attachment by type. The log attachment has content type
	// "application/x-bureau-log"; the artifact attachment has "text/plain"
	// (from the output declaration).
	var logAttachment, artifactAttachment *ticket.TicketAttachment
	for i := range ticketContent.Attachments {
		switch ticketContent.Attachments[i].ContentType {
		case "application/x-bureau-log":
			logAttachment = &ticketContent.Attachments[i]
		case "text/plain":
			artifactAttachment = &ticketContent.Attachments[i]
		}
	}

	if logAttachment == nil {
		t.Fatal("missing log attachment (application/x-bureau-log)")
	}
	if !strings.HasPrefix(logAttachment.Ref, "log/") {
		t.Errorf("log attachment ref = %q, want log/ prefix", logAttachment.Ref)
	}
	if logAttachment.Label != "output log" {
		t.Errorf("log attachment label = %q, want %q", logAttachment.Label, "output log")
	}
	t.Logf("log attachment verified: ref=%s label=%s", logAttachment.Ref, logAttachment.Label)

	if artifactAttachment == nil {
		t.Fatal("missing artifact attachment (text/plain)")
	}
	if !strings.HasPrefix(artifactAttachment.Ref, "art-") {
		t.Errorf("artifact attachment ref = %q, want art-* prefix", artifactAttachment.Ref)
	}
	if artifactAttachment.Label != "gen-data/report" {
		t.Errorf("artifact attachment label = %q, want %q", artifactAttachment.Label, "gen-data/report")
	}
	t.Logf("artifact attachment verified: ref=%s label=%s content_type=%s",
		artifactAttachment.Ref, artifactAttachment.Label, artifactAttachment.ContentType)
}
