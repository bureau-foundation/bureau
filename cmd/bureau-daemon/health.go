// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Health monitoring: each running principal whose template defines a
// HealthCheck gets a health monitor goroutine. The goroutine probes the
// service at the configured interval using one of two transports:
//
//   - HTTP (type "http"): sends GET to the proxy admin socket at the
//     configured Endpoint. For sandboxed third-party services.
//   - Socket (type "socket"): sends a CBOR "health" action to the
//     principal's service socket. For Bureau-native services.
//
// When consecutive failures reach the threshold, the monitor triggers a
// rollback to the previous working sandbox configuration.
//
// The health monitor mirrors the layoutWatcher pattern: per-principal
// goroutine with cancel + done channel for lifecycle management. The
// health monitor's mutex (healthMonitorsMu) is separate from the
// reconcile mutex (reconcileMu) — the monitor mutex guards the map of
// running monitors, while the reconcile mutex guards sandbox state
// mutations during rollback.

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/bureau-foundation/bureau/lib/ipc"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
)

// healthMonitor tracks a running health check goroutine for a single
// principal. Mirrors the layoutWatcher type.
type healthMonitor struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startHealthMonitor begins health monitoring for a principal. Safe to
// call multiple times for the same principal (subsequent calls are
// no-ops). The monitor runs until stopped or the parent context is
// cancelled.
func (d *Daemon) startHealthMonitor(ctx context.Context, principal ref.Entity, healthCheck *schema.HealthCheck) {
	d.healthMonitorsMu.Lock()
	defer d.healthMonitorsMu.Unlock()

	if _, exists := d.healthMonitors[principal]; exists {
		return
	}

	monitorContext, cancel := context.WithCancel(ctx)
	monitor := &healthMonitor{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	d.healthMonitors[principal] = monitor

	go d.runHealthMonitor(monitorContext, principal, healthCheck, monitor.done)
}

// stopHealthMonitor cancels the health monitor for a principal and
// waits for the goroutine to exit. No-op if no monitor is running.
func (d *Daemon) stopHealthMonitor(principal ref.Entity) {
	d.healthMonitorsMu.Lock()
	monitor, exists := d.healthMonitors[principal]
	if !exists {
		d.healthMonitorsMu.Unlock()
		return
	}
	delete(d.healthMonitors, principal)
	d.healthMonitorsMu.Unlock()

	monitor.cancel()
	<-monitor.done
}

// stopAllHealthMonitors cancels all running health monitors and waits
// for all goroutines to exit. Called during daemon shutdown.
func (d *Daemon) stopAllHealthMonitors() {
	d.healthMonitorsMu.Lock()
	monitors := make(map[ref.Entity]*healthMonitor, len(d.healthMonitors))
	for principal, monitor := range d.healthMonitors {
		monitors[principal] = monitor
	}
	d.healthMonitors = make(map[ref.Entity]*healthMonitor)
	d.healthMonitorsMu.Unlock()

	// Cancel all monitors first, then wait. Parallel shutdown.
	for _, monitor := range monitors {
		monitor.cancel()
	}
	for _, monitor := range monitors {
		<-monitor.done
	}
}

// healthCheckDefaults applies default values to a HealthCheck config.
// Returns a copy with all zero fields replaced by their documented
// defaults. Does not modify the original.
func healthCheckDefaults(config *schema.HealthCheck) schema.HealthCheck {
	result := *config
	if result.Type == "" {
		result.Type = schema.HealthCheckTypeHTTP
	}
	if result.TimeoutSeconds == 0 {
		result.TimeoutSeconds = 5
	}
	if result.FailureThreshold == 0 {
		result.FailureThreshold = 3
	}
	if result.GracePeriodSeconds == 0 {
		result.GracePeriodSeconds = 30
	}
	return result
}

// validateHealthCheck checks that a HealthCheck configuration is valid.
// Called during template resolution so misconfigured health checks are
// caught at reconcile time (loud failure), not silently failing at
// probe time.
func validateHealthCheck(check *schema.HealthCheck) error {
	effectiveType := check.Type
	if effectiveType == "" {
		effectiveType = schema.HealthCheckTypeHTTP
	}
	switch effectiveType {
	case schema.HealthCheckTypeHTTP:
		if check.Endpoint == "" {
			return fmt.Errorf("endpoint is required for HTTP health checks")
		}
	case schema.HealthCheckTypeSocket:
		// No endpoint needed — the daemon sends a CBOR "health" action
		// to the principal's service socket.
	default:
		return fmt.Errorf("unknown health check type %q (valid: %q, %q)",
			check.Type, schema.HealthCheckTypeHTTP, schema.HealthCheckTypeSocket)
	}
	if check.IntervalSeconds <= 0 {
		return fmt.Errorf("interval_seconds must be positive")
	}
	return nil
}

// runHealthMonitor is the main goroutine for a single principal's
// health monitoring. It waits the grace period, then probes the
// service at the configured interval using the transport selected by
// the HealthCheck type. On sustained failure, it triggers a rollback.
func (d *Daemon) runHealthMonitor(ctx context.Context, principal ref.Entity, healthCheck *schema.HealthCheck, done chan struct{}) {
	defer close(done)

	config := healthCheckDefaults(healthCheck)

	// Log the socket path for socket-type health checks so
	// operators can verify the daemon is probing the right path.
	var healthTarget string
	if config.Type == schema.HealthCheckTypeSocket {
		healthTarget = d.serviceSocketPathFunc(principal)
	} else {
		healthTarget = config.Endpoint
	}
	d.logger.Info("health monitor started",
		"principal", principal,
		"type", config.Type,
		"target", healthTarget,
		"interval_seconds", config.IntervalSeconds,
		"timeout_seconds", config.TimeoutSeconds,
		"failure_threshold", config.FailureThreshold,
		"grace_period_seconds", config.GracePeriodSeconds,
	)

	// Grace period: wait for the service to initialize before checking.
	select {
	case <-ctx.Done():
		return
	case <-d.clock.After(time.Duration(config.GracePeriodSeconds) * time.Second):
	}

	consecutiveFailures := 0
	ticker := d.clock.NewTicker(time.Duration(config.IntervalSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		healthy := d.checkHealth(ctx, principal, &config)
		if healthy {
			if consecutiveFailures > 0 {
				d.logger.Info("health check recovered",
					"principal", principal,
					"previous_failures", consecutiveFailures,
				)
			}
			consecutiveFailures = 0
			continue
		}

		consecutiveFailures++
		d.logger.Warn("health check failed",
			"principal", principal,
			"type", config.Type,
			"endpoint", config.Endpoint,
			"consecutive_failures", consecutiveFailures,
			"threshold", config.FailureThreshold,
		)

		if consecutiveFailures >= config.FailureThreshold {
			d.logger.Error("health check threshold reached, triggering rollback",
				"principal", principal,
				"consecutive_failures", consecutiveFailures,
			)
			d.rollbackPrincipal(ctx, principal)
			return
		}
	}
}

// checkHealth dispatches a health probe to the appropriate transport
// based on the config type. Returns true if the service is healthy.
func (d *Daemon) checkHealth(ctx context.Context, principal ref.Entity, config *schema.HealthCheck) bool {
	switch config.Type {
	case schema.HealthCheckTypeSocket:
		return d.checkHealthSocket(ctx, principal, config.TimeoutSeconds)
	default:
		return d.checkHealthHTTP(ctx, principal, config.Endpoint, config.TimeoutSeconds)
	}
}

// checkHealthHTTP sends a single HTTP GET to the principal's proxy admin
// socket at the configured health endpoint. Returns true if the
// response is HTTP 200.
func (d *Daemon) checkHealthHTTP(ctx context.Context, principal ref.Entity, endpoint string, timeoutSeconds int) bool {
	adminSocket := d.adminSocketPathFunc(principal)
	client := proxyAdminClient(adminSocket)

	requestContext, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(requestContext, http.MethodGet,
		"http://localhost"+endpoint, nil)
	if err != nil {
		d.logger.Error("health check: creating request",
			"principal", principal, "error", err)
		return false
	}

	response, err := client.Do(request)
	if err != nil {
		// Connection error — proxy is unreachable (crashed, socket gone).
		return false
	}
	defer response.Body.Close()

	return response.StatusCode == http.StatusOK
}

// checkHealthSocket sends a CBOR "health" action to the principal's
// service socket. Returns true if the service responds with
// {healthy: true}. Connection failures, timeouts, and error responses
// all count as unhealthy.
func (d *Daemon) checkHealthSocket(ctx context.Context, principal ref.Entity, timeoutSeconds int) bool {
	socketPath := d.serviceSocketPathFunc(principal)
	client := service.NewServiceClientFromToken(socketPath, nil)

	requestContext, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	var response service.HealthResponse
	if err := client.Call(requestContext, "health", nil, &response); err != nil {
		d.logger.Warn("health check socket probe failed",
			"principal", principal,
			"socket_path", socketPath,
			"error", err,
		)
		return false
	}
	if !response.Healthy {
		d.logger.Info("health check socket returned unhealthy",
			"principal", principal,
			"reason", response.Reason,
		)
	}
	return response.Healthy
}

// rollbackPrincipal destroys the current sandbox and recreates it with
// the previous working spec. Called by the health monitor goroutine
// when the failure threshold is reached.
//
// Acquires reconcileMu to serialize with the reconcile loop. The health
// monitor goroutine does NOT hold this mutex during normal operation —
// only during the rollback action itself.
func (d *Daemon) rollbackPrincipal(ctx context.Context, principal ref.Entity) {
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()

	// Guard: the principal may have been destroyed by a concurrent
	// reconcile while we were waiting for the mutex.
	if !d.running[principal] {
		d.logger.Info("health rollback skipped: principal already stopped",
			"principal", principal)
		return
	}

	// Save state needed for recreation. destroyPrincipal clears all tracking
	// maps, so these must be captured before the call.
	previousSpec := d.previousSpecs[principal]
	template := d.lastTemplates[principal]
	previousCiphertext, credentialRollback := d.previousCredentials[principal]

	// Remove this monitor from the health monitors map before calling
	// destroyPrincipal. destroyPrincipal calls stopHealthMonitor, which
	// waits on the monitor's done channel — but this goroutine IS the
	// monitor, so waiting would deadlock. Removing the map entry first
	// makes stopHealthMonitor a no-op for this principal.
	d.healthMonitorsMu.Lock()
	delete(d.healthMonitors, principal)
	d.healthMonitorsMu.Unlock()

	if err := d.destroyPrincipal(ctx, principal, false); err != nil {
		d.logger.Error("health rollback: failed to destroy sandbox, sandbox may be orphaned",
			"principal", principal, "error", err)
		return
	}

	d.logger.Info("health rollback: sandbox destroyed", "principal", principal)

	// If no previous spec exists, the principal was on its first-ever
	// spec or the daemon restarted (previousSpecs is in-memory only).
	// The next reconcile cycle will recreate from the current config
	// if the admin fixes the template.
	if previousSpec == nil {
		d.logger.Error("health rollback: no previous spec to roll back to",
			"principal", principal)
		if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
			schema.NewHealthCheckMessage(principal.AccountLocalpart(), schema.HealthCheckDestroyed, "")); err != nil {
			d.logger.Error("failed to post health rollback notification",
				"principal", principal, "error", err)
		}
		if credentialRollback {
			if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
				schema.NewCredentialsRotatedMessage(principal.AccountLocalpart(), schema.CredRotationRollbackFailed,
					"no previous sandbox spec available for rollback")); err != nil {
				d.logger.Error("failed to post credential rollback failure notification",
					"principal", principal, "error", err)
			}
		}
		return
	}

	// Use previous credentials if available (credential rotation rollback).
	// After a credential rotation, previousCredentials holds the ciphertext
	// that was working before the rotation. Reading fresh from Matrix would
	// return the new (possibly broken) credentials that triggered the health
	// check failure. For non-rotation rollbacks (structural restart), there
	// are no previous credentials, so we read from Matrix as before.
	var ciphertext string
	if credentialRollback {
		ciphertext = previousCiphertext
		d.logger.Info("health rollback: using previous credentials",
			"principal", principal)
	} else {
		credentials, err := d.readCredentials(ctx, principal)
		if err != nil {
			d.logger.Error("health rollback: cannot read credentials",
				"principal", principal, "error", err)
			if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
				schema.NewHealthCheckMessage(principal.AccountLocalpart(), schema.HealthCheckRollbackFailed, fmt.Sprintf("cannot read credentials: %v", err))); err != nil {
				d.logger.Error("failed to post health rollback notification",
					"principal", principal, "error", err)
			}
			return
		}
		ciphertext = credentials.Ciphertext
	}

	// Read the current assignment for authorization grants and service config.
	config, err := d.readMachineConfig(ctx)
	if err != nil {
		d.logger.Error("health rollback: cannot read machine config",
			"principal", principal, "error", err)
		return
	}

	var assignment *schema.PrincipalAssignment
	for i := range config.Principals {
		if config.Principals[i].Principal == principal {
			assignment = &config.Principals[i]
			break
		}
	}
	if assignment == nil {
		d.logger.Info("health rollback: principal no longer in config, not recreating",
			"principal", principal)
		return
	}

	// Resolve authorization grants so the proxy starts with enforcement.
	grants := d.resolveGrantsForProxy(principal.UserID())

	// Recreate with the previous working spec and credentials.
	response, err := d.launcherRequest(ctx, launcherIPCRequest{
		Action:               ipc.ActionCreateSandbox,
		Principal:            principal.AccountLocalpart(),
		EncryptedCredentials: ciphertext,
		Grants:               grants,
		SandboxSpec:          previousSpec,
	})
	if err != nil {
		d.logger.Error("health rollback: create-sandbox IPC failed",
			"principal", principal, "error", err)
		if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
			schema.NewHealthCheckMessage(principal.AccountLocalpart(), schema.HealthCheckRollbackFailed, "create-sandbox IPC failed")); err != nil {
			d.logger.Error("failed to post health rollback notification",
				"principal", principal, "error", err)
		}
		return
	}
	if !response.OK {
		d.logger.Error("health rollback: create-sandbox rejected",
			"principal", principal, "error", response.Error)
		if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
			schema.NewHealthCheckMessage(principal.AccountLocalpart(), schema.HealthCheckRollbackFailed, response.Error)); err != nil {
			d.logger.Error("failed to post health rollback notification",
				"principal", principal, "error", err)
		}
		return
	}

	d.running[principal] = true
	d.notifyStatusChange()
	d.lastSpecs[principal] = previousSpec
	d.lastCredentials[principal] = ciphertext
	d.lastGrants[principal] = grants
	d.lastActivityAt = d.clock.Now()

	d.logger.Info("health rollback: sandbox recreated with previous working configuration",
		"principal", principal)

	d.startLayoutWatcher(ctx, principal)
	d.configureConsumerProxy(ctx, principal)

	directory := d.buildServiceDirectory()
	if err := d.pushDirectoryToProxy(ctx, principal, directory); err != nil {
		d.logger.Error("health rollback: failed to push service directory",
			"principal", principal, "error", err)
	}

	// Start a new health monitor with a fresh grace period for the
	// rolled-back sandbox. Restore the template so the daemon's tracking
	// state is consistent for future reconciles.
	if template != nil && template.HealthCheck != nil {
		d.lastTemplates[principal] = template
		d.startHealthMonitor(ctx, principal, template.HealthCheck)
	}

	// Start new exit watchers for the recreated sandbox.
	if d.shutdownCtx != nil {
		watcherCtx, watcherCancel := context.WithCancel(d.shutdownCtx)
		d.exitWatchers[principal] = watcherCancel
		go d.watchSandboxExit(watcherCtx, principal)

		proxyCtx, proxyCancel := context.WithCancel(d.shutdownCtx)
		d.proxyExitWatchers[principal] = proxyCancel
		go d.watchProxyExit(proxyCtx, principal)
	}

	if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
		schema.NewHealthCheckMessage(principal.AccountLocalpart(), schema.HealthCheckRolledBack, "")); err != nil {
		d.logger.Error("failed to post health rollback notification",
			"principal", principal, "error", err)
	}

	// Post a credential-specific notification when rolling back a
	// credential rotation. Operators need to know the rotation failed
	// and old credentials were restored — distinct from a structural
	// config rollback.
	if credentialRollback {
		if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.MatrixEventTypeMessage,
			schema.NewCredentialsRotatedMessage(principal.AccountLocalpart(), schema.CredRotationRolledBack, "")); err != nil {
			d.logger.Error("failed to post credential rollback notification",
				"principal", principal, "error", err)
		}
	}

	d.notifyReconcile()
}
