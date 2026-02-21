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
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/authorization"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ipc"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/version"
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
	desired := make(map[ref.Entity]schema.PrincipalAssignment, len(config.Principals))
	triggerContents := make(map[ref.Entity][]byte)
	conditionRoomIDs := make(map[ref.Entity]ref.RoomID) // principal → resolved workspace room ID
	for _, assignment := range config.Principals {
		if !assignment.AutoStart {
			continue
		}
		principal := assignment.Principal
		satisfied, triggerContent, conditionRoomID := d.evaluateStartCondition(ctx, principal, assignment.StartCondition)
		if !satisfied {
			continue
		}
		desired[principal] = assignment
		if triggerContent != nil {
			triggerContents[principal] = triggerContent
		}
		if !conditionRoomID.IsZero() {
			conditionRoomIDs[principal] = conditionRoomID
		}
	}

	// Check for credential rotation on running principals. When the
	// ciphertext in the m.bureau.credentials state event changes,
	// destroy the sandbox so the "create missing" pass below recreates
	// it with the new credentials. This handles API key rotation, token
	// refresh, and any other credential update without manual intervention.
	var rotatedPrincipals []ref.Entity
	for principal := range d.running {
		if _, shouldRun := desired[principal]; !shouldRun {
			continue // Will be destroyed in the "remove" pass below.
		}

		credentials, err := d.readCredentials(ctx, principal)
		if err != nil {
			d.logger.Error("reading credentials for rotation check",
				"principal", principal, "error", err)
			continue
		}

		previousCiphertext, tracked := d.lastCredentials[principal]
		if !tracked {
			// First reconcile after daemon (re)start for this principal.
			// Record the current ciphertext for future comparisons.
			d.lastCredentials[principal] = credentials.Ciphertext
			continue
		}

		if credentials.Ciphertext == previousCiphertext {
			continue
		}

		d.logger.Info("credentials changed, restarting principal",
			"principal", principal)

		if err := d.destroyPrincipal(ctx, principal); err != nil {
			d.logger.Error("failed to destroy sandbox during credential rotation",
				"principal", principal, "error", err)
			continue
		}

		d.logger.Info("principal stopped for credential rotation (will recreate)",
			"principal", principal)
		rotatedPrincipals = append(rotatedPrincipals, principal)

		if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
			schema.NewCredentialsRotatedMessage(principal.AccountLocalpart(), "restarting", "")); err != nil {
			d.logger.Error("failed to post credential rotation message",
				"principal", principal, "error", err)
		}
	}

	// Hot-reload authorization grants when they change on a running
	// principal. The proxy uses grants for Matrix API gating and service
	// directory filtering.
	if d.lastGrants == nil {
		d.lastGrants = make(map[ref.Entity][]schema.Grant)
	}
	for principal, assignment := range desired {
		if !d.running[principal] {
			continue
		}
		newGrants := d.resolveGrantsForProxy(principal.UserID(), assignment, config)
		oldGrants := d.lastGrants[principal]
		if !reflect.DeepEqual(oldGrants, newGrants) {
			d.logger.Info("authorization grants changed, updating proxy",
				"principal", principal)
			if err := d.pushAuthorizationToProxy(ctx, principal, newGrants); err != nil {
				d.logger.Error("push authorization grants failed during hot-reload",
					"principal", principal, "error", err)
				continue
			}
			d.lastGrants[principal] = newGrants

			// Force the token refresh goroutine to re-mint service
			// tokens on its next tick so they carry the updated grants.
			d.lastTokenMint[principal] = time.Time{}

			if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
				schema.NewGrantsUpdatedMessage(principal.AccountLocalpart(), len(newGrants))); err != nil {
				d.logger.Error("failed to post grants update confirmation",
					"principal", principal, "error", err)
			}
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
	for principal := range desired {
		if !d.running[principal] {
			continue
		}
		newAllowances := d.authorizationIndex.Allowances(principal.UserID())
		oldAllowances := d.lastObserveAllowances[principal]
		if !reflect.DeepEqual(oldAllowances, newAllowances) {
			d.lastObserveAllowances[principal] = newAllowances
			d.enforceObserveAllowanceChange(principal.UserID())
		}
	}

	// Check for hot-reloadable changes on already-running principals.
	for principal, assignment := range desired {
		if !d.running[principal] {
			continue
		}
		d.reconcileRunningPrincipal(ctx, principal, assignment)
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
	for principal, assignment := range desired {
		if !d.running[principal] {
			continue
		}
		if _, hasWatcher := d.exitWatchers[principal]; hasWatcher {
			continue // Already fully managed.
		}
		d.adoptSurvivingPrincipal(ctx, principal, assignment)
	}

	// Create sandboxes for principals that should be running but aren't.
	// StartConditions were already evaluated when building the desired
	// set above — only principals with satisfied conditions are here.
	for principal, assignment := range desired {
		if d.running[principal] {
			continue
		}

		// Skip principals in backoff from a previous start failure.
		// Event-driven clearing (config change, service sync, sandbox
		// exit) resets the backoff so retries happen immediately when
		// the root cause may have been resolved.
		if failure := d.startFailures[principal]; failure != nil {
			if d.clock.Now().Before(failure.nextRetryAt) {
				d.logger.Debug("principal in start backoff, skipping",
					"principal", principal,
					"category", failure.category,
					"attempts", failure.attempts,
					"retry_at", failure.nextRetryAt,
				)
				continue
			}
		}

		d.logger.Info("starting principal", "principal", principal)

		// Read the credentials for this principal.
		credentials, err := d.readCredentials(ctx, principal)
		if err != nil {
			if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
				d.logger.Warn("no credentials found for principal, skipping", "principal", principal)
				d.recordStartFailure(principal, failureCategoryCredentials, "no credentials provisioned")
				continue
			}
			d.logger.Error("reading credentials", "principal", principal, "error", err)
			d.recordStartFailure(principal, failureCategoryCredentials, err.Error())
			continue
		}

		// Resolve the template and build the sandbox spec when the
		// assignment references a template. When Template is empty, the
		// launcher creates only a proxy process (no bwrap sandbox).
		var sandboxSpec *schema.SandboxSpec
		var resolvedTemplate *schema.TemplateContent
		if assignment.Template != "" {
			template, err := resolveTemplate(ctx, d.session, assignment.Template, d.machine.Server())
			if err != nil {
				d.logger.Error("resolving template", "principal", principal, "template", assignment.Template, "error", err)
				d.recordStartFailure(principal, failureCategoryTemplate, err.Error())
				continue
			}
			resolvedTemplate = template
			sandboxSpec = resolveInstanceConfig(template, &assignment)
			d.logger.Info("resolved sandbox spec from template",
				"principal", principal,
				"template", assignment.Template,
				"command", sandboxSpec.Command,
			)

			if d.applyPipelineExecutorOverlay(sandboxSpec) {
				d.pipelineExecutors[principal] = true
				d.logger.Info("applied pipeline executor overlay",
					"principal", principal,
					"template", assignment.Template,
					"command", sandboxSpec.Command,
				)
			}

			// Ensure the Nix environment's store path (and its full
			// transitive closure) exists locally before handing the
			// spec to the launcher. On failure, skip this principal
			// with exponential backoff until a config change clears it.
			if sandboxSpec.EnvironmentPath != "" {
				if err := d.prefetchEnvironment(ctx, sandboxSpec.EnvironmentPath); err != nil {
					d.logger.Error("prefetching nix environment",
						"principal", principal,
						"store_path", sandboxSpec.EnvironmentPath,
						"error", err,
					)
					isFirstFailure := d.startFailures[principal] == nil
					d.recordStartFailure(principal, failureCategoryNixPrefetch, err.Error())
					if isFirstFailure {
						if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
							schema.NewNixPrefetchFailedMessage(principal.AccountLocalpart(), sandboxSpec.EnvironmentPath, err.Error())); err != nil {
							d.logger.Error("failed to post nix prefetch failure notification",
								"principal", principal, "error", err)
						}
					}
					continue
				}
			}
		}

		// Validate the command binary is resolvable before sending the
		// spec to the launcher. The binary must be findable either in
		// the Nix environment (EnvironmentPath/bin/) or on the daemon's
		// PATH. Shell interpreters (sh, bash) are always available
		// inside the sandbox and don't need host-side validation.
		if sandboxSpec != nil && len(sandboxSpec.Command) > 0 {
			if err := d.validateCommandFunc(sandboxSpec.Command[0], sandboxSpec.EnvironmentPath); err != nil {
				templateName := assignment.Template
				d.logger.Error("command binary not found",
					"principal", principal,
					"template", templateName,
					"command", sandboxSpec.Command,
					"error", err,
				)
				isFirstFailure := d.startFailures[principal] == nil
				d.recordStartFailure(principal, failureCategoryCommandBinary, err.Error())
				if isFirstFailure {
					if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
						schema.NewPrincipalStartFailedMessage(principal.AccountLocalpart(),
							fmt.Sprintf("template %q command binary %q not found: %v", templateName, sandboxSpec.Command[0], err))); err != nil {
						d.logger.Error("failed to post command binary failure notification",
							"principal", principal, "error", err)
					}
				}
				continue
			}
		}

		// Ensure the principal is a member of the config room. In the
		// standard deployment path (bureau agent create / bureau service
		// create), principal.Create invites and joins the principal to
		// the config room before writing the MachineConfig. But
		// alternative paths — HA failover, fleet placement, manual
		// config edits — write PrincipalAssignments without handling
		// room membership. The daemon invites here; the proxy's
		// acceptPendingInvites joins during startup.
		d.ensurePrincipalRoomAccess(ctx, assignment.Principal, d.configRoomID)

		// Invite the principal to any workspace room it references so it
		// can publish state events after joining. Two sources:
		// StartCondition.RoomAlias (resolved during condition evaluation)
		// and WORKSPACE_ROOM_ID in the payload (static per-principal
		// config from workspace create). Best-effort: if the invite
		// fails, the sandbox is still created.
		workspaceRoomID := conditionRoomIDs[principal]
		if workspaceRoomID.IsZero() && sandboxSpec != nil {
			if value, ok := sandboxSpec.Payload["WORKSPACE_ROOM_ID"].(string); ok {
				if parsed, parseErr := ref.ParseRoomID(value); parseErr == nil {
					workspaceRoomID = parsed
				} else {
					d.logger.Warn("invalid WORKSPACE_ROOM_ID in payload",
						"principal", principal,
						"value", value,
						"error", parseErr,
					)
				}
			}
		}
		if !workspaceRoomID.IsZero() {
			d.ensurePrincipalRoomAccess(ctx, assignment.Principal, workspaceRoomID)
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
					"principal", principal,
					"required_services", sandboxSpec.RequiredServices,
					"error", err,
				)
				isFirstFailure := d.startFailures[principal] == nil
				d.recordStartFailure(principal, failureCategoryServiceResolution, err.Error())
				if isFirstFailure {
					if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
						schema.NewPrincipalStartFailedMessage(principal.AccountLocalpart(), err.Error())); err != nil {
						d.logger.Error("failed to post service resolution failure notification",
							"principal", principal, "error", err)
					}
				}
				continue
			}
			serviceMounts = mounts
		}

		// If the principal has credential/* grants, mount the daemon's
		// credential provisioning socket into the sandbox so the agent
		// can inject per-principal credentials via the provisioning API.
		serviceMounts = d.appendCredentialServiceMount(principal, serviceMounts)

		// Compute the full set of service roles that need tokens: the
		// template's RequiredServices plus any daemon-managed services
		// the principal qualifies for (currently: credential provisioning
		// for principals with credential/* grants).
		var allServiceRoles []string
		if sandboxSpec != nil {
			allServiceRoles = append(allServiceRoles, sandboxSpec.RequiredServices...)
		}
		allServiceRoles = append(allServiceRoles, d.credentialServiceRoles(principal)...)

		// Mint service tokens for each role. The tokens carry pre-resolved
		// grants scoped to the service namespace. Written to disk and
		// bind-mounted read-only at /run/bureau/service/token/ so the agent
		// can authenticate to services without a daemon round-trip.
		var tokenDirectory string
		var mintedTokens []activeToken
		if len(allServiceRoles) > 0 {
			tokenDir, minted, err := d.mintServiceTokens(principal, allServiceRoles)
			if err != nil {
				d.logger.Error("minting service tokens",
					"principal", principal,
					"error", err,
				)
				isFirstFailure := d.startFailures[principal] == nil
				d.recordStartFailure(principal, failureCategoryTokenMinting, err.Error())
				if isFirstFailure {
					if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
						schema.NewPrincipalStartFailedMessage(principal.AccountLocalpart(), fmt.Sprintf("failed to mint service tokens: %v", err))); err != nil {
						d.logger.Error("failed to post token minting failure notification",
							"principal", principal, "error", err)
					}
				}
				continue
			}
			tokenDirectory = tokenDir
			mintedTokens = minted
		}

		// Resolve authorization grants before sandbox creation so the
		// proxy starts with enforcement from the first request. The
		// launcher pipes these to the proxy's stdin alongside credentials.
		grants := d.resolveGrantsForProxy(principal.UserID(), assignment, config)

		// Send create-sandbox to the launcher.
		response, err := d.launcherRequest(ctx, launcherIPCRequest{
			Action:               "create-sandbox",
			Principal:            principal.AccountLocalpart(),
			EncryptedCredentials: credentials.Ciphertext,
			Grants:               grants,
			SandboxSpec:          sandboxSpec,
			TriggerContent:       triggerContents[principal],
			ServiceMounts:        serviceMounts,
			TokenDirectory:       tokenDirectory,
		})
		if err != nil {
			d.logger.Error("create-sandbox IPC failed", "principal", principal, "error", err)
			d.recordStartFailure(principal, failureCategoryLauncherIPC, err.Error())
			continue
		}
		if !response.OK {
			d.logger.Error("create-sandbox rejected", "principal", principal, "error", response.Error)
			d.recordStartFailure(principal, failureCategoryLauncherIPC, response.Error)
			continue
		}

		d.clearStartFailure(principal)
		d.running[principal] = true
		d.lastCredentials[principal] = credentials.Ciphertext
		d.lastObserveAllowances[principal] = d.authorizationIndex.Allowances(principal.UserID())
		d.lastSpecs[principal] = sandboxSpec
		d.lastTemplates[principal] = resolvedTemplate
		d.lastGrants[principal] = grants
		d.lastTokenMint[principal] = d.clock.Now()
		d.recordMintedTokens(principal, mintedTokens)
		d.lastServiceMounts[principal] = serviceMounts
		d.lastActivityAt = d.clock.Now()
		d.logger.Info("principal started", "principal", principal)

		// Start watching the tmux session for layout changes. This also
		// restores any previously saved layout from Matrix.
		d.startLayoutWatcher(ctx, principal)

		// Start health monitoring if the template defines a health check.
		// The monitor waits a grace period before its first probe, giving
		// the sandbox and proxy time to initialize.
		if resolvedTemplate != nil && resolvedTemplate.HealthCheck != nil {
			d.startHealthMonitor(ctx, principal, resolvedTemplate.HealthCheck)
		}

		// Register all known local service routes on the new consumer's
		// proxy so it can reach services that were discovered before it
		// started. The proxy socket is created synchronously by Start(),
		// so it should be accepting connections by the time the launcher
		// responds to create-sandbox.
		d.configureConsumerProxy(ctx, principal)

		// Register external HTTP API services declared by the template
		// (e.g., Anthropic, OpenAI). The proxy forwards requests to
		// these upstreams with credential injection so agents reach
		// external APIs at /http/<name>/... without seeing API keys.
		if sandboxSpec != nil && len(sandboxSpec.ProxyServices) > 0 {
			d.configureExternalProxyServices(ctx, principal, sandboxSpec.ProxyServices)
		}

		// Push the service directory so the new consumer's agent can
		// discover services via GET /v1/services.
		directory := d.buildServiceDirectory()
		if err := d.pushDirectoryToProxy(ctx, principal, directory); err != nil {
			d.logger.Error("failed to push service directory to new consumer proxy",
				"consumer", principal,
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
			d.exitWatchers[principal] = watcherCancel
			go d.watchSandboxExit(watcherCtx, principal)

			proxyCtx, proxyCancel := context.WithCancel(d.shutdownCtx)
			d.proxyExitWatchers[principal] = proxyCancel
			go d.watchProxyExit(proxyCtx, principal)
		}
	}

	// Report credential rotation outcomes. Rotated principals were
	// destroyed above and should have been recreated by the create loop.
	for _, principal := range rotatedPrincipals {
		status := "completed"
		var errorMessage string
		if !d.running[principal] {
			status = "failed"
			errorMessage = "principal not in desired state after credential rotation"
		}
		if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
			schema.NewCredentialsRotatedMessage(principal.AccountLocalpart(), status, errorMessage)); err != nil {
			d.logger.Error("failed to post credential rotation outcome",
				"principal", principal, "error", err)
		}
	}

	// Destroy sandboxes for principals that should not be running.
	// Pipeline executor principals are exempt: they manage their own
	// lifecycle (run steps, publish results, exit). Killing them when
	// their start condition becomes unsatisfied races with the pipeline
	// itself — the pipeline may publish the state change that
	// invalidated its condition (e.g., teardown sets workspace status
	// to "archived", making the "destroying" start condition false).
	// The daemon will clean up pipeline executors when their sandbox
	// exits via watchSandboxExit.
	for principal := range d.running {
		if _, shouldRun := desired[principal]; shouldRun {
			continue
		}
		if d.pipelineExecutors[principal] {
			continue
		}

		d.logger.Info("stopping principal", "principal", principal)

		if err := d.destroyPrincipal(ctx, principal); err != nil {
			d.logger.Error("failed to destroy sandbox", "principal", principal, "error", err)
			continue
		}
		d.logger.Info("principal stopped", "principal", principal)
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
func (d *Daemon) reconcileRunningPrincipal(ctx context.Context, principal ref.Entity, assignment schema.PrincipalAssignment) {
	// Can only detect changes for principals created from templates.
	if assignment.Template == "" {
		return
	}

	// Re-resolve the template to get the current desired spec.
	template, err := resolveTemplate(ctx, d.session, assignment.Template, d.machine.Server())
	if err != nil {
		d.logger.Error("re-resolving template for running principal",
			"principal", principal, "template", assignment.Template, "error", err)
		return
	}
	newSpec := resolveInstanceConfig(template, &assignment)

	// Apply the same pipeline executor overlay that the create path
	// uses so the stored spec (which has the overlay applied) doesn't
	// always differ from the freshly resolved spec, which would cause
	// an infinite destroy-recreate loop.
	d.applyPipelineExecutorOverlay(newSpec)

	// Compare with the previously deployed spec.
	oldSpec := d.lastSpecs[principal]
	if oldSpec == nil {
		// No previous spec stored (principal was created without one,
		// or from a previous daemon instance). Store the current spec
		// and template for future comparisons but don't trigger any
		// changes.
		d.lastSpecs[principal] = newSpec
		d.lastTemplates[principal] = template
		return
	}

	// Structural changes take precedence: destroy the sandbox and let the
	// "create missing" pass in reconcile() rebuild it with the new spec.
	// This handles changes to command, mounts, namespaces, resources,
	// security, environment variables, etc. — anything that requires a
	// new bwrap invocation.
	if structurallyChanged(oldSpec, newSpec) {
		d.logger.Info("structural change detected, restarting sandbox",
			"principal", principal,
			"template", assignment.Template,
		)

		// Save the current spec as the rollback target before destroying.
		// The "create missing" pass will recreate the sandbox with the new
		// spec; if health checks fail, the daemon can roll back to this
		// previous working configuration.
		rollbackSpec := d.lastSpecs[principal]

		if err := d.destroyPrincipal(ctx, principal); err != nil {
			d.logger.Error("failed to destroy sandbox during structural restart",
				"principal", principal, "error", err)
			return
		}

		d.previousSpecs[principal] = rollbackSpec
		d.logger.Info("sandbox destroyed for structural restart (will recreate)",
			"principal", principal)

		if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
			schema.NewPrincipalRestartedMessage(principal.AccountLocalpart(), assignment.Template)); err != nil {
			d.logger.Error("failed to post structural restart notification",
				"principal", principal, "error", err)
		}
		return
	}

	// Payload-only change: hot-reload by rewriting the payload file
	// in-place (preserving the bind-mounted inode). The agent reads the
	// updated file after receiving notification — see handleUpdatePayload
	// for the safety argument.
	if payloadChanged(oldSpec, newSpec) {
		d.logger.Info("payload changed for running principal, hot-reloading",
			"principal", principal,
		)
		response, err := d.launcherRequest(ctx, launcherIPCRequest{
			Action:    "update-payload",
			Principal: principal.AccountLocalpart(),
			Payload:   newSpec.Payload,
		})
		if err != nil {
			d.logger.Error("update-payload IPC failed",
				"principal", principal, "error", err)
			return
		}
		if !response.OK {
			d.logger.Error("update-payload rejected",
				"principal", principal, "error", response.Error)
			return
		}
		d.lastSpecs[principal] = newSpec
		d.lastActivityAt = d.clock.Now()
		d.logger.Info("payload hot-reloaded", "principal", principal)

		if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
			schema.NewPayloadUpdatedMessage(principal.AccountLocalpart())); err != nil {
			d.logger.Error("failed to post payload update notification",
				"principal", principal, "error", err)
		}
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
func (d *Daemon) adoptSurvivingPrincipal(ctx context.Context, principal ref.Entity, assignment schema.PrincipalAssignment) {
	d.logger.Info("setting up management for adopted principal", "principal", principal)

	// Start watching the tmux session for layout changes.
	d.startLayoutWatcher(ctx, principal)

	// Start health monitoring if the template defines a health check.
	if template := d.lastTemplates[principal]; template != nil && template.HealthCheck != nil {
		d.startHealthMonitor(ctx, principal, template.HealthCheck)
	}

	// Register all known local service routes on the adopted consumer's
	// proxy so it can reach services discovered while the daemon was down.
	d.configureConsumerProxy(ctx, principal)

	// Register external HTTP API services from the template.
	if spec := d.lastSpecs[principal]; spec != nil && len(spec.ProxyServices) > 0 {
		d.configureExternalProxyServices(ctx, principal, spec.ProxyServices)
	}

	// Push the service directory so the consumer's agent can discover
	// services via GET /v1/services.
	directory := d.buildServiceDirectory()
	if err := d.pushDirectoryToProxy(ctx, principal, directory); err != nil {
		d.logger.Error("failed to push service directory to adopted consumer proxy",
			"consumer", principal,
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
		d.exitWatchers[principal] = watcherCancel
		go d.watchSandboxExit(watcherCtx, principal)

		proxyCtx, proxyCancel := context.WithCancel(d.shutdownCtx)
		d.proxyExitWatchers[principal] = proxyCancel
		go d.watchProxyExit(proxyCtx, principal)
	}

	if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
		schema.NewPrincipalAdoptedMessage(principal.AccountLocalpart())); err != nil {
		d.logger.Error("failed to post adoption notification",
			"principal", principal, "error", err)
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
func (d *Daemon) evaluateStartCondition(ctx context.Context, principal ref.Entity, condition *schema.StartCondition) (bool, json.RawMessage, ref.RoomID) {
	if condition == nil {
		return true, nil, ref.RoomID{}
	}

	// Determine which room to check. When RoomAlias is empty, check the
	// principal's config room (the room where the MachineConfig lives).
	roomID := d.configRoomID
	conditionRoomID := ref.RoomID{} // Only set when RoomAlias is used (workspace rooms).
	if condition.RoomAlias != "" {
		conditionAlias, err := ref.ParseRoomAlias(condition.RoomAlias)
		if err != nil {
			d.logger.Error("invalid start condition room alias",
				"principal", principal,
				"room_alias", condition.RoomAlias,
				"error", err,
			)
			return false, nil, ref.RoomID{}
		}
		resolved, err := d.session.ResolveAlias(ctx, conditionAlias)
		if err != nil {
			if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
				d.logger.Info("start condition room alias not found, deferring principal",
					"principal", principal,
					"room_alias", condition.RoomAlias,
				)
				return false, nil, ref.RoomID{}
			}
			d.logger.Error("resolving start condition room alias",
				"principal", principal,
				"room_alias", condition.RoomAlias,
				"error", err,
			)
			return false, nil, ref.RoomID{}
		}
		roomID = resolved
		conditionRoomID = resolved
	}

	content, err := d.session.GetStateEvent(ctx, roomID, condition.EventType, condition.StateKey)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			d.logger.Info("start condition not met, deferring principal",
				"principal", principal,
				"event_type", condition.EventType,
				"state_key", condition.StateKey,
				"room_id", roomID,
			)
			return false, nil, ref.RoomID{}
		}
		d.logger.Error("checking start condition",
			"principal", principal,
			"event_type", condition.EventType,
			"state_key", condition.StateKey,
			"room_id", roomID,
			"error", err,
		)
		return false, nil, ref.RoomID{}
	}

	// Event exists. If ContentMatch is specified, verify all criteria
	// match the event content.
	if len(condition.ContentMatch) > 0 {
		var contentMap map[string]any
		if err := json.Unmarshal(content, &contentMap); err != nil {
			d.logger.Info("start condition content not a JSON object, deferring principal",
				"principal", principal,
				"event_type", condition.EventType,
				"room_id", roomID,
				"error", err,
			)
			return false, nil, ref.RoomID{}
		}
		matched, failedKey, err := condition.ContentMatch.Evaluate(contentMap)
		if err != nil {
			d.logger.Error("start condition content_match expression error",
				"principal", principal,
				"event_type", condition.EventType,
				"room_id", roomID,
				"key", failedKey,
				"error", err,
			)
			return false, nil, ref.RoomID{}
		}
		if !matched {
			d.logger.Info("start condition content_match not satisfied, deferring principal",
				"principal", principal,
				"event_type", condition.EventType,
				"room_id", roomID,
				"key", failedKey,
			)
			return false, nil, ref.RoomID{}
		}
	}

	return true, content, conditionRoomID
}

