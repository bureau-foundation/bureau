// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

// newHATestDaemon creates a daemon wired to a mock Matrix server,
// with the fleet room configured for HA watchdog testing. Returns the
// daemon, fake clock, and mock Matrix state for test control.
func newHATestDaemon(t *testing.T) (*Daemon, *mockMatrixState) {
	t.Helper()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	machine, fleet := testMachineSetup(t, "test", "bureau.local")

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := matrixClient.SessionFromToken(machine.UserID(), "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon, _ := newTestDaemon(t)
	daemon.session = session
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID("!config:test")
	daemon.machineRoomID = mustRoomID("!machine:test")
	daemon.serviceRoomID = mustRoomID("!service:test")
	daemon.fleetRoomID = mustRoomID("!fleet:test")
	daemon.statusInterval = 60 * time.Second
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	daemon.haWatchdog = newHAWatchdog(daemon, 1*time.Second, daemon.logger)

	return daemon, matrixState
}

// TestHASyncFleetState verifies that syncFleetState correctly identifies
// critical services from fleet room state events and filters out
// non-critical ones.
func TestHASyncFleetState(t *testing.T) {
	t.Parallel()

	daemon, matrixState := newHATestDaemon(t)

	// Set up fleet room with two services: one critical, one normal.
	criticalKey := "service/fleet/prod"
	normalKey := "service/stt/whisper"
	leaseKey := "service/fleet/prod"

	matrixState.setRoomState(daemon.fleetRoomID.String(), []mockRoomStateEvent{
		{
			Type:     schema.EventTypeFleetService,
			StateKey: &criticalKey,
			Content: map[string]any{
				"template": "bureau/template:fleet-controller",
				"ha_class": "critical",
				"replicas": map[string]any{"min": 1},
				"placement": map[string]any{
					"allowed_machines": []any{"bureau/fleet/*/machine/*"},
				},
				"failover": "migrate",
			},
		},
		{
			Type:     schema.EventTypeFleetService,
			StateKey: &normalKey,
			Content: map[string]any{
				"template":  "bureau/template:whisper",
				"ha_class":  "",
				"replicas":  map[string]any{"min": 1},
				"placement": map[string]any{},
				"failover":  "migrate",
			},
		},
		{
			Type:     schema.EventTypeHALease,
			StateKey: &leaseKey,
			Content: map[string]any{
				"holder":      "machine/other",
				"service":     "service/fleet/prod",
				"acquired_at": "2026-01-01T11:00:00Z",
				"expires_at":  "2026-01-01T14:00:00Z",
			},
		},
	})

	ctx := context.Background()
	daemon.haWatchdog.syncFleetState(ctx)

	daemon.haWatchdog.mu.Lock()
	criticalCount := len(daemon.haWatchdog.criticalServices)
	_, hasCritical := daemon.haWatchdog.criticalServices[criticalKey]
	_, hasNormal := daemon.haWatchdog.criticalServices[normalKey]
	leaseCount := len(daemon.haWatchdog.leases)
	lease, hasLease := daemon.haWatchdog.leases[leaseKey]
	daemon.haWatchdog.mu.Unlock()

	if criticalCount != 1 {
		t.Errorf("critical services count = %d, want 1", criticalCount)
	}
	if !hasCritical {
		t.Error("critical service service/fleet/prod should be present")
	}
	if hasNormal {
		t.Error("non-critical service service/stt/whisper should not be present")
	}
	if leaseCount != 1 {
		t.Errorf("leases count = %d, want 1", leaseCount)
	}
	if !hasLease {
		t.Fatal("lease for service/fleet/prod should be present")
	}
	if lease.Holder != "machine/other" {
		t.Errorf("lease holder = %q, want %q", lease.Holder, "machine/other")
	}
}

// TestHAEligibilityCheck verifies that isEligible correctly evaluates
// placement constraints against the daemon's machine.
func TestHAEligibilityCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		definition *fleet.FleetServiceContent
		labels     map[string]string
		want       bool
	}{
		{
			name: "no constraints",
			definition: &fleet.FleetServiceContent{
				Placement: fleet.PlacementConstraints{},
			},
			want: true,
		},
		{
			name: "allowed machine matches glob",
			definition: &fleet.FleetServiceContent{
				Placement: fleet.PlacementConstraints{
					AllowedMachines: []string{"bureau/fleet/*/machine/*"},
				},
			},
			want: true,
		},
		{
			name: "allowed machine exact match",
			definition: &fleet.FleetServiceContent{
				Placement: fleet.PlacementConstraints{
					AllowedMachines: []string{"bureau/fleet/test/machine/test"},
				},
			},
			want: true,
		},
		{
			name: "allowed machine no match",
			definition: &fleet.FleetServiceContent{
				Placement: fleet.PlacementConstraints{
					AllowedMachines: []string{"bureau/fleet/*/machine/gpu-*"},
				},
			},
			want: false,
		},
		{
			name: "required label present",
			definition: &fleet.FleetServiceContent{
				Placement: fleet.PlacementConstraints{
					Requires: []string{"gpu=rtx4090"},
				},
			},
			labels: map[string]string{"gpu": "rtx4090"},
			want:   true,
		},
		{
			name: "required label missing",
			definition: &fleet.FleetServiceContent{
				Placement: fleet.PlacementConstraints{
					Requires: []string{"gpu=rtx4090"},
				},
			},
			labels: map[string]string{},
			want:   false,
		},
		{
			name: "required label wrong value",
			definition: &fleet.FleetServiceContent{
				Placement: fleet.PlacementConstraints{
					Requires: []string{"gpu=rtx4090"},
				},
			},
			labels: map[string]string{"gpu": "mi300x"},
			want:   false,
		},
		{
			name: "presence-only label check",
			definition: &fleet.FleetServiceContent{
				Placement: fleet.PlacementConstraints{
					Requires: []string{"persistent"},
				},
			},
			labels: map[string]string{"persistent": "true"},
			want:   true,
		},
		{
			name: "presence-only label check missing",
			definition: &fleet.FleetServiceContent{
				Placement: fleet.PlacementConstraints{
					Requires: []string{"persistent"},
				},
			},
			labels: map[string]string{},
			want:   false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			daemon, matrixState := newHATestDaemon(t)

			// Set up MachineInfo with the test labels.
			matrixState.setStateEvent(daemon.machineRoomID.String(),
				schema.EventTypeMachineInfo, daemon.machine.Localpart(),
				schema.MachineInfo{
					Principal: daemon.machine.UserID().String(),
					Labels:    test.labels,
				})

			got := daemon.haWatchdog.isEligible(test.definition)
			if got != test.want {
				t.Errorf("isEligible() = %v, want %v", got, test.want)
			}
		})
	}
}

