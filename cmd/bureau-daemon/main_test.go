// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
	"github.com/bureau-foundation/bureau/transport"
)

func TestRoomAliasLocalpart(t *testing.T) {
	tests := []struct {
		name      string
		fullAlias string
		expected  string
	}{
		{
			name:      "simple alias",
			fullAlias: "#bureau/machine:bureau.local",
			expected:  "bureau/machine",
		},
		{
			name:      "fleet-scoped alias",
			fullAlias: "#bureau/fleet/prod/machine/workstation:bureau.local",
			expected:  "bureau/fleet/prod/machine/workstation",
		},
		{
			name:      "different server",
			fullAlias: "#test:example.org",
			expected:  "test",
		},
		{
			name:      "no # prefix",
			fullAlias: "bureau/machine:bureau.local",
			expected:  "bureau/machine",
		},
		{
			name:      "no server suffix",
			fullAlias: "#bureau/machine",
			expected:  "bureau/machine",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := principal.RoomAliasLocalpart(test.fullAlias)
			if result != test.expected {
				t.Errorf("RoomAliasLocalpart(%q) = %q, want %q",
					test.fullAlias, result, test.expected)
			}
		})
	}
}

func TestConfigRoomPowerLevels(t *testing.T) {
	adminUserID := ref.MustParseUserID("@bureau-admin:bureau.local")
	machine, _ := testMachineSetup(t, "workstation", "bureau.local")
	machineUserID := machine.UserID()
	levels := schema.ConfigRoomPowerLevels(adminUserID, machineUserID)

	// Admin should have power level 100.
	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	adminLevel, ok := users[adminUserID.String()]
	if !ok {
		t.Fatalf("admin %q not in users map", adminUserID)
	}
	if adminLevel != 100 {
		t.Errorf("admin power level = %v, want 100", adminLevel)
	}

	// Machine should have power level 50 (operational writes: MachineConfig
	// for HA hosting, layout publishes, invites). PL 50 cannot modify
	// credentials, power levels, or room metadata (all PL 100).
	machineLevel, ok := users[machineUserID.String()]
	if !ok {
		t.Fatalf("machine %q not in users map", machineUserID)
	}
	if machineLevel != 50 {
		t.Errorf("machine power level = %v, want 50", machineLevel)
	}

	// Default user power level should be 0.
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	// MachineConfig requires PL 50 (machine and fleet controllers can write placements).
	// Credentials require PL 100 (admin only).
	events, ok := levels["events"].(map[ref.EventType]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}
	if events[schema.EventTypeMachineConfig] != 50 {
		t.Errorf("%s power level = %v, want 50", schema.EventTypeMachineConfig, events[schema.EventTypeMachineConfig])
	}
	if events[schema.EventTypeCredentials] != 100 {
		t.Errorf("%s power level = %v, want 100", schema.EventTypeCredentials, events[schema.EventTypeCredentials])
	}

	// Default event power level should be 100 (admin-only room).
	if levels["events_default"] != 100 {
		t.Errorf("events_default = %v, want 100", levels["events_default"])
	}

	// Invite should require PL 50 (machine can invite during creation).
	if levels["invite"] != 50 {
		t.Errorf("invite = %v, want 50", levels["invite"])
	}
}

func TestUptimeSeconds(t *testing.T) {
	// uptimeSeconds should return a positive value on Linux.
	uptime := uptimeSeconds()
	if uptime <= 0 {
		t.Errorf("uptimeSeconds() = %d, want > 0", uptime)
	}
}

func TestPublishStatus_SandboxCount(t *testing.T) {
	// Verify that the running map count is correctly calculated.
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "bureau.local")
	daemon.running[testEntity(t, daemon.fleet, "iree/amdgpu/pm")] = true
	daemon.running[testEntity(t, daemon.fleet, "service/stt/whisper")] = true
	daemon.running[testEntity(t, daemon.fleet, "service/tts/piper")] = true
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Count running principals the same way collectStatus does.
	runningCount := 0
	for range daemon.running {
		runningCount++
	}
	if runningCount != 3 {
		t.Errorf("running count = %d, want 3", runningCount)
	}
}

