// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Pipeline execution: the daemon-side integration that spawns an
// ephemeral sandbox running bureau-pipeline-executor, waits for it to
// finish, reads the JSONL result file, and posts structured results as
// threaded Matrix replies.
//
// The pipeline executor runs in a proper bwrap sandbox with its own
// proxy process (holding the daemon's Matrix token via DirectCredentials).
// The result file is a host-side temporary file bind-mounted RW into the
// sandbox so the daemon can read it after the sandbox exits.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// maxUnixSocketPathLength is the maximum length of a Unix domain socket
// path. On Linux, struct sockaddr_un.sun_path is 108 bytes, but the
// last byte must be a null terminator, giving 107 usable bytes.
const maxUnixSocketPathLength = 107

// handlePipelineExecute validates the pipeline.execute command,
// starts an async goroutine for the execution lifecycle, and returns
// an "accepted" result immediately. The goroutine handles sandbox
// creation, waiting, result reading, threaded result posting, and
// cleanup.
func handlePipelineExecute(ctx context.Context, d *Daemon, roomID ref.RoomID, eventID ref.EventID, command schema.CommandMessage) (any, error) {
	if d.pipelineExecutorBinary == "" {
		return nil, fmt.Errorf("daemon not configured for pipeline execution (--pipeline-executor-binary not set)")
	}

	// Validate the binary exists and is executable.
	info, err := os.Stat(d.pipelineExecutorBinary)
	if err != nil {
		return nil, fmt.Errorf("pipeline executor binary: %w", err)
	}
	if info.Mode()&0111 == 0 {
		return nil, fmt.Errorf("pipeline executor binary is not executable: %s", d.pipelineExecutorBinary)
	}

	// Extract the pipeline reference from command parameters. The
	// pipeline can be specified as a pipeline ref (room:template), a
	// file path, or inline in the payload. The executor handles all
	// three — we just need to pass it through.
	pipelineRef, _ := command.Parameters["pipeline"].(string)
	if pipelineRef == "" {
		// When no explicit pipeline ref is provided, the executor
		// falls back to payload-based resolution (pipeline_ref or
		// pipeline_inline keys).
		if command.Parameters["pipeline_ref"] == nil && command.Parameters["pipeline_inline"] == nil {
			return nil, fmt.Errorf("parameter 'pipeline', 'pipeline_ref', or 'pipeline_inline' is required")
		}
	}

	// Generate an ephemeral principal localpart. The counter ensures
	// uniqueness; the pipeline ref is logged separately for context.
	localpart := d.pipelineLocalpart()

	d.logger.Info("pipeline.execute accepted",
		"room_id", roomID,
		"localpart", localpart,
		"pipeline", pipelineRef,
		"request_id", command.RequestID,
	)

	// Start async execution. Uses the daemon's shutdown context (not
	// the sync cycle's context) so the pipeline survives across sync
	// iterations but cancels on daemon shutdown.
	go d.executePipeline(d.shutdownCtx, roomID, eventID, command, localpart, pipelineRef)

	return map[string]any{
		"status":    "accepted",
		"principal": localpart,
	}, nil
}

// pipelineLocalpart generates a unique principal localpart for a
// pipeline execution. Uses a monotonic counter for short, unique IDs.
// The pipeline ref and other human-readable context live in log
// messages, not the localpart — socket path length is the constraint.
func (d *Daemon) pipelineLocalpart() string {
	id := d.ephemeralCounter.Add(1)
	return fmt.Sprintf("pipeline/%d", id)
}

