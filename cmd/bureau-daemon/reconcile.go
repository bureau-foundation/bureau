// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// reconcile reads the current MachineConfig from Matrix and ensures the
// running sandboxes match the desired state.
func (d *Daemon) reconcile(ctx context.Context) error {
	config, err := d.readMachineConfig(ctx)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			// No config yet — nothing to do.
			d.logger.Info("no machine config found, waiting for assignment")
			return nil
		}
		return fmt.Errorf("reading machine config: %w", err)
	}

	// Cache the config for observation authorization. This is the only
	// place that updates lastConfig — it's always consistent with the
	// daemon's running state.
	d.lastConfig = config

	// Determine the desired set of principals.
	desired := make(map[string]schema.PrincipalAssignment, len(config.Principals))
	for _, assignment := range config.Principals {
		if assignment.AutoStart {
			desired[assignment.Localpart] = assignment
		}
	}

	// Check for hot-reloadable changes on already-running principals.
	for localpart, assignment := range desired {
		if !d.running[localpart] {
			continue
		}
		d.reconcileRunningPrincipal(ctx, localpart, assignment)
	}

	// Create sandboxes for principals that should be running but aren't.
	for localpart, assignment := range desired {
		if d.running[localpart] {
			continue
		}

		// Check if the principal's start condition is satisfied. When a
		// StartCondition references a state event that doesn't exist yet,
		// the principal is deferred until a subsequent /sync delivers it.
		if !d.evaluateStartCondition(ctx, localpart, assignment.StartCondition) {
			continue
		}

		d.logger.Info("starting principal", "principal", localpart)

		// Read the credentials for this principal.
		credentials, err := d.readCredentials(ctx, localpart)
		if err != nil {
			if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
				d.logger.Warn("no credentials found for principal, skipping", "principal", localpart)
				continue
			}
			d.logger.Error("reading credentials", "principal", localpart, "error", err)
			continue
		}

		// Resolve the template and build the sandbox spec when the
		// assignment references a template. When Template is empty, the
		// launcher creates only a proxy process (no bwrap sandbox).
		var sandboxSpec *schema.SandboxSpec
		if assignment.Template != "" {
			template, err := resolveTemplate(ctx, d.session, assignment.Template, d.serverName)
			if err != nil {
				d.logger.Error("resolving template", "principal", localpart, "template", assignment.Template, "error", err)
				continue
			}
			sandboxSpec = resolveInstanceConfig(template, &assignment)
			d.logger.Info("resolved sandbox spec from template",
				"principal", localpart,
				"template", assignment.Template,
				"command", sandboxSpec.Command,
			)

			// Ensure the Nix environment's store path (and its full
			// transitive closure) exists locally before handing the
			// spec to the launcher. On failure, skip this principal
			// — the reconcile loop retries on the next sync cycle.
			if sandboxSpec.EnvironmentPath != "" {
				if err := d.prefetchEnvironment(ctx, sandboxSpec.EnvironmentPath); err != nil {
					d.logger.Error("prefetching nix environment",
						"principal", localpart,
						"store_path", sandboxSpec.EnvironmentPath,
						"error", err,
					)
					d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
						fmt.Sprintf("Failed to prefetch Nix environment for %s: %v (will retry on next reconcile cycle)",
							localpart, err),
					))
					continue
				}
			}
		}

		// Send create-sandbox to the launcher.
		response, err := d.launcherRequest(ctx, launcherIPCRequest{
			Action:               "create-sandbox",
			Principal:            localpart,
			EncryptedCredentials: credentials.Ciphertext,
			MatrixPolicy:         assignment.MatrixPolicy,
			SandboxSpec:          sandboxSpec,
		})
		if err != nil {
			d.logger.Error("create-sandbox IPC failed", "principal", localpart, "error", err)
			continue
		}
		if !response.OK {
			d.logger.Error("create-sandbox rejected", "principal", localpart, "error", response.Error)
			continue
		}

		d.running[localpart] = true
		if d.lastSpecs == nil {
			d.lastSpecs = make(map[string]*schema.SandboxSpec)
		}
		d.lastSpecs[localpart] = sandboxSpec
		d.lastActivityAt = time.Now()
		d.logger.Info("principal started", "principal", localpart)

		// Start watching the tmux session for layout changes. This also
		// restores any previously saved layout from Matrix.
		d.startLayoutWatcher(ctx, localpart)

		// Register all known local service routes on the new consumer's
		// proxy so it can reach services that were discovered before it
		// started. The proxy socket is created synchronously by Start(),
		// so it should be accepting connections by the time the launcher
		// responds to create-sandbox.
		d.configureConsumerProxy(ctx, localpart)

		// Push service visibility patterns so the proxy knows which
		// services this agent is allowed to discover. This must happen
		// before the directory push, since the proxy filters the
		// directory based on visibility patterns.
		if err := d.pushVisibilityToProxy(ctx, localpart, assignment.ServiceVisibility); err != nil {
			d.logger.Error("failed to push service visibility to new consumer proxy",
				"consumer", localpart,
				"error", err,
			)
		}

		// Push the service directory so the new consumer's agent can
		// discover services via GET /v1/services.
		directory := d.buildServiceDirectory()
		if err := d.pushDirectoryToProxy(ctx, localpart, directory); err != nil {
			d.logger.Error("failed to push service directory to new consumer proxy",
				"consumer", localpart,
				"error", err,
			)
		}
	}

	// Destroy sandboxes for principals that should not be running.
	for localpart := range d.running {
		if _, shouldRun := desired[localpart]; shouldRun {
			continue
		}

		d.logger.Info("stopping principal", "principal", localpart)

		// Stop the layout watcher before destroying the sandbox. This
		// ensures a clean shutdown rather than having the watcher see
		// the tmux session disappear underneath it.
		d.stopLayoutWatcher(localpart)

		response, err := d.launcherRequest(ctx, launcherIPCRequest{
			Action:    "destroy-sandbox",
			Principal: localpart,
		})
		if err != nil {
			d.logger.Error("destroy-sandbox IPC failed", "principal", localpart, "error", err)
			continue
		}
		if !response.OK {
			d.logger.Error("destroy-sandbox rejected", "principal", localpart, "error", response.Error)
			continue
		}

		delete(d.running, localpart)
		delete(d.lastSpecs, localpart)
		d.lastActivityAt = time.Now()
		d.logger.Info("principal stopped", "principal", localpart)
	}

	return nil
}

