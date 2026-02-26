// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Pipeline execution: ticket-driven integration for spawning ephemeral
// bureau-pipeline-executor sandboxes. Command handlers create pip- tickets
// and return "accepted"; the ticket watcher (processPipelineTickets) picks
// them up from /sync and creates sandboxes through the standard daemon
// lifecycle (d.running, exit watchers, clean shutdown).
//
// The pipeline executor runs in a bwrap sandbox with its own proxy process
// (holding the daemon's Matrix token via DirectCredentials). The executor
// reports all progress via tickets (claim, step notes, close).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/ipc"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/artifact"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/messaging"
)

// maxUnixSocketPathLength is the maximum length of a Unix domain socket
// path. On Linux, struct sockaddr_un.sun_path is 108 bytes, but the
// last byte must be a null terminator, giving 107 usable bytes.
const maxUnixSocketPathLength = 107

// handlePipelineExecute validates the pipeline.execute command, creates
// a pip- ticket, and returns "accepted" with the ticket ID. The ticket
// watcher (processPipelineTickets) picks up the ticket from the next
// /sync and creates the executor sandbox through the standard daemon
// lifecycle.
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

	// Extract the pipeline reference from command parameters.
	pipelineRef, _ := command.Parameters["pipeline"].(string)
	if pipelineRef == "" {
		return nil, fmt.Errorf("parameter 'pipeline' is required")
	}

	// The room parameter specifies where the pip- ticket is created.
	ticketRoomIDString, _ := command.Parameters["room"].(string)
	if ticketRoomIDString == "" {
		return nil, fmt.Errorf("parameter 'room' is required (room ID where the pipeline ticket is created)")
	}
	ticketRoomID, err := ref.ParseRoomID(ticketRoomIDString)
	if err != nil {
		return nil, fmt.Errorf("parameter 'room' is not a valid room ID: %w", err)
	}

	// Verify the target room has pipeline execution enabled.
	_, err = d.session.GetStateEvent(ctx, ticketRoomID, schema.EventTypePipelineConfig, "")
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return nil, fmt.Errorf("pipeline execution not enabled in room %s (run 'bureau pipeline enable' for the space)", d.displayRoom(ticketRoomID))
		}
		return nil, fmt.Errorf("checking pipeline config in room %s: %w", d.displayRoom(ticketRoomID), err)
	}

	// Discover the ticket service.
	ticketSocketPath := d.findLocalTicketSocket()
	if ticketSocketPath == "" {
		return nil, fmt.Errorf("no ticket service running on this machine (required for pipeline execution)")
	}

	// Build an ephemeral Entity for the ticket creation token.
	// The actual executor principal is generated later by the
	// ticket watcher when it creates the sandbox.
	localpart := d.pipelineLocalpart()
	pipelineEntity, err := ref.NewEntityFromAccountLocalpart(d.fleet, localpart)
	if err != nil {
		return nil, fmt.Errorf("invalid pipeline localpart: %w", err)
	}

	// Extract variables from command parameters (string values only,
	// excluding the pipeline ref and room). Inject MACHINE for
	// affinity so the ticket watcher knows which daemon should
	// execute this ticket.
	pipelineVariables := make(map[string]string)
	for key, value := range command.Parameters {
		if key == "pipeline" || key == "room" {
			continue
		}
		if stringValue, ok := value.(string); ok {
			pipelineVariables[key] = stringValue
		}
	}
	pipelineVariables["MACHINE"] = d.machine.Localpart()

	ticketID, _, err := d.createPipelineTicket(
		ctx, ticketSocketPath, pipelineEntity, ticketRoomID,
		pipelineRef, pipelineVariables)
	if err != nil {
		return nil, fmt.Errorf("creating pipeline ticket: %w", err)
	}

	d.logger.Info("pipeline.execute accepted",
		"room_id", roomID,
		"ticket_id", ticketID,
		"ticket_room", ticketRoomID,
		"pipeline", pipelineRef,
		"request_id", command.RequestID,
	)

	return map[string]any{
		"status":    "accepted",
		"ticket_id": ticketID,
		"room":      ticketRoomID.String(),
	}, nil
}