// executePipeline is the async lifecycle for a pipeline.execute
// command. It creates an ephemeral sandbox, waits for the executor
// to finish, reads the JSONL result file, posts the result as a
// threaded Matrix reply, and cleans up the sandbox.
func (d *Daemon) executePipeline(
	ctx context.Context,
	roomID ref.RoomID, commandEventID ref.EventID,
	command schema.CommandMessage,
	localpart, pipelineRef string,
) {
	// Exit immediately if the daemon is already shutting down. The
	// goroutine was started in handlePipelineExecute; by the time it
	// is scheduled, the shutdown context may have been cancelled.
	if err := ctx.Err(); err != nil {
		d.logger.Info("pipeline execution cancelled before start",
			"localpart", localpart, "reason", err)
		return
	}

	// Construct a fleet-scoped Entity from the pipeline localpart so
	// destroyPipelineSandbox can use it with Entity-keyed daemon maps.
	pipelineEntity, err := ref.NewEntityFromAccountLocalpart(d.fleet, localpart)
	if err != nil {
		d.logger.Error("invalid pipeline localpart",
			"localpart", localpart, "error", err)
		return
	}

	// Validate that the admin socket path (the longest variant) fits
	// within the Unix sun_path limit. This catches misconfiguration
	// (overlong fleet names, non-default run dirs) at pipeline start
	// rather than letting it surface as a cryptic "bind: invalid
	// argument" inside the launcher.
	adminSocketPath := pipelineEntity.AdminSocketPath(d.fleetRunDir)
	if length := len(adminSocketPath); length > maxUnixSocketPathLength {
		d.postPipelineError(ctx, roomID, commandEventID, command, time.Now(),
			fmt.Sprintf("admin socket path exceeds Unix limit: %d bytes > %d max (%s)",
				length, maxUnixSocketPathLength, adminSocketPath))
		return
	}

	start := time.Now()

	// Create a temp directory for the result file. This lives on the
	// host filesystem and is bind-mounted into the sandbox as RW. The
	// daemon reads it after the sandbox exits.
	resultDirectory, err := os.MkdirTemp("", "bureau-pipeline-result-*")
	if err != nil {
		d.postPipelineError(ctx, roomID, commandEventID, command, start,
			fmt.Sprintf("creating result temp directory: %v", err))
		return
	}
	defer os.RemoveAll(resultDirectory)

	resultFilePath := filepath.Join(resultDirectory, "result.jsonl")
	// Create the empty file so bwrap can bind-mount it.
	if err := os.WriteFile(resultFilePath, nil, 0644); err != nil {
		d.postPipelineError(ctx, roomID, commandEventID, command, start,
			fmt.Sprintf("creating result file: %v", err))
		return
	}

	// Discover and prepare artifact service access for the sandbox.
	// Best-effort: if no artifact service is running, the executor
	// runs without artifact support and only inline outputs work.
	var artifactSocketPath, artifactTokenPath string
	if socketPath := d.findLocalArtifactSocket(); socketPath != "" {
		token := &servicetoken.Token{
			Subject:   pipelineEntity.UserID(),
			Machine:   d.machine,
			Audience:  "artifact",
			Grants:    []servicetoken.Grant{{Actions: []string{"artifact/store"}}},
			IssuedAt:  d.clock.Now().Unix(),
			ExpiresAt: d.clock.Now().Add(1 * time.Hour).Unix(),
		}
		tokenBytes, mintErr := servicetoken.Mint(d.tokenSigningPrivateKey, token)
		if mintErr != nil {
			d.logger.Warn("failed to mint artifact service token for pipeline executor",
				"error", mintErr)
		} else {
			artifactTokenPath = filepath.Join(resultDirectory, "artifact.token")
			if writeErr := os.WriteFile(artifactTokenPath, tokenBytes, 0600); writeErr != nil {
				d.logger.Warn("failed to write artifact token file",
					"path", artifactTokenPath, "error", writeErr)
				artifactTokenPath = ""
			} else {
				artifactSocketPath = socketPath
			}
		}
	}

	// Build the sandbox spec for the pipeline executor.
	spec := d.buildPipelineExecutorSpec(pipelineRef, resultFilePath, artifactSocketPath, artifactTokenPath, command)

	// Build credentials from the daemon's own Matrix session. The
	// pipeline executor talks to Matrix through its proxy, which needs
	// a valid token.
	credentials := map[string]string{
		"MATRIX_TOKEN":   d.session.AccessToken(),
		"MATRIX_USER_ID": d.machine.UserID().String(),
	}

	// Create the ephemeral sandbox.
	response, err := d.launcherRequest(ctx, launcherIPCRequest{
		Action:            "create-sandbox",
		Principal:         localpart,
		DirectCredentials: credentials,
		SandboxSpec:       spec,
	})
	if err != nil {
		d.postPipelineError(ctx, roomID, commandEventID, command, start,
			fmt.Sprintf("create-sandbox IPC failed: %v", err))
		return
	}
	if !response.OK {
		d.postPipelineError(ctx, roomID, commandEventID, command, start,
			fmt.Sprintf("create-sandbox rejected: %s", response.Error))
		return
	}

	d.logger.Info("pipeline executor sandbox created",
		"localpart", localpart,
		"proxy_pid", response.ProxyPID,
	)

	// Wait for the executor to finish. This blocks until the process
	// exits or the context is cancelled (daemon shutdown).
	exitCode, exitDescription, exitOutput, waitError := d.launcherWaitSandbox(ctx, localpart)
	if waitError != nil {
		d.postPipelineError(ctx, roomID, commandEventID, command, start,
			fmt.Sprintf("waiting for pipeline executor: %v", waitError))
		d.destroyPipelineSandbox(ctx, pipelineEntity)
		return
	}

	if exitCode != 0 && exitOutput != "" {
		d.logger.Warn("pipeline executor exited with captured output",
			"localpart", localpart,
			"exit_code", exitCode,
			"exit_description", exitDescription,
			"output", exitOutput,
		)
	} else {
		d.logger.Info("pipeline executor exited",
			"localpart", localpart,
			"exit_code", exitCode,
			"exit_description", exitDescription,
		)
	}

	// Read the JSONL result file.
	entries, readError := readPipelineResultFile(resultFilePath)
	if readError != nil {
		d.logger.Warn("failed to read pipeline result file",
			"localpart", localpart,
			"path", resultFilePath,
			"error", readError,
		)
		// Don't fail — we still have the exit code.
	}

	// Post the structured result as a threaded reply.
	d.postPipelineResult(ctx, roomID, commandEventID, command, start,
		exitCode, exitDescription, exitOutput, entries)

	// Clean up the ephemeral sandbox.
	d.destroyPipelineSandbox(ctx, pipelineEntity)
}