// readMachineConfig reads the MachineConfig state event from the config room.
func (d *Daemon) readMachineConfig(ctx context.Context) (*schema.MachineConfig, error) {
	content, err := d.session.GetStateEvent(ctx, d.configRoomID, schema.EventTypeMachineConfig, d.machine.Localpart())
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

	// Inject WORKSPACE_PATH so pipelines can include the host-side
	// absolute path in Matrix events. Agents and CLIs can't derive
	// this from the project name alone because the workspace root is
	// a per-machine daemon configuration (--workspace-root flag).
	if project, ok := spec.Payload["PROJECT"].(string); ok && project != "" {
		spec.Payload["WORKSPACE_PATH"] = filepath.Join(d.workspaceRoot, project)
	}

	// Apply the pipeline environment (Nix store path providing git, sh,
	// etc.) only when the template didn't already specify one.
	if d.pipelineEnvironment != "" && spec.EnvironmentPath == "" {
		spec.EnvironmentPath = d.pipelineEnvironment
	}

	return true
}

// readCredentials reads the Credentials state event for a principal.
func (d *Daemon) readCredentials(ctx context.Context, principal ref.Entity) (*schema.Credentials, error) {
	stateKey := principal.Localpart()
	content, err := d.session.GetStateEvent(ctx, d.configRoomID, schema.EventTypeCredentials, stateKey)
	if err != nil {
		return nil, err
	}

	var credentials schema.Credentials
	if err := json.Unmarshal(content, &credentials); err != nil {
		return nil, fmt.Errorf("parsing credentials for %s: %w", principal, err)
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
	currentPrincipals := make(map[ref.UserID]bool, len(config.Principals))

	for _, assignment := range config.Principals {
		userID := assignment.Principal.UserID()
		currentPrincipals[userID] = true

		// Merge machine defaults with per-principal policy. Per-principal
		// policy is additive: grants, denials, allowances, and allowance
		// denials are appended to the machine defaults. A principal cannot
		// have fewer permissions than the machine default.
		merged := mergeAuthorizationPolicy(config.DefaultPolicy, assignment.Authorization)
		d.authorizationIndex.SetPrincipal(userID, merged)
	}

	// Remove principals that are no longer in config. This also clears
	// their temporal grants.
	for _, userID := range d.authorizationIndex.Principals() {
		if !currentPrincipals[userID] {
			d.authorizationIndex.RemovePrincipal(userID)
			d.logger.Info("removed principal from authorization index",
				"principal", userID)
		}
	}
}

// mergeAuthorizationPolicy merges a machine-wide default policy with a
// per-principal policy. Both may be nil. The result is always a valid
// (possibly empty) AuthorizationPolicy. Per-principal entries are appended
// after defaults so they are evaluated after machine-wide rules. Each
// entry is stamped with its Source to preserve provenance through the merge.
func mergeAuthorizationPolicy(defaultPolicy, principalPolicy *schema.AuthorizationPolicy) schema.AuthorizationPolicy {
	var merged schema.AuthorizationPolicy

	if defaultPolicy != nil {
		merged.Grants = appendGrantsWithSource(merged.Grants, defaultPolicy.Grants, schema.SourceMachineDefault)
		merged.Denials = appendDenialsWithSource(merged.Denials, defaultPolicy.Denials, schema.SourceMachineDefault)
		merged.Allowances = appendAllowancesWithSource(merged.Allowances, defaultPolicy.Allowances, schema.SourceMachineDefault)
		merged.AllowanceDenials = appendAllowanceDenialsWithSource(merged.AllowanceDenials, defaultPolicy.AllowanceDenials, schema.SourceMachineDefault)
	}

	if principalPolicy != nil {
		merged.Grants = appendGrantsWithSource(merged.Grants, principalPolicy.Grants, schema.SourcePrincipal)
		merged.Denials = appendDenialsWithSource(merged.Denials, principalPolicy.Denials, schema.SourcePrincipal)
		merged.Allowances = appendAllowancesWithSource(merged.Allowances, principalPolicy.Allowances, schema.SourcePrincipal)
		merged.AllowanceDenials = appendAllowanceDenialsWithSource(merged.AllowanceDenials, principalPolicy.AllowanceDenials, schema.SourcePrincipal)
	}

	return merged
}

// Source-stamping helpers for the merge pipeline. Each copies the entry
// and sets its Source field, so the original config data is not mutated.

func appendGrantsWithSource(destination []schema.Grant, entries []schema.Grant, source string) []schema.Grant {
	for _, entry := range entries {
		entry.Source = source
		destination = append(destination, entry)
	}
	return destination
}

func appendDenialsWithSource(destination []schema.Denial, entries []schema.Denial, source string) []schema.Denial {
	for _, entry := range entries {
		entry.Source = source
		destination = append(destination, entry)
	}
	return destination
}

func appendAllowancesWithSource(destination []schema.Allowance, entries []schema.Allowance, source string) []schema.Allowance {
	for _, entry := range entries {
		entry.Source = source
		destination = append(destination, entry)
	}
	return destination
}

func appendAllowanceDenialsWithSource(destination []schema.AllowanceDenial, entries []schema.AllowanceDenial, source string) []schema.AllowanceDenial {
	for _, entry := range entries {
		entry.Source = source
		destination = append(destination, entry)
	}
	return destination
}

// ensurePrincipalRoomAccess invites a principal to a room so it becomes
// a member when the proxy calls acceptPendingInvites on startup. Used for
// both config rooms (lifecycle messaging) and workspace rooms (state
// event publishing). Called before create-sandbox so the invite is
// pending by the time the proxy starts.
//
// The invite is best-effort: failures are logged but do not block sandbox
// creation. M_FORBIDDEN typically means the user is already a member.
func (d *Daemon) ensurePrincipalRoomAccess(ctx context.Context, principalEntity ref.Entity, roomID ref.RoomID) {
	if err := d.session.InviteUser(ctx, roomID, principalEntity.UserID()); err != nil {
		// M_FORBIDDEN typically means the user is already a member.
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			d.logger.Warn("failed to invite principal to room",
				"principal", principalEntity,
				"room_id", roomID,
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
func (d *Daemon) resolveServiceMounts(ctx context.Context, requiredServices []string, workspaceRoomID ref.RoomID) ([]launcherServiceMount, error) {
	// Collect rooms to search, ordered by specificity (most specific first).
	// Workspace room bindings override machine config room bindings.
	var rooms []ref.RoomID
	if !workspaceRoomID.IsZero() {
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
func (d *Daemon) resolveServiceSocket(ctx context.Context, role string, rooms []ref.RoomID) (string, error) {
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

		if binding.Principal.IsZero() {
			return "", fmt.Errorf("service binding for role %q in room %s has empty principal", role, roomID)
		}

		// Ensure the service is a member of the room where its binding
		// was found. Services need room membership to write state events
		// (session lifecycle, metrics, context index, etc.). The invite
		// is idempotent — M_FORBIDDEN means the service is already
		// joined or the invite is otherwise redundant.
		if inviteErr := d.session.InviteUser(ctx, roomID, binding.Principal.UserID()); inviteErr != nil {
			if !messaging.IsMatrixError(inviteErr, "M_FORBIDDEN") {
				return "", fmt.Errorf("inviting service %s to room %s: %w", binding.Principal, roomID, inviteErr)
			}
		}

		// Derive the host-side socket path from the service principal.
		// For local services this is the actual proxy socket; for remote
		// services (future) the daemon would create a tunnel socket and
		// use that path instead.
		socketPath := binding.Principal.SocketPath(d.fleetRunDir)
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
// at /run/bureau/service/token/) and the list of minted token entries
// for tracking by the caller.
//
// For each required service role:
//   - Resolves the principal's grants from the authorization index
//   - Filters grants to those whose action patterns match the service
//     namespace (role + "/**")
//   - Converts schema.Grant to servicetoken.Grant (strips audit metadata)
//   - Mints a servicetoken.Token with the daemon's signing key
//   - Writes the raw signed bytes to <stateDir>/tokens/<accountLocalpart>/<role>
//
// Returns ("", nil, nil) if there are no required services. Returns an
// error if token minting or file I/O fails — callers should treat this
// as a fatal condition that blocks sandbox creation.
func (d *Daemon) mintServiceTokens(principal ref.Entity, requiredServices []string) (string, []activeToken, error) {
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
	tokenDir := filepath.Join(d.stateDir, "tokens", principal.AccountLocalpart())
	if err := os.MkdirAll(tokenDir, 0700); err != nil {
		return "", nil, fmt.Errorf("creating token directory: %w", err)
	}

	// Get the principal's resolved grants from the authorization index.
	grants := d.authorizationIndex.Grants(principal.UserID())

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
			Subject:   principal.UserID(),
			Machine:   d.machine,
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

		tokenPath := filepath.Join(tokenDir, role+".token")
		if err := atomicWriteFile(tokenPath, tokenBytes, 0600); err != nil {
			return "", nil, fmt.Errorf("writing token for service %q: %w", role, err)
		}

		minted = append(minted, activeToken{
			id:          tokenID,
			serviceRole: role,
			expiresAt:   now.Add(tokenTTL),
		})

		d.logger.Info("minted service token",
			"principal", principal,
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
func (d *Daemon) cancelExitWatcher(principal ref.Entity) {
	if cancel, ok := d.exitWatchers[principal]; ok {
		cancel()
	}
}

// cancelProxyExitWatcher cancels the watchProxyExit goroutine for a
// principal. Must be called alongside cancelExitWatcher before any
// intentional destroy-sandbox IPC so the watcher does not interpret
// the proxy's death (from the sandbox being torn down) as a crash.
// Caller must hold reconcileMu.
func (d *Daemon) cancelProxyExitWatcher(principal ref.Entity) {
	if cancel, ok := d.proxyExitWatchers[principal]; ok {
		cancel()
	}
}

// destroyPrincipal stops a running principal: cancels exit watchers, stops
// monitors, destroys the sandbox via launcher IPC, revokes service tokens,
// and clears all tracking state. Must be called with reconcileMu held.
//
// On launcher IPC failure, tracking maps are still cleaned up — the sandbox
// is assumed orphaned (the launcher or OS will clean it up) and service
// tokens will expire via their natural 5-minute TTL. The error is returned
// so callers can log it and decide whether to continue (emergency shutdown
// paths must continue past errors).
func (d *Daemon) destroyPrincipal(ctx context.Context, principal ref.Entity) error {
	d.cancelExitWatcher(principal)
	d.cancelProxyExitWatcher(principal)
	d.stopHealthMonitor(principal)
	d.stopLayoutWatcher(principal)

	var destroyError error
	response, err := d.launcherRequest(ctx, launcherIPCRequest{
		Action:    "destroy-sandbox",
		Principal: principal.AccountLocalpart(),
	})
	if err != nil {
		destroyError = fmt.Errorf("destroy-sandbox IPC failed: %w", err)
	} else if !response.OK {
		destroyError = fmt.Errorf("destroy-sandbox rejected: %s", response.Error)
	}

	d.revokeAndCleanupTokens(ctx, principal)
	delete(d.running, principal)
	delete(d.pipelineExecutors, principal)
	delete(d.exitWatchers, principal)
	delete(d.proxyExitWatchers, principal)
	delete(d.lastSpecs, principal)
	delete(d.previousSpecs, principal)
	delete(d.lastTemplates, principal)
	delete(d.lastCredentials, principal)
	delete(d.lastGrants, principal)
	delete(d.lastTokenMint, principal)
	delete(d.lastObserveAllowances, principal)
	d.lastActivityAt = d.clock.Now()

	return destroyError
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
func (d *Daemon) watchSandboxExit(ctx context.Context, principal ref.Entity) {
	exitCode, exitDescription, exitOutput, err := d.launcherWaitSandbox(ctx, principal.AccountLocalpart())
	if err != nil {
		if ctx.Err() != nil {
			return // Daemon shutting down.
		}
		d.logger.Error("wait-sandbox failed",
			"principal", principal,
			"error", err,
		)
		return
	}

	if exitCode != 0 && exitOutput != "" {
		d.logger.Warn("sandbox exited with captured output",
			"principal", principal,
			"exit_code", exitCode,
			"description", exitDescription,
			"output", exitOutput,
		)
	} else {
		d.logger.Info("sandbox exited",
			"principal", principal,
			"exit_code", exitCode,
			"description", exitDescription,
		)
	}

	d.reconcileMu.Lock()
	if d.running[principal] {
		d.cancelProxyExitWatcher(principal)
		d.stopHealthMonitor(principal)
		d.stopLayoutWatcher(principal)
		d.revokeAndCleanupTokens(ctx, principal)
		delete(d.running, principal)
		delete(d.pipelineExecutors, principal)
		delete(d.exitWatchers, principal)
		delete(d.proxyExitWatchers, principal)
		delete(d.lastSpecs, principal)
		delete(d.previousSpecs, principal)
		delete(d.lastTemplates, principal)
		delete(d.lastCredentials, principal)
		delete(d.lastGrants, principal)
		delete(d.lastTokenMint, principal)
		delete(d.lastObserveAllowances, principal)
		d.lastActivityAt = d.clock.Now()
	}

	// For normal exits (code 0), clear any stale start failure so
	// reconcile can immediately re-evaluate StartConditions. One-shot
	// principals (setup/teardown) exit 0 after completing their work;
	// their conditions will have changed so they won't restart.
	//
	// For abnormal exits (code != 0), record a crash failure with
	// exponential backoff to prevent a retry storm. Without this, a
	// sandbox that starts successfully but immediately crashes causes
	// an infinite zero-delay restart loop: watchSandboxExit fires,
	// reconcile restarts the sandbox, it crashes again, repeat.
	var crashBackoff time.Duration
	if exitCode == 0 {
		d.clearStartFailure(principal)
	} else {
		d.recordStartFailure(principal, failureCategorySandboxCrash, exitDescription)
		crashBackoff = time.Until(d.startFailures[principal].nextRetryAt)
		d.logger.Warn("sandbox crash, backing off before retry",
			"principal", principal,
			"exit_code", exitCode,
			"attempts", d.startFailures[principal].attempts,
			"retry_at", d.startFailures[principal].nextRetryAt,
		)
	}
	d.reconcileMu.Unlock()

	// Post exit notification to config room. On failure, include the
	// captured terminal output so operators can diagnose the problem
	// from the Matrix room without needing to reproduce the failure.
	// Truncate captured output to the last 50 lines for the Matrix
	// message. The full captured output (up to 500 lines) is in the
	// daemon log.
	var capturedOutput string
	if exitCode != 0 && exitOutput != "" {
		capturedOutput = tailLines(exitOutput, maxMatrixOutputLines)
	}
	if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
		schema.NewSandboxExitedMessage(principal.AccountLocalpart(), exitCode, exitDescription, capturedOutput)); err != nil {
		d.logger.Error("failed to post sandbox exit notification",
			"principal", principal, "error", err)
	}

	// Trigger re-reconciliation so the loop can decide whether to restart.
	// For crash exits, reconcile will see the backoff and skip the
	// principal. We then schedule a deferred reconcile for when the
	// backoff expires so the principal retries without waiting for an
	// external sync event.
	// This acquires reconcileMu again — safe because we released it above.
	if err := d.reconcile(ctx); err != nil {
		d.logger.Error("reconciliation after sandbox exit failed",
			"principal", principal,
			"error", err,
		)
	}
	d.notifyReconcile()

	// Schedule a deferred reconcile after the crash backoff expires.
	// Without this, the principal would only retry when the next sync
	// event triggers reconciliation, which could be arbitrarily far in
	// the future if no Matrix state changes. The goroutine exits
	// cleanly on daemon shutdown via context cancellation.
	if crashBackoff > 0 && d.shutdownCtx != nil {
		go func() {
			select {
			case <-d.shutdownCtx.Done():
				return
			case <-d.clock.After(crashBackoff):
				if err := d.reconcile(d.shutdownCtx); err != nil {
					d.logger.Error("deferred reconciliation after crash backoff expired",
						"principal", principal,
						"error", err,
					)
				}
				d.notifyReconcile()
			}
		}()
	}
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
// version.Compare to determine whether launcher or proxy updates are
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
		entity, err := ref.NewEntityFromAccountLocalpart(d.fleet, entry.Localpart)
		if err != nil {
			d.logger.Error("invalid localpart from launcher",
				"localpart", entry.Localpart, "error", err)
			continue
		}
		d.running[entity] = true
		d.logger.Info("adopted pre-existing sandbox",
			"principal", entity,
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
		if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
			schema.NewBureauVersionPrefetchFailedMessage(err.Error())); err != nil {
			d.logger.Error("failed to post bureau version prefetch failure notification", "error", err)
		}
		return
	}

	// Query the launcher for its current binary hash and proxy binary path.
	launcherHash, proxyBinaryPath, err := d.queryLauncherStatus(ctx)
	if err != nil {
		d.logger.Error("querying launcher status for version comparison", "error", err)
		return
	}

	// Compare desired versions against running versions.
	diff, err := version.Compare(desired, d.daemonBinaryHash, launcherHash, proxyBinaryPath)
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
	if diff.ProxyChanged || diff.LauncherChanged {
		if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
			schema.NewBureauVersionReconciledMessage(diff.ProxyChanged, diff.LauncherChanged)); err != nil {
			d.logger.Error("failed to post bureau version summary", "error", err)
		}
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
// termination), an error description string, the captured terminal output
// (populated only on non-zero exit), and any IPC-level error.
//
// Unlike launcherRequest, this method is designed for long-lived
// connections. The context controls cancellation: when cancelled, the
// connection is closed, which unblocks the launcher's handler.
//
// The error string in the response (if any) describes the process exit
// condition (signal name, etc.) and is informational — the exit code is
// the authoritative success/failure indicator.
func (d *Daemon) launcherWaitSandbox(ctx context.Context, principalLocalpart string) (int, string, string, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", d.launcherSocket)
	if err != nil {
		return -1, "", "", fmt.Errorf("connecting to launcher at %s: %w", d.launcherSocket, err)
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
		return -1, "", "", fmt.Errorf("sending wait-sandbox request: %w", err)
	}

	// Clear the write deadline and read without a deadline — the
	// launcher responds only after the sandbox process exits.
	conn.SetDeadline(time.Time{})

	var response launcherIPCResponse
	if err := codec.NewDecoder(conn).Decode(&response); err != nil {
		if ctx.Err() != nil {
			return -1, "", "", ctx.Err()
		}
		return -1, "", "", fmt.Errorf("reading wait-sandbox response: %w", err)
	}

	if !response.OK {
		return -1, "", "", fmt.Errorf("wait-sandbox: %s", response.Error)
	}

	exitCode := 0
	if response.ExitCode != nil {
		exitCode = *response.ExitCode
	}
	return exitCode, response.Error, response.Output, nil
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
func (d *Daemon) watchProxyExit(ctx context.Context, principal ref.Entity) {
	exitCode, err := d.launcherWaitProxy(ctx, principal.AccountLocalpart())
	if err != nil {
		if ctx.Err() != nil {
			return // Watcher cancelled or daemon shutting down.
		}
		d.logger.Error("wait-proxy failed",
			"principal", principal,
			"error", err,
		)
		return
	}

	d.logger.Warn("proxy exited unexpectedly",
		"principal", principal,
		"exit_code", exitCode,
	)

	d.reconcileMu.Lock()
	if !d.running[principal] {
		// Already handled by another path (sandbox exit, reconcile, etc.).
		d.reconcileMu.Unlock()
		return
	}

	// Remove our own cancel function from the map before calling
	// destroyPrincipal. destroyPrincipal calls cancelProxyExitWatcher,
	// which would cancel THIS goroutine's context — the same context
	// passed to the launcher IPC call inside destroyPrincipal. Removing
	// the entry first makes cancelProxyExitWatcher a no-op for us.
	delete(d.proxyExitWatchers, principal)

	if err := d.destroyPrincipal(ctx, principal); err != nil {
		d.logger.Error("proxy exit handler: failed to destroy sandbox",
			"principal", principal, "error", err)
	}

	// Record a proxy crash failure with exponential backoff. The proxy
	// dying is always abnormal — without backoff, a crashing proxy
	// causes the same retry storm as a crashing sandbox.
	d.recordStartFailure(principal, failureCategoryProxyCrash, fmt.Sprintf("proxy exited with code %d", exitCode))
	crashBackoff := time.Until(d.startFailures[principal].nextRetryAt)
	d.logger.Warn("proxy crash, backing off before retry",
		"principal", principal,
		"exit_code", exitCode,
		"attempts", d.startFailures[principal].attempts,
		"retry_at", d.startFailures[principal].nextRetryAt,
	)
	d.reconcileMu.Unlock()

	if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
		schema.NewProxyCrashMessage(principal.AccountLocalpart(), "detected", exitCode, "")); err != nil {
		d.logger.Error("failed to post proxy crash notification",
			"principal", principal, "error", err)
	}

	// Trigger re-reconciliation. Reconcile will see the crash backoff
	// and skip the principal until the backoff expires.
	if err := d.reconcile(ctx); err != nil {
		d.logger.Error("reconciliation after proxy exit failed",
			"principal", principal,
			"error", err,
		)
		if _, sendErr := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
			schema.NewProxyCrashMessage(principal.AccountLocalpart(), "failed", exitCode, err.Error())); sendErr != nil {
			d.logger.Error("failed to post proxy recovery failure notification",
				"principal", principal, "error", sendErr)
		}
	} else {
		d.reconcileMu.Lock()
		recovered := d.running[principal]
		d.reconcileMu.Unlock()

		status := "recovered"
		var errorMessage string
		if !recovered {
			status = "backing_off"
			errorMessage = "proxy crashed, retry scheduled with exponential backoff"
		}
		if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
			schema.NewProxyCrashMessage(principal.AccountLocalpart(), status, exitCode, errorMessage)); err != nil {
			d.logger.Error("failed to post proxy recovery status",
				"principal", principal, "error", err)
		}
	}
	d.notifyReconcile()

	// Schedule a deferred reconcile after the crash backoff expires.
	// When the deferred reconcile succeeds and the principal is running,
	// post a "recovered" notification. Without this, the "backing_off"
	// message above is the last status posted and observers never learn
	// that recovery succeeded.
	if crashBackoff > 0 && d.shutdownCtx != nil {
		go func() {
			select {
			case <-d.shutdownCtx.Done():
				return
			case <-d.clock.After(crashBackoff):
				if err := d.reconcile(d.shutdownCtx); err != nil {
					d.logger.Error("deferred reconciliation after proxy crash backoff expired",
						"principal", principal,
						"error", err,
					)
				} else {
					d.reconcileMu.Lock()
					nowRunning := d.running[principal]
					d.reconcileMu.Unlock()
					if nowRunning {
						if _, err := d.sendEventRetry(d.shutdownCtx, d.configRoomID,
							schema.MatrixEventTypeMessage,
							schema.NewProxyCrashMessage(principal.AccountLocalpart(), "recovered", exitCode, "")); err != nil {
							d.logger.Error("failed to post deferred proxy recovery notification",
								"principal", principal, "error", err)
						}
					}
				}
				d.notifyReconcile()
			}
		}()
	}
}

// maxMatrixOutputLines is the maximum number of lines of captured sandbox
// output included in Matrix exit notifications. Keeps messages readable
// while still providing enough context to diagnose most failures. The
// full output (up to 500 lines from the launcher) is logged at WARN level
// in the daemon's structured log.
const maxMatrixOutputLines = 50

// tailLines returns the last n lines of s, matching tail -n semantics:
// a trailing newline terminates the last line (does not start a new one).
// If s has n or fewer lines, it is returned unchanged.
func tailLines(s string, n int) string {
	if len(s) == 0 {
		return s
	}

	// A trailing newline terminates the last line — search from before it
	// so it doesn't count as an extra line separator.
	searchFrom := len(s) - 1
	if s[searchFrom] == '\n' {
		searchFrom--
	}

	// Walk backwards counting newline separators. For n lines we need
	// n-1 separators between them, plus one more newline to find the
	// cut point (the newline before the first of our n lines).
	count := 0
	for i := searchFrom; i >= 0; i-- {
		if s[i] == '\n' {
			count++
			if count == n {
				return s[i+1:]
			}
		}
	}
	return s
}

// countLines returns the number of lines in s. An empty string has zero
// lines; a string with no trailing newline counts its last segment.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	count := strings.Count(s, "\n")
	if s[len(s)-1] != '\n' {
		count++
	}
	return count
}

// shellInterpreters are commands that wrap other commands. When the sandbox
// command starts with one of these, the actual binary being executed is
// embedded in the shell arguments and cannot be statically validated.
var shellInterpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true,
	"/bin/sh": true, "/bin/bash": true, "/bin/zsh": true, "/bin/dash": true,
	"/usr/bin/sh": true, "/usr/bin/bash": true, "/usr/bin/env": true,
}

// validateCommandBinary checks that the command binary is resolvable before
// sandbox creation. For Nix environments, the binary is looked up inside
// the environment's bin/ directory (which gets bind-mounted at /usr/local/bin
// inside the sandbox). For host-resolved commands, exec.LookPath is used.
// Shell interpreters are skipped since they are always available in the
// sandbox's minimal /usr mount.
func validateCommandBinary(command string, environmentPath string) error {
	if command == "" {
		return fmt.Errorf("command is empty")
	}

	// Shell interpreters are always available inside the sandbox.
	if shellInterpreters[command] {
		return nil
	}

	// Absolute paths: check the file directly. If the path is inside a
	// Nix store, the prefetchEnvironment step already confirmed the
	// closure exists on disk.
	if filepath.IsAbs(command) {
		if _, err := os.Stat(command); err != nil {
			return fmt.Errorf("%q not found: %w", command, err)
		}
		return nil
	}

	// Bare command name: check the Nix environment first (if present),
	// then fall back to the daemon's PATH.
	if environmentPath != "" {
		candidate := filepath.Join(environmentPath, "bin", command)
		if _, err := os.Stat(candidate); err == nil {
			return nil
		}
	}

	if _, err := exec.LookPath(command); err != nil {
		if environmentPath != "" {
			return fmt.Errorf("%q not found in environment %s/bin/ or on PATH", command, environmentPath)
		}
		return fmt.Errorf("%q not found on PATH", command)
	}

	return nil
}