// removePipelineTicketByPrincipal removes the pipelineTickets entry
// for the given principal. The map is keyed by ticket state key, so
// this does a linear scan. The map is always small (one entry per
// active pipeline executor), so this is not a performance concern.
func (d *Daemon) removePipelineTicketByPrincipal(principal ref.Entity) {
	for key, entity := range d.pipelineTickets {
		if entity == principal {
			delete(d.pipelineTickets, key)
			return
		}
	}
}

// processPipelineTickets scans a /sync response for open pipeline
// tickets and creates executor sandboxes for tickets that belong to
// this machine. This is the ticket-driven pipeline execution path:
// command handlers and worktree handlers create pip- tickets and
// return "accepted" immediately; this function picks them up from the
// next /sync and spawns the executor through the standard daemon
// lifecycle (d.running, exit watchers, clean shutdown).
//
// Machine affinity is determined by the MACHINE variable in the
// ticket's PipelineExecutionContent.Variables: the ticket watcher only
// executes tickets where MACHINE matches this daemon's machine
// localpart. The reconcile-driven path (applyPipelineExecutorOverlay)
// creates tickets and sandboxes together in one pass and registers
// them in d.pipelineTickets, so they are skipped here.
//
// Called from processSyncResponse after reconcile and command processing.
// Caller holds reconcileMu.
func (d *Daemon) processPipelineTickets(ctx context.Context, response *messaging.SyncResponse) {
	if d.pipelineExecutorBinary == "" {
		return
	}

	for roomID, room := range response.Rooms.Join {
		// Skip rooms without pipeline_config — pipeline execution
		// requires explicit opt-in via "bureau pipeline enable".
		if !d.pipelineEnabledRooms[roomID] {
			continue
		}

		// Combine state and timeline events — ticket state events
		// can appear in either section depending on sync gaps.
		allEvents := make([]messaging.Event, 0, len(room.State.Events)+len(room.Timeline.Events))
		allEvents = append(allEvents, room.State.Events...)
		allEvents = append(allEvents, room.Timeline.Events...)

		for _, event := range allEvents {
			if event.Type != schema.EventTypeTicket {
				continue
			}
			if event.StateKey == nil {
				continue
			}
			stateKey := *event.StateKey
			if !strings.HasPrefix(stateKey, "pip-") {
				continue
			}

			// Already tracked — either by this watcher on a previous
			// sync or by the reconcile-driven overlay in this sync.
			if _, tracked := d.pipelineTickets[stateKey]; tracked {
				continue
			}

			// Unmarshal the ticket content to check type, status, and
			// machine affinity.
			contentBytes, err := json.Marshal(event.Content)
			if err != nil {
				d.logger.Warn("failed to marshal ticket event content",
					"room_id", roomID, "room", d.displayRoom(roomID),
					"state_key", stateKey, "error", err)
				continue
			}
			var ticketContent ticket.TicketContent
			if err := json.Unmarshal(contentBytes, &ticketContent); err != nil {
				d.logger.Warn("failed to unmarshal ticket content",
					"room_id", roomID, "room", d.displayRoom(roomID),
					"state_key", stateKey, "error", err)
				continue
			}

			if ticketContent.Type != ticket.TypePipeline {
				continue
			}
			if ticketContent.Status != ticket.StatusOpen {
				continue
			}
			if ticketContent.Pipeline == nil {
				d.logger.Warn("pipeline ticket has no pipeline content",
					"room_id", roomID, "room", d.displayRoom(roomID),
					"state_key", stateKey)
				continue
			}

			// Machine affinity: only execute tickets created for this
			// machine. Command handlers inject MACHINE into the ticket
			// variables when creating pip- tickets.
			ticketMachine := ticketContent.Pipeline.Variables["MACHINE"]
			if ticketMachine != d.machine.Localpart() {
				continue
			}

			d.startPipelineExecutor(ctx, roomID, stateKey, &ticketContent)
		}
	}
}

