// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// haWatchdog manages the daemon's participation in the HA lease protocol
// for critical services. It monitors heartbeats of machines hosting
// critical services and competes to acquire leases when the current
// holder becomes unresponsive.
//
// The watchdog reads FleetServiceContent and HALeaseContent state events
// from the fleet room (#bureau/fleet). Services with ha_class:"critical"
// are tracked. When a lease holder's heartbeat goes stale (older than
// 3x the status interval), any eligible daemon may attempt to acquire
// the lease using a random-backoff-plus-verify protocol that compensates
// for Matrix's last-writer-wins semantics.
type haWatchdog struct {
	daemon *Daemon

	// mu protects criticalServices, leases, and heldLeases.
	mu sync.Mutex

	// criticalServices caches ha_class:critical service definitions
	// from #bureau/fleet. Keyed by service localpart.
	criticalServices map[string]*schema.FleetServiceContent

	// leases caches current HA lease state. Keyed by service localpart.
	leases map[string]*schema.HALeaseContent

	// heldLeases tracks leases this daemon currently holds and is
	// actively renewing. Each entry's CancelFunc stops the renewal
	// goroutine. Keyed by service localpart.
	heldLeases map[string]context.CancelFunc

	// clock provides time operations. Uses the daemon's clock for
	// testability (fake clock in tests, real clock in production).
	clock clock.Clock

	// baseDelay is the base unit for all HA acquisition timing.
	// Preferred machines wait 1-3 × baseDelay before claiming,
	// non-preferred machines wait 4-10 × baseDelay, and the
	// verification window is 2 × baseDelay. Production default
	// is 1s. Integration tests set this to 0 for instantaneous
	// acquisition while still exercising the full protocol path.
	baseDelay time.Duration

	logger *slog.Logger
}

// newHAWatchdog creates an haWatchdog bound to the given daemon.
// baseDelay controls the timing of the acquisition protocol: all random
// backoff ranges and the verification window scale linearly from this
// value. Production uses 1s; integration tests use 0 for instant
// acquisition.
func newHAWatchdog(daemon *Daemon, baseDelay time.Duration, logger *slog.Logger) *haWatchdog {
	return &haWatchdog{
		daemon:           daemon,
		criticalServices: make(map[string]*schema.FleetServiceContent),
		leases:           make(map[string]*schema.HALeaseContent),
		heldLeases:       make(map[string]context.CancelFunc),
		clock:            daemon.clock,
		baseDelay:        baseDelay,
		logger:           logger,
	}
}

// syncFleetState reads FleetServiceContent and HALeaseContent state events
// from the fleet room and updates the watchdog's cached state. Only services
// with ha_class:"critical" are retained.
func (w *haWatchdog) syncFleetState(ctx context.Context) {
	events, err := w.daemon.session.GetRoomState(ctx, w.daemon.fleetRoomID)
	if err != nil {
		w.logger.Error("fetching fleet room state for HA watchdog", "error", err)
		return
	}

	newCriticalServices := make(map[string]*schema.FleetServiceContent)
	newLeases := make(map[string]*schema.HALeaseContent)

	for _, event := range events {
		if event.StateKey == nil {
			continue
		}
		stateKey := *event.StateKey

		contentJSON, err := json.Marshal(event.Content)
		if err != nil {
			continue
		}

		switch event.Type {
		case schema.EventTypeFleetService:
			var definition schema.FleetServiceContent
			if err := json.Unmarshal(contentJSON, &definition); err != nil {
				w.logger.Warn("parsing fleet service definition",
					"state_key", stateKey, "error", err)
				continue
			}
			if definition.HAClass == "critical" {
				newCriticalServices[stateKey] = &definition
			}

		case schema.EventTypeHALease:
			var lease schema.HALeaseContent
			if err := json.Unmarshal(contentJSON, &lease); err != nil {
				w.logger.Warn("parsing HA lease",
					"state_key", stateKey, "error", err)
				continue
			}
			newLeases[stateKey] = &lease
		}
	}

	w.mu.Lock()
	w.criticalServices = newCriticalServices
	w.leases = newLeases
	w.mu.Unlock()

	w.logger.Debug("synced fleet state for HA watchdog",
		"critical_services", len(newCriticalServices),
		"leases", len(newLeases),
	)
}