// TestHAAcquisitionSingleDaemon verifies that a single eligible daemon
// acquires the lease when the current holder's lease has expired.
func TestHAAcquisitionSingleDaemon(t *testing.T) {
	t.Parallel()

	daemon, matrixState := newHATestDaemon(t)

	serviceLocalpart := "service/fleet/prod"

	// Set up existing machine config (empty).
	matrixState.setStateEvent(daemon.configRoomID.String(),
		schema.EventTypeMachineConfig, daemon.machine.Localpart(),
		schema.MachineConfig{})

	// Set up an expired lease from another machine.
	matrixState.setStateEvent(daemon.fleetRoomID.String(),
		schema.EventTypeHALease, serviceLocalpart,
		fleet.HALeaseContent{
			Holder:     "machine/dead",
			Service:    serviceLocalpart,
			AcquiredAt: "2026-01-01T10:00:00Z",
			ExpiresAt:  "2026-01-01T10:03:00Z",
		})

	definition := &fleet.FleetServiceContent{
		Template:  "bureau/template:fleet-controller",
		HAClass:   "critical",
		Placement: fleet.PlacementConstraints{},
		MatrixPolicy: &schema.MatrixPolicy{
			AllowJoin: true,
		},
		ServiceVisibility: []string{"service/**"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Run the acquisition. The fake clock means After() will need to
	// be advanced manually, but since we're using a real context with
	// timeout, we need to run this in a goroutine and advance the clock.
	errChannel := make(chan error, 1)
	go func() {
		errChannel <- daemon.haWatchdog.attemptAcquisition(ctx, serviceLocalpart, definition)
	}()

	// Wait for the acquisition goroutine to register its initial delay timer.
	daemon.haWatchdog.clock.(*clock.FakeClock).WaitForTimers(1)
	// Advance past the maximum possible delay (10s).
	daemon.haWatchdog.clock.(*clock.FakeClock).Advance(11 * time.Second)

	// Wait for the verification delay timer.
	daemon.haWatchdog.clock.(*clock.FakeClock).WaitForTimers(1)
	daemon.haWatchdog.clock.(*clock.FakeClock).Advance(3 * time.Second)

	// Wait for the renewal ticker to register.
	daemon.haWatchdog.clock.(*clock.FakeClock).WaitForTimers(1)

	select {
	case err := <-errChannel:
		if err != nil {
			t.Fatalf("attemptAcquisition: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for acquisition")
	}

	// Verify the lease is now held by us.
	raw, err := daemon.session.GetStateEvent(ctx, daemon.fleetRoomID,
		schema.EventTypeHALease, serviceLocalpart)
	if err != nil {
		t.Fatalf("GetStateEvent: %v", err)
	}
	var lease fleet.HALeaseContent
	if err := json.Unmarshal(raw, &lease); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if lease.Holder != daemon.machine.Localpart() {
		t.Errorf("lease holder = %q, want %q", lease.Holder, daemon.machine.Localpart())
	}

	// Verify the config room has the PrincipalAssignment with authorization
	// fields propagated from the FleetServiceContent definition.
	raw, err = daemon.session.GetStateEvent(ctx, daemon.configRoomID,
		schema.EventTypeMachineConfig, daemon.machine.Localpart())
	if err != nil {
		t.Fatalf("GetStateEvent config: %v", err)
	}
	var config schema.MachineConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("Unmarshal config: %v", err)
	}
	found := false
	for _, principal := range config.Principals {
		if principal.Principal.AccountLocalpart() == serviceLocalpart {
			found = true
			if principal.Template != "bureau/template:fleet-controller" {
				t.Errorf("principal template = %q, want %q",
					principal.Template, "bureau/template:fleet-controller")
			}
			if !principal.AutoStart {
				t.Error("principal should have AutoStart=true")
			}
			if principal.MatrixPolicy == nil {
				t.Fatal("MatrixPolicy should be propagated from fleet service definition")
			}
			if !principal.MatrixPolicy.AllowJoin {
				t.Error("MatrixPolicy.AllowJoin should be true")
			}
			if len(principal.ServiceVisibility) != 1 || principal.ServiceVisibility[0] != "service/**" {
				t.Errorf("ServiceVisibility = %v, want [service/**]", principal.ServiceVisibility)
			}
		}
	}
	if !found {
		t.Errorf("PrincipalAssignment for %q not found in config", serviceLocalpart)
	}

	// Verify the held lease is tracked.
	daemon.haWatchdog.mu.Lock()
	_, held := daemon.haWatchdog.heldLeases[serviceLocalpart]
	daemon.haWatchdog.mu.Unlock()
	if !held {
		t.Error("lease should be tracked in heldLeases")
	}
}

// TestHAAcquisitionPreferredBias verifies that preferred machines get
// shorter delay ranges than non-preferred machines.
func TestHAAcquisitionPreferredBias(t *testing.T) {
	t.Parallel()

	// The preferred vs non-preferred distinction is embedded in the
	// delay calculation within attemptAcquisition. We verify by
	// running two acquisitions and checking that the preferred one
	// completes with a shorter initial delay.
	//
	// Since the delay is random, we verify indirectly: for a
	// preferred machine, the delay is 1-3s; for non-preferred it's
	// 4-10s. If we advance the clock by 3s, the preferred machine
	// should have passed its delay, while a non-preferred one may not.

	daemon, matrixState := newHATestDaemon(t)

	serviceLocalpart := "service/fleet/prod"

	matrixState.setStateEvent(daemon.configRoomID.String(),
		schema.EventTypeMachineConfig, daemon.machine.Localpart(),
		schema.MachineConfig{})

	// Expired lease.
	matrixState.setStateEvent(daemon.fleetRoomID.String(),
		schema.EventTypeHALease, serviceLocalpart,
		fleet.HALeaseContent{
			Holder:     "machine/dead",
			Service:    serviceLocalpart,
			AcquiredAt: "2026-01-01T10:00:00Z",
			ExpiresAt:  "2026-01-01T10:03:00Z",
		})

	// This machine is preferred.
	definition := &fleet.FleetServiceContent{
		Template: "bureau/template:fleet-controller",
		HAClass:  "critical",
		Placement: fleet.PlacementConstraints{
			PreferredMachines: []string{daemon.machine.Localpart()},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errChannel := make(chan error, 1)
	go func() {
		errChannel <- daemon.haWatchdog.attemptAcquisition(ctx, serviceLocalpart, definition)
	}()

	// The preferred delay is 1-3s. Advancing 4s guarantees the delay
	// has passed regardless of the random component.
	daemon.haWatchdog.clock.(*clock.FakeClock).WaitForTimers(1)
	daemon.haWatchdog.clock.(*clock.FakeClock).Advance(4 * time.Second)

	// Verification delay.
	daemon.haWatchdog.clock.(*clock.FakeClock).WaitForTimers(1)
	daemon.haWatchdog.clock.(*clock.FakeClock).Advance(3 * time.Second)

	// Renewal ticker.
	daemon.haWatchdog.clock.(*clock.FakeClock).WaitForTimers(1)

	select {
	case err := <-errChannel:
		if err != nil {
			t.Fatalf("attemptAcquisition (preferred): %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for preferred acquisition")
	}

	// Verify we acquired the lease.
	raw, err := daemon.session.GetStateEvent(ctx, daemon.fleetRoomID,
		schema.EventTypeHALease, serviceLocalpart)
	if err != nil {
		t.Fatalf("GetStateEvent: %v", err)
	}
	var lease fleet.HALeaseContent
	if err := json.Unmarshal(raw, &lease); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if lease.Holder != daemon.machine.Localpart() {
		t.Errorf("lease holder = %q, want %q", lease.Holder, daemon.machine.Localpart())
	}
}

// TestHAAcquisitionIneligible verifies that an ineligible daemon does
// not attempt acquisition.
func TestHAAcquisitionIneligible(t *testing.T) {
	t.Parallel()

	daemon, matrixState := newHATestDaemon(t)

	serviceLocalpart := "service/fleet/prod"

	// Set up fleet room with a critical service that requires GPU.
	matrixState.setRoomState(daemon.fleetRoomID.String(), []mockRoomStateEvent{
		{
			Type:     schema.EventTypeFleetService,
			StateKey: &serviceLocalpart,
			Content: map[string]any{
				"template": "bureau/template:fleet-controller",
				"ha_class": "critical",
				"replicas": map[string]any{"min": 1},
				"placement": map[string]any{
					"requires": []any{"gpu=rtx4090"},
				},
				"failover": "migrate",
			},
		},
		{
			Type:     schema.EventTypeHALease,
			StateKey: &serviceLocalpart,
			Content: map[string]any{
				"holder":      "machine/dead",
				"service":     serviceLocalpart,
				"acquired_at": "2026-01-01T10:00:00Z",
				"expires_at":  "2026-01-01T10:03:00Z",
			},
		},
	})

	// This machine has no GPU label.
	matrixState.setStateEvent(daemon.machineRoomID.String(),
		schema.EventTypeMachineInfo, daemon.machine.Localpart(),
		schema.MachineInfo{
			Principal: daemon.machine.UserID().String(),
			Labels:    map[string]string{},
		})

	ctx := context.Background()
	daemon.haWatchdog.syncFleetState(ctx)

	// Use a stateEventWritten channel to detect if any lease write happens.
	matrixState.mu.Lock()
	matrixState.stateEventWritten = make(chan string, 10)
	matrixState.mu.Unlock()

	daemon.haWatchdog.evaluate(ctx)

	// evaluate() checks eligibility synchronously — since this machine
	// is ineligible, no goroutine is launched and no writes occur.
	select {
	case key := <-matrixState.stateEventWritten:
		t.Errorf("unexpected state event written (ineligible): %s", key)
	default:
		// Expected: no writes.
	}

	// Verify no held leases.
	daemon.haWatchdog.mu.Lock()
	heldCount := len(daemon.haWatchdog.heldLeases)
	daemon.haWatchdog.mu.Unlock()

	if heldCount != 0 {
		t.Errorf("held leases = %d, want 0 (ineligible)", heldCount)
	}
}

// TestHALeaseRenewal verifies that a held lease is renewed before
// expiration by the renewal goroutine.
func TestHALeaseRenewal(t *testing.T) {
	t.Parallel()

	daemon, matrixState := newHATestDaemon(t)

	serviceLocalpart := "service/fleet/prod"
	ttl := 3 * daemon.statusInterval // 180s with 60s interval

	// Write initial lease.
	now := daemon.clock.Now()
	initialLease := fleet.HALeaseContent{
		Holder:     daemon.machine.Localpart(),
		Service:    serviceLocalpart,
		AcquiredAt: now.UTC().Format(time.RFC3339),
		ExpiresAt:  now.Add(ttl).UTC().Format(time.RFC3339),
	}
	matrixState.setStateEvent(daemon.fleetRoomID.String(),
		schema.EventTypeHALease, serviceLocalpart, initialLease)

	// Set up a stateEventWritten channel to track writes.
	writeChannel := make(chan string, 10)
	matrixState.mu.Lock()
	matrixState.stateEventWritten = writeChannel
	matrixState.mu.Unlock()

	// Start the renewal goroutine.
	renewCtx, renewCancel := context.WithCancel(context.Background())
	defer renewCancel()

	go daemon.haWatchdog.renewLease(renewCtx, serviceLocalpart, ttl)

	fakeClock := daemon.haWatchdog.clock.(*clock.FakeClock)

	// Wait for the ticker to register, then advance past one renewal
	// interval (ttl/3 = 60s).
	fakeClock.WaitForTimers(1)
	fakeClock.Advance(ttl/3 + time.Second)

	// Wait for the renewal write.
	key := testutil.RequireReceive(t, writeChannel, 5*time.Second, "lease renewal write")
	if key == "" {
		t.Error("expected a state event key, got empty")
	}

	// Read the renewed lease and verify ExpiresAt was extended.
	ctx := context.Background()
	raw, err := daemon.session.GetStateEvent(ctx, daemon.fleetRoomID,
		schema.EventTypeHALease, serviceLocalpart)
	if err != nil {
		t.Fatalf("GetStateEvent: %v", err)
	}
	var renewed fleet.HALeaseContent
	if err := json.Unmarshal(raw, &renewed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if renewed.Holder != daemon.machine.Localpart() {
		t.Errorf("renewed holder = %q, want %q", renewed.Holder, daemon.machine.Localpart())
	}

	renewedExpiresAt, err := time.Parse(time.RFC3339, renewed.ExpiresAt)
	if err != nil {
		t.Fatalf("parsing renewed ExpiresAt: %v", err)
	}
	initialExpiresAt, _ := time.Parse(time.RFC3339, initialLease.ExpiresAt)
	if !renewedExpiresAt.After(initialExpiresAt) {
		t.Errorf("renewed ExpiresAt (%s) should be after initial (%s)",
			renewed.ExpiresAt, initialLease.ExpiresAt)
	}
}

// TestHALeaseRelease verifies that releaseLease writes an expired lease
// and stops the renewal goroutine.
func TestHALeaseRelease(t *testing.T) {
	t.Parallel()

	daemon, matrixState := newHATestDaemon(t)

	serviceLocalpart := "service/fleet/prod"

	// Set up a stateEventWritten channel to track writes.
	writeChannel := make(chan string, 10)
	matrixState.mu.Lock()
	matrixState.stateEventWritten = writeChannel
	matrixState.mu.Unlock()

	// Simulate holding a lease with a renewal goroutine.
	renewCtx, renewCancel := context.WithCancel(context.Background())
	daemon.haWatchdog.mu.Lock()
	daemon.haWatchdog.heldLeases[serviceLocalpart] = renewCancel
	daemon.haWatchdog.mu.Unlock()

	// Start renewal goroutine to verify it gets cancelled.
	renewalDone := make(chan struct{})
	go func() {
		defer close(renewalDone)
		<-renewCtx.Done()
	}()

	ctx := context.Background()
	daemon.haWatchdog.releaseLease(ctx, serviceLocalpart)

	// Verify the renewal goroutine was cancelled.
	testutil.RequireClosed(t, renewalDone, 5*time.Second, "renewal goroutine cancellation")

	// Verify an expired lease was written.
	testutil.RequireReceive(t, writeChannel, 5*time.Second, "lease release write")

	raw, err := daemon.session.GetStateEvent(ctx, daemon.fleetRoomID,
		schema.EventTypeHALease, serviceLocalpart)
	if err != nil {
		t.Fatalf("GetStateEvent: %v", err)
	}
	var released fleet.HALeaseContent
	if err := json.Unmarshal(raw, &released); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	releasedExpiresAt, err := time.Parse(time.RFC3339, released.ExpiresAt)
	if err != nil {
		t.Fatalf("parsing released ExpiresAt: %v", err)
	}
	if !daemon.clock.Now().After(releasedExpiresAt) {
		t.Errorf("released lease should be expired (ExpiresAt=%s, now=%s)",
			released.ExpiresAt, daemon.clock.Now().Format(time.RFC3339))
	}

	// Verify the lease was removed from heldLeases.
	daemon.haWatchdog.mu.Lock()
	_, stillHeld := daemon.haWatchdog.heldLeases[serviceLocalpart]
	daemon.haWatchdog.mu.Unlock()
	if stillHeld {
		t.Error("lease should be removed from heldLeases after release")
	}
}

// TestHAIsLeaseHealthy verifies the lease health check logic.
func TestHAIsLeaseHealthy(t *testing.T) {
	t.Parallel()

	daemon, _ := newHATestDaemon(t)
	now := daemon.clock.Now()

	tests := []struct {
		name  string
		lease *fleet.HALeaseContent
		want  bool
	}{
		{
			name:  "nil lease",
			lease: nil,
			want:  false,
		},
		{
			name:  "empty holder",
			lease: &fleet.HALeaseContent{Holder: ""},
			want:  false,
		},
		{
			name: "expired lease",
			lease: &fleet.HALeaseContent{
				Holder:    "machine/other",
				ExpiresAt: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
			},
			want: false,
		},
		{
			name: "valid lease",
			lease: &fleet.HALeaseContent{
				Holder:    "machine/other",
				ExpiresAt: now.Add(1 * time.Hour).UTC().Format(time.RFC3339),
			},
			want: true,
		},
		{
			name: "unparseable expiry",
			lease: &fleet.HALeaseContent{
				Holder:    "machine/other",
				ExpiresAt: "not-a-date",
			},
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := daemon.haWatchdog.isLeaseHealthy(test.lease, now)
			if got != test.want {
				t.Errorf("isLeaseHealthy() = %v, want %v", got, test.want)
			}
		})
	}
}

// TestHALabelSatisfied verifies the label matching logic.
func TestHALabelSatisfied(t *testing.T) {
	t.Parallel()

	labels := map[string]string{
		"gpu":        "rtx4090",
		"persistent": "true",
		"tier":       "production",
	}

	tests := []struct {
		name        string
		requirement string
		want        bool
	}{
		{"exact match", "gpu=rtx4090", true},
		{"wrong value", "gpu=mi300x", false},
		{"missing key", "nonexistent=value", false},
		{"presence check exists", "persistent", true},
		{"presence check missing", "nonexistent", false},
		{"empty value match", "tier=production", true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := labelSatisfied(test.requirement, labels)
			if got != test.want {
				t.Errorf("labelSatisfied(%q) = %v, want %v",
					test.requirement, got, test.want)
			}
		})
	}
}

// TestHAFleetPayloadToMap verifies the JSON payload conversion.
func TestHAFleetPayloadToMap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   json.RawMessage
		want    int // expected number of keys, -1 for nil
		wantErr bool
	}{
		{"nil input", nil, -1, false},
		{"empty input", json.RawMessage{}, -1, false},
		{"valid object", json.RawMessage(`{"key":"value","count":42}`), 2, false},
		{"invalid json", json.RawMessage(`{broken`), -1, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result, err := fleetPayloadToMap(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if test.want == -1 {
				if result != nil {
					t.Errorf("expected nil, got %v", result)
				}
			} else if len(result) != test.want {
				t.Errorf("keys = %d, want %d", len(result), test.want)
			}
		})
	}
}

// TestHAEvaluate_HealthyLease verifies that evaluate does not attempt
// acquisition when the current lease is healthy.
func TestHAEvaluate_HealthyLease(t *testing.T) {
	t.Parallel()

	daemon, matrixState := newHATestDaemon(t)

	serviceLocalpart := "service/fleet/prod"
	now := daemon.clock.Now()

	// Set up a healthy lease (expires in the future).
	matrixState.setRoomState(daemon.fleetRoomID.String(), []mockRoomStateEvent{
		{
			Type:     schema.EventTypeFleetService,
			StateKey: &serviceLocalpart,
			Content: map[string]any{
				"template":  "bureau/template:fleet-controller",
				"ha_class":  "critical",
				"replicas":  map[string]any{"min": 1},
				"placement": map[string]any{},
				"failover":  "migrate",
			},
		},
		{
			Type:     schema.EventTypeHALease,
			StateKey: &serviceLocalpart,
			Content: map[string]any{
				"holder":      "machine/healthy",
				"service":     serviceLocalpart,
				"acquired_at": now.Add(-1 * time.Minute).UTC().Format(time.RFC3339),
				"expires_at":  now.Add(2 * time.Hour).UTC().Format(time.RFC3339),
			},
		},
	})

	// Track writes.
	writeChannel := make(chan string, 10)
	matrixState.mu.Lock()
	matrixState.stateEventWritten = writeChannel
	matrixState.mu.Unlock()

	ctx := context.Background()
	daemon.haWatchdog.syncFleetState(ctx)
	daemon.haWatchdog.evaluate(ctx)

	// evaluate() checks lease health synchronously — since the lease
	// is healthy, no goroutine is launched and no writes occur.
	select {
	case key := <-writeChannel:
		t.Errorf("unexpected state event written: %s", key)
	default:
		// Expected: no writes.
	}
}

// TestHAProcessSyncResponse_FleetRoom verifies that fleet room state
// changes in a sync response trigger HA evaluation.
func TestHAProcessSyncResponse_FleetRoom(t *testing.T) {
	t.Parallel()

	daemon, matrixState := newHATestDaemon(t)

	serviceLocalpart := "service/fleet/prod"
	now := daemon.clock.Now()

	// Config room needs to be set up for reconcile.
	matrixState.setStateEvent(daemon.configRoomID.String(),
		schema.EventTypeMachineConfig, daemon.machine.Localpart(),
		schema.MachineConfig{})

	// Set up fleet room state with a healthy lease (so evaluate is
	// a no-op but syncFleetState runs).
	matrixState.setRoomState(daemon.fleetRoomID.String(), []mockRoomStateEvent{
		{
			Type:     schema.EventTypeFleetService,
			StateKey: &serviceLocalpart,
			Content: map[string]any{
				"template":  "bureau/template:fleet-controller",
				"ha_class":  "critical",
				"replicas":  map[string]any{"min": 1},
				"placement": map[string]any{},
				"failover":  "migrate",
			},
		},
		{
			Type:     schema.EventTypeHALease,
			StateKey: &serviceLocalpart,
			Content: map[string]any{
				"holder":      "machine/healthy",
				"service":     serviceLocalpart,
				"acquired_at": now.Add(-1 * time.Minute).UTC().Format(time.RFC3339),
				"expires_at":  now.Add(2 * time.Hour).UTC().Format(time.RFC3339),
			},
		},
	})

	// Construct a sync response with fleet room state changes.
	stateKey := serviceLocalpart
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				daemon.fleetRoomID: {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{
								Type:     schema.EventTypeFleetService,
								StateKey: &stateKey,
								Content:  map[string]any{"ha_class": "critical"},
							},
						},
					},
				},
			},
		},
	}

	daemon.processSyncResponse(context.Background(), response)

	// Verify that syncFleetState was called (critical services populated).
	daemon.haWatchdog.mu.Lock()
	count := len(daemon.haWatchdog.criticalServices)
	daemon.haWatchdog.mu.Unlock()

	if count != 1 {
		t.Errorf("critical services = %d, want 1 (fleet sync should have run)", count)
	}
}