// startPipelineExecutor creates an ephemeral sandbox for a pipeline
// ticket. The sandbox runs bureau-pipeline-executor with the daemon's
// own Matrix credentials (DirectCredentials), ticket and artifact
// service access, and the standard exit watcher lifecycle.
//
// Caller holds reconcileMu (called from processPipelineTickets).
func (d *Daemon) startPipelineExecutor(
	ctx context.Context,
	roomID ref.RoomID,
	ticketStateKey string,
	ticketContent *ticket.TicketContent,
) {
	localpart := d.pipelineLocalpart()

	pipelineEntity, err := ref.NewEntityFromAccountLocalpart(d.fleet, localpart)
	if err != nil {
		d.logger.Error("invalid pipeline localpart",
			"localpart", localpart, "error", err)
		return
	}

	// Validate that the admin socket path fits within the Unix sun_path
	// limit. This catches misconfiguration early rather than surfacing as
	// a cryptic "bind: invalid argument" inside the launcher.
	adminSocketPath := pipelineEntity.ProxyAdminSocketPath(d.fleetRunDir)
	if length := len(adminSocketPath); length > maxUnixSocketPathLength {
		d.logger.Error("admin socket path exceeds Unix limit",
			"localpart", localpart,
			"length", length,
			"max", maxUnixSocketPathLength,
			"path", adminSocketPath,
		)
		return
	}

	// Discover service sockets and prepare tokens.
	ticketSocketPath := d.findLocalTicketSocket()
	if ticketSocketPath == "" {
		d.logger.Error("no ticket service running for pipeline executor",
			"localpart", localpart, "ticket", ticketStateKey)
		return
	}

	// Create a temp directory for service tokens. Cleaned up by
	// the exit watcher when the sandbox exits.
	tokenDirectory, err := os.MkdirTemp("", "bureau-pipeline-tokens-*")
	if err != nil {
		d.logger.Error("creating pipeline token directory",
			"localpart", localpart, "error", err)
		return
	}

	// Mint a ticket service token for ongoing operations (claim,
	// progress, notes, attachments, close, show).
	ticketToken := &servicetoken.Token{
		Subject:   pipelineEntity.UserID(),
		Machine:   d.machine,
		Audience:  "ticket",
		Grants:    []servicetoken.Grant{{Actions: []string{"ticket/update", "ticket/close", "ticket/show", "ticket/attach"}}},
		IssuedAt:  d.clock.Now().Unix(),
		ExpiresAt: d.clock.Now().Add(1 * time.Hour).Unix(),
	}
	ticketTokenBytes, err := servicetoken.Mint(d.tokenSigningPrivateKey, ticketToken)
	if err != nil {
		d.logger.Error("minting ticket token for pipeline executor",
			"localpart", localpart, "error", err)
		os.RemoveAll(tokenDirectory)
		return
	}
	ticketTokenPath := filepath.Join(tokenDirectory, "ticket.token")
	if err := os.WriteFile(ticketTokenPath, ticketTokenBytes, 0600); err != nil {
		d.logger.Error("writing ticket token for pipeline executor",
			"localpart", localpart, "error", err)
		os.RemoveAll(tokenDirectory)
		return
	}

	// Discover artifact service and mint a token if available.
	var artifactSocketPath, artifactTokenPath string
	if socketPath := d.findLocalArtifactSocket(); socketPath != "" {
		artifactToken := &servicetoken.Token{
			Subject:   pipelineEntity.UserID(),
			Machine:   d.machine,
			Audience:  "artifact",
			Grants:    []servicetoken.Grant{{Actions: []string{artifact.ActionStore}}},
			IssuedAt:  d.clock.Now().Unix(),
			ExpiresAt: d.clock.Now().Add(1 * time.Hour).Unix(),
		}
		tokenBytes, mintErr := servicetoken.Mint(d.tokenSigningPrivateKey, artifactToken)
		if mintErr != nil {
			d.logger.Warn("failed to mint artifact token for pipeline executor",
				"localpart", localpart, "error", mintErr)
		} else {
			path := filepath.Join(tokenDirectory, "artifact.token")
			if writeErr := os.WriteFile(path, tokenBytes, 0600); writeErr != nil {
				d.logger.Warn("failed to write artifact token for pipeline executor",
					"localpart", localpart, "error", writeErr)
			} else {
				artifactSocketPath = socketPath
				artifactTokenPath = path
			}
		}
	}

	// Build the sandbox spec.
	spec := d.buildPipelineExecutorSpec(
		artifactSocketPath, artifactTokenPath,
		ticketStateKey, roomID, ticketSocketPath, ticketTokenPath)

	// Build trigger content from the ticket for the executor to read
	// as trigger.json. The executor extracts Pipeline.PipelineRef and
	// Pipeline.Variables from this.
	triggerContent, err := json.Marshal(ticketContent)
	if err != nil {
		d.logger.Error("marshaling trigger content for pipeline executor",
			"localpart", localpart, "error", err)
		os.RemoveAll(tokenDirectory)
		return
	}

	// Create the sandbox with the daemon's own Matrix credentials.
	credentials := map[string]string{
		"MATRIX_TOKEN":   d.session.AccessToken(),
		"MATRIX_USER_ID": d.machine.UserID().String(),
	}

	response, err := d.launcherRequest(ctx, launcherIPCRequest{
		Action:            ipc.ActionCreateSandbox,
		Principal:         localpart,
		DirectCredentials: credentials,
		SandboxSpec:       spec,
		TriggerContent:    triggerContent,
	})
	if err != nil {
		d.logger.Error("create-sandbox IPC failed for pipeline executor",
			"localpart", localpart, "ticket", ticketStateKey, "error", err)
		os.RemoveAll(tokenDirectory)
		return
	}
	if !response.OK {
		d.logger.Error("create-sandbox rejected for pipeline executor",
			"localpart", localpart, "ticket", ticketStateKey, "error", response.Error)
		os.RemoveAll(tokenDirectory)
		return
	}

	// Track the executor in standard daemon lifecycle maps.
	d.running[pipelineEntity] = true
	d.notifyStatusChange()
	d.dynamicPrincipals[pipelineEntity] = true
	d.pipelineTickets[ticketStateKey] = pipelineEntity
	d.lastSpecs[pipelineEntity] = spec
	d.lastActivityAt = d.clock.Now()

	d.logger.Info("pipeline executor sandbox created",
		"localpart", localpart,
		"ticket", ticketStateKey,
		"room_id", roomID,
		"pipeline_ref", ticketContent.Pipeline.PipelineRef,
	)

	// Start exit watcher through standard lifecycle. The watcher
	// cleans up d.running, d.dynamicPrincipals, d.pipelineTickets,
	// and destroys the sandbox when the executor exits.
	if d.shutdownCtx != nil {
		watcherCtx, watcherCancel := context.WithCancel(d.shutdownCtx)
		d.exitWatchers[pipelineEntity] = watcherCancel
		go d.watchPipelineSandboxExit(watcherCtx, pipelineEntity, tokenDirectory)
	}
}

