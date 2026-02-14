// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/authorization"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ipc"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
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

	// Rebuild the authorization index from the current config. Each
	// principal gets its machine-default grants/allowances merged with
	// any per-principal authorization policy. SetPrincipal preserves
	// temporal grants that were added incrementally via /sync.
	// Principals removed from config are cleaned up from the index.
	d.rebuildAuthorizationIndex(config)

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
	triggerContents := make(map[string][]byte)
	conditionRoomIDs := make(map[string]string) // localpart → resolved workspace room ID
	for _, assignment := range config.Principals {
		if !assignment.AutoStart {
			continue
		}
		satisfied, triggerContent, conditionRoomID := d.evaluateStartCondition(ctx, assignment.Localpart, assignment.StartCondition)
		if !satisfied {
			continue
		}
		desired[assignment.Localpart] = assignment
		if triggerContent != nil {
			triggerContents[assignment.Localpart] = triggerContent
		}
		if conditionRoomID != "" {
			conditionRoomIDs[assignment.Localpart] = conditionRoomID
		}
	}

	// Check for credential rotation on running principals. When the
	// ciphertext in the m.bureau.credentials state event changes,
	// destroy the sandbox so the "create missing" pass below recreates
	// it with the new credentials. This handles API key rotation, token
	// refresh, and any other credential update without manual intervention.
	var rotatedPrincipals []string
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

		d.cancelExitWatcher(localpart)
		d.cancelProxyExitWatcher(localpart)
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

		d.revokeAndCleanupTokens(ctx, localpart)
		delete(d.running, localpart)
		delete(d.exitWatchers, localpart)
		delete(d.proxyExitWatchers, localpart)
		delete(d.lastSpecs, localpart)
		delete(d.previousSpecs, localpart)
		delete(d.lastTemplates, localpart)
		delete(d.lastCredentials, localpart)
		delete(d.lastGrants, localpart)
		delete(d.lastTokenMint, localpart)
		delete(d.lastObserveAllowances, localpart)
		d.lastActivityAt = d.clock.Now()
		d.logger.Info("principal stopped for credential rotation (will recreate)",
			"principal", localpart)
		rotatedPrincipals = append(rotatedPrincipals, localpart)

		d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
			fmt.Sprintf("Restarting %s: credentials updated", localpart),
		))
	}

	// Hot-reload authorization grants when they change on a running
	// principal. The proxy uses grants for Matrix API gating and service
	// directory filtering.
	if d.lastGrants == nil {
		d.lastGrants = make(map[string][]schema.Grant)
	}
	for localpart, assignment := range desired {
		if !d.running[localpart] {
			continue
		}
		newGrants := d.resolveGrantsForProxy(localpart, assignment, config)
		oldGrants := d.lastGrants[localpart]
		if !reflect.DeepEqual(oldGrants, newGrants) {
			d.logger.Info("authorization grants changed, updating proxy",
				"principal", localpart)
			if err := d.pushAuthorizationToProxy(ctx, localpart, newGrants); err != nil {
				d.logger.Error("push authorization grants failed during hot-reload",
					"principal", localpart, "error", err)
				continue
			}
			d.lastGrants[localpart] = newGrants

			// Force the token refresh goroutine to re-mint service
			// tokens on its next tick so they carry the updated grants.
			d.lastTokenMint[localpart] = time.Time{}

			d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
				fmt.Sprintf("Authorization grants updated for %s (%d grants)", localpart, len(newGrants)),
			))
		}
	}

	// Enforce observation allowance changes on active sessions. When a
	// principal's allowances tighten (fewer allowed observers, or
	// readwrite downgraded to readonly), in-flight sessions that no
	// longer pass authorization are terminated by closing their client
	// connection. The authorization index was rebuilt above (so
	// authorizeObserve uses the new allowances); this block detects
	// which principals changed by comparing index allowances to the
	// last-known state.
	for localpart := range desired {
		if !d.running[localpart] {
			continue
		}
		newAllowances := d.authorizationIndex.Allowances(localpart)
		oldAllowances := d.lastObserveAllowances[localpart]
		if !reflect.DeepEqual(oldAllowances, newAllowances) {
			d.lastObserveAllowances[localpart] = newAllowances
			d.enforceObserveAllowanceChange(localpart)
		}
	}

	// Check for hot-reloadable changes on already-running principals.
	for localpart, assignment := range desired {
		if !d.running[localpart] {
			continue
		}
		d.reconcileRunningPrincipal(ctx, localpart, assignment)
	}

	// Set up management goroutines for principals that survived a daemon
	// restart. After adoptPreExistingSandboxes populates d.running from
	// the launcher's list-sandboxes response, those entries have no exit
	// watcher, layout watcher, health monitor, or consumer proxy config.
	// The earlier reconcile passes (credential tracking, authorization
	// grants push, observe allowances, reconcileRunningPrincipal for lastSpecs)
	// already handled their state — this pass starts the goroutines.
	//
	// On subsequent reconcile calls all d.running entries have exit
	// watchers, so this loop is a no-op in steady state.
	for localpart, assignment := range desired {
		if !d.running[localpart] {
			continue
		}
		if _, hasWatcher := d.exitWatchers[localpart]; hasWatcher {
			continue // Already fully managed.
		}
		d.adoptSurvivingPrincipal(ctx, localpart, assignment)
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

			if d.applyPipelineExecutorOverlay(sandboxSpec) {
				d.logger.Info("applied pipeline executor overlay",
					"principal", localpart,
					"template", assignment.Template,
					"command", sandboxSpec.Command,
				)
			}

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

		// Invite the principal to any workspace room it references so it
		// can publish state events after joining. Two sources:
		// StartCondition.RoomAlias (resolved during condition evaluation)
		// and WORKSPACE_ROOM_ID in the payload (static per-principal
		// config from workspace create). Best-effort: if the invite
		// fails, the sandbox is still created.
		workspaceRoomID := conditionRoomIDs[localpart]
		if workspaceRoomID == "" && sandboxSpec != nil {
			if value, ok := sandboxSpec.Payload["WORKSPACE_ROOM_ID"].(string); ok {
				workspaceRoomID = value
			}
		}
		if workspaceRoomID != "" {
			d.ensurePrincipalRoomAccess(ctx, localpart, workspaceRoomID)
		}

		// Resolve required service sockets to host-side paths. The daemon
		// looks up m.bureau.room_service state events in rooms the principal
		// will be a member of (workspace room first, then config room).
		// Failure to resolve any required service blocks sandbox creation.
		var serviceMounts []launcherServiceMount
		if sandboxSpec != nil && len(sandboxSpec.RequiredServices) > 0 {
			mounts, err := d.resolveServiceMounts(ctx, sandboxSpec.RequiredServices, workspaceRoomID)
			if err != nil {
				d.logger.Error("resolving required services",
					"principal", localpart,
					"required_services", sandboxSpec.RequiredServices,
					"error", err,
				)
				d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
					fmt.Sprintf("Cannot start %s: %v", localpart, err),
				))
				continue
			}
			serviceMounts = mounts
		}

		// If the principal has credential/* grants, mount the daemon's
		// credential provisioning socket into the sandbox so the agent
		// can inject per-principal credentials via the provisioning API.
		serviceMounts = d.appendCredentialServiceMount(localpart, serviceMounts)

		// Compute the full set of service roles that need tokens: the
		// template's RequiredServices plus any daemon-managed services
		// the principal qualifies for (currently: credential provisioning
		// for principals with credential/* grants).
		var allServiceRoles []string
		if sandboxSpec != nil {
			allServiceRoles = append(allServiceRoles, sandboxSpec.RequiredServices...)
		}
		allServiceRoles = append(allServiceRoles, d.credentialServiceRoles(localpart)...)

		// Mint service tokens for each role. The tokens carry pre-resolved
		// grants scoped to the service namespace. Written to disk and
		// bind-mounted read-only at /run/bureau/tokens/ so the agent can
		// authenticate to services without a daemon round-trip.
		var tokenDirectory string
		var mintedTokens []activeToken
		if len(allServiceRoles) > 0 {
			tokenDir, minted, err := d.mintServiceTokens(localpart, allServiceRoles)
			if err != nil {
				d.logger.Error("minting service tokens",
					"principal", localpart,
					"error", err,
				)
				d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
					fmt.Sprintf("Cannot start %s: failed to mint service tokens: %v", localpart, err),
				))
				continue
			}
			tokenDirectory = tokenDir
			mintedTokens = minted
		}

		// Resolve authorization grants before sandbox creation so the
		// proxy starts with enforcement from the first request. The
		// launcher pipes these to the proxy's stdin alongside credentials.
		grants := d.resolveGrantsForProxy(localpart, assignment, config)

		// Send create-sandbox to the launcher.
		response, err := d.launcherRequest(ctx, launcherIPCRequest{
			Action:               "create-sandbox",
			Principal:            localpart,
			EncryptedCredentials: credentials.Ciphertext,
			Grants:               grants,
			SandboxSpec:          sandboxSpec,
			TriggerContent:       triggerContents[localpart],
			ServiceMounts:        serviceMounts,
			TokenDirectory:       tokenDirectory,
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
		d.lastObserveAllowances[localpart] = d.authorizationIndex.Allowances(localpart)
		d.lastSpecs[localpart] = sandboxSpec
		d.lastTemplates[localpart] = resolvedTemplate
		d.lastGrants[localpart] = grants
		d.lastTokenMint[localpart] = d.clock.Now()
		d.recordMintedTokens(localpart, mintedTokens)
		d.lastServiceMounts[localpart] = serviceMounts
		d.lastActivityAt = d.clock.Now()
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

		// Push the service directory so the new consumer's agent can
		// discover services via GET /v1/services.
		directory := d.buildServiceDirectory()
		if err := d.pushDirectoryToProxy(ctx, localpart, directory); err != nil {
			d.logger.Error("failed to push service directory to new consumer proxy",
				"consumer", localpart,
				"error", err,
			)
		}

		// Watch for sandbox process exit and proxy process exit. Each
		// goroutine blocks on a launcher IPC call (wait-sandbox or
		// wait-proxy) until the respective process exits, then clears
		// d.running and triggers re-reconciliation.
		//
		// Uses shutdownCtx (not the sync cycle's ctx) so the watchers
		// survive across sync iterations but cancel on daemon shutdown.
		// shutdownCtx is nil in unit tests — those tests don't exercise
		// exit watching (mock launchers return immediately for all IPC).
		//
		// Each watcher gets a per-principal cancellable context so the
		// daemon can cancel it before an intentional destroy (credential
		// rotation, structural restart, condition change). Without this,
		// the old watcher would see the destroy as an unexpected exit,
		// clear d.running, and trigger a duplicate recreation.
		if d.shutdownCtx != nil {
			watcherCtx, watcherCancel := context.WithCancel(d.shutdownCtx)
			d.exitWatchers[localpart] = watcherCancel
			go d.watchSandboxExit(watcherCtx, localpart)

			proxyCtx, proxyCancel := context.WithCancel(d.shutdownCtx)
			d.proxyExitWatchers[localpart] = proxyCancel
			go d.watchProxyExit(proxyCtx, localpart)
		}
	}

	// Report credential rotation outcomes. Rotated principals were
	// destroyed above and should have been recreated by the create loop.
	for _, localpart := range rotatedPrincipals {
		if d.running[localpart] {
			d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
				fmt.Sprintf("Restarted %s with new credentials", localpart),
			))
		} else {
			d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
				fmt.Sprintf("FAILED to restart %s after credential rotation", localpart),
			))
		}
	}

	// Destroy sandboxes for principals that should not be running.
	for localpart := range d.running {
		if _, shouldRun := desired[localpart]; shouldRun {
			continue
		}

		d.logger.Info("stopping principal", "principal", localpart)

		// Cancel exit watchers before destroying — the destroy is
		// intentional, not an unexpected exit.
		d.cancelExitWatcher(localpart)
		d.cancelProxyExitWatcher(localpart)

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

		d.revokeAndCleanupTokens(ctx, localpart)
		delete(d.running, localpart)
		delete(d.exitWatchers, localpart)
		delete(d.proxyExitWatchers, localpart)
		delete(d.lastCredentials, localpart)
		delete(d.lastGrants, localpart)
		delete(d.lastTokenMint, localpart)
		delete(d.lastObserveAllowances, localpart)
		delete(d.lastSpecs, localpart)
		delete(d.previousSpecs, localpart)
		delete(d.lastTemplates, localpart)
		d.lastActivityAt = d.clock.Now()
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

		d.cancelExitWatcher(localpart)
		d.cancelProxyExitWatcher(localpart)
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

		d.revokeAndCleanupTokens(ctx, localpart)
		delete(d.running, localpart)
		delete(d.exitWatchers, localpart)
		delete(d.proxyExitWatchers, localpart)
		delete(d.lastSpecs, localpart)
		delete(d.lastGrants, localpart)
		delete(d.lastTokenMint, localpart)
		delete(d.lastObserveAllowances, localpart)
		d.lastActivityAt = d.clock.Now()
		d.logger.Info("sandbox destroyed for structural restart (will recreate)",
			"principal", localpart)

		d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
			fmt.Sprintf("Restarting %s: sandbox configuration changed (template %s)",
				localpart, assignment.Template),
		))
		return
	}

	// Payload-only change: hot-reload by rewriting the payload file
	// in-place (preserving the bind-mounted inode). The agent reads the
	// updated file after receiving notification — see handleUpdatePayload
	// for the safety argument.
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
		d.lastActivityAt = d.clock.Now()
		d.logger.Info("payload hot-reloaded", "principal", localpart)

		d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
			fmt.Sprintf("Payload updated for %s", localpart),
		))
	}
}