// buildPipelineExecutorSpec constructs a SandboxSpec for the pipeline
// executor. The sandbox provides:
//   - bwrap isolation with PID namespace
//   - The pipeline executor binary as the entrypoint
//   - BUREAU_SANDBOX=1, BUREAU_RESULT_PATH, TERM environment variables
//   - The result file bind-mounted RW
//   - The workspace root bind-mounted RW (for git operations)
//   - The Nix environment (if configured) for toolchain access
//   - Security defaults: new session, die-with-parent, no-new-privs
//
// findLocalArtifactSocket returns the host-side socket path for the
// local artifact service, or empty string if no artifact service is
// running on this machine. Uses the same discovery pattern as
// pushUpstreamConfig: search d.services for a local service with the
// "content-addressed-store" capability.
func (d *Daemon) findLocalArtifactSocket() string {
	for _, service := range d.services {
		if service.Machine != d.machine {
			continue
		}
		for _, capability := range service.Capabilities {
			if capability == "content-addressed-store" {
				return service.Principal.SocketPath(d.fleetRunDir)
			}
		}
	}
	return ""
}

func (d *Daemon) buildPipelineExecutorSpec(
	pipelineRef string,
	resultFilePath string,
	artifactSocketPath string,
	artifactTokenPath string,
	command schema.CommandMessage,
) *schema.SandboxSpec {
	// Build the command. When a pipeline ref is provided, pass it as
	// the CLI argument. Otherwise, the executor resolves via payload.
	executorCommand := []string{d.pipelineExecutorBinary}
	if pipelineRef != "" {
		executorCommand = append(executorCommand, pipelineRef)
	}

	spec := &schema.SandboxSpec{
		Command: executorCommand,
		EnvironmentVariables: map[string]string{
			"BUREAU_SANDBOX":     "1",
			"BUREAU_RESULT_PATH": "/run/bureau/result.jsonl",
			"TERM":               "xterm-256color",
		},
		Filesystem: []schema.TemplateMount{
			// Executor binary: bind-mounted at its host path so bwrap
			// can find it. Required when the binary is not under /nix/store
			// (e.g., Bazel outputs, go install paths). Harmless when it is
			// under /nix/store since the more-specific mount coexists with
			// the /nix/store mount.
			{Source: d.pipelineExecutorBinary, Dest: d.pipelineExecutorBinary, Mode: "ro"},
			// Result file: bind-mounted RW so the executor can write
			// JSONL and the daemon reads the host file after exit.
			{Source: resultFilePath, Dest: "/run/bureau/result.jsonl", Mode: "rw"},
			// Workspace root: pipeline JSONC references /workspace/${PROJECT}.
			{Source: d.workspaceRoot, Dest: "/workspace", Mode: "rw"},
		},
		Namespaces: &schema.TemplateNamespaces{
			PID: true,
		},
		Security: &schema.TemplateSecurity{
			NewSession:    true,
			DieWithParent: true,
			NoNewPrivs:    true,
		},
	}

	// When a Nix environment is configured, the launcher bind-mounts
	// /nix/store and prepends the environment's bin/ to PATH. This
	// gives the executor access to tools like git, sh, etc.
	if d.pipelineEnvironment != "" {
		spec.EnvironmentPath = d.pipelineEnvironment
	}

	// Artifact service: bind-mount the socket and token file when an
	// artifact service is available. Pipeline steps with artifact-mode
	// outputs need these to store files in the CAS. When not available,
	// the executor's artifact client is nil and artifact outputs fail
	// with a clear error at step capture time.
	if artifactSocketPath != "" && artifactTokenPath != "" {
		spec.Filesystem = append(spec.Filesystem,
			schema.TemplateMount{Source: artifactSocketPath, Dest: "/run/bureau/artifact.sock", Mode: "rw"},
			schema.TemplateMount{Source: artifactTokenPath, Dest: "/run/bureau/artifact.token", Mode: "ro"},
		)
		spec.EnvironmentVariables["BUREAU_ARTIFACT_SOCKET"] = "/run/bureau/artifact.sock"
		spec.EnvironmentVariables["BUREAU_ARTIFACT_TOKEN"] = "/run/bureau/artifact.token"
	}

	// Build the payload from command parameters. The executor reads
	// the payload for pipeline_ref, pipeline_inline, and variables.
	if len(command.Parameters) > 0 {
		spec.Payload = command.Parameters
	}

	return spec
}