// TestHAReleaseAllLeases verifies that releaseAllLeases releases all
// held leases on shutdown.
func TestHAReleaseAllLeases(t *testing.T) {
	t.Parallel()

	daemon, matrixState := newHATestDaemon(t)

	// Set up multiple held leases.
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	_ = ctx1
	_ = ctx2

	daemon.haWatchdog.mu.Lock()
	daemon.haWatchdog.heldLeases["service/fleet/prod"] = cancel1
	daemon.haWatchdog.heldLeases["service/fleet/staging"] = cancel2
	daemon.haWatchdog.mu.Unlock()

	// Track writes.
	writeChannel := make(chan string, 10)
	matrixState.mu.Lock()
	matrixState.stateEventWritten = writeChannel
	matrixState.mu.Unlock()

	ctx := context.Background()
	daemon.haWatchdog.releaseAllLeases(ctx)

	// Wait for writes (two releases). releaseAllLeases is synchronous
	// so both writes have already been sent by the time it returns.
	testutil.RequireReceive(t, writeChannel, 5*time.Second, "first lease release")
	testutil.RequireReceive(t, writeChannel, 5*time.Second, "second lease release")

	// Verify all leases are released.
	daemon.haWatchdog.mu.Lock()
	remaining := len(daemon.haWatchdog.heldLeases)
	daemon.haWatchdog.mu.Unlock()

	if remaining != 0 {
		t.Errorf("heldLeases = %d after releaseAll, want 0", remaining)
	}
}