func TestStatusSignificantlyChanged(t *testing.T) {
	baseline := &schema.MachineStatus{
		Principal:        "@bureau/fleet/test/machine/box:test.local",
		CPUPercent:       50,
		MemoryUsedMB:     8000,
		TransportAddress: "10.0.0.1:7891",
		LastActivityAt:   "2026-01-01T12:00:00Z",
		Sandboxes:        schema.SandboxCounts{Running: 3},
		GPUStats: []schema.GPUStatus{
			{
				PCISlot:                 "0000:c3:00.0",
				UtilizationPercent:      60,
				VRAMUsedBytes:           20_000_000_000,
				TemperatureMillidegrees: 65000,
			},
		},
		Principals: map[string]schema.PrincipalResourceUsage{
			"agent/coder": {CPUPercent: 40, MemoryMB: 500, Status: schema.PrincipalRunning},
		},
	}

	// copyBaseline returns a deep-enough copy for testing (slices and
	// maps are cloned so mutations don't alias the original).
	copyBaseline := func() schema.MachineStatus {
		status := *baseline
		status.GPUStats = make([]schema.GPUStatus, len(baseline.GPUStats))
		copy(status.GPUStats, baseline.GPUStats)
		status.Principals = make(map[string]schema.PrincipalResourceUsage, len(baseline.Principals))
		for k, v := range baseline.Principals {
			status.Principals[k] = v
		}
		return status
	}

	tests := []struct {
		name    string
		modify  func(*schema.MachineStatus)
		changed bool
	}{
		{
			name:    "nil previous always changed",
			changed: true, // tested separately below
		},
		{
			name:    "identical status not changed",
			modify:  func(s *schema.MachineStatus) {},
			changed: false,
		},
		{
			name:    "uptime change ignored",
			modify:  func(s *schema.MachineStatus) { s.UptimeSeconds += 60 },
			changed: false,
		},

		// CPU deadband (±5%).
		{
			name:    "CPU within deadband",
			modify:  func(s *schema.MachineStatus) { s.CPUPercent += cpuDeadbandPercent },
			changed: false,
		},
		{
			name:    "CPU exceeds deadband",
			modify:  func(s *schema.MachineStatus) { s.CPUPercent += cpuDeadbandPercent + 1 },
			changed: true,
		},
		{
			name:    "CPU exceeds deadband negative",
			modify:  func(s *schema.MachineStatus) { s.CPUPercent -= cpuDeadbandPercent + 1 },
			changed: true,
		},

		// Memory deadband (±100MB).
		{
			name:    "memory within deadband",
			modify:  func(s *schema.MachineStatus) { s.MemoryUsedMB += memoryDeadbandMB },
			changed: false,
		},
		{
			name:    "memory exceeds deadband",
			modify:  func(s *schema.MachineStatus) { s.MemoryUsedMB += memoryDeadbandMB + 1 },
			changed: true,
		},

		// Sandbox count — always meaningful.
		{
			name:    "sandbox count changed",
			modify:  func(s *schema.MachineStatus) { s.Sandboxes.Running++ },
			changed: true,
		},

		// Transport address — topology.
		{
			name:    "transport address changed",
			modify:  func(s *schema.MachineStatus) { s.TransportAddress = "10.0.0.2:7891" },
			changed: true,
		},

		// Activity timestamp.
		{
			name:    "activity timestamp changed",
			modify:  func(s *schema.MachineStatus) { s.LastActivityAt = "2026-01-01T12:05:00Z" },
			changed: true,
		},

		// GPU — utilization deadband.
		{
			name: "GPU utilization within deadband",
			modify: func(s *schema.MachineStatus) {
				s.GPUStats[0].UtilizationPercent += gpuUtilizationDeadbandPercent
			},
			changed: false,
		},
		{
			name: "GPU utilization exceeds deadband",
			modify: func(s *schema.MachineStatus) {
				s.GPUStats[0].UtilizationPercent += gpuUtilizationDeadbandPercent + 1
			},
			changed: true,
		},

		// GPU — temperature deadband (2°C = 2000 millidegrees).
		{
			name: "GPU temperature within deadband",
			modify: func(s *schema.MachineStatus) {
				s.GPUStats[0].TemperatureMillidegrees += gpuTemperatureDeadbandMilli
			},
			changed: false,
		},
		{
			name: "GPU temperature exceeds deadband",
			modify: func(s *schema.MachineStatus) {
				s.GPUStats[0].TemperatureMillidegrees += gpuTemperatureDeadbandMilli + 1
			},
			changed: true,
		},

		// GPU — VRAM deadband (50MB).
		{
			name: "GPU VRAM within deadband",
			modify: func(s *schema.MachineStatus) {
				s.GPUStats[0].VRAMUsedBytes += gpuVRAMDeadbandBytes
			},
			changed: false,
		},
		{
			name: "GPU VRAM exceeds deadband",
			modify: func(s *schema.MachineStatus) {
				s.GPUStats[0].VRAMUsedBytes += gpuVRAMDeadbandBytes + 1
			},
			changed: true,
		},

		// GPU count change.
		{
			name: "GPU added",
			modify: func(s *schema.MachineStatus) {
				s.GPUStats = append(s.GPUStats, schema.GPUStatus{PCISlot: "0000:d4:00.0"})
			},
			changed: true,
		},
		{
			name:    "GPU removed",
			modify:  func(s *schema.MachineStatus) { s.GPUStats = nil },
			changed: true,
		},

		// Principal status transition — always meaningful.
		{
			name: "principal status transition",
			modify: func(s *schema.MachineStatus) {
				usage := s.Principals["agent/coder"]
				usage.Status = schema.PrincipalIdle
				s.Principals["agent/coder"] = usage
			},
			changed: true,
		},

		// Principal CPU deadband (±3%).
		{
			name: "principal CPU within deadband",
			modify: func(s *schema.MachineStatus) {
				usage := s.Principals["agent/coder"]
				usage.CPUPercent += principalCPUDeadbandPercent
				s.Principals["agent/coder"] = usage
			},
			changed: false,
		},
		{
			name: "principal CPU exceeds deadband",
			modify: func(s *schema.MachineStatus) {
				usage := s.Principals["agent/coder"]
				usage.CPUPercent += principalCPUDeadbandPercent + 1
				s.Principals["agent/coder"] = usage
			},
			changed: true,
		},

		// Principal memory deadband (±50MB).
		{
			name: "principal memory within deadband",
			modify: func(s *schema.MachineStatus) {
				usage := s.Principals["agent/coder"]
				usage.MemoryMB += principalMemoryDeadbandMB
				s.Principals["agent/coder"] = usage
			},
			changed: false,
		},
		{
			name: "principal memory exceeds deadband",
			modify: func(s *schema.MachineStatus) {
				usage := s.Principals["agent/coder"]
				usage.MemoryMB += principalMemoryDeadbandMB + 1
				s.Principals["agent/coder"] = usage
			},
			changed: true,
		},

		// Principal added/removed.
		{
			name: "principal added",
			modify: func(s *schema.MachineStatus) {
				s.Principals["agent/reviewer"] = schema.PrincipalResourceUsage{Status: schema.PrincipalStarting}
			},
			changed: true,
		},
		{
			name: "principal removed",
			modify: func(s *schema.MachineStatus) {
				delete(s.Principals, "agent/coder")
			},
			changed: true,
		},
	}

	// Special case: nil previous always returns true.
	t.Run("nil previous always changed", func(t *testing.T) {
		current := copyBaseline()
		if !statusSignificantlyChanged(&current, nil) {
			t.Error("statusSignificantlyChanged(current, nil) = false, want true")
		}
	})

	for _, test := range tests {
		if test.modify == nil {
			continue // handled above
		}
		t.Run(test.name, func(t *testing.T) {
			current := copyBaseline()
			test.modify(&current)
			got := statusSignificantlyChanged(&current, baseline)
			if got != test.changed {
				t.Errorf("statusSignificantlyChanged() = %v, want %v", got, test.changed)
			}
		})
	}
}