// destroyPipelineSandbox sends a destroy-sandbox request for an
// ephemeral pipeline principal. Errors are logged but not propagated
// — the result has already been posted by this point.
func (d *Daemon) destroyPipelineSandbox(ctx context.Context, principal ref.Entity) {
	response, err := d.launcherRequest(ctx, launcherIPCRequest{
		Action:    "destroy-sandbox",
		Principal: principal.AccountLocalpart(),
	})
	if err != nil {
		d.logger.Error("destroy-sandbox IPC failed for pipeline executor",
			"principal", principal, "error", err)
		return
	}
	if !response.OK {
		d.logger.Error("destroy-sandbox rejected for pipeline executor",
			"principal", principal, "error", response.Error)
		return
	}
	d.logger.Info("pipeline executor sandbox destroyed", "principal", principal)
}

// --- Result file reading ---

// pipelineResultEntry is the generic structure for reading JSONL
// entries from the result file. The Type field determines which
// additional fields are populated.
type pipelineResultEntry struct {
	Type       string            `json:"type"`
	Pipeline   string            `json:"pipeline,omitempty"`
	StepCount  int               `json:"step_count,omitempty"`
	Timestamp  string            `json:"timestamp,omitempty"`
	Index      int               `json:"index,omitempty"`
	Name       string            `json:"name,omitempty"`
	Status     string            `json:"status,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`
	Error      string            `json:"error,omitempty"`
	FailedStep string            `json:"failed_step,omitempty"`
	LogEventID ref.EventID       `json:"log_event_id,omitempty"`
	Outputs    map[string]string `json:"outputs,omitempty"`
}

// readPipelineResultFile reads a JSONL result file and returns the
// parsed entries. Returns an empty slice (not an error) if the file
// is empty (executor was killed before writing anything).
func readPipelineResultFile(path string) ([]pipelineResultEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening result file: %w", err)
	}
	defer file.Close()

	var entries []pipelineResultEntry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry pipelineResultEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return entries, fmt.Errorf("parsing result line: %w (line: %s)", err, string(line))
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("reading result file: %w", err)
	}
	return entries, nil
}

// --- Result posting ---