// watchPipelineSandboxExit is the exit watcher for pipeline executor
// sandboxes created by the ticket watcher. It blocks until the sandbox
// exits, then cleans up lifecycle state and the token directory.
// This is a slimmer variant of watchSandboxExit: pipeline executors
// don't have proxy exit watchers, health monitors, or layout watchers.
func (d *Daemon) watchPipelineSandboxExit(ctx context.Context, principal ref.Entity, tokenDirectory string) {
	exitCode, exitDescription, exitOutput, waitError := d.launcherWaitSandbox(ctx, principal.AccountLocalpart())

	if waitError != nil {
		d.logger.Error("waiting for pipeline executor sandbox",
			"principal", principal, "error", waitError)
	} else if exitCode != 0 && exitOutput != "" {
		d.logger.Warn("pipeline executor exited with captured output",
			"principal", principal,
			"exit_code", exitCode,
			"exit_description", exitDescription,
			"output", exitOutput,
		)
	} else {
		d.logger.Info("pipeline executor exited",
			"principal", principal,
			"exit_code", exitCode,
			"exit_description", exitDescription,
		)
	}

	// Clean up the token directory created by startPipelineExecutor.
	if tokenDirectory != "" {
		os.RemoveAll(tokenDirectory)
	}

	d.reconcileMu.Lock()
	if d.running[principal] {
		delete(d.running, principal)
		d.notifyStatusChange()
		delete(d.dynamicPrincipals, principal)
		d.removePipelineTicketByPrincipal(principal)
		delete(d.exitWatchers, principal)
		delete(d.lastSpecs, principal)
		d.lastActivityAt = d.clock.Now()
	}
	d.reconcileMu.Unlock()

	// Destroy the sandbox.
	d.destroyPipelineSandbox(ctx, principal)
}