func TestIntAbs(t *testing.T) {
	tests := []struct {
		input    int
		expected int
	}{
		{0, 0},
		{5, 5},
		{-5, 5},
		{-2147483648, 2147483648}, // min int32 magnitude (safe in int on 64-bit)
	}
	for _, test := range tests {
		if got := intAbs(test.input); got != test.expected {
			t.Errorf("intAbs(%d) = %d, want %d", test.input, got, test.expected)
		}
	}
}

func TestInt64Abs(t *testing.T) {
	tests := []struct {
		input    int64
		expected int64
	}{
		{0, 0},
		{100, 100},
		{-100, 100},
	}
	for _, test := range tests {
		if got := int64Abs(test.input); got != test.expected {
			t.Errorf("int64Abs(%d) = %d, want %d", test.input, got, test.expected)
		}
	}
}

// noopTransportListener is a minimal transport.Listener that returns an
// empty address. Used in status loop tests where transport is not under test.
type noopTransportListener struct{}

func (noopTransportListener) Serve(context.Context, http.Handler) error { return nil }
func (noopTransportListener) Address() string                           { return "" }
func (noopTransportListener) Close() error                              { return nil }

var _ transport.Listener = noopTransportListener{}

// fixedStatus is the deterministic MachineStatus returned by the
// collectStatusFunc override in newStatusTestDaemon. Defined at test
// scope so individual tests can reference it for assertions.
var fixedStatus = schema.MachineStatus{
	Principal:    "@bureau/fleet/test/machine/statustest:test.local",
	CPUPercent:   25,
	MemoryUsedMB: 8000,
	Sandboxes:    schema.SandboxCounts{Running: 0},
}

