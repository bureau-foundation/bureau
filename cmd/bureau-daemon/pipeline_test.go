// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/testutil"
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

func TestBuildPipelineExecutorSpec(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.pipelineExecutorBinary = "/nix/store/abc-executor/bin/bureau-pipeline-executor"
	daemon.pipelineEnvironment = "/nix/store/xyz-runner-env"
	daemon.workspaceRoot = "/var/bureau/workspace"

	spec := daemon.buildPipelineExecutorSpec(
		"/run/artifact.sock", "/tmp/artifact.token",
		"pip-a3f9", ref.MustParseRoomID("!pipeline-room:bureau.local"),
		"/run/ticket.sock", "/tmp/ticket.token",
	)

	// Verify command — just the executor binary, no CLI arguments.
	if len(spec.Command) != 1 {
		t.Fatalf("expected 1 command element, got %d: %v", len(spec.Command), spec.Command)
	}
	if spec.Command[0] != daemon.pipelineExecutorBinary {
		t.Errorf("command[0] = %q, want %q", spec.Command[0], daemon.pipelineExecutorBinary)
	}

	// Verify environment variables.
	if spec.EnvironmentVariables["BUREAU_SANDBOX"] != "1" {
		t.Errorf("BUREAU_SANDBOX = %q, want '1'", spec.EnvironmentVariables["BUREAU_SANDBOX"])
	}
	if _, hasResultPath := spec.EnvironmentVariables["BUREAU_RESULT_PATH"]; hasResultPath {
		t.Error("BUREAU_RESULT_PATH should not be set (executor uses tickets, not JSONL)")
	}
	if spec.EnvironmentVariables["BUREAU_TICKET_ID"] != "pip-a3f9" {
		t.Errorf("BUREAU_TICKET_ID = %q, want 'pip-a3f9'", spec.EnvironmentVariables["BUREAU_TICKET_ID"])
	}
	if spec.EnvironmentVariables["BUREAU_TICKET_ROOM"] != "!pipeline-room:bureau.local" {
		t.Errorf("BUREAU_TICKET_ROOM = %q", spec.EnvironmentVariables["BUREAU_TICKET_ROOM"])
	}

	// Verify filesystem mounts by destination rather than by index.
	findMount := func(dest string) *schema.TemplateMount {
		for index := range spec.Filesystem {
			if spec.Filesystem[index].Dest == dest {
				return &spec.Filesystem[index]
			}
		}
		return nil
	}

	// Executor binary mount.
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

	// No result file mount (executor uses tickets, not JSONL).
	if resultMount := findMount("/run/bureau/result.jsonl"); resultMount != nil {
		t.Error("unexpected result file mount — executor uses tickets for reporting")
	}

	// Workspace root mount.
	workspaceMount := findMount("/workspace")
	if workspaceMount == nil {
		t.Error("workspace root mount not found")
	} else {
		if workspaceMount.Source != "/var/bureau/workspace" {
			t.Errorf("workspace mount source = %q", workspaceMount.Source)
		}
		if workspaceMount.Mode != "rw" {
			t.Errorf("workspace mount mode = %q, want 'rw'", workspaceMount.Mode)
		}
	}

	// Artifact service mounts.
	artifactSocketMount := findMount("/run/bureau/artifact.sock")
	if artifactSocketMount == nil {
		t.Error("artifact socket mount not found")
	} else if artifactSocketMount.Source != "/run/artifact.sock" {
		t.Errorf("artifact socket source = %q", artifactSocketMount.Source)
	}

	// Ticket service mounts.
	ticketSocketMount := findMount("/run/bureau/service/ticket.sock")
	if ticketSocketMount == nil {
		t.Error("ticket socket mount not found")
	} else if ticketSocketMount.Source != "/run/ticket.sock" {
		t.Errorf("ticket socket source = %q", ticketSocketMount.Source)
	}
	ticketTokenMount := findMount("/run/bureau/service/token/ticket.token")
	if ticketTokenMount == nil {
		t.Error("ticket token mount not found")
	} else if ticketTokenMount.Source != "/tmp/ticket.token" {
		t.Errorf("ticket token source = %q", ticketTokenMount.Source)
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
}

func TestBuildPipelineExecutorSpec_NoEnvironment(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.pipelineExecutorBinary = "/usr/bin/executor"
	daemon.pipelineEnvironment = "" // No Nix environment.
	daemon.workspaceRoot = "/var/bureau/workspace"

	spec := daemon.buildPipelineExecutorSpec(
		"", "", "pip-1234", ref.MustParseRoomID("!room:test"), "/run/ticket.sock", "/tmp/ticket.token")

	if spec.EnvironmentPath != "" {
		t.Errorf("EnvironmentPath should be empty, got %q", spec.EnvironmentPath)
	}
}

func TestBuildPipelineExecutorSpec_NoArtifact(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.pipelineExecutorBinary = "/usr/bin/executor"
	daemon.workspaceRoot = "/var/bureau/workspace"

	// Empty artifact socket/token paths — no artifact service available.
	spec := daemon.buildPipelineExecutorSpec(
		"", "", "pip-1234", ref.MustParseRoomID("!room:test"), "/run/ticket.sock", "/tmp/ticket.token")

	// Verify no artifact mounts are present.
	for _, mount := range spec.Filesystem {
		if strings.Contains(mount.Dest, "artifact") {
			t.Errorf("unexpected artifact mount: %+v", mount)
		}
	}

	// Verify ticket mounts ARE present (always required).
	var hasTicketSocket bool
	for _, mount := range spec.Filesystem {
		if mount.Dest == "/run/bureau/service/ticket.sock" {
			hasTicketSocket = true
		}
	}
	if !hasTicketSocket {
		t.Error("ticket socket mount not found")
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
		"bureau/pipeline:test", "!tickets:bureau.local", "req-1")

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

	// Set up token signing so createPipelineTicket can mint a service
	// token for the ticket create call.
	_, signingKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	harness.daemon.tokenSigningPrivateKey = signingKey

	// Set up a mock ticket service. The handler creates pip- tickets
	// and returns the ticket ID and room.
	socketDir := testutil.SocketDir(t)
	harness.daemon.runDir = socketDir
	harness.daemon.fleetRunDir = harness.daemon.fleet.RunDir(socketDir)

	ticketEntity := testEntity(t, harness.daemon.fleet, "service/ticket/workspace")
	ticketSocketPath := ticketEntity.ServiceSocketPath(harness.daemon.fleetRunDir)

	// Create parent directories for the socket path.
	if err := os.MkdirAll(filepath.Dir(ticketSocketPath), 0755); err != nil {
		t.Fatal(err)
	}

	// Register the ticket service in the daemon's service directory
	// so findLocalTicketSocket() discovers it.
	harness.daemon.services["service/ticket/workspace"] = &schema.Service{
		Principal:    ticketEntity,
		Machine:      harness.daemon.machine,
		Capabilities: []string{"dependency-graph"},
		Protocol:     "cbor",
	}

	// Start a mock ticket service that handles the "create" action.
	mockServer := service.NewSocketServer(ticketSocketPath,
		slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)
	mockServer.Handle("create", func(ctx context.Context, raw []byte) (any, error) {
		return ticketCreateResponse{
			ID:   "pip-test-1234",
			Room: "!tickets:bureau.local",
		}, nil
	})

	serverCtx, serverCancel := context.WithCancel(context.Background())
	t.Cleanup(serverCancel)
	go mockServer.Serve(serverCtx)
	<-mockServer.Ready()

	const roomID = "!workspace:test"
	harness.matrixState.setStateEvent(roomID, schema.MatrixEventTypePowerLevels, "", map[string]any{
		"users": map[string]any{
			"@admin:bureau.local": float64(100),
		},
	})

	// Pipeline execution requires m.bureau.pipeline_config in the
	// target room. Without it, handlePipelineExecute rejects the
	// command before discovering the ticket service.
	harness.matrixState.setStateEvent("!tickets:bureau.local",
		schema.EventTypePipelineConfig, "", map[string]any{"version": 1})

	event := buildPipelineCommandEvent(ref.MustParseEventID("$pipe3"), ref.MustParseUserID("@admin:bureau.local"),
		"bureau/pipeline:dev-init", "!tickets:bureau.local", "req-accepted")

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, mustRoomID(roomID), []messaging.Event{event})

	// The handler creates a pip- ticket and returns "accepted" with
	// the ticket ID. No goroutine, no sandbox creation — the ticket
	// watcher picks up the ticket from the next /sync.
	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected exactly 1 sent message, got %d", len(messages))
	}

	message := messages[0]
	if message.Content["status"] != "success" {
		t.Errorf("status = %v, want 'success' (error: %v)", message.Content["status"], message.Content["error"])
	}

	result, ok := message.Content["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %v", message.Content["result"])
	}
	if result["status"] != "accepted" {
		t.Errorf("result.status = %v, want 'accepted'", result["status"])
	}
	ticketID, _ := result["ticket_id"].(string)
	if ticketID != "pip-test-1234" {
		t.Errorf("result.ticket_id = %q, want %q", ticketID, "pip-test-1234")
	}
	room, _ := result["room"].(string)
	if room != "!tickets:bureau.local" {
		t.Errorf("result.room = %q, want %q", room, "!tickets:bureau.local")
	}
}