// evaluate checks each critical service's lease state and decides whether
// this daemon should attempt acquisition. Called from the sync loop when
// fleet room changes are detected.
func (w *haWatchdog) evaluate(ctx context.Context) {
	w.mu.Lock()
	// Snapshot the state under the lock so we can release it before
	// doing I/O (Matrix calls, sleeps).
	services := make(map[string]*schema.FleetServiceContent, len(w.criticalServices))
	for key, value := range w.criticalServices {
		services[key] = value
	}
	leases := make(map[string]*schema.HALeaseContent, len(w.leases))
	for key, value := range w.leases {
		leases[key] = value
	}
	w.mu.Unlock()

	now := w.clock.Now()

	for serviceLocalpart, definition := range services {
		lease := leases[serviceLocalpart]

		if w.isLeaseHealthy(lease, now) {
			continue
		}

		if !w.isEligible(definition) {
			w.logger.Debug("ineligible for HA lease",
				"service", serviceLocalpart)
			continue
		}

		w.logger.Info("lease holder offline, attempting acquisition",
			"service", serviceLocalpart,
			"holder", leaseHolder(lease),
		)

		// Attempt acquisition in a goroutine so we don't block the
		// sync loop. The acquisition involves random delays and
		// Matrix round-trips.
		go func(localpart string, def *schema.FleetServiceContent) {
			if err := w.attemptAcquisition(ctx, localpart, def); err != nil {
				w.logger.Error("HA lease acquisition failed",
					"service", localpart, "error", err)
			}
		}(serviceLocalpart, definition)
	}
}

// isLeaseHealthy returns true if the lease is held by a machine whose
// lease has not expired. The lease ExpiresAt field serves as the
// heartbeat proxy: a healthy holder renews the lease at ttl/3 intervals,
// so an unexpired lease implies recent activity.
func (w *haWatchdog) isLeaseHealthy(lease *schema.HALeaseContent, now time.Time) bool {
	if lease == nil || lease.Holder == "" {
		return false
	}

	expiresAt, err := time.Parse(time.RFC3339, lease.ExpiresAt)
	if err != nil {
		// Unparseable expiry is treated as expired.
		return false
	}

	return !now.After(expiresAt)
}

