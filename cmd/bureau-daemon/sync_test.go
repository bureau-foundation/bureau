// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

func TestRoomHasStateChanges(t *testing.T) {
	t.Parallel()

	stateKey := "test"

	tests := []struct {
		name     string
		room     messaging.JoinedRoom
		expected bool
	}{
		{
			name:     "empty room",
			room:     messaging.JoinedRoom{},
			expected: false,
		},
		{
			name: "timeline events without state key",
			room: messaging.JoinedRoom{
				Timeline: messaging.TimelineSection{
					Events: []messaging.Event{
						{Type: schema.MatrixEventTypeMessage, Content: map[string]any{"body": "hello"}},
					},
				},
			},
			expected: false,
		},
		{
			name: "state section has events",
			room: messaging.JoinedRoom{
				State: messaging.StateSection{
					Events: []messaging.Event{
						{Type: schema.EventTypeMachineConfig, StateKey: &stateKey},
					},
				},
			},
			expected: true,
		},
		{
			name: "timeline has state event",
			room: messaging.JoinedRoom{
				Timeline: messaging.TimelineSection{
					Events: []messaging.Event{
						{Type: schema.EventTypeService, StateKey: &stateKey, Content: map[string]any{}},
					},
				},
			},
			expected: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := roomHasStateChanges(test.room)
			if result != test.expected {
				t.Errorf("roomHasStateChanges() = %v, want %v", result, test.expected)
			}
		})
	}
}

func TestRoomHasExternalStateChanges(t *testing.T) {
	t.Parallel()

	selfUserID := ref.MustParseUserID("@bureau/fleet/test/machine/self:bureau.local")
	peerUserID := ref.MustParseUserID("@bureau/fleet/test/machine/peer:bureau.local")
	stateKey := "test"

	tests := []struct {
		name     string
		room     messaging.JoinedRoom
		expected bool
	}{
		{
			name:     "empty room",
			room:     messaging.JoinedRoom{},
			expected: false,
		},
		{
			name: "state section with only self events",
			room: messaging.JoinedRoom{
				State: messaging.StateSection{
					Events: []messaging.Event{
						{Type: schema.EventTypeMachineStatus, StateKey: &stateKey, Sender: selfUserID},
					},
				},
			},
			expected: false,
		},
		{
			name: "state section with peer event",
			room: messaging.JoinedRoom{
				State: messaging.StateSection{
					Events: []messaging.Event{
						{Type: schema.EventTypeMachineStatus, StateKey: &stateKey, Sender: peerUserID},
					},
				},
			},
			expected: true,
		},
		{
			name: "state section with mixed self and peer events",
			room: messaging.JoinedRoom{
				State: messaging.StateSection{
					Events: []messaging.Event{
						{Type: schema.EventTypeMachineStatus, StateKey: &stateKey, Sender: selfUserID},
						{Type: schema.EventTypeMachineStatus, StateKey: &stateKey, Sender: peerUserID},
					},
				},
			},
			expected: true,
		},
		{
			name: "timeline state event from self",
			room: messaging.JoinedRoom{
				Timeline: messaging.TimelineSection{
					Events: []messaging.Event{
						{Type: schema.EventTypeMachineStatus, StateKey: &stateKey, Sender: selfUserID},
					},
				},
			},
			expected: false,
		},
		{
			name: "timeline state event from peer",
			room: messaging.JoinedRoom{
				Timeline: messaging.TimelineSection{
					Events: []messaging.Event{
						{Type: schema.EventTypeMachineStatus, StateKey: &stateKey, Sender: peerUserID},
					},
				},
			},
			expected: true,
		},
		{
			name: "timeline non-state event from peer ignored",
			room: messaging.JoinedRoom{
				Timeline: messaging.TimelineSection{
					Events: []messaging.Event{
						{Type: schema.MatrixEventTypeMessage, Sender: peerUserID, Content: map[string]any{"body": "hello"}},
					},
				},
			},
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := roomHasExternalStateChanges(test.room, selfUserID)
			if result != test.expected {
				t.Errorf("roomHasExternalStateChanges() = %v, want %v", result, test.expected)
			}
		})
	}
}

// TestProcessSyncResponse_MachineRoomSelfEventsFiltered verifies that the
// daemon's own MachineStatus heartbeat events in the machine room do NOT
// trigger syncPeerAddresses. This prevents a feedback loop where each
// heartbeat publish triggers an unnecessary GetRoomState call.
func TestProcessSyncResponse_MachineRoomSelfEventsFiltered(t *testing.T) {
	t.Parallel()

	machine, fleet := testMachineSetup(t, "test", "bureau.local")

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machineRoomID = "!machine:test"
	const serviceRoomID = "!service:test"

	// Machine room has NO state events for GetRoomState.
	matrixState.setRoomState(machineRoomID, nil)

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
	daemon.runDir = principal.DefaultRunDir
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.serviceRoomID = mustRoomID(serviceRoomID)
	daemon.launcherSocket = "/nonexistent/launcher.sock"
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)

	// Pre-populate a peer address. If syncPeerAddresses runs, it will
	// see no state events and remove this entry.
	daemon.peerAddresses["@machine/peer:bureau.local"] = "10.0.0.1:9090"

	// Sync response with a machine room state change from the daemon
	// itself (self-event from heartbeat).
	selfKey := machine.UserID().StateKey()
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				mustRoomID(machineRoomID): {
					Timeline: messaging.TimelineSection{
						Events: []messaging.Event{
							{
								Type:     schema.EventTypeMachineStatus,
								StateKey: &selfKey,
								Sender:   machine.UserID(),
								Content: map[string]any{
									"principal":   machine.UserID().String(),
									"cpu_percent": 42,
								},
							},
						},
					},
				},
			},
		},
	}

	daemon.processSyncResponse(context.Background(), response)

	// The pre-populated peer address should still be present because
	// syncPeerAddresses was NOT called (self-event was filtered).
	if _, ok := daemon.peerAddresses["@machine/peer:bureau.local"]; !ok {
		t.Error("peer address should still exist (self-event should not trigger syncPeerAddresses)")
	}
}