// adoptSurvivingPrincipal sets up the management goroutines for a principal
// that survived a daemon restart. The launcher continued running the sandbox
// while the daemon was down — adoptPreExistingSandboxes added the principal
// to d.running, and earlier reconcile passes handled state tracking
// (credentials, grants, observe allowances, lastSpecs). This method starts the
// goroutines that the "create missing" pass normally starts: exit watcher,
// layout watcher, health monitor, and consumer proxy configuration.
// Authorization grants are handled by the hot-reload block that runs before
// the adoption pass.
//
// Caller must hold reconcileMu.
func (d *Daemon) adoptSurvivingPrincipal(ctx context.Context, localpart string, assignment schema.PrincipalAssignment) {
	d.logger.Info("setting up management for adopted principal", "principal", localpart)

	// Start watching the tmux session for layout changes.
	d.startLayoutWatcher(ctx, localpart)

	// Start health monitoring if the template defines a health check.
	if template := d.lastTemplates[localpart]; template != nil && template.HealthCheck != nil {
		d.startHealthMonitor(ctx, localpart, template.HealthCheck)
	}

	// Register all known local service routes on the adopted consumer's
	// proxy so it can reach services discovered while the daemon was down.
	d.configureConsumerProxy(ctx, localpart)

	// Push the service directory so the consumer's agent can discover
	// services via GET /v1/services.
	directory := d.buildServiceDirectory()
	if err := d.pushDirectoryToProxy(ctx, localpart, directory); err != nil {
		d.logger.Error("failed to push service directory to adopted consumer proxy",
			"consumer", localpart,
			"error", err,
		)
	}

	// Authorization grants are handled by the hot-reload block that runs
	// before the adoption pass. On daemon restart, d.lastGrants is empty,
	// so the hot-reload detects the mismatch and pushes grants.

	// Watch for sandbox and proxy process exit. Uses shutdownCtx (not
	// the sync cycle's ctx) so the watchers survive across sync iterations.
	if d.shutdownCtx != nil {
		watcherCtx, watcherCancel := context.WithCancel(d.shutdownCtx)
		d.exitWatchers[localpart] = watcherCancel
		go d.watchSandboxExit(watcherCtx, localpart)

		proxyCtx, proxyCancel := context.WithCancel(d.shutdownCtx)
		d.proxyExitWatchers[localpart] = proxyCancel
		go d.watchProxyExit(proxyCtx, localpart)
	}

	d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
		fmt.Sprintf("Adopted %s from previous daemon instance", localpart),
	))
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
// satisfied. Returns (satisfied, eventContent, resolvedRoomID):
//   - satisfied: true when the condition is met
//   - eventContent: the full content of the matched state event, passed
//     through as trigger content to the sandbox
//   - resolvedRoomID: the Matrix room ID resolved from StartCondition.RoomAlias,
//     empty when the condition uses the config room or is nil
//
// When StartCondition is nil, returns (true, nil, "").
//
// The condition references a state event in a specific room. When RoomAlias is
// set, the daemon resolves it to a room ID. When empty, the daemon checks the
// principal's own config room (where MachineConfig lives). If the state event
// exists, the condition is met. If it's M_NOT_FOUND, the principal is deferred.
func (d *Daemon) evaluateStartCondition(ctx context.Context, localpart string, condition *schema.StartCondition) (bool, json.RawMessage, string) {
	if condition == nil {
		return true, nil, ""
	}

	// Determine which room to check. When RoomAlias is empty, check the
	// principal's config room (the room where the MachineConfig lives).
	roomID := d.configRoomID
	conditionRoomID := "" // Only set when RoomAlias is used (workspace rooms).
	if condition.RoomAlias != "" {
		resolved, err := d.session.ResolveAlias(ctx, condition.RoomAlias)
		if err != nil {
			if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
				d.logger.Info("start condition room alias not found, deferring principal",
					"principal", localpart,
					"room_alias", condition.RoomAlias,
				)
				return false, nil, ""
			}
			d.logger.Error("resolving start condition room alias",
				"principal", localpart,
				"room_alias", condition.RoomAlias,
				"error", err,
			)
			return false, nil, ""
		}
		roomID = resolved
		conditionRoomID = resolved
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
			return false, nil, ""
		}
		d.logger.Error("checking start condition",
			"principal", localpart,
			"event_type", condition.EventType,
			"state_key", condition.StateKey,
			"room_id", roomID,
			"error", err,
		)
		return false, nil, ""
	}

	// Event exists. If ContentMatch is specified, verify all criteria
	// match the event content.
	if len(condition.ContentMatch) > 0 {
		var contentMap map[string]any
		if err := json.Unmarshal(content, &contentMap); err != nil {
			d.logger.Info("start condition content not a JSON object, deferring principal",
				"principal", localpart,
				"event_type", condition.EventType,
				"room_id", roomID,
				"error", err,
			)
			return false, nil, ""
		}
		matched, failedKey, err := condition.ContentMatch.Evaluate(contentMap)
		if err != nil {
			d.logger.Error("start condition content_match expression error",
				"principal", localpart,
				"event_type", condition.EventType,
				"room_id", roomID,
				"key", failedKey,
				"error", err,
			)
			return false, nil, ""
		}
		if !matched {
			d.logger.Info("start condition content_match not satisfied, deferring principal",
				"principal", localpart,
				"event_type", condition.EventType,
				"room_id", roomID,
				"key", failedKey,
			)
			return false, nil, ""
		}
	}

	return true, content, conditionRoomID
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