// isEligible checks whether this daemon's machine can host the given
// service based on placement constraints.
func (w *haWatchdog) isEligible(definition *schema.FleetServiceContent) bool {
	constraints := definition.Placement

	// Check allowed_machines globs. If the list is non-empty, this
	// machine's localpart must match at least one pattern.
	if len(constraints.AllowedMachines) > 0 {
		matched := false
		for _, pattern := range constraints.AllowedMachines {
			if ok, _ := path.Match(pattern, w.daemon.machine.Localpart()); ok {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// Check required labels. The daemon does not cache MachineInfo
	// locally, so we fetch the current state event from the machine
	// room. This is acceptable because evaluate() runs infrequently
	// (only on fleet room state changes).
	if len(constraints.Requires) > 0 {
		machineLabels := w.fetchMachineLabels()
		for _, requirement := range constraints.Requires {
			if !labelSatisfied(requirement, machineLabels) {
				return false
			}
		}
	}

	return true
}

// fetchMachineLabels reads this machine's MachineInfo from the machine
// room and returns its labels. Returns nil on any error (treated as no
// labels).
func (w *haWatchdog) fetchMachineLabels() map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	raw, err := w.daemon.session.GetStateEvent(ctx, w.daemon.machineRoomID,
		schema.EventTypeMachineInfo, w.daemon.machine.Localpart())
	if err != nil {
		w.logger.Debug("failed to fetch machine info for eligibility check", "error", err)
		return nil
	}

	var info schema.MachineInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		w.logger.Debug("failed to parse machine info for eligibility check", "error", err)
		return nil
	}

	return info.Labels
}

// labelSatisfied checks whether a single label requirement is met.
// Requirements can be "key=value" (exact match) or just "key" (presence check).
func labelSatisfied(requirement string, labels map[string]string) bool {
	key, value, hasEquals := strings.Cut(requirement, "=")
	if hasEquals {
		return labels[key] == value
	}
	// Presence check: key exists with any value.
	_, exists := labels[requirement]
	return exists
}

// attemptAcquisition runs the random-backoff-plus-verify protocol to
// acquire an HA lease. This is designed to handle Matrix's
// last-writer-wins semantics: multiple daemons may attempt to acquire
// the same lease simultaneously, and the verify step detects whether
// we won.
func (w *haWatchdog) attemptAcquisition(ctx context.Context, serviceLocalpart string, definition *schema.FleetServiceContent) error {
	// Determine delay range based on preference. Preferred machines
	// get shorter delays (1-3 × baseDelay), others get longer delays
	// (4-10 × baseDelay). When baseDelay is 0 (integration tests),
	// all delays are 0 and acquisition is instantaneous.
	var delay time.Duration
	if w.baseDelay > 0 {
		minDelay := 4 * w.baseDelay
		maxDelay := 10 * w.baseDelay
		for _, preferred := range definition.Placement.PreferredMachines {
			if preferred == w.daemon.machine.Localpart() {
				minDelay = 1 * w.baseDelay
				maxDelay = 3 * w.baseDelay
				break
			}
		}

		delayRange := maxDelay - minDelay
		//nolint:gosec // The random delay is for jitter, not security.
		delay = minDelay + time.Duration(rand.Int63n(int64(delayRange)))
	}

	if delay > 0 {
		w.logger.Info("waiting before lease acquisition attempt",
			"service", serviceLocalpart,
			"delay", delay,
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.clock.After(delay):
		}
	}

	// Read the current lease state from Matrix.
	currentLease, err := w.readLease(ctx, serviceLocalpart)
	if err != nil {
		return fmt.Errorf("reading current lease: %w", err)
	}

	// If the lease was acquired by another machine while we waited,
	// check whether the new holder is healthy.
	if currentLease != nil && currentLease.Holder != "" {
		expiresAt, parseErr := time.Parse(time.RFC3339, currentLease.ExpiresAt)
		if parseErr == nil && !w.clock.Now().After(expiresAt) {
			// Another daemon already acquired a valid lease. Back off.
			w.logger.Info("lease already acquired by another machine",
				"service", serviceLocalpart,
				"holder", currentLease.Holder,
			)
			return nil
		}
	}

	// Write our lease claim.
	now := w.clock.Now()
	ttl := w.leaseTTL()
	newLease := schema.HALeaseContent{
		Holder:     w.daemon.machine.Localpart(),
		Service:    serviceLocalpart,
		AcquiredAt: now.UTC().Format(time.RFC3339),
		ExpiresAt:  now.Add(ttl).UTC().Format(time.RFC3339),
	}

	if _, err := w.daemon.session.SendStateEvent(ctx, w.daemon.fleetRoomID,
		schema.EventTypeHALease, serviceLocalpart, newLease); err != nil {
		return fmt.Errorf("writing lease claim: %w", err)
	}

	w.logger.Info("wrote lease claim, verifying",
		"service", serviceLocalpart)

	// Verification window: wait then read back. The delay gives Matrix
	// time to propagate the write before we check if we won.
	verificationDelay := 2 * w.baseDelay
	if verificationDelay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.clock.After(verificationDelay):
		}
	}

	// Read the lease back to see if we won.
	verifiedLease, err := w.readLease(ctx, serviceLocalpart)
	if err != nil {
		return fmt.Errorf("reading lease for verification: %w", err)
	}

	if verifiedLease == nil || verifiedLease.Holder != w.daemon.machine.Localpart() {
		w.logger.Info("lost HA lease race",
			"service", serviceLocalpart,
			"winner", leaseHolder(verifiedLease),
		)
		return nil
	}

	// We won the lease.
	w.logger.Info("acquired HA lease",
		"service", serviceLocalpart,
		"ttl", ttl,
	)

	w.startHosting(ctx, serviceLocalpart, definition, ttl)
	return nil
}