func TestProcessSyncResponse_ConfigRoom(t *testing.T) {
	t.Parallel()

	machine, fleet := testMachineSetup(t, "test", "bureau.local")
	machineName := machine.UserID().StateKey()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machineRoomID = "!machine:test"
	const serviceRoomID = "!service:test"

	// Set up machine config so reconcile finds something.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Principal: testEntity(t, fleet, "test/agent"),
			AutoStart: true,
		}},
	})

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
	daemon.runDir = principal.DefaultRunDir
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.serviceRoomID = mustRoomID(serviceRoomID)
	daemon.launcherSocket = "/nonexistent/launcher.sock"
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)

	// Construct a sync response with a config room state change.
	stateKey := machineName
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				mustRoomID(configRoomID): {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{
								Type:     schema.EventTypeMachineConfig,
								StateKey: &stateKey,
								Content:  map[string]any{},
							},
						},
					},
				},
			},
		},
	}

	// Process the sync response. This should trigger reconcile, which will
	// try to create a sandbox via the launcher. Since the launcher socket
	// doesn't exist, the create will fail, but reconcile itself should succeed
	// (it logs errors and continues).
	daemon.processSyncResponse(context.Background(), response)

	// Verify that lastConfig was set (reconcile ran and read the config).
	if daemon.lastConfig == nil {
		t.Error("lastConfig should be set after sync-triggered reconcile")
	}
	if daemon.lastConfig != nil && len(daemon.lastConfig.Principals) != 1 {
		t.Errorf("lastConfig.Principals = %d, want 1", len(daemon.lastConfig.Principals))
	}
}

// TestProcessSyncResponse_PreReconcileServiceSync verifies that when the
// daemon's first MachineConfig arrives, it forces a service directory sync
// before reconcile. This handles the case where service events arrived in
// /sync while the daemon was unconfigured (lastConfig == nil) — the service
// room handler suppresses processing when unconfigured, and /sync won't
// re-deliver those events. Without the pre-reconcile sync, RequiredServices
// resolution would fail with "not yet in the service directory" and the
// daemon would never discover the service.
func TestProcessSyncResponse_PreReconcileServiceSync(t *testing.T) {
	t.Parallel()

	machine, fleet := testMachineSetup(t, "test", "bureau.local")
	machineName := machine.UserID().StateKey()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machineRoomID = "!machine:test"
	const serviceRoomID = "!service:test"

	// Set up a MachineConfig for reconcile to find.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Principal: testEntity(t, fleet, "test/agent"),
			AutoStart: true,
		}},
	})

	// Set up a service in the service room. This simulates a service
	// that registered while the daemon was unconfigured — the event was
	// in a /sync response that was suppressed.
	serviceKey := "bureau/fleet/test/service/stt/whisper:bureau.local"
	serviceSender, err := ref.ParseUserID("@bureau/fleet/test/service/stt/whisper:bureau.local")
	if err != nil {
		t.Fatalf("parse service sender: %v", err)
	}
	matrixState.setRoomState(serviceRoomID, []mockRoomStateEvent{
		{
			Type:     schema.EventTypeService,
			StateKey: &serviceKey,
			Sender:   serviceSender,
			Content: map[string]any{
				"principal":   "@bureau/fleet/test/service/stt/whisper:bureau.local",
				"machine":     "@bureau/fleet/test/machine/remote:bureau.local",
				"endpoints":   map[string]any{"cbor": "service.sock"},
				"description": "Pre-existing service",
			},
		},
	})

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
	daemon.runDir = principal.DefaultRunDir
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.serviceRoomID = mustRoomID(serviceRoomID)
	daemon.launcherSocket = "/nonexistent/launcher.sock"
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)

	// Verify precondition: service directory is empty, lastConfig is nil.
	if len(daemon.services) != 0 {
		t.Fatalf("precondition: services should be empty, got %d", len(daemon.services))
	}
	if daemon.lastConfig != nil {
		t.Fatal("precondition: lastConfig should be nil")
	}

	// Send a config room change while the daemon is unconfigured.
	// The pre-reconcile service sync should discover the service
	// BEFORE reconcile runs, so RequiredServices resolution would
	// succeed if the agent had any.
	configStateKey := machineName
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				mustRoomID(configRoomID): {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{Type: schema.EventTypeMachineConfig, StateKey: &configStateKey},
						},
					},
				},
			},
		},
	}
	daemon.processSyncResponse(context.Background(), response)

	// The pre-reconcile sync should have populated d.services.
	if len(daemon.services) != 1 {
		t.Errorf("services count = %d, want 1 (pre-reconcile sync should have discovered the service)", len(daemon.services))
	}
	if _, ok := daemon.services[serviceKey]; !ok {
		t.Errorf("service %q not found in directory after pre-reconcile sync", serviceKey)
	}

	// lastConfig should now be set by reconcile.
	if daemon.lastConfig == nil {
		t.Error("lastConfig should be set after reconcile")
	}
}