// pipelineLocalpart generates a unique principal localpart for a
// pipeline execution. Uses a monotonic counter for short, unique IDs.
// The pipeline ref and other human-readable context live in log
// messages, not the localpart — socket path length is the constraint.
func (d *Daemon) pipelineLocalpart() string {
	id := d.ephemeralCounter.Add(1)
	return fmt.Sprintf("pipeline/%d", id)
}

// findLocalArtifactSocket returns the host-side socket path for the
// local artifact service, or empty string if no artifact service is
// running on this machine. Searches d.services for a local service
// with the "content-addressed-store" capability.
func (d *Daemon) findLocalArtifactSocket() string {
	for _, service := range d.services {
		if service.Machine != d.machine {
			continue
		}
		for _, capability := range service.Capabilities {
			if capability == "content-addressed-store" {
				return service.Principal.ServiceSocketPath(d.fleetRunDir)
			}
		}
	}
	return ""
}

// findLocalTicketSocket returns the host-side socket path for the
// local ticket service, or empty string if no ticket service is
// running on this machine. Discovers the ticket service by its
// "dependency-graph" capability in the service directory.
func (d *Daemon) findLocalTicketSocket() string {
	for _, svc := range d.services {
		if svc.Machine != d.machine {
			continue
		}
		for _, capability := range svc.Capabilities {
			if capability == "dependency-graph" {
				return svc.Principal.ServiceSocketPath(d.fleetRunDir)
			}
		}
	}
	return ""
}

// ticketCreateResponse is the response from the ticket service's
// "create" action. Field names match the Go field names of the
// server-side createResponse struct (CBOR uses Go field names when
// no cbor tags are present).
type ticketCreateResponse struct {
	ID   string
	Room string
}

// createPipelineTicket creates a pip- ticket for a pipeline execution
// via the ticket service. Returns the ticket ID and JSON-serialized
// TicketContent for use as trigger.json. The caller provides the
// room ID where the ticket should be created — that room must have
// ticket config with allowed_types including "pipeline".
func (d *Daemon) createPipelineTicket(
	ctx context.Context,
	ticketSocketPath string,
	principal ref.Entity,
	roomID ref.RoomID,
	pipelineRef string,
	variables map[string]string,
) (string, []byte, error) {
	// Mint a short-lived token for the create call only. The
	// sandbox gets a separate, longer-lived token for ongoing
	// ticket operations (claim, progress, notes, close).
	createToken := &servicetoken.Token{
		Subject:   principal.UserID(),
		Machine:   d.machine,
		Audience:  "ticket",
		Grants:    []servicetoken.Grant{{Actions: []string{"ticket/create"}}},
		IssuedAt:  d.clock.Now().Unix(),
		ExpiresAt: d.clock.Now().Add(1 * time.Minute).Unix(),
	}
	tokenBytes, err := servicetoken.Mint(d.tokenSigningPrivateKey, createToken)
	if err != nil {
		return "", nil, fmt.Errorf("minting ticket create token: %w", err)
	}

	client := service.NewServiceClientFromToken(ticketSocketPath, tokenBytes)

	pipelineContent := &ticket.PipelineExecutionContent{
		PipelineRef: pipelineRef,
		Variables:   variables,
	}

	fields := map[string]any{
		"room":     roomID.String(),
		"type":     "pipeline",
		"title":    fmt.Sprintf("Pipeline: %s", pipelineNameFromRef(pipelineRef)),
		"priority": 2,
		"pipeline": pipelineContent,
	}

	var response ticketCreateResponse
	if err := client.Call(ctx, "create", fields, &response); err != nil {
		return "", nil, err
	}

	// Build the TicketContent that the executor reads as trigger.json.
	// This reconstructs the content the ticket service created — the
	// create response only returns ID and Room, not the full content.
	// The executor extracts Pipeline.PipelineRef and Pipeline.Variables;
	// other fields are available as EVENT_-prefixed trigger variables.
	now := d.clock.Now().UTC().Format(time.RFC3339)
	triggerContent := ticket.TicketContent{
		Version:   ticket.TicketContentVersion,
		Title:     fmt.Sprintf("Pipeline: %s", pipelineNameFromRef(pipelineRef)),
		Status:    ticket.StatusOpen,
		Priority:  2,
		Type:      ticket.TypePipeline,
		CreatedBy: principal.UserID(),
		CreatedAt: now,
		UpdatedAt: now,
		Pipeline:  pipelineContent,
	}

	triggerBytes, err := json.Marshal(triggerContent)
	if err != nil {
		return "", nil, fmt.Errorf("marshaling trigger content: %w", err)
	}

	return response.ID, triggerBytes, nil
}