func TestHandlePipelineExecute_RoomNotEnabled(t *testing.T) {
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

	// Deliberately do NOT set pipeline_config on the target room.
	// The handler should reject the command with a helpful message.
	event := buildPipelineCommandEvent(ref.MustParseEventID("$pipe-noconfig"), ref.MustParseUserID("@admin:bureau.local"),
		"bureau/pipeline:test", "!tickets:bureau.local", "req-noconfig")

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
	if !strings.Contains(errorText, "pipeline execution not enabled") {
		t.Errorf("error should mention pipeline not enabled, got: %q", errorText)
	}
	if !strings.Contains(errorText, "bureau pipeline enable") {
		t.Errorf("error should suggest 'bureau pipeline enable', got: %q", errorText)
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
		"bureau/pipeline:test", "!tickets:bureau.local", "req-denied")

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

func TestHandlePipelineExecute_PipelineRefRejected(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

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

	// Send "pipeline_ref" instead of "pipeline" — the old parameter
	// name is no longer supported; "pipeline" is required.
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

	message := messages[0]
	if message.Content["status"] != "error" {
		t.Errorf("status = %v, want 'error'", message.Content["status"])
	}
	errorText, _ := message.Content["error"].(string)
	if !strings.Contains(errorText, "'pipeline' is required") {
		t.Errorf("error should mention missing 'pipeline' parameter, got: %q", errorText)
	}
}

func TestHandlePipelineExecute_MissingRoom(t *testing.T) {
	t.Parallel()
	harness := newCommandTestHarness(t)

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

	// Pipeline is provided but room is missing.
	event := messaging.Event{
		EventID: ref.MustParseEventID("$pipe6"),
		Type:    schema.MatrixEventTypeMessage,
		Sender:  ref.MustParseUserID("@admin:bureau.local"),
		Content: map[string]any{
			"msgtype": schema.MsgTypeCommand,
			"body":    "pipeline.execute",
			"command": "pipeline.execute",
			"parameters": map[string]any{
				"pipeline": "bureau/pipeline:test",
			},
		},
	}

	ctx := context.Background()
	harness.daemon.processCommandMessages(ctx, mustRoomID(roomID), []messaging.Event{event})

	messages := harness.getSentMessages()
	if len(messages) != 1 {
		t.Fatalf("expected exactly 1 sent message, got %d", len(messages))
	}

	message := messages[0]
	if message.Content["status"] != "error" {
		t.Errorf("status = %v, want 'error'", message.Content["status"])
	}
	errorText, _ := message.Content["error"].(string)
	if !strings.Contains(errorText, "'room' is required") {
		t.Errorf("error should mention missing 'room' parameter, got: %q", errorText)
	}
}

// buildPipelineCommandEvent creates a messaging.Event for a
// pipeline.execute command with pipeline ref and room parameters.
func buildPipelineCommandEvent(eventID ref.EventID, sender ref.UserID, pipelineRef, ticketRoom, requestID string) messaging.Event {
	content := map[string]any{
		"msgtype": schema.MsgTypeCommand,
		"body":    "pipeline.execute " + pipelineRef,
		"command": "pipeline.execute",
		"parameters": map[string]any{
			"pipeline": pipelineRef,
			"room":     ticketRoom,
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