func TestProcessSyncResponse_ServicesRoom(t *testing.T) {
	t.Parallel()

	machine, fleet := testMachineSetup(t, "test", "bureau.local")

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machineRoomID = "!machine:test"
	const serviceRoomID = "!service:test"

	// Set up a MachineConfig so the daemon is configured. Service
	// directory sync is skipped for unconfigured daemons (no proxies
	// to push the directory to).
	machineName := machine.UserID().StateKey()
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Principal: testEntity(t, fleet, "test/agent"),
			AutoStart: true,
		}},
	})

	// Set up the service room with one service via GetRoomState.
	// State key uses the full localpart:server format (UserID without '@').
	serviceKey := "bureau/fleet/test/service/stt/whisper:bureau.local"
	serviceSender, err := ref.ParseUserID("@bureau/fleet/test/service/stt/whisper:bureau.local")
	if err != nil {
		t.Fatalf("parse service sender: %v", err)
	}
	matrixState.setRoomState(serviceRoomID, []mockRoomStateEvent{
		{
			Type:     schema.EventTypeService,
			StateKey: &serviceKey,
			Sender:   serviceSender,
			Content: map[string]any{
				"principal":   "@bureau/fleet/test/service/stt/whisper:bureau.local",
				"machine":     "@bureau/fleet/test/machine/remote:bureau.local",
				"endpoints":   map[string]any{"http": "http.sock"},
				"description": "Speech to text",
			},
		},
	})

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
	daemon.runDir = principal.DefaultRunDir
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.serviceRoomID = mustRoomID(serviceRoomID)
	daemon.launcherSocket = "/nonexistent/launcher.sock"
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)

	// First, deliver a config room event to establish lastConfig.
	// Service directory sync is only processed for configured daemons.
	configStateKey := machineName
	configResponse := &messaging.SyncResponse{
		NextBatch: "batch_0",
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				mustRoomID(configRoomID): {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{Type: schema.EventTypeMachineConfig, StateKey: &configStateKey},
						},
					},
				},
			},
		},
	}
	daemon.processSyncResponse(context.Background(), configResponse)

	// Construct a sync response with a service room state change.
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				mustRoomID(serviceRoomID): {
					Timeline: messaging.TimelineSection{
						Events: []messaging.Event{
							{
								Type:     schema.EventTypeService,
								StateKey: &serviceKey,
								Content: map[string]any{
									"principal": "@bureau/fleet/test/service/stt/whisper:bureau.local",
									"machine":   "@bureau/fleet/test/machine/remote:bureau.local",
									"endpoints": map[string]any{"http": "http.sock"},
								},
							},
						},
					},
				},
			},
		},
	}

	daemon.processSyncResponse(context.Background(), response)

	// Verify the service was picked up.
	if len(daemon.services) != 1 {
		t.Errorf("services count = %d, want 1", len(daemon.services))
	}
	service, ok := daemon.services[serviceKey]
	if !ok {
		t.Fatalf("service %q not found in directory", serviceKey)
	}
	wantEndpoints := map[string]string{"http": "http.sock"}
	if len(service.Endpoints) != len(wantEndpoints) {
		t.Errorf("service endpoints = %v, want %v", service.Endpoints, wantEndpoints)
	}
	for key, wantValue := range wantEndpoints {
		if service.Endpoints[key] != wantValue {
			t.Errorf("service endpoint %q = %q, want %q", key, service.Endpoints[key], wantValue)
		}
	}
}

func TestSyncServiceDirectory_RejectsCrossServerServiceEvent(t *testing.T) {
	t.Parallel()

	machine, fleet := testMachineSetup(t, "test", "bureau.local")

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machineRoomID = "!machine:test"
	const serviceRoomID = "!service:test"

	// Set up the service room with two services via GetRoomState:
	// one legitimate (sender matches state_key server) and one
	// spoofed (sender from evil.com, state_key on bureau.local).
	legitimateKey := "bureau/fleet/test/service/stt/whisper:bureau.local"
	spoofedKey := "bureau/fleet/test/service/stt/evil:bureau.local"

	legitimateSender, err := ref.ParseUserID("@bureau/fleet/test/service/stt/whisper:bureau.local")
	if err != nil {
		t.Fatalf("parse legitimate sender: %v", err)
	}
	spoofedSender, err := ref.ParseUserID("@attacker:evil.com")
	if err != nil {
		t.Fatalf("parse spoofed sender: %v", err)
	}

	matrixState.setRoomState(serviceRoomID, []mockRoomStateEvent{
		{
			Type:     schema.EventTypeService,
			StateKey: &legitimateKey,
			Sender:   legitimateSender,
			Content: map[string]any{
				"principal":   "@bureau/fleet/test/service/stt/whisper:bureau.local",
				"machine":     "@bureau/fleet/test/machine/gpu:bureau.local",
				"endpoints":   map[string]any{"cbor": "service.sock"},
				"description": "Legitimate service",
			},
		},
		{
			Type:     schema.EventTypeService,
			StateKey: &spoofedKey,
			Sender:   spoofedSender,
			Content: map[string]any{
				"principal":   "@bureau/fleet/test/service/stt/evil:bureau.local",
				"machine":     "@bureau/fleet/test/machine/gpu:bureau.local",
				"endpoints":   map[string]any{"cbor": "evil.sock"},
				"description": "Spoofed service from federated attacker",
			},
		},
	})

	// Empty machine room state (no peers needed for this test).
	matrixState.setRoomState(machineRoomID, nil)

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
	daemon.runDir = principal.DefaultRunDir
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.serviceRoomID = mustRoomID(serviceRoomID)
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	added, _, _, syncErr := daemon.syncServiceDirectory(context.Background())
	if syncErr != nil {
		t.Fatalf("syncServiceDirectory: %v", syncErr)
	}

	// Only the legitimate service should be in the directory.
	if len(daemon.services) != 1 {
		t.Errorf("services count = %d, want 1 (spoofed service should be rejected)", len(daemon.services))
		for key := range daemon.services {
			t.Logf("  service: %q", key)
		}
	}

	if _, ok := daemon.services[legitimateKey]; !ok {
		t.Error("legitimate service not found in directory")
	}
	if _, ok := daemon.services[spoofedKey]; ok {
		t.Error("spoofed service should have been rejected but was found in directory")
	}

	// Only the legitimate service should appear in the added list.
	if len(added) != 1 {
		t.Errorf("added count = %d, want 1", len(added))
	}
}