// applyPipelineExecutorOverlay detects when a template-resolved SandboxSpec
// has no command but the payload references a pipeline, and overlays the
// pipeline executor binary as the entrypoint. This bridges the gap between
// generic templates (like "base") and pipeline-driven principals (workspace
// setup/teardown) created by "bureau workspace create".
//
// The overlay is additive: it sets the command, appends filesystem mounts
// for the executor binary and workspace root, sets BUREAU_SANDBOX=1, and
// applies the pipeline environment if the spec doesn't already have one.
// Existing spec fields (namespaces, security, other mounts) are preserved.
func (d *Daemon) applyPipelineExecutorOverlay(spec *schema.SandboxSpec) bool {
	if len(spec.Command) > 0 || d.pipelineExecutorBinary == "" {
		return false
	}

	// Check whether the payload references a pipeline. The executor
	// resolves pipeline_ref (Tier 2) and pipeline_inline (Tier 3) from
	// the payload file at /run/bureau/payload.json.
	_, hasRef := spec.Payload["pipeline_ref"].(string)
	_, hasInline := spec.Payload["pipeline_inline"]
	if !hasRef && !hasInline {
		return false
	}

	spec.Command = []string{d.pipelineExecutorBinary}

	spec.Filesystem = append(spec.Filesystem,
		// Executor binary: bind-mounted at its host path so bwrap can
		// find it. Needed when the binary is outside /nix/store (Bazel
		// outputs, go install paths). Harmless when it is under
		// /nix/store since the more-specific mount coexists with the
		// /nix/store mount from the environment.
		schema.TemplateMount{Source: d.pipelineExecutorBinary, Dest: d.pipelineExecutorBinary, Mode: "ro"},
		// Workspace root: pipeline steps reference /workspace/${PROJECT}
		// for git operations, file creation, etc.
		schema.TemplateMount{Source: d.workspaceRoot, Dest: "/workspace", Mode: "rw"},
	)

	if spec.EnvironmentVariables == nil {
		spec.EnvironmentVariables = make(map[string]string)
	}
	spec.EnvironmentVariables["BUREAU_SANDBOX"] = "1"

	// Apply the pipeline environment (Nix store path providing git, sh,
	// etc.) only when the template didn't already specify one.
	if d.pipelineEnvironment != "" && spec.EnvironmentPath == "" {
		spec.EnvironmentPath = d.pipelineEnvironment
	}

	return true
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

// rebuildAuthorizationIndex rebuilds the authorization index from the
// current MachineConfig. For each principal in the config, it merges the
// machine-wide DefaultPolicy with the per-principal Authorization policy
// and calls SetPrincipal to update the index. SetPrincipal preserves any
// temporal grants that were added incrementally via /sync.
//
// Principals that were previously in the index but are no longer in the
// config are removed. This handles principals being removed from the
// MachineConfig (decommissioned, moved to another machine, etc.).
//
// Caller must hold reconcileMu.
func (d *Daemon) rebuildAuthorizationIndex(config *schema.MachineConfig) {
	// Lazily initialize the index. Production code always sets this in
	// the Daemon constructor; tests that predate authorization may omit
	// it. Lazy init avoids updating every existing test site while
	// keeping the index available for any test that triggers reconcile.
	if d.authorizationIndex == nil {
		d.authorizationIndex = authorization.NewIndex()
	}

	// Track which principals are in the current config so we can clean
	// up stale entries afterwards.
	currentPrincipals := make(map[string]bool, len(config.Principals))

	for _, assignment := range config.Principals {
		currentPrincipals[assignment.Localpart] = true

		// Merge machine defaults with per-principal policy. Per-principal
		// policy is additive: grants, denials, allowances, and allowance
		// denials are appended to the machine defaults. A principal cannot
		// have fewer permissions than the machine default.
		merged := mergeAuthorizationPolicy(config.DefaultPolicy, assignment.Authorization)
		d.authorizationIndex.SetPrincipal(assignment.Localpart, merged)
	}

	// Remove principals that are no longer in config. This also clears
	// their temporal grants.
	for _, localpart := range d.authorizationIndex.Principals() {
		if !currentPrincipals[localpart] {
			d.authorizationIndex.RemovePrincipal(localpart)
			d.logger.Info("removed principal from authorization index",
				"principal", localpart)
		}
	}
}

// mergeAuthorizationPolicy merges a machine-wide default policy with a
// per-principal policy. Both may be nil. The result is always a valid
// (possibly empty) AuthorizationPolicy. Per-principal entries are appended
// after defaults so they are evaluated after machine-wide rules.
func mergeAuthorizationPolicy(defaultPolicy, principalPolicy *schema.AuthorizationPolicy) schema.AuthorizationPolicy {
	var merged schema.AuthorizationPolicy

	if defaultPolicy != nil {
		merged.Grants = append(merged.Grants, defaultPolicy.Grants...)
		merged.Denials = append(merged.Denials, defaultPolicy.Denials...)
		merged.Allowances = append(merged.Allowances, defaultPolicy.Allowances...)
		merged.AllowanceDenials = append(merged.AllowanceDenials, defaultPolicy.AllowanceDenials...)
	}

	if principalPolicy != nil {
		merged.Grants = append(merged.Grants, principalPolicy.Grants...)
		merged.Denials = append(merged.Denials, principalPolicy.Denials...)
		merged.Allowances = append(merged.Allowances, principalPolicy.Allowances...)
		merged.AllowanceDenials = append(merged.AllowanceDenials, principalPolicy.AllowanceDenials...)
	}

	return merged
}

// ensurePrincipalRoomAccess invites a principal to a workspace room so it can
// publish state events (m.bureau.workspace at PL 0). Called before
// create-sandbox for principals whose StartCondition references a workspace
// room or whose payload contains WORKSPACE_ROOM_ID.
//
// The invite is best-effort: failures are logged but do not block sandbox
// creation. M_FORBIDDEN typically means the user is already a member.
func (d *Daemon) ensurePrincipalRoomAccess(ctx context.Context, localpart, workspaceRoomID string) {
	userID := principal.MatrixUserID(localpart, d.serverName)

	// Invite the principal to the workspace room. Workspace rooms use
	// membership as the authorization boundary (m.bureau.workspace at PL 0,
	// room is invite-only), so the invite is the only access control step
	// needed. Power level modification is not attempted — Continuwuity
	// validates all fields in m.room.power_levels against the sender's PL
	// (not just changed ones), so only admin (PL 100) can modify PLs.
	if err := d.session.InviteUser(ctx, workspaceRoomID, userID); err != nil {
		// M_FORBIDDEN typically means the user is already a member.
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			d.logger.Warn("failed to invite principal to workspace room",
				"principal", localpart,
				"room_id", workspaceRoomID,
				"error", err,
			)
		}
	}
}

