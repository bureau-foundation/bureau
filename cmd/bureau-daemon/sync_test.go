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
						{Type: "m.room.message", Content: map[string]any{"body": "hello"}},
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
						{Type: "m.bureau.machine_config", StateKey: &stateKey},
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
						{Type: "m.bureau.service", StateKey: &stateKey, Content: map[string]any{}},
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

func TestProcessSyncResponse_ConfigRoom(t *testing.T) {
	t.Parallel()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machinesRoomID = "!machines:test"
	const servicesRoomID = "!services:test"

	// Set up machine config so reconcile finds something.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, "machine/test", schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Localpart: "test/agent",
			AutoStart: true,
		}},
	})

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := matrixClient.SessionFromToken("@machine/test:bureau.local", "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon := &Daemon{
		runDir:            principal.DefaultRunDir,
		session:           session,
		machineName:       "machine/test",
		machineUserID:     "@machine/test:bureau.local",
		serverName:        "bureau.local",
		configRoomID:      configRoomID,
		machinesRoomID:    machinesRoomID,
		servicesRoomID:    servicesRoomID,
		launcherSocket:    "/nonexistent/launcher.sock",
		running:           make(map[string]bool),
		lastCredentials:   make(map[string]string),
		lastVisibility:    make(map[string][]string),
		lastObservePolicy: make(map[string]*schema.ObservePolicy),
		services:          make(map[string]*schema.Service),
		proxyRoutes:       make(map[string]string),
		peerAddresses:     make(map[string]string),
		peerTransports:    make(map[string]http.RoundTripper),
		layoutWatchers:    make(map[string]*layoutWatcher),
		logger:            slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)

	// Construct a sync response with a config room state change.
	stateKey := "machine/test"
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[string]messaging.JoinedRoom{
				configRoomID: {
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

func TestProcessSyncResponse_ServicesRoom(t *testing.T) {
	t.Parallel()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machinesRoomID = "!machines:test"
	const servicesRoomID = "!services:test"

	// Set up the services room with one service via GetRoomState.
	serviceKey := "service/stt/whisper"
	matrixState.setRoomState(servicesRoomID, []mockRoomStateEvent{
		{
			Type:     schema.EventTypeService,
			StateKey: &serviceKey,
			Content: map[string]any{
				"principal":   "@service/stt/whisper:bureau.local",
				"machine":     "@machine/remote:bureau.local",
				"protocol":    "http",
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
	session, err := matrixClient.SessionFromToken("@machine/test:bureau.local", "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon := &Daemon{
		runDir:            principal.DefaultRunDir,
		session:           session,
		machineName:       "machine/test",
		machineUserID:     "@machine/test:bureau.local",
		serverName:        "bureau.local",
		configRoomID:      configRoomID,
		machinesRoomID:    machinesRoomID,
		servicesRoomID:    servicesRoomID,
		launcherSocket:    "/nonexistent/launcher.sock",
		running:           make(map[string]bool),
		lastCredentials:   make(map[string]string),
		lastVisibility:    make(map[string][]string),
		lastObservePolicy: make(map[string]*schema.ObservePolicy),
		services:          make(map[string]*schema.Service),
		proxyRoutes:       make(map[string]string),
		peerAddresses:     make(map[string]string),
		peerTransports:    make(map[string]http.RoundTripper),
		layoutWatchers:    make(map[string]*layoutWatcher),
		logger:            slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)

	// Construct a sync response with a services room state change.
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[string]messaging.JoinedRoom{
				servicesRoomID: {
					Timeline: messaging.TimelineSection{
						Events: []messaging.Event{
							{
								Type:     schema.EventTypeService,
								StateKey: &serviceKey,
								Content: map[string]any{
									"principal": "@service/stt/whisper:bureau.local",
									"machine":   "@machine/remote:bureau.local",
									"protocol":  "http",
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
	if service.Protocol != "http" {
		t.Errorf("service protocol = %q, want %q", service.Protocol, "http")
	}
}

func TestProcessSyncResponse_MachinesRoom(t *testing.T) {
	t.Parallel()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machinesRoomID = "!machines:test"
	const servicesRoomID = "!services:test"

	// Set up the machines room with one peer's status.
	peerKey := "machine/peer"
	matrixState.setRoomState(machinesRoomID, []mockRoomStateEvent{
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
	session, err := matrixClient.SessionFromToken("@machine/test:bureau.local", "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon := &Daemon{
		runDir:            principal.DefaultRunDir,
		session:           session,
		machineName:       "machine/test",
		machineUserID:     "@machine/test:bureau.local",
		serverName:        "bureau.local",
		configRoomID:      configRoomID,
		machinesRoomID:    machinesRoomID,
		servicesRoomID:    servicesRoomID,
		launcherSocket:    "/nonexistent/launcher.sock",
		running:           make(map[string]bool),
		lastCredentials:   make(map[string]string),
		lastVisibility:    make(map[string][]string),
		lastObservePolicy: make(map[string]*schema.ObservePolicy),
		services:          make(map[string]*schema.Service),
		proxyRoutes:       make(map[string]string),
		peerAddresses:     make(map[string]string),
		peerTransports:    make(map[string]http.RoundTripper),
		layoutWatchers:    make(map[string]*layoutWatcher),
		logger:            slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)

	// Construct a sync response with a machines room state change.
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[string]messaging.JoinedRoom{
				machinesRoomID: {
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

func TestProcessSyncResponse_NoChanges(t *testing.T) {
	t.Parallel()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := matrixClient.SessionFromToken("@machine/test:bureau.local", "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon := &Daemon{
		runDir:            principal.DefaultRunDir,
		session:           session,
		machineName:       "machine/test",
		machineUserID:     "@machine/test:bureau.local",
		serverName:        "bureau.local",
		configRoomID:      "!config:test",
		machinesRoomID:    "!machines:test",
		servicesRoomID:    "!services:test",
		launcherSocket:    "/nonexistent/launcher.sock",
		running:           make(map[string]bool),
		lastCredentials:   make(map[string]string),
		lastVisibility:    make(map[string][]string),
		lastObservePolicy: make(map[string]*schema.ObservePolicy),
		services:          make(map[string]*schema.Service),
		proxyRoutes:       make(map[string]string),
		peerAddresses:     make(map[string]string),
		peerTransports:    make(map[string]http.RoundTripper),
		layoutWatchers:    make(map[string]*layoutWatcher),
		logger:            slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
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

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machinesRoomID = "!machines:test"
	const servicesRoomID = "!services:test"

	// Set up machine config.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, "machine/test", schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Localpart: "test/agent",
			AutoStart: true,
		}},
	})

	// Set up machines room state for GetRoomState (used by syncPeerAddresses).
	peerKey := "machine/peer"
	matrixState.setRoomState(machinesRoomID, []mockRoomStateEvent{
		{
			Type:     schema.EventTypeMachineStatus,
			StateKey: &peerKey,
			Content: map[string]any{
				"principal":         "@machine/peer:bureau.local",
				"transport_address": "10.0.0.1:9090",
			},
		},
	})

	// Set up services room state.
	serviceKey := "service/tts/piper"
	matrixState.setRoomState(servicesRoomID, []mockRoomStateEvent{
		{
			Type:     schema.EventTypeService,
			StateKey: &serviceKey,
			Content: map[string]any{
				"principal":   "@service/tts/piper:bureau.local",
				"machine":     "@machine/peer:bureau.local",
				"protocol":    "http",
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
	session, err := matrixClient.SessionFromToken("@machine/test:bureau.local", "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon := &Daemon{
		runDir:            principal.DefaultRunDir,
		session:           session,
		machineName:       "machine/test",
		machineUserID:     "@machine/test:bureau.local",
		serverName:        "bureau.local",
		configRoomID:      configRoomID,
		machinesRoomID:    machinesRoomID,
		servicesRoomID:    servicesRoomID,
		launcherSocket:    "/nonexistent/launcher.sock",
		statusInterval:    time.Hour,
		running:           make(map[string]bool),
		lastCredentials:   make(map[string]string),
		lastVisibility:    make(map[string][]string),
		lastObservePolicy: make(map[string]*schema.ObservePolicy),
		services:          make(map[string]*schema.Service),
		proxyRoutes:       make(map[string]string),
		peerAddresses:     make(map[string]string),
		peerTransports:    make(map[string]http.RoundTripper),
		layoutWatchers:    make(map[string]*layoutWatcher),
		logger:            slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
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
	if service.Protocol != "http" {
		t.Errorf("service protocol = %q, want %q", service.Protocol, "http")
	}
}

// TestProcessSyncResponse_WorkspaceRoomTriggersReconcile verifies that state
// changes in non-core rooms (workspace rooms joined via invite) trigger
// reconcile. This is essential for the workspace flow: when a setup principal
// publishes m.bureau.workspace.ready, the daemon sees the state change via
// /sync and re-reconciles, unblocking deferred agent principals whose
// StartCondition references that event.
func TestProcessSyncResponse_WorkspaceRoomTriggersReconcile(t *testing.T) {
	t.Parallel()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machinesRoomID = "!machines:test"
	const servicesRoomID = "!services:test"

	// Set up machine config so reconcile sets lastConfig.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, "machine/test", schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Localpart: "test/agent",
			AutoStart: true,
		}},
	})

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := matrixClient.SessionFromToken("@machine/test:bureau.local", "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon := &Daemon{
		runDir:            principal.DefaultRunDir,
		session:           session,
		machineName:       "machine/test",
		machineUserID:     "@machine/test:bureau.local",
		serverName:        "bureau.local",
		configRoomID:      configRoomID,
		machinesRoomID:    machinesRoomID,
		servicesRoomID:    servicesRoomID,
		launcherSocket:    "/nonexistent/launcher.sock",
		running:           make(map[string]bool),
		lastCredentials:   make(map[string]string),
		lastVisibility:    make(map[string][]string),
		lastObservePolicy: make(map[string]*schema.ObservePolicy),
		services:          make(map[string]*schema.Service),
		proxyRoutes:       make(map[string]string),
		peerAddresses:     make(map[string]string),
		peerTransports:    make(map[string]http.RoundTripper),
		layoutWatchers:    make(map[string]*layoutWatcher),
		logger:            slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)

	// Sync response with a workspace.ready event in a workspace room
	// (not one of the three core rooms).
	stateKey := ""
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Join: map[string]messaging.JoinedRoom{
				"!workspace/iree:test": {
					State: messaging.StateSection{
						Events: []messaging.Event{
							{Type: schema.EventTypeWorkspaceReady, StateKey: &stateKey},
						},
					},
				},
			},
		},
	}

	daemon.processSyncResponse(context.Background(), response)

	// Workspace room state changes should trigger reconcile so deferred
	// principals can re-evaluate StartConditions.
	if daemon.lastConfig == nil {
		t.Error("lastConfig should be set (workspace room state change triggers reconcile)")
	}
}

// TestProcessSyncResponse_AcceptsInvites verifies that the daemon auto-joins
// rooms it's invited to (workspace rooms from "bureau workspace create") and
// triggers reconcile afterward.
func TestProcessSyncResponse_AcceptsInvites(t *testing.T) {
	t.Parallel()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machinesRoomID = "!machines:test"
	const servicesRoomID = "!services:test"
	const workspaceRoomID = "!workspace:test"

	// Set up machine config so reconcile has something to process.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, "machine/test", schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Localpart: "test/agent",
			AutoStart: true,
		}},
	})

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := matrixClient.SessionFromToken("@machine/test:bureau.local", "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon := &Daemon{
		runDir:            principal.DefaultRunDir,
		session:           session,
		machineName:       "machine/test",
		machineUserID:     "@machine/test:bureau.local",
		serverName:        "bureau.local",
		configRoomID:      configRoomID,
		machinesRoomID:    machinesRoomID,
		servicesRoomID:    servicesRoomID,
		launcherSocket:    "/nonexistent/launcher.sock",
		running:           make(map[string]bool),
		lastCredentials:   make(map[string]string),
		lastVisibility:    make(map[string][]string),
		lastObservePolicy: make(map[string]*schema.ObservePolicy),
		services:          make(map[string]*schema.Service),
		proxyRoutes:       make(map[string]string),
		peerAddresses:     make(map[string]string),
		peerTransports:    make(map[string]http.RoundTripper),
		layoutWatchers:    make(map[string]*layoutWatcher),
		logger:            slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	t.Cleanup(daemon.stopAllLayoutWatchers)

	// Sync response with an invite to a workspace room.
	response := &messaging.SyncResponse{
		NextBatch: "batch_1",
		Rooms: messaging.RoomsSection{
			Invite: map[string]messaging.InvitedRoom{
				workspaceRoomID: {},
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

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	const configRoomID = "!config:test"
	const machinesRoomID = "!machines:test"
	const servicesRoomID = "!services:test"
	const workspaceRoomID = "!workspace:test"

	// Set up machine config.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, "machine/test", schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{{
			Localpart: "test/agent",
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
	session, err := matrixClient.SessionFromToken("@machine/test:bureau.local", "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon := &Daemon{
		runDir:            principal.DefaultRunDir,
		session:           session,
		machineName:       "machine/test",
		machineUserID:     "@machine/test:bureau.local",
		serverName:        "bureau.local",
		configRoomID:      configRoomID,
		machinesRoomID:    machinesRoomID,
		servicesRoomID:    servicesRoomID,
		launcherSocket:    "/nonexistent/launcher.sock",
		statusInterval:    time.Hour,
		running:           make(map[string]bool),
		lastCredentials:   make(map[string]string),
		lastVisibility:    make(map[string][]string),
		lastObservePolicy: make(map[string]*schema.ObservePolicy),
		services:          make(map[string]*schema.Service),
		proxyRoutes:       make(map[string]string),
		peerAddresses:     make(map[string]string),
		peerTransports:    make(map[string]http.RoundTripper),
		layoutWatchers:    make(map[string]*layoutWatcher),
		logger:            slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
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