func TestSyncServiceDirectory_AcceptsSameServerServiceEvent(t *testing.T) {
	t.Parallel()

	machine, _ := testMachineSetup(t, "test", "bureau.local")

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const serviceRoomID = "!service:test"
	const machineRoomID = "!machine:test"

	serviceKey := "bureau/fleet/test/service/ticket/main:bureau.local"
	sender, err := ref.ParseUserID("@bureau/fleet/test/service/ticket/main:bureau.local")
	if err != nil {
		t.Fatalf("parse sender: %v", err)
	}

	matrixState.setRoomState(serviceRoomID, []mockRoomStateEvent{
		{
			Type:     schema.EventTypeService,
			StateKey: &serviceKey,
			Sender:   sender,
			Content: map[string]any{
				"principal":   "@bureau/fleet/test/service/ticket/main:bureau.local",
				"machine":     "@bureau/fleet/test/machine/gpu:bureau.local",
				"endpoints":   map[string]any{"cbor": "service.sock"},
				"description": "Same-server service",
			},
		},
	})

	matrixState.setRoomState(machineRoomID, nil)

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
	daemon.serviceRoomID = mustRoomID(serviceRoomID)
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.machine = machine
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	_, _, _, syncErr := daemon.syncServiceDirectory(context.Background())
	if syncErr != nil {
		t.Fatalf("syncServiceDirectory: %v", syncErr)
	}

	if len(daemon.services) != 1 {
		t.Errorf("services count = %d, want 1", len(daemon.services))
	}
	if _, ok := daemon.services[serviceKey]; !ok {
		t.Error("same-server service should be accepted but was not found")
	}
}

func TestSyncServiceDirectory_ZeroSenderBypasses(t *testing.T) {
	// When the sender is zero (e.g., events from the initial state
	// section that lack sender information), the federation check
	// should be skipped and the event accepted.
	t.Parallel()

	machine, _ := testMachineSetup(t, "test", "bureau.local")

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const serviceRoomID = "!service:test"
	const machineRoomID = "!machine:test"

	serviceKey := "bureau/fleet/test/service/artifact/cache:bureau.local"

	// No sender field — zero value UserID.
	matrixState.setRoomState(serviceRoomID, []mockRoomStateEvent{
		{
			Type:     schema.EventTypeService,
			StateKey: &serviceKey,
			Content: map[string]any{
				"principal":   "@bureau/fleet/test/service/artifact/cache:bureau.local",
				"machine":     "@bureau/fleet/test/machine/storage:bureau.local",
				"endpoints":   map[string]any{"cbor": "service.sock"},
				"description": "Service with no sender info",
			},
		},
	})

	matrixState.setRoomState(machineRoomID, nil)

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
	daemon.serviceRoomID = mustRoomID(serviceRoomID)
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.machine = machine
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	_, _, _, syncErr := daemon.syncServiceDirectory(context.Background())
	if syncErr != nil {
		t.Fatalf("syncServiceDirectory: %v", syncErr)
	}

	if len(daemon.services) != 1 {
		t.Errorf("services count = %d, want 1 (zero sender should bypass check)", len(daemon.services))
	}
}

func TestProcessSyncResponse_MachinesRoom(t *testing.T) {
	t.Parallel()

	machine, fleet := testMachineSetup(t, "test", "bureau.local")

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machineRoomID = "!machine:test"
	const serviceRoomID = "!service:test"

	// Set up the machine room with one peer's status.
	peerKey := "machine/peer"
	matrixState.setRoomState(machineRoomID, []mockRoomStateEvent{
		{
			Type:     schema.EventTypeMachineStatus,
			StateKey: &peerKey,
			Content: map[string]any{
				"principal":         "@machine/peer:bureau.local",
				"transport_address": "192.168.1.100:9090",
			},
		},
	})

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
	daemon.runDir = principal.DefaultRunDir
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.serviceRoomID = mustRoomID(serviceRoomID)
	daemon.launcherSocket = "/nonexistent/launcher.sock"
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)

	// Construct a sync response with a machine room state change.
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				mustRoomID(machineRoomID): {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{
								Type:     schema.EventTypeMachineStatus,
								StateKey: &peerKey,
								Content: map[string]any{
									"principal":         "@machine/peer:bureau.local",
									"transport_address": "192.168.1.100:9090",
								},
							},
						},
					},
				},
			},
		},
	}

	daemon.processSyncResponse(context.Background(), response)

	// Verify the peer address was discovered.
	address, ok := daemon.peerAddresses["@machine/peer:bureau.local"]
	if !ok {
		t.Fatal("peer address not found for @machine/peer:bureau.local")
	}
	if address != "192.168.1.100:9090" {
		t.Errorf("peer address = %q, want %q", address, "192.168.1.100:9090")
	}
}

// TestSyncPeerAddresses_RemovesStalePeers verifies that syncPeerAddresses
// removes peers whose MachineStatus state events are no longer present.
func TestSyncPeerAddresses_RemovesStalePeers(t *testing.T) {
	t.Parallel()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const machineRoomID = "!machine:test"

	// State events: only peerA is present. peerB will be stale.
	peerAKey := "machine/peer-a"
	matrixState.setRoomState(machineRoomID, []mockRoomStateEvent{
		{
			Type:     schema.EventTypeMachineStatus,
			StateKey: &peerAKey,
			Content: map[string]any{
				"principal":         "@machine/peer-a:bureau.local",
				"transport_address": "10.0.0.1:9090",
			},
		},
	})

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

	// Pre-populate with two peers. peerB is stale (not in state events).
	daemon, _ := newTestDaemon(t)
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.session = session
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.peerAddresses["@machine/peer-a:bureau.local"] = "10.0.0.1:9090"
	daemon.peerAddresses["@machine/peer-b:bureau.local"] = "10.0.0.2:9090"
	daemon.peerTransports["10.0.0.1:9090"] = &http.Transport{}
	daemon.peerTransports["10.0.0.2:9090"] = &http.Transport{}
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	if err := daemon.syncPeerAddresses(context.Background()); err != nil {
		t.Fatalf("syncPeerAddresses: %v", err)
	}

	// peerA should still be present.
	if address, ok := daemon.peerAddresses["@machine/peer-a:bureau.local"]; !ok {
		t.Error("peer-a should still be in peerAddresses")
	} else if address != "10.0.0.1:9090" {
		t.Errorf("peer-a address = %q, want %q", address, "10.0.0.1:9090")
	}

	// peerB should be removed.
	if _, ok := daemon.peerAddresses["@machine/peer-b:bureau.local"]; ok {
		t.Error("peer-b should have been removed from peerAddresses")
	}

	// peerA's transport should still be cached.
	daemon.peerTransportsMu.RLock()
	_, peerATransportExists := daemon.peerTransports["10.0.0.1:9090"]
	_, peerBTransportExists := daemon.peerTransports["10.0.0.2:9090"]
	daemon.peerTransportsMu.RUnlock()

	if !peerATransportExists {
		t.Error("peer-a transport should still be cached")
	}
	if peerBTransportExists {
		t.Error("peer-b transport should have been cleaned up")
	}
}