// resolveServiceMounts resolves a list of required service roles to host-side
// socket paths by looking up m.bureau.room_service state events. Rooms are
// checked in specificity order: workspace room first (if any), then the
// machine config room. The first binding found for each role wins.
//
// Returns an error if any required service cannot be resolved — callers should
// treat this as a fatal condition that blocks sandbox creation (no silent
// fallbacks, no degraded mode).
func (d *Daemon) resolveServiceMounts(ctx context.Context, requiredServices []string, workspaceRoomID string) ([]launcherServiceMount, error) {
	// Collect rooms to search, ordered by specificity (most specific first).
	// Workspace room bindings override machine config room bindings.
	var rooms []string
	if workspaceRoomID != "" {
		rooms = append(rooms, workspaceRoomID)
	}
	rooms = append(rooms, d.configRoomID)

	var mounts []launcherServiceMount
	for _, role := range requiredServices {
		socketPath, err := d.resolveServiceSocket(ctx, role, rooms)
		if err != nil {
			return nil, fmt.Errorf("resolving required service %q: %w", role, err)
		}
		mounts = append(mounts, launcherServiceMount{
			Role:       role,
			SocketPath: socketPath,
		})
	}
	return mounts, nil
}

// resolveServiceSocket looks up a single service role in the given rooms.
// For each room, it fetches the m.bureau.room_service state event with the
// role as the state key. If found, derives the host-side socket path from
// the binding's principal localpart.
func (d *Daemon) resolveServiceSocket(ctx context.Context, role string, rooms []string) (string, error) {
	for _, roomID := range rooms {
		content, err := d.session.GetStateEvent(ctx, roomID, schema.EventTypeRoomService, role)
		if err != nil {
			if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
				continue // Not bound in this room, try next.
			}
			return "", fmt.Errorf("fetching service binding in room %s: %w", roomID, err)
		}

		var binding schema.RoomServiceContent
		if err := json.Unmarshal(content, &binding); err != nil {
			return "", fmt.Errorf("parsing service binding for role %q in room %s: %w", role, roomID, err)
		}

		if binding.Principal == "" {
			return "", fmt.Errorf("service binding for role %q in room %s has empty principal", role, roomID)
		}

		// Derive the host-side socket path from the service principal's
		// localpart. For local services this is the actual proxy socket;
		// for remote services (future) the daemon would create a tunnel
		// socket and use that path instead.
		socketPath := principal.RunDirSocketPath(d.runDir, binding.Principal)
		d.logger.Info("resolved service binding",
			"role", role,
			"principal", binding.Principal,
			"socket", socketPath,
			"room", roomID,
		)
		return socketPath, nil
	}

	return "", fmt.Errorf("no binding found for service role %q in any accessible room", role)
}