// readLease fetches the current HALeaseContent for a service from Matrix.
// Returns nil (not an error) if no lease event exists.
func (w *haWatchdog) readLease(ctx context.Context, serviceLocalpart string) (*schema.HALeaseContent, error) {
	raw, err := w.daemon.session.GetStateEvent(ctx, w.daemon.fleetRoomID,
		schema.EventTypeHALease, serviceLocalpart)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return nil, nil
		}
		return nil, err
	}

	var lease schema.HALeaseContent
	if err := json.Unmarshal(raw, &lease); err != nil {
		return nil, fmt.Errorf("parsing lease: %w", err)
	}
	return &lease, nil
}

// startHosting begins hosting a service after winning the lease. It writes
// a PrincipalAssignment to the daemon's own config room and starts a
// renewal goroutine.
func (w *haWatchdog) startHosting(ctx context.Context, serviceLocalpart string, definition *schema.FleetServiceContent, ttl time.Duration) {
	// Write PrincipalAssignment to our config room so the reconcile
	// loop picks it up and starts the sandbox.
	payload, err := fleetPayloadToMap(definition.Payload)
	if err != nil {
		w.logger.Error("cannot host service with malformed payload",
			"service", serviceLocalpart, "error", err)
		return
	}

	entity, err := ref.NewEntityFromAccountLocalpart(w.daemon.fleet, serviceLocalpart)
	if err != nil {
		w.logger.Error("cannot parse service localpart for HA assignment",
			"service", serviceLocalpart, "error", err)
		return
	}

	assignment := schema.PrincipalAssignment{
		Principal:         entity,
		Template:          definition.Template,
		AutoStart:         true,
		Payload:           payload,
		MatrixPolicy:      definition.MatrixPolicy,
		ServiceVisibility: definition.ServiceVisibility,
		Authorization:     definition.Authorization,
	}

	w.writePrincipalAssignment(ctx, serviceLocalpart, assignment)

	// Start renewal goroutine.
	renewCtx, renewCancel := context.WithCancel(ctx)

	w.mu.Lock()
	// Cancel any existing renewal for this service (defensive).
	if existingCancel, exists := w.heldLeases[serviceLocalpart]; exists {
		existingCancel()
	}
	w.heldLeases[serviceLocalpart] = renewCancel
	w.mu.Unlock()

	go w.renewLease(renewCtx, serviceLocalpart, ttl)
}

// writePrincipalAssignment reads the current MachineConfig from the
// config room, adds or updates the PrincipalAssignment for the given
// service, and writes the updated config back.
func (w *haWatchdog) writePrincipalAssignment(ctx context.Context, serviceLocalpart string, assignment schema.PrincipalAssignment) {
	raw, err := w.daemon.session.GetStateEvent(ctx, w.daemon.configRoomID,
		schema.EventTypeMachineConfig, w.daemon.machine.Localpart())
	if err != nil && !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		w.logger.Error("reading machine config for HA assignment",
			"service", serviceLocalpart, "error", err)
		return
	}

	var config schema.MachineConfig
	if raw != nil {
		if err := json.Unmarshal(raw, &config); err != nil {
			w.logger.Error("parsing machine config for HA assignment",
				"service", serviceLocalpart, "error", err)
			return
		}
	}

	// Check if the assignment already exists.
	found := false
	for index := range config.Principals {
		if config.Principals[index].Principal.AccountLocalpart() == serviceLocalpart {
			config.Principals[index] = assignment
			found = true
			break
		}
	}
	if !found {
		config.Principals = append(config.Principals, assignment)
	}

	if _, err := w.daemon.session.SendStateEvent(ctx, w.daemon.configRoomID,
		schema.EventTypeMachineConfig, w.daemon.machine.Localpart(), config); err != nil {
		w.logger.Error("writing machine config for HA assignment",
			"service", serviceLocalpart, "error", err)
	}
}