// newStatusTestDaemon creates a Daemon wired for status loop testing.
// The mock HTTP server counts PUT requests to state event and presence
// endpoints. Returns the daemon, the fake clock, and a counting session.
func newStatusTestDaemon(t *testing.T) (*Daemon, *clock.FakeClock, *countingSession) {
	t.Helper()
	daemon, fakeClock := newTestDaemon(t)
	daemon.statusInterval = 60 * time.Second
	daemon.maxIdleInterval = 5 * time.Minute
	daemon.machine, daemon.fleet = testMachineSetup(t, "statustest", "test.local")
	daemon.machineRoomID = mustRoomID("!machineroom:test.local")
	daemon.transportListener = noopTransportListener{}
	daemon.statusNotify = make(chan struct{}, 1)

	// Override collectStatus with a deterministic function that always
	// returns the same metrics. This decouples the adaptive loop tests
	// from real /proc readings that fluctuate on busy machines.
	daemon.collectStatusFunc = func(ctx context.Context) schema.MachineStatus {
		return fixedStatus
	}

	// Signal channel for synchronizing with the statusLoop goroutine.
	// Tests read from this to confirm an iteration (publish or skip)
	// has completed before asserting on counters.
	daemon.statusLoopIterationDone = make(chan struct{}, 10)

	cs := newCountingSession(t, daemon.machine)
	daemon.session = cs.session

	return daemon, fakeClock, cs
}

// countingSession wraps a mock Matrix HTTP server that counts state event
// PUTs and presence PUTs separately. Thread-safe for use from the status
// loop goroutine while the test goroutine reads counts.
type countingSession struct {
	session        *messaging.DirectSession
	stateEventPuts atomic.Int64
	presencePuts   atomic.Int64
	publishSignal  chan struct{} // receives a value on each state event PUT
}