// TestSyncPeerAddresses_UpdatesChangedAddress verifies that when a peer's
// transport address changes, the old address is updated and its cached
// transport is cleaned up.
func TestSyncPeerAddresses_UpdatesChangedAddress(t *testing.T) {
	t.Parallel()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const machineRoomID = "!machine:test"

	// State events: peerA has a new address.
	peerAKey := "machine/peer-a"
	matrixState.setRoomState(machineRoomID, []mockRoomStateEvent{
		{
			Type:     schema.EventTypeMachineStatus,
			StateKey: &peerAKey,
			Content: map[string]any{
				"principal":         "@machine/peer-a:bureau.local",
				"transport_address": "10.0.0.99:9090",
			},
		},
	})

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
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.session = session
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.peerAddresses["@machine/peer-a:bureau.local"] = "10.0.0.1:9090"
	daemon.peerTransports["10.0.0.1:9090"] = &http.Transport{}
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	if err := daemon.syncPeerAddresses(context.Background()); err != nil {
		t.Fatalf("syncPeerAddresses: %v", err)
	}

	// Address should be updated.
	if address := daemon.peerAddresses["@machine/peer-a:bureau.local"]; address != "10.0.0.99:9090" {
		t.Errorf("peer-a address = %q, want %q", address, "10.0.0.99:9090")
	}

	// Old transport should be cleaned up, new one not yet cached.
	daemon.peerTransportsMu.RLock()
	_, oldTransportExists := daemon.peerTransports["10.0.0.1:9090"]
	_, newTransportExists := daemon.peerTransports["10.0.0.99:9090"]
	daemon.peerTransportsMu.RUnlock()

	if oldTransportExists {
		t.Error("transport for old address should have been cleaned up")
	}
	if newTransportExists {
		t.Error("transport for new address should not be cached yet (lazy creation)")
	}
}

// TestSyncPeerAddresses_SharedAddressNotRemovedPrematurely verifies that when
// two peers share a transport address and one is removed, the cached transport
// is preserved because the other peer still references it.
func TestSyncPeerAddresses_SharedAddressNotRemovedPrematurely(t *testing.T) {
	t.Parallel()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const machineRoomID = "!machine:test"

	// Only peerA remains. peerB (same address) is gone.
	peerAKey := "machine/peer-a"
	matrixState.setRoomState(machineRoomID, []mockRoomStateEvent{
		{
			Type:     schema.EventTypeMachineStatus,
			StateKey: &peerAKey,
			Content: map[string]any{
				"principal":         "@machine/peer-a:bureau.local",
				"transport_address": "10.0.0.1:9090",
			},
		},
	})

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

	// Two peers sharing the same address (e.g., behind a load balancer).
	sharedAddress := "10.0.0.1:9090"
	daemon, _ := newTestDaemon(t)
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.session = session
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.peerAddresses["@machine/peer-a:bureau.local"] = sharedAddress
	daemon.peerAddresses["@machine/peer-b:bureau.local"] = sharedAddress
	daemon.peerTransports[sharedAddress] = &http.Transport{}
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	if err := daemon.syncPeerAddresses(context.Background()); err != nil {
		t.Fatalf("syncPeerAddresses: %v", err)
	}

	// peerB should be removed but peerA still uses the shared address.
	if _, ok := daemon.peerAddresses["@machine/peer-b:bureau.local"]; ok {
		t.Error("peer-b should have been removed")
	}

	// Transport for the shared address should be preserved.
	daemon.peerTransportsMu.RLock()
	_, transportExists := daemon.peerTransports[sharedAddress]
	daemon.peerTransportsMu.RUnlock()

	if !transportExists {
		t.Error("transport for shared address should be preserved (peer-a still uses it)")
	}
}

func TestProcessSyncResponse_NoChanges(t *testing.T) {
	t.Parallel()

	machine, fleet := testMachineSetup(t, "test", "bureau.local")

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

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
	daemon.runDir = principal.DefaultRunDir
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID("!config:test")
	daemon.machineRoomID = mustRoomID("!machine:test")
	daemon.serviceRoomID = mustRoomID("!service:test")
	daemon.launcherSocket = "/nonexistent/launcher.sock"
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)

	// Empty sync response — no handlers should fire.
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms:     messaging.RoomsSection{},
	}

	daemon.processSyncResponse(context.Background(), response)

	// Verify nothing changed.
	if daemon.lastConfig != nil {
		t.Error("lastConfig should be nil (no reconcile)")
	}
	if len(daemon.services) != 0 {
		t.Errorf("services = %d, want 0", len(daemon.services))
	}
	if len(daemon.peerAddresses) != 0 {
		t.Errorf("peerAddresses = %d, want 0", len(daemon.peerAddresses))
	}
}