// tokenTTL is the default TTL for service tokens. Set to 5 minutes to
// limit exposure from a compromised token. The daemon's refresh goroutine
// renews at 80% of this duration.
const tokenTTL = 5 * time.Minute

// mintServiceTokens mints a signed service token for each required
// service and writes them to disk. Returns the host-side directory path
// containing the token files (suitable for bind-mounting into the sandbox
// at /run/bureau/tokens/) and the list of minted token entries for
// tracking by the caller.
//
// For each required service role:
//   - Resolves the principal's grants from the authorization index
//   - Filters grants to those whose action patterns match the service
//     namespace (role + "/**")
//   - Converts schema.Grant to servicetoken.Grant (strips audit metadata)
//   - Mints a servicetoken.Token with the daemon's signing key
//   - Writes the raw signed bytes to <stateDir>/tokens/<localpart>/<role>
//
// Returns ("", nil, nil) if there are no required services. Returns an
// error if token minting or file I/O fails — callers should treat this
// as a fatal condition that blocks sandbox creation.
func (d *Daemon) mintServiceTokens(localpart string, requiredServices []string) (string, []activeToken, error) {
	if len(requiredServices) == 0 {
		return "", nil, nil
	}

	if d.tokenSigningPrivateKey == nil {
		return "", nil, fmt.Errorf("token signing keypair not initialized (required for service token minting)")
	}
	if d.stateDir == "" {
		return "", nil, fmt.Errorf("state directory not configured (required for writing service tokens)")
	}

	// Ensure the token directory exists.
	tokenDir := filepath.Join(d.stateDir, "tokens", localpart)
	if err := os.MkdirAll(tokenDir, 0700); err != nil {
		return "", nil, fmt.Errorf("creating token directory: %w", err)
	}

	// Get the principal's resolved grants from the authorization index.
	grants := d.authorizationIndex.Grants(localpart)

	now := d.clock.Now()
	var minted []activeToken

	for _, role := range requiredServices {
		// Filter grants to those relevant to this service's namespace.
		// A grant is relevant if any of its action patterns could match
		// actions in the service's namespace (role + "/**"). We check
		// this by testing whether the pattern matches "role/" as a
		// prefix, or is a broad wildcard like "**" or "service/**".
		//
		// The filtering is intentionally generous: we include any grant
		// whose action patterns COULD match a service-namespaced action.
		// The service itself performs the definitive authorization check
		// using servicetoken.GrantsAllow on the specific action.
		filtered := filterGrantsForService(grants, role)

		// Convert schema.Grant → servicetoken.Grant (strip audit metadata).
		tokenGrants := make([]servicetoken.Grant, len(filtered))
		for i, grant := range filtered {
			tokenGrants[i] = servicetoken.Grant{
				Actions: grant.Actions,
				Targets: grant.Targets,
			}
		}

		// Generate a unique token ID for emergency revocation.
		tokenID, err := generateTokenID()
		if err != nil {
			return "", nil, fmt.Errorf("generating token ID for service %q: %w", role, err)
		}

		token := &servicetoken.Token{
			Subject:   localpart,
			Machine:   d.machineName,
			Audience:  role,
			Grants:    tokenGrants,
			ID:        tokenID,
			IssuedAt:  now.Unix(),
			ExpiresAt: now.Add(tokenTTL).Unix(),
		}

		tokenBytes, err := servicetoken.Mint(d.tokenSigningPrivateKey, token)
		if err != nil {
			return "", nil, fmt.Errorf("minting token for service %q: %w", role, err)
		}

		tokenPath := filepath.Join(tokenDir, role)
		if err := atomicWriteFile(tokenPath, tokenBytes, 0600); err != nil {
			return "", nil, fmt.Errorf("writing token for service %q: %w", role, err)
		}

		minted = append(minted, activeToken{
			id:          tokenID,
			serviceRole: role,
			expiresAt:   now.Add(tokenTTL),
		})

		d.logger.Info("minted service token",
			"principal", localpart,
			"service", role,
			"token_id", tokenID,
			"grants", len(tokenGrants),
			"expires_at", now.Add(tokenTTL).Format(time.RFC3339),
		)
	}

	return tokenDir, minted, nil
}