// reconcileRunningPrincipal checks if a running principal's configuration has
// changed and applies hot-reloadable updates. Currently handles:
//   - Payload changes: rewrites the bind-mounted payload file without restart
//
// Structural changes (command, mounts, namespaces, resources, security,
// environment) are logged as requiring a manual restart. Automatic restart
// is not yet implemented to avoid disrupting running agent work.
func (d *Daemon) reconcileRunningPrincipal(ctx context.Context, localpart string, assignment schema.PrincipalAssignment) {
	// Can only detect changes for principals created from templates.
	if assignment.Template == "" {
		return
	}

	// Re-resolve the template to get the current desired spec.
	template, err := resolveTemplate(ctx, d.session, assignment.Template, d.serverName)
	if err != nil {
		d.logger.Error("re-resolving template for running principal",
			"principal", localpart, "template", assignment.Template, "error", err)
		return
	}
	newSpec := resolveInstanceConfig(template, &assignment)

	// Compare with the previously deployed spec.
	oldSpec := d.lastSpecs[localpart]
	if oldSpec == nil {
		// No previous spec stored (principal was created without one,
		// or from a previous daemon instance). Store the current spec
		// for future comparisons but don't trigger any changes.
		if d.lastSpecs == nil {
			d.lastSpecs = make(map[string]*schema.SandboxSpec)
		}
		d.lastSpecs[localpart] = newSpec
		return
	}

	// Check if only the payload changed while the structural config
	// (everything that affects the sandbox environment) is unchanged.
	if payloadChanged(oldSpec, newSpec) {
		if structurallyChanged(oldSpec, newSpec) {
			d.logger.Warn("sandbox configuration changed for running principal (restart required)",
				"principal", localpart,
				"template", assignment.Template,
			)
			// Store the new spec so we don't warn on every reconcile.
			d.lastSpecs[localpart] = newSpec
			return
		}

		// Payload-only change: hot-reload by rewriting the payload file.
		d.logger.Info("payload changed for running principal, hot-reloading",
			"principal", localpart,
		)
		response, err := d.launcherRequest(ctx, launcherIPCRequest{
			Action:    "update-payload",
			Principal: localpart,
			Payload:   newSpec.Payload,
		})
		if err != nil {
			d.logger.Error("update-payload IPC failed",
				"principal", localpart, "error", err)
			return
		}
		if !response.OK {
			d.logger.Error("update-payload rejected",
				"principal", localpart, "error", response.Error)
			return
		}
		d.lastSpecs[localpart] = newSpec
		d.lastActivityAt = time.Now()
		d.logger.Info("payload hot-reloaded", "principal", localpart)
	}
}

// payloadChanged returns true if the payloads of two SandboxSpecs differ.
func payloadChanged(old, new *schema.SandboxSpec) bool {
	return !reflect.DeepEqual(old.Payload, new.Payload)
}

// structurallyChanged returns true if any non-payload fields of two
// SandboxSpecs differ. Structural changes require a sandbox restart
// because they affect the sandbox environment (mounts, namespaces,
// resources, security, command, environment variables, etc.).
func structurallyChanged(old, new *schema.SandboxSpec) bool {
	// Compare all fields except Payload by zeroing the payload and
	// comparing the rest via JSON serialization. This is resilient to
	// new fields being added to SandboxSpec — they'll be included in
	// the comparison automatically.
	oldCopy := *old
	newCopy := *new
	oldCopy.Payload = nil
	newCopy.Payload = nil

	oldJSON, err := json.Marshal(oldCopy)
	if err != nil {
		return true // Assume changed on marshal error.
	}
	newJSON, err := json.Marshal(newCopy)
	if err != nil {
		return true
	}
	return string(oldJSON) != string(newJSON)
}