func TestInitialSync(t *testing.T) {
	t.Parallel()

	machine, fleet := testMachineSetup(t, "test", "bureau.local")
	machineName := machine.UserID().StateKey()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machineRoomID = "!machine:test"
	const serviceRoomID = "!service:test"

	// Set up machine config.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Principal: testEntity(t, fleet, "test/agent"),
			AutoStart: true,
		}},
	})

	// Set up machine room state for GetRoomState (used by syncPeerAddresses).
	peerKey := "machine/peer"
	matrixState.setRoomState(machineRoomID, []mockRoomStateEvent{
		{
			Type:     schema.EventTypeMachineStatus,
			StateKey: &peerKey,
			Content: map[string]any{
				"principal":         "@machine/peer:bureau.local",
				"transport_address": "10.0.0.1:9090",
			},
		},
	})

	// Set up service room state with proper localpart:server state key.
	serviceKey := "bureau/fleet/test/service/tts/piper:bureau.local"
	serviceSender, err := ref.ParseUserID("@bureau/fleet/test/service/tts/piper:bureau.local")
	if err != nil {
		t.Fatalf("parse service sender: %v", err)
	}
	matrixState.setRoomState(serviceRoomID, []mockRoomStateEvent{
		{
			Type:     schema.EventTypeService,
			StateKey: &serviceKey,
			Sender:   serviceSender,
			Content: map[string]any{
				"principal":   "@bureau/fleet/test/service/tts/piper:bureau.local",
				"machine":     "@bureau/fleet/test/machine/peer:bureau.local",
				"endpoints":   map[string]any{"http": "http.sock"},
				"description": "Text to speech",
			},
		},
	})

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
	daemon.runDir = principal.DefaultRunDir
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.serviceRoomID = mustRoomID(serviceRoomID)
	daemon.launcherSocket = "/nonexistent/launcher.sock"
	daemon.statusInterval = time.Hour
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sinceToken, err := daemon.initialSync(ctx)
	if err != nil {
		t.Fatalf("initialSync: %v", err)
	}

	// Verify we got a since token.
	if sinceToken == "" {
		t.Error("sinceToken should not be empty after initial sync")
	}

	// Verify reconcile ran (lastConfig set).
	if daemon.lastConfig == nil {
		t.Fatal("lastConfig should be set after initial sync")
	}
	if len(daemon.lastConfig.Principals) != 1 {
		t.Errorf("lastConfig.Principals = %d, want 1", len(daemon.lastConfig.Principals))
	}

	// Verify peer addresses synced.
	address, ok := daemon.peerAddresses["@machine/peer:bureau.local"]
	if !ok {
		t.Error("peer address not found for @machine/peer:bureau.local")
	}
	if address != "10.0.0.1:9090" {
		t.Errorf("peer address = %q, want %q", address, "10.0.0.1:9090")
	}

	// Verify service directory synced.
	if len(daemon.services) != 1 {
		t.Errorf("services count = %d, want 1", len(daemon.services))
	}
	service, ok := daemon.services[serviceKey]
	if !ok {
		t.Fatalf("service %q not found", serviceKey)
	}
	wantEndpoints := map[string]string{"http": "http.sock"}
	if len(service.Endpoints) != len(wantEndpoints) {
		t.Errorf("service endpoints = %v, want %v", service.Endpoints, wantEndpoints)
	}
	for key, wantValue := range wantEndpoints {
		if service.Endpoints[key] != wantValue {
			t.Errorf("service endpoint %q = %q, want %q", key, service.Endpoints[key], wantValue)
		}
	}
}

// TestProcessSyncResponse_WorkspaceRoomTriggersReconcile verifies that state
// changes in non-core rooms (workspace rooms joined via invite) trigger
// reconcile. This is essential for the workspace flow: when a setup principal
// publishes m.bureau.workspace, the daemon sees the state change via
// /sync and re-reconciles, unblocking deferred agent principals whose
// StartCondition references that event.
func TestProcessSyncResponse_WorkspaceRoomTriggersReconcile(t *testing.T) {
	t.Parallel()

	machine, fleet := testMachineSetup(t, "test", "bureau.local")
	machineName := machine.UserID().StateKey()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machineRoomID = "!machine:test"
	const serviceRoomID = "!service:test"

	// Set up machine config so reconcile sets lastConfig.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Principal: testEntity(t, fleet, "test/agent"),
			AutoStart: true,
		}},
	})

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
	daemon.runDir = principal.DefaultRunDir
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.serviceRoomID = mustRoomID(serviceRoomID)
	daemon.launcherSocket = "/nonexistent/launcher.sock"
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)

	// First, deliver a config room event so the daemon discovers its
	// MachineConfig and sets lastConfig. Workspace room events are
	// only processed after the daemon is configured (unconfigured
	// daemons have no principals whose StartConditions could match).
	configStateKey := machineName
	configResponse := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				mustRoomID(configRoomID): {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{Type: schema.EventTypeMachineConfig, StateKey: &configStateKey},
						},
					},
				},
			},
		},
	}
	daemon.processSyncResponse(context.Background(), configResponse)
	if daemon.lastConfig == nil {
		t.Fatal("lastConfig should be set after config room event")
	}

	// Now deliver a workspace room event. With lastConfig set, the
	// daemon processes non-core room state changes and triggers
	// reconcile for StartCondition re-evaluation.
	stateKey := ""
	workspaceResponse := &messaging.SyncResponse{
		NextBatch: "batch_2",
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				mustRoomID("!workspace/iree:test"): {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{Type: schema.EventTypeWorkspace, StateKey: &stateKey},
						},
					},
				},
			},
		},
	}
	daemon.processSyncResponse(context.Background(), workspaceResponse)

	// Workspace room state changes should trigger reconcile so deferred
	// principals can re-evaluate StartConditions (verified by the
	// lastConfig still being set — reconcile ran without error).
	if daemon.lastConfig == nil {
		t.Error("lastConfig should still be set after workspace room reconcile")
	}
}