// atomicWriteFile writes data to path atomically by writing to a
// temporary file and renaming. This is safe for files inside
// bind-mounted directories — the sandbox sees the update via VFS path
// traversal because rename within the same directory is atomic on Linux.
func atomicWriteFile(path string, data []byte, permission os.FileMode) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, permission); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// filterGrantsForService returns the subset of grants whose action
// patterns could match actions in the given service namespace. A service
// with role "ticket" expects actions like "ticket/create", "ticket/close",
// etc. A grant with action patterns ["ticket/*"] or ["**"] would match;
// a grant with ["observe/*"] would not.
//
// A grant is included if any of its action patterns either:
//   - Starts with the role prefix (literal match: "ticket/create" starts
//     with "ticket/")
//   - Is a glob that matches a synthetic probe action in the namespace
//     (e.g., "**" matches "ticket/x", "service/*" matches "service/x")
//   - Equals the role exactly (bare "ticket" — targets the service itself)
//
// Grants with no action patterns are skipped (they authorize nothing).
func filterGrantsForService(grants []schema.Grant, role string) []schema.Grant {
	prefix := role + "/"
	probe := role + "/x" // synthetic action for glob testing
	var filtered []schema.Grant
	for _, grant := range grants {
		for _, pattern := range grant.Actions {
			if strings.HasPrefix(pattern, prefix) || pattern == role || principal.MatchPattern(pattern, probe) {
				filtered = append(filtered, grant)
				break
			}
		}
	}
	return filtered
}