// evaluateStartCondition checks whether a principal's StartCondition is
// satisfied. Returns true if the principal should proceed with launch, false
// if it should be deferred. When StartCondition is nil, always returns true.
//
// The condition references a state event in a specific room. When RoomAlias is
// set, the daemon resolves it to a room ID. When empty, the daemon checks the
// principal's own config room (where MachineConfig lives). If the state event
// exists, the condition is met. If it's M_NOT_FOUND, the principal is deferred.
func (d *Daemon) evaluateStartCondition(ctx context.Context, localpart string, condition *schema.StartCondition) bool {
	if condition == nil {
		return true
	}

	// Determine which room to check. When RoomAlias is empty, check the
	// principal's config room (the room where the MachineConfig lives).
	roomID := d.configRoomID
	if condition.RoomAlias != "" {
		resolved, err := d.session.ResolveAlias(ctx, condition.RoomAlias)
		if err != nil {
			if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
				d.logger.Info("start condition room alias not found, deferring principal",
					"principal", localpart,
					"room_alias", condition.RoomAlias,
				)
				return false
			}
			d.logger.Error("resolving start condition room alias",
				"principal", localpart,
				"room_alias", condition.RoomAlias,
				"error", err,
			)
			return false
		}
		roomID = resolved
	}

	_, err := d.session.GetStateEvent(ctx, roomID, condition.EventType, condition.StateKey)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			d.logger.Info("start condition not met, deferring principal",
				"principal", localpart,
				"event_type", condition.EventType,
				"state_key", condition.StateKey,
				"room_id", roomID,
			)
			return false
		}
		d.logger.Error("checking start condition",
			"principal", localpart,
			"event_type", condition.EventType,
			"state_key", condition.StateKey,
			"room_id", roomID,
			"error", err,
		)
		return false
	}

	return true
}

// readMachineConfig reads the MachineConfig state event from the config room.
func (d *Daemon) readMachineConfig(ctx context.Context) (*schema.MachineConfig, error) {
	content, err := d.session.GetStateEvent(ctx, d.configRoomID, schema.EventTypeMachineConfig, d.machineName)
	if err != nil {
		return nil, err
	}

	var config schema.MachineConfig
	if err := json.Unmarshal(content, &config); err != nil {
		return nil, fmt.Errorf("parsing machine config: %w", err)
	}
	return &config, nil
}

// readCredentials reads the Credentials state event for a specific principal.
func (d *Daemon) readCredentials(ctx context.Context, principalLocalpart string) (*schema.Credentials, error) {
	content, err := d.session.GetStateEvent(ctx, d.configRoomID, schema.EventTypeCredentials, principalLocalpart)
	if err != nil {
		return nil, err
	}

	var credentials schema.Credentials
	if err := json.Unmarshal(content, &credentials); err != nil {
		return nil, fmt.Errorf("parsing credentials for %q: %w", principalLocalpart, err)
	}
	return &credentials, nil
}

// launcherIPCRequest mirrors the launcher's IPCRequest type. Defined here to
// avoid importing cmd/bureau-launcher (which is a main package and cannot be
// imported). The JSON wire format is the contract between daemon and launcher.
type launcherIPCRequest struct {
	Action               string               `json:"action"`
	Principal            string               `json:"principal,omitempty"`
	EncryptedCredentials string               `json:"encrypted_credentials,omitempty"`
	MatrixPolicy         *schema.MatrixPolicy `json:"matrix_policy,omitempty"`

	// SandboxSpec is the fully-resolved sandbox configuration produced by
	// the daemon's template resolution pipeline. When set, the launcher
	// uses this to build the bwrap command line and configure the sandbox
	// environment. When nil (current behavior), the launcher spawns only
	// the proxy process without a bwrap sandbox.
	SandboxSpec *schema.SandboxSpec `json:"sandbox_spec,omitempty"`

	// Payload is the new payload data for update-payload requests. The
	// launcher atomically rewrites the payload file that is bind-mounted
	// into the sandbox at /run/bureau/payload.json.
	Payload map[string]any `json:"payload,omitempty"`
}

// launcherIPCResponse mirrors the launcher's IPCResponse type.
type launcherIPCResponse struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	ProxyPID int    `json:"proxy_pid,omitempty"`
}

// launcherRequest sends a request to the launcher and reads the response.
func (d *Daemon) launcherRequest(ctx context.Context, request launcherIPCRequest) (*launcherIPCResponse, error) {
	// Connect to the launcher's unix socket.
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", d.launcherSocket)
	if err != nil {
		return nil, fmt.Errorf("connecting to launcher at %s: %w", d.launcherSocket, err)
	}
	defer conn.Close()

	// Use the context's deadline if set, otherwise fall back to 30 seconds
	// (matching the launcher's handler timeout).
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	conn.SetDeadline(deadline)

	// Send the request.
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return nil, fmt.Errorf("sending request to launcher: %w", err)
	}

	// Read the response.
	var response launcherIPCResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return nil, fmt.Errorf("reading response from launcher: %w", err)
	}

	return &response, nil
}
