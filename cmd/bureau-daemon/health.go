// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Health monitoring: each running principal whose template defines a
// HealthCheck gets a health monitor goroutine. The goroutine polls the
// proxy admin socket at the configured endpoint and interval. When
// consecutive failures reach the threshold, it triggers a rollback to
// the previous working sandbox configuration.
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

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
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
func (d *Daemon) startHealthMonitor(ctx context.Context, localpart string, healthCheck *schema.HealthCheck) {
	d.healthMonitorsMu.Lock()
	defer d.healthMonitorsMu.Unlock()

	if _, exists := d.healthMonitors[localpart]; exists {
		return
	}

	monitorContext, cancel := context.WithCancel(ctx)
	monitor := &healthMonitor{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	d.healthMonitors[localpart] = monitor

	go d.runHealthMonitor(monitorContext, localpart, healthCheck, monitor.done)
}

// stopHealthMonitor cancels the health monitor for a principal and
// waits for the goroutine to exit. No-op if no monitor is running.
func (d *Daemon) stopHealthMonitor(localpart string) {
	d.healthMonitorsMu.Lock()
	monitor, exists := d.healthMonitors[localpart]
	if !exists {
		d.healthMonitorsMu.Unlock()
		return
	}
	delete(d.healthMonitors, localpart)
	d.healthMonitorsMu.Unlock()

	monitor.cancel()
	<-monitor.done
}

// stopAllHealthMonitors cancels all running health monitors and waits
// for all goroutines to exit. Called during daemon shutdown.
func (d *Daemon) stopAllHealthMonitors() {
	d.healthMonitorsMu.Lock()
	monitors := make(map[string]*healthMonitor, len(d.healthMonitors))
	for localpart, monitor := range d.healthMonitors {
		monitors[localpart] = monitor
	}
	d.healthMonitors = make(map[string]*healthMonitor)
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

// runHealthMonitor is the main goroutine for a single principal's
// health monitoring. It waits the grace period, then polls the proxy
// admin socket at the configured interval. On sustained failure, it
// triggers a rollback.
func (d *Daemon) runHealthMonitor(ctx context.Context, localpart string, healthCheck *schema.HealthCheck, done chan struct{}) {
	defer close(done)

	config := healthCheckDefaults(healthCheck)

	d.logger.Info("health monitor started",
		"principal", localpart,
		"endpoint", config.Endpoint,
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

		healthy := d.checkHealth(ctx, localpart, config.Endpoint, config.TimeoutSeconds)
		if healthy {
			if consecutiveFailures > 0 {
				d.logger.Info("health check recovered",
					"principal", localpart,
					"previous_failures", consecutiveFailures,
				)
			}
			consecutiveFailures = 0
			continue
		}

		consecutiveFailures++
		d.logger.Warn("health check failed",
			"principal", localpart,
			"endpoint", config.Endpoint,
			"consecutive_failures", consecutiveFailures,
			"threshold", config.FailureThreshold,
		)

		if consecutiveFailures >= config.FailureThreshold {
			d.logger.Error("health check threshold reached, triggering rollback",
				"principal", localpart,
				"consecutive_failures", consecutiveFailures,
			)
			d.rollbackPrincipal(ctx, localpart)
			return
		}
	}
}

// checkHealth sends a single HTTP GET to the principal's proxy admin
// socket at the configured health endpoint. Returns true if the
// response is HTTP 200.
func (d *Daemon) checkHealth(ctx context.Context, localpart string, endpoint string, timeoutSeconds int) bool {
	adminSocket := d.adminSocketPathFunc(localpart)
	client := proxyAdminClient(adminSocket)

	requestContext, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(requestContext, http.MethodGet,
		"http://localhost"+endpoint, nil)
	if err != nil {
		d.logger.Error("health check: creating request",
			"principal", localpart, "error", err)
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

// rollbackPrincipal destroys the current sandbox and recreates it with
// the previous working spec. Called by the health monitor goroutine
// when the failure threshold is reached.
//
// Acquires reconcileMu to serialize with the reconcile loop. The health
// monitor goroutine does NOT hold this mutex during normal operation —
// only during the rollback action itself.
func (d *Daemon) rollbackPrincipal(ctx context.Context, localpart string) {
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()

	// Guard: the principal may have been destroyed by a concurrent
	// reconcile while we were waiting for the mutex.
	if !d.running[localpart] {
		d.logger.Info("health rollback skipped: principal already stopped",
			"principal", localpart)
		return
	}

	previousSpec := d.previousSpecs[localpart]

	// Cancel exit watchers before destroying — the rollback destroy
	// is intentional, not an unexpected exit.
	d.cancelExitWatcher(localpart)
	d.cancelProxyExitWatcher(localpart)

	// Stop the layout watcher before destroying the sandbox.
	d.stopLayoutWatcher(localpart)

	// Destroy the current sandbox.
	response, err := d.launcherRequest(ctx, launcherIPCRequest{
		Action:    "destroy-sandbox",
		Principal: localpart,
	})
	if err != nil {
		d.logger.Error("health rollback: destroy-sandbox IPC failed",
			"principal", localpart, "error", err)
		// Clean up state even on IPC failure — the sandbox may be gone.
		d.revokeAndCleanupTokens(ctx, localpart)
		delete(d.running, localpart)
		delete(d.exitWatchers, localpart)
		delete(d.proxyExitWatchers, localpart)
		delete(d.lastCredentials, localpart)
		delete(d.lastObserveAllowances, localpart)
		delete(d.lastSpecs, localpart)
		delete(d.previousSpecs, localpart)
		delete(d.lastTemplates, localpart)
		return
	}
	if !response.OK {
		d.logger.Error("health rollback: destroy-sandbox rejected",
			"principal", localpart, "error", response.Error)
		d.revokeAndCleanupTokens(ctx, localpart)
		delete(d.running, localpart)
		delete(d.exitWatchers, localpart)
		delete(d.proxyExitWatchers, localpart)
		delete(d.lastCredentials, localpart)
		delete(d.lastObserveAllowances, localpart)
		delete(d.lastSpecs, localpart)
		delete(d.previousSpecs, localpart)
		delete(d.lastTemplates, localpart)
		return
	}

	d.revokeAndCleanupTokens(ctx, localpart)
	delete(d.running, localpart)
	delete(d.exitWatchers, localpart)
	delete(d.proxyExitWatchers, localpart)
	delete(d.lastCredentials, localpart)
	delete(d.lastObserveAllowances, localpart)
	delete(d.lastSpecs, localpart)
	d.lastActivityAt = d.clock.Now()

	d.logger.Info("health rollback: sandbox destroyed", "principal", localpart)

	// If no previous spec exists, the principal was on its first-ever
	// spec or the daemon restarted (previousSpecs is in-memory only).
	// Destroy and report — the next reconcile cycle will recreate from
	// the current config if the admin fixes the template.
	if previousSpec == nil {
		d.logger.Error("health rollback: no previous spec to roll back to",
			"principal", localpart)
		delete(d.previousSpecs, localpart)
		delete(d.lastTemplates, localpart)
		d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
			fmt.Sprintf("CRITICAL: %s health check failed, no previous working configuration. Principal destroyed.", localpart)))
		return
	}

	// Read fresh credentials (they may have been rotated).
	credentials, err := d.readCredentials(ctx, localpart)
	if err != nil {
		d.logger.Error("health rollback: cannot read credentials",
			"principal", localpart, "error", err)
		delete(d.previousSpecs, localpart)
		delete(d.lastTemplates, localpart)
		d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
			fmt.Sprintf("CRITICAL: %s health rollback failed (cannot read credentials: %v). Principal destroyed.", localpart, err)))
		return
	}

	// Read the current assignment for authorization grants and service config.
	config, err := d.readMachineConfig(ctx)
	if err != nil {
		d.logger.Error("health rollback: cannot read machine config",
			"principal", localpart, "error", err)
		delete(d.previousSpecs, localpart)
		delete(d.lastTemplates, localpart)
		return
	}

	var assignment *schema.PrincipalAssignment
	for i := range config.Principals {
		if config.Principals[i].Localpart == localpart {
			assignment = &config.Principals[i]
			break
		}
	}
	if assignment == nil {
		d.logger.Info("health rollback: principal no longer in config, not recreating",
			"principal", localpart)
		delete(d.previousSpecs, localpart)
		delete(d.lastTemplates, localpart)
		return
	}

	// Resolve authorization grants so the proxy starts with enforcement.
	grants := d.resolveGrantsForProxy(localpart, *assignment, config)

	// Recreate with the previous working spec.
	response, err = d.launcherRequest(ctx, launcherIPCRequest{
		Action:               "create-sandbox",
		Principal:            localpart,
		EncryptedCredentials: credentials.Ciphertext,
		Grants:               grants,
		SandboxSpec:          previousSpec,
	})
	if err != nil {
		d.logger.Error("health rollback: create-sandbox IPC failed",
			"principal", localpart, "error", err)
		delete(d.previousSpecs, localpart)
		delete(d.lastTemplates, localpart)
		d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
			fmt.Sprintf("CRITICAL: %s rollback create-sandbox failed. Principal destroyed.", localpart)))
		return
	}
	if !response.OK {
		d.logger.Error("health rollback: create-sandbox rejected",
			"principal", localpart, "error", response.Error)
		delete(d.previousSpecs, localpart)
		delete(d.lastTemplates, localpart)
		d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
			fmt.Sprintf("CRITICAL: %s rollback create-sandbox rejected: %s. Principal destroyed.", localpart, response.Error)))
		return
	}

	d.running[localpart] = true
	d.lastSpecs[localpart] = previousSpec
	d.lastGrants[localpart] = grants
	delete(d.previousSpecs, localpart)
	d.lastActivityAt = d.clock.Now()

	d.logger.Info("health rollback: sandbox recreated with previous spec",
		"principal", localpart)

	d.startLayoutWatcher(ctx, localpart)
	d.configureConsumerProxy(ctx, localpart)

	directory := d.buildServiceDirectory()
	if err := d.pushDirectoryToProxy(ctx, localpart, directory); err != nil {
		d.logger.Error("health rollback: failed to push service directory",
			"principal", localpart, "error", err)
	}

	// Start a new health monitor with a fresh grace period for the
	// rolled-back sandbox. Use the current template's health check
	// config (the admin's latest intent).
	if template := d.lastTemplates[localpart]; template != nil && template.HealthCheck != nil {
		d.startHealthMonitor(ctx, localpart, template.HealthCheck)
	}

	// Start new exit watchers for the recreated sandbox.
	if d.shutdownCtx != nil {
		watcherCtx, watcherCancel := context.WithCancel(d.shutdownCtx)
		d.exitWatchers[localpart] = watcherCancel
		go d.watchSandboxExit(watcherCtx, localpart)

		proxyCtx, proxyCancel := context.WithCancel(d.shutdownCtx)
		d.proxyExitWatchers[localpart] = proxyCancel
		go d.watchProxyExit(proxyCtx, localpart)
	}

	d.session.SendMessage(ctx, d.configRoomID, messaging.NewTextMessage(
		fmt.Sprintf("Rolled back %s to previous working configuration after health check failure.", localpart)))

	d.notifyReconcile()
}