// generateTokenID returns a cryptographically random 16-byte hex string
// (32 characters) for use as a unique token identifier. The ID supports
// emergency revocation via the service token blacklist.
func generateTokenID() (string, error) {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return hex.EncodeToString(buffer[:]), nil
}

// cancelExitWatcher cancels the watchSandboxExit goroutine for a principal.
// Must be called before an intentional destroy-sandbox IPC so the watcher
// does not interpret the destroy as an unexpected exit and corrupt daemon state.
// Caller must hold reconcileMu.
func (d *Daemon) cancelExitWatcher(localpart string) {
	if cancel, ok := d.exitWatchers[localpart]; ok {
		cancel()
	}
}

// cancelProxyExitWatcher cancels the watchProxyExit goroutine for a
// principal. Must be called alongside cancelExitWatcher before any
// intentional destroy-sandbox IPC so the watcher does not interpret
// the proxy's death (from the sandbox being torn down) as a crash.
// Caller must hold reconcileMu.
func (d *Daemon) cancelProxyExitWatcher(localpart string) {
	if cancel, ok := d.proxyExitWatchers[localpart]; ok {
		cancel()
	}
}

// watchSandboxExit blocks until the named sandbox's process exits, then clears
// the daemon's running state and triggers re-reconciliation. This enables the
// daemon to detect one-shot principals (setup/teardown) that complete their work
// and exit, as well as unexpected agent crashes that need restart.
//
// The re-reconciliation re-evaluates StartConditions for all principals:
//   - One-shot principals (setup/teardown) won't restart because their condition
//     became false after they published a state change (e.g., "pending" → "active").
//   - Long-running agents restart automatically if their condition is still met.
//
// Must be called as a goroutine. Uses reconcileMu to safely modify shared state.
func (d *Daemon) watchSandboxExit(ctx context.Context, localpart string) {
	exitCode, exitDescription, err := d.launcherWaitSandbox(ctx, localpart)
	if err != nil {
		if ctx.Err() != nil {
			return // Daemon shutting down.
		}
		d.logger.Error("wait-sandbox failed",
			"principal", localpart,
			"error", err,
		)
		return
	}

	d.logger.Info("sandbox exited",
		"principal", localpart,
		"exit_code", exitCode,
		"description", exitDescription,
	)

	d.reconcileMu.Lock()
	if d.running[localpart] {
		d.cancelProxyExitWatcher(localpart)
		d.stopHealthMonitor(localpart)
		d.stopLayoutWatcher(localpart)
		d.revokeAndCleanupTokens(ctx, localpart)
		delete(d.running, localpart)
		delete(d.exitWatchers, localpart)
		delete(d.proxyExitWatchers, localpart)
		delete(d.lastSpecs, localpart)
		delete(d.previousSpecs, localpart)
		delete(d.lastTemplates, localpart)
		delete(d.lastCredentials, localpart)
		delete(d.lastGrants, localpart)
		delete(d.lastTokenMint, localpart)
		delete(d.lastObserveAllowances, localpart)
		d.lastActivityAt = d.clock.Now()
	}
	d.reconcileMu.Unlock()

	// Post exit notification to config room.
	status := "exited normally"
	if exitCode != 0 {
		status = fmt.Sprintf("exited with code %d", exitCode)
		if exitDescription != "" {
			status += fmt.Sprintf(" (%s)", exitDescription)
		}
	}
	d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
		fmt.Sprintf("Sandbox %s %s", localpart, status),
	))

	// Trigger re-reconciliation so the loop can decide whether to restart.
	// This acquires reconcileMu again — safe because we released it above.
	if err := d.reconcile(ctx); err != nil {
		d.logger.Error("reconciliation after sandbox exit failed",
			"principal", localpart,
			"error", err,
		)
	}
	d.notifyReconcile()
}

// Type aliases for the shared IPC types. The canonical definitions live
// in lib/ipc. Aliases use the daemon's local naming convention so call
// sites don't need to change.
type (
	launcherIPCRequest       = ipc.Request
	launcherIPCResponse      = ipc.Response
	launcherServiceMount     = ipc.ServiceMount
	launcherSandboxListEntry = ipc.SandboxListEntry
)

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