// TestProcessSyncResponse_TemplateRoomTriggersReconcile verifies that
// m.bureau.template state events in the template room trigger reconciliation.
// Operators can update a template definition and have running principals
// pick up the change without needing to also touch MachineConfig.
func TestProcessSyncResponse_TemplateRoomTriggersReconcile(t *testing.T) {
	t.Parallel()

	machine, fleet := testMachineSetup(t, "test", "bureau.local")
	machineName := machine.UserID().StateKey()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machineRoomID = "!machine:test"
	const serviceRoomID = "!service:test"
	const templateRoomID = "!template:test"

	// Set up machine config so reconcile has something to work with.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Principal: testEntity(t, fleet, "test/agent"),
			AutoStart: true,
		}},
	})

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
	daemon.runDir = principal.DefaultRunDir
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.serviceRoomID = mustRoomID(serviceRoomID)
	daemon.templateRoomID = mustRoomID(templateRoomID)
	daemon.launcherSocket = "/nonexistent/launcher.sock"
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)

	// First, configure the daemon via a config room event.
	configStateKey := machineName
	configResponse := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				mustRoomID(configRoomID): {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{Type: schema.EventTypeMachineConfig, StateKey: &configStateKey},
						},
					},
				},
			},
		},
	}
	daemon.processSyncResponse(context.Background(), configResponse)
	if daemon.lastConfig == nil {
		t.Fatal("lastConfig should be set after config room event")
	}

	// Clear lastConfig to detect whether the next sync triggers reconcile.
	daemon.lastConfig = nil

	// Deliver a template state event in the template room.
	templateStateKey := "cloudflare-tunnel"
	templateResponse := &messaging.SyncResponse{
		NextBatch: "batch_2",
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				mustRoomID(templateRoomID): {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{Type: schema.EventTypeTemplate, StateKey: &templateStateKey},
						},
					},
				},
			},
		},
	}
	daemon.processSyncResponse(context.Background(), templateResponse)

	// Reconcile should have run, re-reading the config.
	if daemon.lastConfig == nil {
		t.Error("template state event should trigger reconcile (lastConfig should be set)")
	}
}

// TestProcessSyncResponse_TemplateRoomMembershipIgnored verifies that
// membership events in the template room do NOT trigger reconciliation.
// The template room is shared across all machines in the namespace, and
// member events from other machines joining would cause spurious
// reconcile cycles if not filtered.
func TestProcessSyncResponse_TemplateRoomMembershipIgnored(t *testing.T) {
	t.Parallel()

	machine, fleet := testMachineSetup(t, "test", "bureau.local")

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const templateRoomID = "!template:test"

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
	daemon.runDir = principal.DefaultRunDir
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID("!config:test")
	daemon.machineRoomID = mustRoomID("!machine:test")
	daemon.serviceRoomID = mustRoomID("!service:test")
	daemon.templateRoomID = mustRoomID(templateRoomID)
	daemon.launcherSocket = "/nonexistent/launcher.sock"
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)

	// Deliver a membership event in the template room (another machine
	// joining). This should NOT trigger reconcile.
	memberStateKey := "@bureau/fleet/test/machine/other:bureau.local"
	memberResponse := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				mustRoomID(templateRoomID): {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{Type: schema.MatrixEventTypeRoomMember, StateKey: &memberStateKey},
						},
					},
				},
			},
		},
	}
	daemon.processSyncResponse(context.Background(), memberResponse)

	// lastConfig should remain nil — no reconcile should have run.
	if daemon.lastConfig != nil {
		t.Error("membership events in template room should NOT trigger reconcile")
	}
}

// TestProcessSyncResponse_AcceptsInvites verifies that the daemon auto-joins
// rooms it's invited to (workspace rooms from "bureau workspace create") and
// triggers reconcile afterward.
func TestProcessSyncResponse_AcceptsInvites(t *testing.T) {
	t.Parallel()

	machine, fleet := testMachineSetup(t, "test", "bureau.local")
	machineName := machine.UserID().StateKey()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machineRoomID = "!machine:test"
	const serviceRoomID = "!service:test"
	const workspaceRoomID = "!workspace:test"

	// Set up machine config so reconcile has something to process.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Principal: testEntity(t, fleet, "test/agent"),
			AutoStart: true,
		}},
	})

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
	daemon.runDir = principal.DefaultRunDir
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.serviceRoomID = mustRoomID(serviceRoomID)
	daemon.launcherSocket = "/nonexistent/launcher.sock"
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)

	// Sync response with an invite to a workspace room.
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Invite: map[ref.RoomID]messaging.InvitedRoom{
				mustRoomID(workspaceRoomID): {},
			},
		},
	}

	daemon.processSyncResponse(context.Background(), response)

	// Verify the daemon called JoinRoom on the workspace room.
	if !matrixState.hasJoined(workspaceRoomID) {
		t.Error("daemon should have joined the workspace room after invite")
	}

	// Accepting an invite triggers reconcile so deferred principals
	// can re-evaluate StartConditions against the newly joined room.
	if daemon.lastConfig == nil {
		t.Error("lastConfig should be set (invite acceptance triggers reconcile)")
	}
}