func newCountingSession(t *testing.T, machine ref.Machine) *countingSession {
	t.Helper()
	cs := &countingSession{
		publishSignal: make(chan struct{}, 100),
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Count PUT state events (machine status publishes).
		if r.Method == "PUT" && isStateEventPath(r.URL.Path) {
			cs.stateEventPuts.Add(1)
			fmt.Fprintf(w, `{"event_id":"$evt%d"}`, cs.stateEventPuts.Load())
			select {
			case cs.publishSignal <- struct{}{}:
			default:
			}
			return
		}

		// Count PUT presence.
		if r.Method == "PUT" && isPresencePath(r.URL.Path) {
			cs.presencePuts.Add(1)
			fmt.Fprintf(w, `{}`)
			return
		}

		// Accept everything else (e.g., GET for sync filter setup).
		fmt.Fprintf(w, `{}`)
	}))
	t.Cleanup(server.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("creating counting session client: %v", err)
	}
	session, err := client.SessionFromToken(machine.UserID(), "test-token")
	if err != nil {
		t.Fatalf("creating counting session: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	cs.session = session
	return cs
}

// awaitPublishes blocks until the state event PUT count reaches at least
// the given target. Use this to synchronize with the statusLoop goroutine:
// after advancing the fake clock past a tick that should trigger a publish,
// awaitPublishes waits for the HTTP PUT to complete rather than racing
// against the goroutine. If the count never reaches target, the test
// binary's -timeout flag terminates the test.
func (cs *countingSession) awaitPublishes(target int64) {
	for cs.stateEventPuts.Load() < target {
		<-cs.publishSignal
	}
}

// awaitIteration waits for the statusLoop goroutine to complete one
// iteration (either a publish or a skip). Synchronizes the test with
// the loop goroutine after advancing the fake clock, ensuring the
// goroutine has processed the tick before the test asserts. If the
// goroutine never completes, the test binary's -timeout flag
// terminates the test.
func awaitIteration(t *testing.T, daemon *Daemon) {
	t.Helper()
	<-daemon.statusLoopIterationDone
}

func isStateEventPath(path string) bool {
	// Matrix state event PUTs match /_matrix/client/v3/rooms/{room}/state/{type}/{key}
	// We just check for "/state/" in the path since that's the only PUT
	// path the status loop uses.
	return len(path) > 0 && contains(path, "/state/")
}

func isPresencePath(path string) bool {
	return len(path) > 0 && contains(path, "/presence/")
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestStatusLoop_SkipsUnchangedPublish(t *testing.T) {
	daemon, fakeClock, cs := newStatusTestDaemon(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		daemon.statusLoop(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done // ensure statusLoop exits before t.Cleanup closes the session
	}()

	// Wait for the ticker to be registered and the startup publish.
	fakeClock.WaitForTimers(1)
	cs.awaitPublishes(1)

	// Advance just under the liveness window in one shot. FakeClock
	// fires one tick per interval, but the capacity-1 ticker channel
	// only delivers the first. The goroutine processes that single
	// tick with Now() already at epoch+240s. Since collectStatusFunc
	// returns a fixed status, statusSignificantlyChanged returns false,
	// and timeSincePublish (240s) < maxIdleInterval (300s), so the
	// publish is suppressed.
	fakeClock.Advance(daemon.maxIdleInterval - daemon.statusInterval)
	awaitIteration(t, daemon)

	if got := cs.stateEventPuts.Load(); got != 1 {
		t.Fatalf("before liveness: state event puts = %d, want 1 (suppression failed)", got)
	}

	// One more tick crosses the liveness window, proving the loop is
	// still alive and will publish when it should. If change detection
	// had incorrectly published on the intermediate tick, the count
	// would exceed 2 here.
	fakeClock.Advance(daemon.statusInterval)

	cs.awaitPublishes(2)
	if got := cs.stateEventPuts.Load(); got != 2 {
		t.Errorf("at liveness: state event puts = %d, want 2 (startup + liveness)", got)
	}
}

func TestStatusLoop_PublishesAtMaxIdleInterval(t *testing.T) {
	daemon, fakeClock, cs := newStatusTestDaemon(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		daemon.statusLoop(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	fakeClock.WaitForTimers(1)
	cs.awaitPublishes(1)

	// Advance to just before the liveness window in one shot. The
	// goroutine receives one tick (the rest overflow the capacity-1
	// channel) and sees timeSincePublish < maxIdleInterval → suppressed.
	fakeClock.Advance(daemon.maxIdleInterval - daemon.statusInterval)
	awaitIteration(t, daemon)

	if got := cs.stateEventPuts.Load(); got != 1 {
		t.Errorf("before liveness: state event puts = %d, want 1", got)
	}

	// One more tick reaches the liveness boundary. The goroutine sees
	// timeSincePublish >= maxIdleInterval and publishes.
	fakeClock.Advance(daemon.statusInterval)

	cs.awaitPublishes(2)
	if got := cs.stateEventPuts.Load(); got != 2 {
		t.Errorf("at liveness: state event puts = %d, want 2", got)
	}
}

func TestStatusLoop_ResetsOnNotify(t *testing.T) {
	daemon, fakeClock, cs := newStatusTestDaemon(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		daemon.statusLoop(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	fakeClock.WaitForTimers(1)
	cs.awaitPublishes(1)

	// Signal a sandbox change — statusLoop publishes immediately regardless
	// of whether the status has changed, because sandbox lifecycle events
	// are always operationally meaningful.
	daemon.notifyStatusChange()

	cs.awaitPublishes(2)
	if got := cs.stateEventPuts.Load(); got != 2 {
		t.Errorf("after notify: state event puts = %d, want 2", got)
	}

	// The notification publish resets lastPublishTime. Wait for the
	// notify handler iteration to complete (including the statusNotify
	// drain) so the goroutine is back in select before we advance.
	awaitIteration(t, daemon)

	// Advance the full liveness window in one shot. The goroutine gets
	// one tick and sees timeSincePublish >= maxIdleInterval → publishes.
	fakeClock.Advance(daemon.maxIdleInterval)

	cs.awaitPublishes(3)
	if got := cs.stateEventPuts.Load(); got != 3 {
		t.Errorf("liveness after notify: state event puts = %d, want 3", got)
	}
}