// adoptPreExistingSandboxes queries the launcher for running sandboxes and
// pre-populates d.running. Called once during daemon startup, before the
// initial sync and first reconcile. This handles daemon restart recovery:
// when the daemon restarts while the launcher continues running, the
// launcher still has sandbox processes alive. Without this call, reconcile
// would try to create-sandbox for each desired principal, which the launcher
// rejects ("already has a running sandbox"), leaving the principal orphaned
// from daemon management.
//
// The actual management goroutines (exit watchers, layout watchers, health
// monitors) are started later during the first reconcile's "adopt surviving"
// pass, which detects d.running entries without exit watchers.
func (d *Daemon) adoptPreExistingSandboxes(ctx context.Context) error {
	response, err := d.launcherRequest(ctx, launcherIPCRequest{
		Action: "list-sandboxes",
	})
	if err != nil {
		return fmt.Errorf("list-sandboxes IPC: %w", err)
	}
	if !response.OK {
		return fmt.Errorf("list-sandboxes rejected: %s", response.Error)
	}

	for _, entry := range response.Sandboxes {
		d.running[entry.Localpart] = true
		d.logger.Info("adopted pre-existing sandbox",
			"principal", entry.Localpart,
			"proxy_pid", entry.ProxyPID,
		)
	}

	if count := len(response.Sandboxes); count > 0 {
		d.logger.Info("adopted pre-existing sandboxes from launcher",
			"count", count,
		)
	}

	return nil
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
	if err := codec.NewEncoder(conn).Encode(request); err != nil {
		return nil, fmt.Errorf("sending request to launcher: %w", err)
	}

	// Read the response.
	var response launcherIPCResponse
	if err := codec.NewDecoder(conn).Decode(&response); err != nil {
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
	if err := codec.NewEncoder(conn).Encode(request); err != nil {
		return -1, "", fmt.Errorf("sending wait-sandbox request: %w", err)
	}

	// Clear the write deadline and read without a deadline — the
	// launcher responds only after the sandbox process exits.
	conn.SetDeadline(time.Time{})

	var response launcherIPCResponse
	if err := codec.NewDecoder(conn).Decode(&response); err != nil {
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

// launcherWaitProxy opens a long-lived IPC connection to the launcher and
// blocks until the proxy process for the given principal exits. The response
// includes the proxy's exit code.
//
// This mirrors launcherWaitSandbox: a dedicated connection with no read
// deadline, and a cancellation goroutine that closes the connection when the
// context is cancelled (unblocking the Read).
func (d *Daemon) launcherWaitProxy(ctx context.Context, principalLocalpart string) (int, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", d.launcherSocket)
	if err != nil {
		return -1, fmt.Errorf("connecting to launcher at %s: %w", d.launcherSocket, err)
	}
	defer conn.Close()

	// Close the connection when the context is cancelled. This unblocks
	// the Read call below and signals the launcher to clean up its
	// wait-proxy handler.
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
		Action:    "wait-proxy",
		Principal: principalLocalpart,
	}
	if err := codec.NewEncoder(conn).Encode(request); err != nil {
		return -1, fmt.Errorf("sending wait-proxy request: %w", err)
	}

	// Clear the write deadline and read without a deadline — the
	// launcher responds only after the proxy process exits.
	conn.SetDeadline(time.Time{})

	var response launcherIPCResponse
	if err := codec.NewDecoder(conn).Decode(&response); err != nil {
		if ctx.Err() != nil {
			return -1, ctx.Err()
		}
		return -1, fmt.Errorf("reading wait-proxy response: %w", err)
	}

	if !response.OK {
		return -1, fmt.Errorf("wait-proxy: %s", response.Error)
	}

	exitCode := 0
	if response.ExitCode != nil {
		exitCode = *response.ExitCode
	}
	return exitCode, nil
}

// watchProxyExit monitors a principal's proxy process via the launcher's
// wait-proxy IPC. When the proxy dies unexpectedly, the daemon destroys the
// sandbox (the tmux session is still running but useless without the proxy),
// clears all running state, and triggers re-reconciliation to restart.
//
// This provides event-driven proxy death detection that fires within
// milliseconds of the proxy exiting — much faster than health check polling,
// and works even when the template has no HealthCheck configured.
func (d *Daemon) watchProxyExit(ctx context.Context, localpart string) {
	exitCode, err := d.launcherWaitProxy(ctx, localpart)
	if err != nil {
		if ctx.Err() != nil {
			return // Watcher cancelled or daemon shutting down.
		}
		d.logger.Error("wait-proxy failed",
			"principal", localpart,
			"error", err,
		)
		return
	}

	d.logger.Warn("proxy exited unexpectedly",
		"principal", localpart,
		"exit_code", exitCode,
	)

	d.reconcileMu.Lock()
	if !d.running[localpart] {
		// Already handled by another path (sandbox exit, reconcile, etc.).
		d.reconcileMu.Unlock()
		return
	}

	// Cancel the sandbox exit watcher — the destroy below will kill the
	// tmux session, which would trigger watchSandboxExit. We handle the
	// full cleanup here.
	d.cancelExitWatcher(localpart)
	d.stopHealthMonitor(localpart)
	d.stopLayoutWatcher(localpart)

	// Destroy the sandbox. The proxy is dead but the tmux session may
	// still be running. We must destroy before clearing state so that a
	// concurrent reconcile doesn't attempt to recreate while the old
	// session is alive.
	response, err := d.launcherRequest(ctx, launcherIPCRequest{
		Action:    "destroy-sandbox",
		Principal: localpart,
	})
	if err != nil {
		d.logger.Error("proxy exit handler: destroy-sandbox IPC failed",
			"principal", localpart, "error", err)
	} else if !response.OK {
		d.logger.Error("proxy exit handler: destroy-sandbox rejected",
			"principal", localpart, "error", response.Error)
	}

	d.revokeAndCleanupTokens(ctx, localpart)
	delete(d.running, localpart)
	delete(d.exitWatchers, localpart)
	delete(d.proxyExitWatchers, localpart)
	delete(d.lastSpecs, localpart)
	delete(d.previousSpecs, localpart)
	delete(d.lastTemplates, localpart)
	delete(d.lastCredentials, localpart)
	delete(d.lastGrants, localpart)
	delete(d.lastTokenMint, localpart)
	delete(d.lastObserveAllowances, localpart)
	d.lastActivityAt = d.clock.Now()
	d.reconcileMu.Unlock()

	d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
		fmt.Sprintf("CRITICAL: Proxy for %s exited unexpectedly (code %d). Sandbox destroyed, re-reconciling.", localpart, exitCode),
	))

	// Trigger re-reconciliation to restart the principal.
	if err := d.reconcile(ctx); err != nil {
		d.logger.Error("reconciliation after proxy exit failed",
			"principal", localpart,
			"error", err,
		)
		d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
			fmt.Sprintf("FAILED to recover %s after proxy crash: %v", localpart, err),
		))
	} else {
		d.reconcileMu.Lock()
		recovered := d.running[localpart]
		d.reconcileMu.Unlock()

		if recovered {
			d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
				fmt.Sprintf("Recovered %s after proxy crash", localpart),
			))
		} else {
			d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
				fmt.Sprintf("FAILED to recover %s after proxy crash: principal not in desired state", localpart),
			))
		}
	}
	d.notifyReconcile()
}