// TestInitialSync_AcceptsInvites verifies that pending invites from before the
// daemon started are accepted during the initial /sync. This handles the case
// where "bureau workspace create" invited the daemon while it was offline.
func TestInitialSync_AcceptsInvites(t *testing.T) {
	t.Parallel()

	machine, fleet := testMachineSetup(t, "test", "bureau.local")
	machineName := machine.UserID().StateKey()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machineRoomID = "!machine:test"
	const serviceRoomID = "!service:test"
	const workspaceRoomID = "!workspace:test"

	// Set up machine config.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Principal: testEntity(t, fleet, "test/agent"),
			AutoStart: true,
		}},
	})

	// Add a pending invite to a workspace room. This simulates the daemon
	// being invited while it was offline.
	matrixState.addInvite(workspaceRoomID)

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
	daemon.runDir = principal.DefaultRunDir
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.machineRoomID = mustRoomID(machineRoomID)
	daemon.serviceRoomID = mustRoomID(serviceRoomID)
	daemon.launcherSocket = "/nonexistent/launcher.sock"
	daemon.statusInterval = time.Hour
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	t.Cleanup(daemon.stopAllLayoutWatchers)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sinceToken, err := daemon.initialSync(ctx)
	if err != nil {
		t.Fatalf("initialSync: %v", err)
	}
	if sinceToken == "" {
		t.Error("sinceToken should not be empty after initial sync")
	}

	// Verify the daemon joined the workspace room during initial sync.
	if !matrixState.hasJoined(workspaceRoomID) {
		t.Error("daemon should have joined workspace room from initial sync invite")
	}

	// Verify reconcile ran.
	if daemon.lastConfig == nil {
		t.Fatal("lastConfig should be set after initial sync")
	}
}

// TestProcessTemporalGrantEvents_AddGrant verifies that temporal grant
// state events in a sync response are applied to the authorization index.
func TestProcessTemporalGrantEvents_AddGrant(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// Initialize the index with a principal so temporal grants can attach.
	agentAlpha := testEntity(t, daemon.fleet, "agent/alpha")
	daemon.authorizationIndex.SetPrincipal(agentAlpha.UserID(), schema.AuthorizationPolicy{})

	stateKey := "test-grant-ticket"
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				mustRoomID("!config:test"): {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{
								Type:     schema.EventTypeTemporalGrant,
								StateKey: &stateKey,
								Content: map[string]any{
									"principal": agentAlpha.UserID().String(),
									"grant": map[string]any{
										"actions":    []any{schema.ActionServiceRegister},
										"expires_at": "2099-01-01T00:00:00Z",
										"ticket":     "test-grant-ticket",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	daemon.processTemporalGrantEvents(response)

	grants := daemon.authorizationIndex.Grants(testEntity(t, daemon.fleet, "agent/alpha").UserID())
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(grants))
	}
	if grants[0].Ticket != "test-grant-ticket" {
		t.Errorf("grant ticket = %q, want %q", grants[0].Ticket, "test-grant-ticket")
	}
	if grants[0].ExpiresAt != "2099-01-01T00:00:00Z" {
		t.Errorf("grant expires_at = %q, want %q", grants[0].ExpiresAt, "2099-01-01T00:00:00Z")
	}
	if len(grants[0].Actions) != 1 || grants[0].Actions[0] != schema.ActionServiceRegister {
		t.Errorf("grant actions = %v, want [%s]", grants[0].Actions, schema.ActionServiceRegister)
	}
	if grants[0].Source != schema.SourceTemporal {
		t.Errorf("grant source = %q, want %q", grants[0].Source, schema.SourceTemporal)
	}
}

// TestProcessTemporalGrantEvents_RevokeGrant verifies that a tombstoned
// temporal grant event (empty content) revokes the grant from the index.
func TestProcessTemporalGrantEvents_RevokeGrant(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "agent/alpha").UserID(), schema.AuthorizationPolicy{})

	// Pre-add a temporal grant.
	added := daemon.authorizationIndex.AddTemporalGrant(testEntity(t, daemon.fleet, "agent/alpha").UserID(), schema.Grant{
		Actions:   []string{schema.ActionServiceRegister},
		ExpiresAt: "2099-01-01T00:00:00Z",
		Ticket:    "revoke-grant-ticket",
	})
	if !added {
		t.Fatal("AddTemporalGrant returned false")
	}

	// Verify it's there.
	if grants := daemon.authorizationIndex.Grants(testEntity(t, daemon.fleet, "agent/alpha").UserID()); len(grants) != 1 {
		t.Fatalf("before revoke: grants = %d, want 1", len(grants))
	}

	// Process a tombstone event (empty content) for the same ticket.
	stateKey := "revoke-grant-ticket"
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				mustRoomID("!config:test"): {
					Timeline: messaging.TimelineSection{
						Events: []messaging.Event{
							{
								Type:     schema.EventTypeTemporalGrant,
								StateKey: &stateKey,
								Content:  map[string]any{},
							},
						},
					},
				},
			},
		},
	}

	daemon.processTemporalGrantEvents(response)

	grants := daemon.authorizationIndex.Grants(testEntity(t, daemon.fleet, "agent/alpha").UserID())
	if len(grants) != 0 {
		t.Errorf("after revoke: grants = %d, want 0", len(grants))
	}
}

// TestProcessTemporalGrantEvents_TicketFromStateKey verifies that when a
// temporal grant event has no ticket in the grant itself, the state key
// is used as the ticket value.
func TestProcessTemporalGrantEvents_TicketFromStateKey(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	agentAlpha := testEntity(t, daemon.fleet, "agent/alpha")
	daemon.authorizationIndex.SetPrincipal(agentAlpha.UserID(), schema.AuthorizationPolicy{})

	stateKey := "statekey-grant-ticket"
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				mustRoomID("!config:test"): {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{
								Type:     schema.EventTypeTemporalGrant,
								StateKey: &stateKey,
								Content: map[string]any{
									"principal": agentAlpha.UserID().String(),
									"grant": map[string]any{
										"actions":    []any{schema.ActionServiceRegister},
										"expires_at": "2099-01-01T00:00:00Z",
										// No "ticket" field — should be filled from state key.
									},
								},
							},
						},
					},
				},
			},
		},
	}

	daemon.processTemporalGrantEvents(response)

	grants := daemon.authorizationIndex.Grants(testEntity(t, daemon.fleet, "agent/alpha").UserID())
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(grants))
	}
	if grants[0].Ticket != "statekey-grant-ticket" {
		t.Errorf("grant ticket = %q, want %q (from state key)", grants[0].Ticket, "statekey-grant-ticket")
	}
}
