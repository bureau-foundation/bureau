// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// reconcile reads the current MachineConfig from Matrix and ensures the
// running sandboxes match the desired state. Acquires reconcileMu to
// serialize with health monitor rollbacks.
func (d *Daemon) reconcile(ctx context.Context) error {
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()

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

	// Check for Bureau core binary updates before principal reconciliation.
	// Proxy binary updates take effect immediately (for future sandbox
	// creation). Daemon binary changes trigger exec() self-replacement.
	// Launcher binary changes are detected but not yet acted on.
	if config.BureauVersion != nil {
		d.reconcileBureauVersion(ctx, config.BureauVersion)
	}

	// Determine the desired set of principals. Conditions are evaluated
	// here (not in the "create missing" pass) so that running principals
	// whose conditions become false are excluded from the desired set.
	// The "destroy unneeded" pass then naturally stops them. This enables
	// state-driven lifecycle orchestration: when a workspace status
	// changes from "active" to "teardown", agents gated on "active"
	// stop, and a teardown principal gated on "teardown" starts.
	desired := make(map[string]schema.PrincipalAssignment, len(config.Principals))
	for _, assignment := range config.Principals {
		if !assignment.AutoStart {
			continue
		}
		if !d.evaluateStartCondition(ctx, assignment.Localpart, assignment.StartCondition) {
			continue
		}
		desired[assignment.Localpart] = assignment
	}

	// Check for credential rotation on running principals. When the
	// ciphertext in the m.bureau.credentials state event changes,
	// destroy the sandbox so the "create missing" pass below recreates
	// it with the new credentials. This handles API key rotation, token
	// refresh, and any other credential update without manual intervention.
	for localpart := range d.running {
		if _, shouldRun := desired[localpart]; !shouldRun {
			continue // Will be destroyed in the "remove" pass below.
		}

		credentials, err := d.readCredentials(ctx, localpart)
		if err != nil {
			d.logger.Error("reading credentials for rotation check",
				"principal", localpart, "error", err)
			continue
		}

		previousCiphertext, tracked := d.lastCredentials[localpart]
		if !tracked {
			// First reconcile after daemon (re)start for this principal.
			// Record the current ciphertext for future comparisons.
			d.lastCredentials[localpart] = credentials.Ciphertext
			continue
		}

		if credentials.Ciphertext == previousCiphertext {
			continue
		}

		d.logger.Info("credentials changed, restarting principal",
			"principal", localpart)

		d.stopHealthMonitor(localpart)
		d.stopLayoutWatcher(localpart)

		response, err := d.launcherRequest(ctx, launcherIPCRequest{
			Action:    "destroy-sandbox",
			Principal: localpart,
		})
		if err != nil {
			d.logger.Error("destroy-sandbox IPC failed during credential rotation",
				"principal", localpart, "error", err)
			continue
		}
		if !response.OK {
			d.logger.Error("destroy-sandbox rejected during credential rotation",
				"principal", localpart, "error", response.Error)
			continue
		}

		delete(d.running, localpart)
		delete(d.lastSpecs, localpart)
		delete(d.previousSpecs, localpart)
		delete(d.lastTemplates, localpart)
		delete(d.lastCredentials, localpart)
		delete(d.lastVisibility, localpart)
		delete(d.lastMatrixPolicy, localpart)
		delete(d.lastObservePolicy, localpart)
		d.lastActivityAt = time.Now()
		d.logger.Info("principal stopped for credential rotation (will recreate)",
			"principal", localpart)

		d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
			fmt.Sprintf("Restarting %s: credentials updated", localpart),
		))
	}

	// Hot-reload ServiceVisibility patterns when they change. This runs
	// in reconcile() (not reconcileRunningPrincipal) because it applies
	// to all principals including proxy-only ones without templates.
	// The proxy already has PUT /v1/admin/visibility — we just need to
	// call it when the PrincipalAssignment's patterns diverge from what
	// we last pushed.
	for localpart, assignment := range desired {
		if !d.running[localpart] {
			continue
		}
		oldVisibility := d.lastVisibility[localpart]
		newVisibility := assignment.ServiceVisibility
		if !slices.Equal(oldVisibility, newVisibility) {
			d.logger.Info("service visibility changed, updating proxy",
				"principal", localpart)
			if err := d.pushVisibilityToProxy(ctx, localpart, newVisibility); err != nil {
				d.logger.Error("push visibility failed during hot-reload",
					"principal", localpart, "error", err)
				continue
			}
			d.lastVisibility[localpart] = newVisibility
		}
	}

	// Hot-reload MatrixPolicy when it changes on a running principal.
	for localpart, assignment := range desired {
		if !d.running[localpart] {
			continue
		}
		oldPolicy := d.lastMatrixPolicy[localpart]
		newPolicy := assignment.MatrixPolicy
		if !reflect.DeepEqual(oldPolicy, newPolicy) {
			d.logger.Info("matrix policy changed, updating proxy",
				"principal", localpart)
			if err := d.pushMatrixPolicyToProxy(ctx, localpart, newPolicy); err != nil {
				d.logger.Error("push matrix policy failed during hot-reload",
					"principal", localpart, "error", err)
				continue
			}
			d.lastMatrixPolicy[localpart] = newPolicy
		}
	}

	// Enforce ObservePolicy changes on active observation sessions.
	// When a principal's policy tightens (fewer allowed observers, or
	// readwrite downgraded to readonly), in-flight sessions that no
	// longer pass authorization are terminated by closing their client
	// connection. This uses separate tracking maps from lastConfig —
	// lastConfig is updated above (so authorizeObserve uses the new
	// policy), while these maps detect which principals changed.
	defaultPolicyChanged := !reflect.DeepEqual(
		d.lastDefaultObservePolicy, config.DefaultObservePolicy)
	if defaultPolicyChanged {
		d.lastDefaultObservePolicy = config.DefaultObservePolicy
	}
	for localpart, assignment := range desired {
		if !d.running[localpart] {
			continue
		}
		oldPolicy := d.lastObservePolicy[localpart]
		newPolicy := assignment.ObservePolicy
		if defaultPolicyChanged || !reflect.DeepEqual(oldPolicy, newPolicy) {
			d.lastObservePolicy[localpart] = newPolicy
			d.enforceObservePolicyChange(localpart)
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
	// StartConditions were already evaluated when building the desired
	// set above — only principals with satisfied conditions are here.
	for localpart, assignment := range desired {
		if d.running[localpart] {
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
		var resolvedTemplate *schema.TemplateContent
		if assignment.Template != "" {
			template, err := resolveTemplate(ctx, d.session, assignment.Template, d.serverName)
			if err != nil {
				d.logger.Error("resolving template", "principal", localpart, "template", assignment.Template, "error", err)
				continue
			}
			resolvedTemplate = template
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
		d.lastCredentials[localpart] = credentials.Ciphertext
		d.lastVisibility[localpart] = assignment.ServiceVisibility
		d.lastMatrixPolicy[localpart] = assignment.MatrixPolicy
		d.lastObservePolicy[localpart] = assignment.ObservePolicy
		d.lastSpecs[localpart] = sandboxSpec
		d.lastTemplates[localpart] = resolvedTemplate
		d.lastActivityAt = time.Now()
		d.logger.Info("principal started", "principal", localpart)

		// Start watching the tmux session for layout changes. This also
		// restores any previously saved layout from Matrix.
		d.startLayoutWatcher(ctx, localpart)

		// Start health monitoring if the template defines a health check.
		// The monitor waits a grace period before its first probe, giving
		// the sandbox and proxy time to initialize.
		if resolvedTemplate != nil && resolvedTemplate.HealthCheck != nil {
			d.startHealthMonitor(ctx, localpart, resolvedTemplate.HealthCheck)
		}

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

		// Stop health monitoring and layout watching before destroying
		// the sandbox. This ensures clean shutdown rather than having
		// monitors see the sandbox disappear underneath them.
		d.stopHealthMonitor(localpart)
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
		delete(d.lastCredentials, localpart)
		delete(d.lastVisibility, localpart)
		delete(d.lastMatrixPolicy, localpart)
		delete(d.lastObservePolicy, localpart)
		delete(d.lastSpecs, localpart)
		delete(d.previousSpecs, localpart)
		delete(d.lastTemplates, localpart)
		d.lastActivityAt = time.Now()
		d.logger.Info("principal stopped", "principal", localpart)
	}

	return nil
}

// reconcileRunningPrincipal checks if a running principal's configuration has
// changed and takes appropriate action:
//   - Structural changes (command, mounts, namespaces, resources, security,
//     environment): destroys the sandbox. The main reconcile loop's "create
//     missing" pass will recreate it with the new spec on the same cycle.
//   - Payload-only changes: hot-reloads the bind-mounted payload file
//     without restarting the sandbox.
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
		// and template for future comparisons but don't trigger any
		// changes.
		d.lastSpecs[localpart] = newSpec
		d.lastTemplates[localpart] = template
		return
	}

	// Structural changes take precedence: destroy the sandbox and let the
	// "create missing" pass in reconcile() rebuild it with the new spec.
	// This handles changes to command, mounts, namespaces, resources,
	// security, environment variables, etc. — anything that requires a
	// new bwrap invocation.
	if structurallyChanged(oldSpec, newSpec) {
		d.logger.Info("structural change detected, restarting sandbox",
			"principal", localpart,
			"template", assignment.Template,
		)

		d.stopHealthMonitor(localpart)
		d.stopLayoutWatcher(localpart)

		response, err := d.launcherRequest(ctx, launcherIPCRequest{
			Action:    "destroy-sandbox",
			Principal: localpart,
		})
		if err != nil {
			d.logger.Error("destroy-sandbox IPC failed during structural restart",
				"principal", localpart, "error", err)
			return
		}
		if !response.OK {
			d.logger.Error("destroy-sandbox rejected during structural restart",
				"principal", localpart, "error", response.Error)
			return
		}

		// Save the current spec as the rollback target before clearing
		// it. The "create missing" pass will recreate the sandbox with
		// the new spec; if health checks fail, the daemon can roll back
		// to this previous working configuration.
		d.previousSpecs[localpart] = d.lastSpecs[localpart]

		delete(d.running, localpart)
		delete(d.lastSpecs, localpart)
		delete(d.lastVisibility, localpart)
		delete(d.lastMatrixPolicy, localpart)
		delete(d.lastObservePolicy, localpart)
		d.lastActivityAt = time.Now()
		d.logger.Info("sandbox destroyed for structural restart (will recreate)",
			"principal", localpart)

		d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
			fmt.Sprintf("Restarting %s: sandbox configuration changed (template %s)",
				localpart, assignment.Template),
		))
		return
	}

	// Payload-only change: hot-reload by rewriting the payload file.
	// The agent process sees the update via the bind-mounted file at
	// /run/bureau/payload.json (inotify or periodic poll).
	if payloadChanged(oldSpec, newSpec) {
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

	content, err := d.session.GetStateEvent(ctx, roomID, condition.EventType, condition.StateKey)
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

	// Event exists. If ContentMatch is specified, verify all key-value
	// pairs match the event content.
	if len(condition.ContentMatch) > 0 {
		var contentMap map[string]any
		if err := json.Unmarshal(content, &contentMap); err != nil {
			d.logger.Info("start condition content not a JSON object, deferring principal",
				"principal", localpart,
				"event_type", condition.EventType,
				"room_id", roomID,
				"error", err,
			)
			return false
		}
		for matchKey, matchValue := range condition.ContentMatch {
			actual, exists := contentMap[matchKey]
			if !exists {
				d.logger.Info("start condition content_match key missing, deferring principal",
					"principal", localpart,
					"event_type", condition.EventType,
					"room_id", roomID,
					"key", matchKey,
					"expected", matchValue,
				)
				return false
			}
			actualString, ok := actual.(string)
			if !ok || actualString != matchValue {
				d.logger.Info("start condition content_match value mismatch, deferring principal",
					"principal", localpart,
					"event_type", condition.EventType,
					"room_id", roomID,
					"key", matchKey,
					"expected", matchValue,
					"actual", actual,
				)
				return false
			}
		}
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
	DirectCredentials    map[string]string    `json:"direct_credentials,omitempty"`
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

	// BinaryPath is a filesystem path used by the "update-proxy-binary"
	// action. The launcher validates the path and switches its proxy
	// binary for future sandbox creation.
	BinaryPath string `json:"binary_path,omitempty"`
}

// launcherIPCResponse mirrors the launcher's IPCResponse type.
type launcherIPCResponse struct {
	OK              bool   `json:"ok"`
	Error           string `json:"error,omitempty"`
	ProxyPID        int    `json:"proxy_pid,omitempty"`
	BinaryHash      string `json:"binary_hash,omitempty"`
	ProxyBinaryPath string `json:"proxy_binary_path,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
}

// queryLauncherStatus sends a "status" IPC request to the launcher and
// returns the launcher's binary hash and the proxy binary path it is currently
// using for new sandbox creation. These values are needed by
// CompareBureauVersion to determine whether launcher or proxy updates are
// required.
func (d *Daemon) queryLauncherStatus(ctx context.Context) (launcherHash string, proxyBinaryPath string, err error) {
	response, err := d.launcherRequest(ctx, launcherIPCRequest{
		Action: "status",
	})
	if err != nil {
		return "", "", fmt.Errorf("launcher status IPC: %w", err)
	}
	if !response.OK {
		return "", "", fmt.Errorf("launcher status rejected: %s", response.Error)
	}
	return response.BinaryHash, response.ProxyBinaryPath, nil
}

// reconcileBureauVersion compares the desired BureauVersion from MachineConfig
// against the currently running binaries and takes action on any differences.
//
// Proxy binary changes are applied immediately by telling the launcher to use
// the new binary path for future sandbox creation. Daemon and launcher binary
// changes are detected and logged but not yet acted upon — the exec()
// self-update flow is a separate capability.
func (d *Daemon) reconcileBureauVersion(ctx context.Context, desired *schema.BureauVersion) {
	// Prefetch all store paths so they're available locally for hashing.
	if err := d.prefetchBureauVersion(ctx, desired); err != nil {
		d.logger.Error("prefetching bureau version store paths", "error", err)
		d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
			fmt.Sprintf("Failed to prefetch BureauVersion store paths: %v (will retry on next reconcile cycle)", err),
		))
		return
	}

	// Query the launcher for its current binary hash and proxy binary path.
	launcherHash, proxyBinaryPath, err := d.queryLauncherStatus(ctx)
	if err != nil {
		d.logger.Error("querying launcher status for version comparison", "error", err)
		return
	}

	// Compare desired versions against running versions.
	diff, err := CompareBureauVersion(desired, d.daemonBinaryHash, launcherHash, proxyBinaryPath)
	if err != nil {
		d.logger.Error("comparing bureau versions", "error", err)
		return
	}
	if diff == nil || !diff.NeedsUpdate() {
		return
	}

	d.logger.Info("bureau version changes detected",
		"daemon_changed", diff.DaemonChanged,
		"launcher_changed", diff.LauncherChanged,
		"proxy_changed", diff.ProxyChanged,
	)

	// Handle proxy binary update: tell the launcher to use the new binary
	// for future sandbox creation. Existing proxies continue running their
	// current binary until their sandbox is recycled.
	if diff.ProxyChanged {
		response, err := d.launcherRequest(ctx, launcherIPCRequest{
			Action:     "update-proxy-binary",
			BinaryPath: desired.ProxyStorePath,
		})
		if err != nil {
			d.logger.Error("update-proxy-binary IPC failed", "error", err)
		} else if !response.OK {
			d.logger.Error("update-proxy-binary rejected", "error", response.Error)
		} else {
			d.logger.Info("proxy binary updated on launcher",
				"new_path", desired.ProxyStorePath)
		}
	}

	// Daemon self-update via exec(). On success, this call does not
	// return — the process is replaced by the new binary. On failure
	// (or retry skip), execution continues with the current binary.
	// execDaemon handles its own Matrix reporting for both outcomes.
	if diff.DaemonChanged {
		if err := d.execDaemon(ctx, desired.DaemonStorePath); err != nil {
			d.logger.Error("daemon self-update failed", "error", err)
		}
	}

	// Launcher self-update via exec(). The launcher writes its sandbox
	// state to disk, sends the OK response, and calls syscall.Exec().
	// The new launcher reconnects to surviving proxy processes on startup.
	// If exec fails, the launcher records the failure and rejects future
	// retries for the same path.
	if diff.LauncherChanged {
		response, err := d.launcherRequest(ctx, launcherIPCRequest{
			Action:     "exec-update",
			BinaryPath: desired.LauncherStorePath,
		})
		if err != nil {
			d.logger.Error("launcher exec-update IPC failed", "error", err)
		} else if !response.OK {
			d.logger.Warn("launcher exec-update rejected",
				"error", response.Error,
				"store_path", desired.LauncherStorePath,
			)
		} else {
			d.logger.Info("launcher exec-update accepted, exec() imminent",
				"store_path", desired.LauncherStorePath,
			)
		}
	}

	// Report non-daemon version changes to the config room. Daemon
	// changes are reported by execDaemon (pre-exec message) and
	// checkDaemonWatchdog (post-exec success/failure).
	var summaryParts []string
	if diff.ProxyChanged {
		summaryParts = append(summaryParts, "proxy binary updated for future sandbox creation")
	}
	if diff.LauncherChanged {
		summaryParts = append(summaryParts, "launcher exec() initiated")
	}
	if len(summaryParts) > 0 {
		d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
			"BureauVersion: "+strings.Join(summaryParts, "; ")+"."))
	}
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

// launcherWaitSandbox sends a "wait-sandbox" IPC request to the launcher
// and blocks until the named sandbox's process exits. Returns the process
// exit code (0 for success, non-zero for failure, -1 for abnormal
// termination).
//
// Unlike launcherRequest, this method is designed for long-lived
// connections. The context controls cancellation: when cancelled, the
// connection is closed, which unblocks the launcher's handler.
//
// The error string in the response (if any) describes the process exit
// condition (signal name, etc.) and is informational — the exit code is
// the authoritative success/failure indicator.
func (d *Daemon) launcherWaitSandbox(ctx context.Context, principalLocalpart string) (int, string, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", d.launcherSocket)
	if err != nil {
		return -1, "", fmt.Errorf("connecting to launcher at %s: %w", d.launcherSocket, err)
	}
	defer conn.Close()

	// Close the connection when the context is cancelled. This unblocks
	// the Read call below and signals the launcher to clean up its
	// wait-sandbox handler.
	readDone := make(chan struct{})
	defer close(readDone)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-readDone:
		}
	}()

	// Send the request with a short write deadline — the send itself
	// should be fast; only the response blocks for the process lifetime.
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	request := launcherIPCRequest{
		Action:    "wait-sandbox",
		Principal: principalLocalpart,
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return -1, "", fmt.Errorf("sending wait-sandbox request: %w", err)
	}

	// Clear the write deadline and read without a deadline — the
	// launcher responds only after the sandbox process exits.
	conn.SetDeadline(time.Time{})

	var response launcherIPCResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		if ctx.Err() != nil {
			return -1, "", ctx.Err()
		}
		return -1, "", fmt.Errorf("reading wait-sandbox response: %w", err)
	}

	if !response.OK {
		return -1, "", fmt.Errorf("wait-sandbox: %s", response.Error)
	}

	exitCode := 0
	if response.ExitCode != nil {
		exitCode = *response.ExitCode
	}
	return exitCode, response.Error, nil
}