// renewLease periodically renews an HA lease at ttl/3 intervals.
// Runs until the context is cancelled (shutdown or lease release).
func (w *haWatchdog) renewLease(ctx context.Context, serviceLocalpart string, ttl time.Duration) {
	renewalInterval := ttl / 3
	ticker := w.clock.NewTicker(renewalInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := w.clock.Now()
			lease := schema.HALeaseContent{
				Holder:     w.daemon.machine.Localpart(),
				Service:    serviceLocalpart,
				AcquiredAt: now.UTC().Format(time.RFC3339),
				ExpiresAt:  now.Add(ttl).UTC().Format(time.RFC3339),
			}

			if _, err := w.daemon.session.SendStateEvent(ctx, w.daemon.fleetRoomID,
				schema.EventTypeHALease, serviceLocalpart, lease); err != nil {
				w.logger.Error("renewing HA lease",
					"service", serviceLocalpart, "error", err)
				// Don't return on transient errors — the next tick will
				// retry. The lease has a TTL buffer.
			} else {
				w.logger.Debug("renewed HA lease",
					"service", serviceLocalpart,
					"expires_at", lease.ExpiresAt,
				)
			}
		}
	}
}

// releaseLease releases a held lease by writing an expired lease event
// so another daemon can take over quickly. Called on daemon shutdown.
func (w *haWatchdog) releaseLease(ctx context.Context, serviceLocalpart string) {
	w.mu.Lock()
	cancel, held := w.heldLeases[serviceLocalpart]
	if held {
		cancel()
		delete(w.heldLeases, serviceLocalpart)
	}
	w.mu.Unlock()

	if !held {
		return
	}

	// Write an expired lease so other daemons see it immediately.
	now := w.clock.Now()
	lease := schema.HALeaseContent{
		Holder:     w.daemon.machine.Localpart(),
		Service:    serviceLocalpart,
		AcquiredAt: now.UTC().Format(time.RFC3339),
		ExpiresAt:  now.Add(-1 * time.Second).UTC().Format(time.RFC3339),
	}

	if _, err := w.daemon.session.SendStateEvent(ctx, w.daemon.fleetRoomID,
		schema.EventTypeHALease, serviceLocalpart, lease); err != nil {
		w.logger.Error("releasing HA lease",
			"service", serviceLocalpart, "error", err)
	} else {
		w.logger.Info("released HA lease", "service", serviceLocalpart)
	}
}

// releaseAllLeases releases all held leases. Called on daemon shutdown.
func (w *haWatchdog) releaseAllLeases(ctx context.Context) {
	w.mu.Lock()
	serviceLocalparts := make([]string, 0, len(w.heldLeases))
	for localpart := range w.heldLeases {
		serviceLocalparts = append(serviceLocalparts, localpart)
	}
	w.mu.Unlock()

	for _, localpart := range serviceLocalparts {
		w.releaseLease(ctx, localpart)
	}
}

// leaseTTL returns the HA lease duration. The TTL is 3x the daemon's
// status interval, providing enough margin for one missed heartbeat
// while keeping failover responsive.
func (w *haWatchdog) leaseTTL() time.Duration {
	return 3 * w.daemon.statusInterval
}

// leaseHolder returns the holder of a lease, or "<none>" if the lease
// is nil or has no holder.
func leaseHolder(lease *schema.HALeaseContent) string {
	if lease == nil || lease.Holder == "" {
		return "<none>"
	}
	return lease.Holder
}

// fleetPayloadToMap converts a json.RawMessage payload from a
// FleetServiceContent to the map[string]any used by PrincipalAssignment.
// Returns nil if the input is nil or empty.
func fleetPayloadToMap(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parsing fleet service payload: %w", err)
	}
	return result, nil
}