// pipelineNameFromRef extracts a human-readable name from a pipeline
// ref string. For refs like "bureau/pipeline:workspace-setup-dev",
// returns "workspace-setup-dev". For simple names like "init",
// returns them unchanged.
func pipelineNameFromRef(pipelineRef string) string {
	if index := strings.LastIndex(pipelineRef, ":"); index >= 0 {
		return pipelineRef[index+1:]
	}
	return pipelineRef
}

// buildPipelineExecutorSpec constructs a SandboxSpec for the pipeline
// executor. The sandbox provides bwrap isolation with PID namespace,
// the executor binary as the entrypoint, the workspace root bind-mounted
// RW for git operations, the Nix environment (if configured) for
// toolchain access, and security defaults (new session, die-with-parent,
// no-new-privs). Ticket and artifact service sockets are bind-mounted
// when available.
func (d *Daemon) buildPipelineExecutorSpec(
	artifactSocketPath string,
	artifactTokenPath string,
	ticketID string,
	ticketRoom ref.RoomID,
	ticketSocketPath string,
	ticketTokenPath string,
) *schema.SandboxSpec {
	spec := &schema.SandboxSpec{
		Command: []string{d.pipelineExecutorBinary},
		EnvironmentVariables: map[string]string{
			"BUREAU_SANDBOX":     "1",
			"BUREAU_TICKET_ID":   ticketID,
			"BUREAU_TICKET_ROOM": ticketRoom.String(),
			"TERM":               "xterm-256color",
		},
		Filesystem: []schema.TemplateMount{
			// Executor binary: bind-mounted at its host path so bwrap
			// can find it. Required when the binary is not under /nix/store
			// (e.g., Bazel outputs, go install paths). Harmless when it is
			// under /nix/store since the more-specific mount coexists with
			// the /nix/store mount.
			{Source: d.pipelineExecutorBinary, Dest: d.pipelineExecutorBinary, Mode: schema.MountModeRO},
			// Workspace root: pipeline JSONC references /workspace/${PROJECT}.
			{Source: d.workspaceRoot, Dest: "/workspace", Mode: schema.MountModeRW},
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
			schema.TemplateMount{Source: artifactSocketPath, Dest: "/run/bureau/artifact.sock", Mode: schema.MountModeRW},
			schema.TemplateMount{Source: artifactTokenPath, Dest: "/run/bureau/artifact.token", Mode: schema.MountModeRO},
		)
		spec.EnvironmentVariables["BUREAU_ARTIFACT_SOCKET"] = "/run/bureau/artifact.sock"
		spec.EnvironmentVariables["BUREAU_ARTIFACT_TOKEN"] = "/run/bureau/artifact.token"
	}

	// Ticket service: bind-mount the socket and token file. The
	// executor uses these for ticket lifecycle operations: claiming,
	// posting progress, adding step notes, and closing with the
	// pipeline's terminal outcome.
	if ticketSocketPath != "" && ticketTokenPath != "" {
		spec.Filesystem = append(spec.Filesystem,
			schema.TemplateMount{Source: ticketSocketPath, Dest: "/run/bureau/service/ticket.sock", Mode: schema.MountModeRW},
			schema.TemplateMount{Source: ticketTokenPath, Dest: "/run/bureau/service/token/ticket.token", Mode: schema.MountModeRO},
		)
	}

	return spec
}

// destroyPipelineSandbox sends a destroy-sandbox request for an
// ephemeral pipeline principal. Errors are logged but not propagated
// — the result has already been posted by this point.
func (d *Daemon) destroyPipelineSandbox(ctx context.Context, principal ref.Entity) {
	response, err := d.launcherRequest(ctx, launcherIPCRequest{
		Action:    ipc.ActionDestroySandbox,
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