// postPipelineResult posts the pipeline execution outcome as a
// threaded reply to the original command message. Includes step-level
// detail from the JSONL result file when available.
func (d *Daemon) postPipelineResult(
	ctx context.Context,
	roomID ref.RoomID, commandEventID ref.EventID,
	command schema.CommandMessage,
	start time.Time,
	exitCode int,
	exitDescription string,
	exitOutput string,
	entries []pipelineResultEntry,
) {
	durationMilliseconds := time.Since(start).Milliseconds()

	// Find the terminal entry (complete or failed) for summary info.
	var terminalEntry *pipelineResultEntry
	for index := len(entries) - 1; index >= 0; index-- {
		if entries[index].Type == "complete" || entries[index].Type == "failed" {
			terminalEntry = &entries[index]
			break
		}
	}

	// Build step summaries from the result entries.
	var steps []pipeline.PipelineStepResult
	for _, entry := range entries {
		if entry.Type != "step" {
			continue
		}
		steps = append(steps, pipeline.PipelineStepResult{
			Name:       entry.Name,
			Status:     entry.Status,
			DurationMS: entry.DurationMS,
			Error:      entry.Error,
			Outputs:    entry.Outputs,
		})
	}

	// Determine overall status.
	status := "error"
	var body string
	if exitCode == 0 && terminalEntry != nil && terminalEntry.Type == "complete" {
		status = "success"
		body = fmt.Sprintf("pipeline.execute: completed in %dms", durationMilliseconds)
	} else if terminalEntry != nil && terminalEntry.Type == "failed" {
		body = fmt.Sprintf("pipeline.execute: failed at step %q: %s",
			terminalEntry.FailedStep, terminalEntry.Error)
	} else if exitCode != 0 {
		body = fmt.Sprintf("pipeline.execute: executor exited with code %d", exitCode)
		if exitDescription != "" {
			body += fmt.Sprintf(" (%s)", exitDescription)
		}
		if exitOutput != "" {
			truncated := tailLines(exitOutput, maxMatrixOutputLines)
			body += fmt.Sprintf("\n\nCaptured output (last %d lines):\n%s", countLines(truncated), truncated)
		}
	} else {
		// Exit code 0 but no terminal entry — executor produced no
		// result file or it was empty.
		body = "pipeline.execute: completed (no result details)"
		status = "success"
	}

	message := schema.CommandResultMessage{
		MsgType:    schema.MsgTypeCommandResult,
		Body:       body,
		Status:     status,
		ExitCode:   &exitCode,
		DurationMS: durationMilliseconds,
		Steps:      steps,
		RequestID:  command.RequestID,
		RelatesTo:  schema.NewThreadRelation(commandEventID),
	}

	if terminalEntry != nil {
		if !terminalEntry.LogEventID.IsZero() {
			message.LogEventID = terminalEntry.LogEventID
		}
		if len(terminalEntry.Outputs) > 0 {
			message.Outputs = terminalEntry.Outputs
		}
	}

	if _, err := d.sendEventRetry(ctx, roomID, schema.MatrixEventTypeMessage, message); err != nil {
		d.logger.Error("failed to post pipeline result",
			"room_id", roomID,
			"error", err,
		)
	}
}

// postPipelineError posts a pipeline execution error as a threaded
// reply. Used for failures that happen before or outside the executor
// (sandbox creation failures, IPC errors, etc.).
func (d *Daemon) postPipelineError(
	ctx context.Context,
	roomID ref.RoomID, commandEventID ref.EventID,
	command schema.CommandMessage,
	start time.Time,
	errorMessage string,
) {
	d.logger.Error("pipeline execution failed",
		"room_id", roomID,
		"error", errorMessage,
	)

	durationMilliseconds := time.Since(start).Milliseconds()

	message := schema.CommandResultMessage{
		MsgType:    schema.MsgTypeCommandResult,
		Body:       fmt.Sprintf("pipeline.execute: error: %s", errorMessage),
		Status:     "error",
		Error:      errorMessage,
		DurationMS: durationMilliseconds,
		RequestID:  command.RequestID,
		RelatesTo:  schema.NewThreadRelation(commandEventID),
	}

	if _, err := d.sendEventRetry(ctx, roomID, schema.MatrixEventTypeMessage, message); err != nil {
		d.logger.Error("failed to post pipeline error",
			"room_id", roomID,
			"error", err,
		)
	}
}

// postPipelineAccepted posts an immediate "accepted" acknowledgment
// to the command thread so the sender knows the pipeline is starting.
// Called by executePipeline before sandbox creation.
func (d *Daemon) postPipelineAccepted(
	ctx context.Context,
	roomID ref.RoomID, commandEventID ref.EventID,
	command schema.CommandMessage,
	localpart string,
) {
	message := schema.CommandResultMessage{
		MsgType:   schema.MsgTypeCommandResult,
		Body:      fmt.Sprintf("pipeline.execute: starting executor as %s", localpart),
		Status:    "accepted",
		Principal: localpart,
		RequestID: command.RequestID,
		RelatesTo: schema.NewThreadRelation(commandEventID),
	}

	if _, sendError := d.sendEventRetry(ctx, roomID, schema.MatrixEventTypeMessage, message); sendError != nil {
		d.logger.Error("failed to post pipeline accepted message",
			"room_id", roomID,
			"error", sendError,
		)
	}
}
