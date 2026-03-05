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
	"github.com/bureau-foundation/bureau/lib/credential"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/templatedef"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
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

	ns := setupTestNamespace(t)
	admin := ns.Admin

	fleet := createTestFleet(t, admin, ns)

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
	publishPipelineDefinition(t, admin, ns.Namespace, "test-greet", pipeline.PipelineContent{
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
			"pipeline": ns.Namespace.PipelineRoomAliasLocalpart() + ":test-greet",
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

	ns := setupTestNamespace(t)
	admin := ns.Admin

	fleet := createTestFleet(t, admin, ns)

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
	publishPipelineDefinition(t, admin, ns.Namespace, "test-fail", pipeline.PipelineContent{
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
			"pipeline": ns.Namespace.PipelineRoomAliasLocalpart() + ":test-fail",
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

	ns := setupTestNamespace(t)
	admin := ns.Admin

	fleet := createTestFleet(t, admin, ns)

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
	publishPipelineDefinition(t, admin, ns.Namespace, "test-params", pipeline.PipelineContent{
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
			"pipeline": ns.Namespace.PipelineRoomAliasLocalpart() + ":test-params",
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

	ns := setupTestNamespace(t)
	admin := ns.Admin

	fleet := createTestFleet(t, admin, ns)

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

	publishPipelineDefinition(t, admin, ns.Namespace, "test-artifact-attach", pipeline.PipelineContent{
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
			"pipeline": ns.Namespace.PipelineRoomAliasLocalpart() + ":test-artifact-attach",
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

// TestPipelineTemplateCredentials exercises the full template credential
// injection path through pipeline execution:
//
//   - Admin provisions fleet-level credentials encrypted to the machine's
//     age key via credential.FleetProvision (published to the fleet room)
//   - Admin pushes a template with credential_ref pointing to the fleet
//     room (using ${FLEET_ROOM} variable), allowed_pipelines restricting
//     usage, and a secret binding mapping the credential key to an env var
//   - Admin publishes a pipeline that verifies the secret env var is set
//   - Admin sends pipeline.execute with the template parameter
//   - Daemon resolves the template, reads encrypted credentials from the
//     fleet room, authorizes the sender (admin has PL 100 in fleet room),
//     checks allowed_pipelines, and passes everything to the launcher
//   - Launcher decrypts credentials with the machine's age key, maps
//     secret bindings to env vars, and starts the executor in a sandbox
//   - Pipeline step verifies the env var has the expected value
//
// Success proves the entire credential chain: fleet provisioning, variable
// substitution, template resolution, authorization, IPC delivery, launcher
// decryption, secret binding mapping, and sandbox environment injection.
func TestPipelineTemplateCredentials(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin

	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "pipe-tmpl-cred")
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
	ticketSvc := deployTicketService(t, admin, fleet, machine, "tmpl-cred")

	projectRoomID := createTicketProjectRoom(t, admin, "tmpl-cred-project",
		ticketSvc.Entity, machine.UserID.String())

	if err := pipelinecmd.ConfigureRoom(ctx, slog.Default(), admin, projectRoomID, pipelinecmd.ConfigureRoomParams{}); err != nil {
		t.Fatalf("configure pipeline execution: %v", err)
	}

	// --- Provision fleet-level credentials ---
	//
	// The credentials are stored in the fleet config room as an
	// m.bureau.credentials state event. The machine's age key is one
	// of the recipients, so the launcher can decrypt them at sandbox
	// creation time.

	_, err := credential.FleetProvision(ctx, admin, credential.FleetProvisionParams{
		Fleet:         fleet.Ref,
		TargetRoomID:  fleet.FleetRoomID,
		StateKey:      "test-cred",
		MachineRoomID: fleet.MachineRoomID,
		Credentials: map[string]string{
			"SECRET_VALUE": "integration-test-secret-42",
		},
	})
	if err != nil {
		t.Fatalf("fleet provision credentials: %v", err)
	}

	// --- Publish template with credential_ref ---
	//
	// The template binds the fleet credential to this pipeline's
	// sandbox via a secret mapping: key "SECRET_VALUE" from the
	// encrypted bundle becomes env var "MY_SECRET" in the sandbox.
	// ${FLEET_ROOM} is substituted by the daemon at resolution time.
	// allowed_pipelines restricts which pipelines may use these
	// credentials.

	grantTemplateAccess(t, admin, machine)

	templateRefString := ns.Namespace.TemplateRoomAliasLocalpart() + ":test-cred-template"
	templateRef, err := schema.ParseTemplateRef(templateRefString)
	if err != nil {
		t.Fatalf("parse template ref: %v", err)
	}
	_, err = templatedef.Push(ctx, admin, templateRef, schema.TemplateContent{
		Description:   "Template with fleet credential binding",
		CredentialRef: "${FLEET_ROOM}:test-cred",
		AllowedPipelines: &[]string{
			"test-secret-check",
		},
		Secrets: []schema.SecretBinding{
			{Key: "SECRET_VALUE", Env: "MY_SECRET"},
		},
	}, testServer)
	if err != nil {
		t.Fatalf("push template: %v", err)
	}

	// --- Publish pipeline that verifies the secret ---

	publishPipelineDefinition(t, admin, ns.Namespace, "test-secret-check", pipeline.PipelineContent{
		Description: "Verify template credential injection",
		Steps: []pipeline.PipelineStep{
			// test(1) returns 0 if the comparison is true. If the
			// credential was not decrypted, mapped, or injected, this
			// step fails and the pipeline closes with "failure".
			// Use bare $MY_SECRET (no braces): the pipeline executor
			// expands ${NAME} as pipeline variables before shell
			// execution; $NAME is left for shell interpretation.
			{Name: "verify-secret", Run: `test "$MY_SECRET" = "integration-test-secret-42"`},
		},
	})

	// --- Execute pipeline with template ---

	future, err := command.Send(ctx, command.SendParams{
		Session: admin,
		RoomID:  machine.ConfigRoomID,
		Command: "pipeline.execute",
		Parameters: map[string]any{
			"pipeline": ns.Namespace.PipelineRoomAliasLocalpart() + ":test-secret-check",
			"room":     projectRoomID.String(),
			"template": templateRefString,
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
		t.Errorf("pipeline conclusion = %q, want %s (credential not injected?)",
			final.Pipeline.Conclusion, pipeline.ConclusionSuccess)
	}

	t.Log("template credential injection verified: fleet credentials decrypted and injected as env var in pipeline sandbox")
}

// TestPipelineTemplateAllowedPipelinesEnforced verifies that the daemon
// rejects pipeline.execute when the requested pipeline is not in the
// template's allowed_pipelines list. The command result should be an
// error, not an accepted acknowledgment — the rejection happens before
// any ticket is created.
//
// This proves the allowed_pipelines scope restriction works end-to-end:
// a template's credentials cannot be used by arbitrary pipelines even
// when the sender has full credential access.
func TestPipelineTemplateAllowedPipelinesEnforced(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin

	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "pipe-allowed")
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
	ticketSvc := deployTicketService(t, admin, fleet, machine, "allowed")

	projectRoomID := createTicketProjectRoom(t, admin, "allowed-project",
		ticketSvc.Entity, machine.UserID.String())

	if err := pipelinecmd.ConfigureRoom(ctx, slog.Default(), admin, projectRoomID, pipelinecmd.ConfigureRoomParams{}); err != nil {
		t.Fatalf("configure pipeline execution: %v", err)
	}

	// --- Provision fleet credentials ---

	_, err := credential.FleetProvision(ctx, admin, credential.FleetProvisionParams{
		Fleet:         fleet.Ref,
		TargetRoomID:  fleet.FleetRoomID,
		StateKey:      "restricted-cred",
		MachineRoomID: fleet.MachineRoomID,
		Credentials: map[string]string{
			"TOKEN": "should-not-be-accessible",
		},
	})
	if err != nil {
		t.Fatalf("fleet provision credentials: %v", err)
	}

	// --- Template with restricted allowed_pipelines ---
	//
	// The template only allows "only-this-pipeline". Any other
	// pipeline ref should be rejected.

	grantTemplateAccess(t, admin, machine)

	templateRefString := ns.Namespace.TemplateRoomAliasLocalpart() + ":restricted-tmpl"
	templateRef, err := schema.ParseTemplateRef(templateRefString)
	if err != nil {
		t.Fatalf("parse template ref: %v", err)
	}
	_, err = templatedef.Push(ctx, admin, templateRef, schema.TemplateContent{
		Description:   "Template restricted to specific pipeline",
		CredentialRef: "${FLEET_ROOM}:restricted-cred",
		AllowedPipelines: &[]string{
			"only-this-pipeline",
		},
		Secrets: []schema.SecretBinding{
			{Key: "TOKEN", Env: "RESTRICTED_TOKEN"},
		},
	}, testServer)
	if err != nil {
		t.Fatalf("push template: %v", err)
	}

	// --- Publish a different pipeline ---

	publishPipelineDefinition(t, admin, ns.Namespace, "wrong-pipeline", pipeline.PipelineContent{
		Description: "Pipeline not in allowed_pipelines",
		Steps: []pipeline.PipelineStep{
			{Name: "should-not-run", Run: "echo this should never execute"},
		},
	})

	// --- Execute with mismatched pipeline and template ---

	future, err := command.Send(ctx, command.SendParams{
		Session: admin,
		RoomID:  machine.ConfigRoomID,
		Command: "pipeline.execute",
		Parameters: map[string]any{
			"pipeline": ns.Namespace.PipelineRoomAliasLocalpart() + ":wrong-pipeline",
			"room":     projectRoomID.String(),
			"template": templateRefString,
		},
	})
	if err != nil {
		t.Fatalf("send pipeline.execute: %v", err)
	}
	defer future.Discard()

	result, err := future.Wait(ctx)
	if err != nil {
		t.Fatalf("wait for result: %v", err)
	}

	// The daemon should reject this before creating a ticket.
	if !result.IsError() {
		t.Fatalf("expected error result for disallowed pipeline, got status %q", result.Status)
	}
	if !strings.Contains(result.Error, "allowed_pipelines") {
		t.Errorf("error message should mention allowed_pipelines, got: %s", result.Error)
	}
	if !result.TicketID.IsZero() {
		t.Error("rejected command should not create a ticket")
	}

	t.Logf("allowed_pipelines enforcement verified: %s", result.Error)
}

// TestPipelineTemplateCredentialDenied verifies that the daemon rejects
// pipeline.execute when neither the command sender nor the template
// author has credential access in the credential room.
//
// Setup: create a dedicated credential room where the admin's power
// level is demoted to 50 (below the admin threshold of 100). The admin
// is both the command sender and the template author, so both credential
// access checks fail. The daemon returns an error before creating any
// ticket.
//
// This proves the two-actor authorization model works: even when the
// sender has full control of the machine and config room (PL 100 there),
// they cannot access credentials in a room where they lack authorization.
func TestPipelineTemplateCredentialDenied(t *testing.T) {
	t.Parallel()

	ns := setupTestNamespace(t)
	admin := ns.Admin

	fleet := createTestFleet(t, admin, ns)

	machine := newTestMachine(t, fleet, "pipe-cred-deny")
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
	ticketSvc := deployTicketService(t, admin, fleet, machine, "creddeny")

	projectRoomID := createTicketProjectRoom(t, admin, "creddeny-project",
		ticketSvc.Entity, machine.UserID.String())

	if err := pipelinecmd.ConfigureRoom(ctx, slog.Default(), admin, projectRoomID, pipelinecmd.ConfigureRoomParams{}); err != nil {
		t.Fatalf("configure pipeline execution: %v", err)
	}

	// --- Create a dedicated credential room with restricted access ---
	//
	// The admin creates the room (PL 100), provisions credentials,
	// invites the machine (so the daemon can resolve the alias and
	// read credentials), then demotes themselves to PL 50. After
	// demotion, neither the admin (command sender) nor the admin
	// (template author) has PL 100 in this room.

	credRoomAlias := ns.Namespace.SpaceAliasLocalpart() + "/restricted-creds"
	credRoom, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:  "restricted-credentials",
		Alias: credRoomAlias,
	})
	if err != nil {
		t.Fatalf("create credential room: %v", err)
	}

	// Invite the machine so the daemon can resolve the alias and read
	// state. The machine needs membership to call ResolveAlias and
	// GetStateEvent on this room.
	if err := admin.InviteUser(ctx, credRoom.RoomID, machine.UserID); err != nil {
		t.Fatalf("invite machine to credential room: %v", err)
	}

	// Provision credentials while admin still has PL 100.
	_, err = credential.FleetProvision(ctx, admin, credential.FleetProvisionParams{
		Fleet:         fleet.Ref,
		TargetRoomID:  credRoom.RoomID,
		StateKey:      "restricted",
		MachineRoomID: fleet.MachineRoomID,
		Credentials: map[string]string{
			"PROTECTED_TOKEN": "should-not-leak",
		},
	})
	if err != nil {
		t.Fatalf("fleet provision credentials: %v", err)
	}

	// Demote admin to PL 50 in the credential room. This requires
	// writing a power_levels state event with the admin's level
	// lowered. After this, the admin cannot modify power levels or
	// write state events in this room (but can still read state).
	if err := schema.GrantPowerLevels(ctx, admin, credRoom.RoomID,
		schema.PowerLevelGrants{
			Users: map[ref.UserID]int{
				admin.UserID(): 50,
			},
		}); err != nil {
		t.Fatalf("demote admin in credential room: %v", err)
	}

	// --- Publish template with credential_ref to the restricted room ---
	//
	// The credential_ref uses a literal room alias (not ${FLEET_ROOM})
	// pointing to the restricted credential room.

	grantTemplateAccess(t, admin, machine)

	templateRefString := ns.Namespace.TemplateRoomAliasLocalpart() + ":denied-cred-tmpl"
	templateRef, err := schema.ParseTemplateRef(templateRefString)
	if err != nil {
		t.Fatalf("parse template ref: %v", err)
	}
	_, err = templatedef.Push(ctx, admin, templateRef, schema.TemplateContent{
		Description:   "Template with restricted credential access",
		CredentialRef: credRoomAlias + ":restricted",
		Secrets: []schema.SecretBinding{
			{Key: "PROTECTED_TOKEN", Env: "LEAKED_TOKEN"},
		},
	}, testServer)
	if err != nil {
		t.Fatalf("push template: %v", err)
	}

	// --- Publish a pipeline ---

	publishPipelineDefinition(t, admin, ns.Namespace, "denied-pipeline", pipeline.PipelineContent{
		Description: "Pipeline that should be denied credentials",
		Steps: []pipeline.PipelineStep{
			{Name: "should-not-run", Run: "echo this should never execute"},
		},
	})

	// --- Execute pipeline — expect credential access denied ---

	future, err := command.Send(ctx, command.SendParams{
		Session: admin,
		RoomID:  machine.ConfigRoomID,
		Command: "pipeline.execute",
		Parameters: map[string]any{
			"pipeline": ns.Namespace.PipelineRoomAliasLocalpart() + ":denied-pipeline",
			"room":     projectRoomID.String(),
			"template": templateRefString,
		},
	})
	if err != nil {
		t.Fatalf("send pipeline.execute: %v", err)
	}
	defer future.Discard()

	result, err := future.Wait(ctx)
	if err != nil {
		t.Fatalf("wait for result: %v", err)
	}

	// The daemon should reject this with a credential access error
	// before creating any ticket.
	if !result.IsError() {
		t.Fatalf("expected error result for denied credentials, got status %q", result.Status)
	}
	if !strings.Contains(result.Error, "credential access denied") {
		t.Errorf("error message should mention credential access denied, got: %s", result.Error)
	}
	if !result.TicketID.IsZero() {
		t.Error("rejected command should not create a ticket")
	}

	t.Logf("credential access denial verified: %s", result.Error)
}
