// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"

	"net/http/httptest"
)

// TestReconcile_StartConditionMet verifies that a principal with a
// satisfied StartCondition launches normally.
func TestReconcile_StartConditionMet(t *testing.T) {
	t.Parallel()

	const (
		configRoomID    = "!config:test.local"
		templateRoomID  = "!templates:test.local"
		workspaceRoomID = "!workspace:test.local"
		serverName      = "test.local"
		machineName     = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)

	// Set up the workspace room with a ready event (condition is met).
	matrixState.setRoomAlias("#iree/amdgpu/inference:test.local", workspaceRoomID)
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspaceReady, "", schema.WorkspaceReady{
		SetupPrincipal: "iree/setup",
		CompletedAt:    "2026-02-10T00:00:00Z",
	})

	// Configure a principal that gates on workspace.ready.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/templates:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType: schema.EventTypeWorkspaceReady,
					StateKey:  "",
					RoomAlias: "#iree/amdgpu/inference:test.local",
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, createdPrincipals, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	createdPrincipals.mu.Lock()
	defer createdPrincipals.mu.Unlock()

	if len(createdPrincipals.names) != 1 || createdPrincipals.names[0] != "iree/amdgpu/pm" {
		t.Errorf("expected create-sandbox for iree/amdgpu/pm, got %v", createdPrincipals.names)
	}
	if !daemon.running["iree/amdgpu/pm"] {
		t.Error("principal should be running after start condition is met")
	}
}

// TestReconcile_StartConditionNotMet verifies that a principal with an
// unsatisfied StartCondition is deferred (no create-sandbox call).
func TestReconcile_StartConditionNotMet(t *testing.T) {
	t.Parallel()

	const (
		configRoomID    = "!config:test.local"
		templateRoomID  = "!templates:test.local"
		workspaceRoomID = "!workspace:test.local"
		serverName      = "test.local"
		machineName     = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)

	// Set up the workspace room alias but do NOT publish the ready event.
	matrixState.setRoomAlias("#iree/amdgpu/inference:test.local", workspaceRoomID)

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/templates:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType: schema.EventTypeWorkspaceReady,
					StateKey:  "",
					RoomAlias: "#iree/amdgpu/inference:test.local",
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, createdPrincipals, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	createdPrincipals.mu.Lock()
	defer createdPrincipals.mu.Unlock()

	if len(createdPrincipals.names) != 0 {
		t.Errorf("expected no create-sandbox calls (condition not met), got %v", createdPrincipals.names)
	}
	if daemon.running["iree/amdgpu/pm"] {
		t.Error("principal should not be running when start condition is not met")
	}
}

// TestReconcile_StartConditionDeferredThenLaunches verifies the lifecycle:
// first reconcile defers the principal (event missing), then the event appears,
// and the second reconcile launches it.
func TestReconcile_StartConditionDeferredThenLaunches(t *testing.T) {
	t.Parallel()

	const (
		configRoomID    = "!config:test.local"
		templateRoomID  = "!templates:test.local"
		workspaceRoomID = "!workspace:test.local"
		serverName      = "test.local"
		machineName     = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)
	matrixState.setRoomAlias("#iree/amdgpu/inference:test.local", workspaceRoomID)

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/templates:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType: schema.EventTypeWorkspaceReady,
					StateKey:  "",
					RoomAlias: "#iree/amdgpu/inference:test.local",
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, createdPrincipals, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	// First reconcile: event missing → deferred.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile() error: %v", err)
	}

	createdPrincipals.mu.Lock()
	if len(createdPrincipals.names) != 0 {
		t.Errorf("first reconcile: expected no create-sandbox calls, got %v", createdPrincipals.names)
	}
	createdPrincipals.mu.Unlock()

	// Simulate the setup principal publishing workspace.ready.
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspaceReady, "", schema.WorkspaceReady{
		SetupPrincipal: "iree/setup",
		CompletedAt:    "2026-02-10T01:00:00Z",
	})

	// Second reconcile: event now exists → launches.
	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("second reconcile() error: %v", err)
	}

	createdPrincipals.mu.Lock()
	defer createdPrincipals.mu.Unlock()

	if len(createdPrincipals.names) != 1 || createdPrincipals.names[0] != "iree/amdgpu/pm" {
		t.Errorf("second reconcile: expected create-sandbox for iree/amdgpu/pm, got %v", createdPrincipals.names)
	}
	if !daemon.running["iree/amdgpu/pm"] {
		t.Error("principal should be running after start condition becomes satisfied")
	}
}

// TestReconcile_NoStartCondition verifies that a principal without a
// StartCondition launches normally (no gating).
func TestReconcile_NoStartCondition(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!templates:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)

	// Principal with no StartCondition.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "agent/test",
				Template:  "bureau/templates:test-template",
				AutoStart: true,
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "agent/test", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, createdPrincipals, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	createdPrincipals.mu.Lock()
	defer createdPrincipals.mu.Unlock()

	if len(createdPrincipals.names) != 1 || createdPrincipals.names[0] != "agent/test" {
		t.Errorf("expected create-sandbox for agent/test, got %v", createdPrincipals.names)
	}
}

// TestReconcile_StartConditionConfigRoom verifies that a StartCondition with
// an empty RoomAlias checks the config room (where MachineConfig lives).
func TestReconcile_StartConditionConfigRoom(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!templates:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)

	// Principal gates on a custom state event in its own config room.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "agent/gated",
				Template:  "bureau/templates:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType: "m.bureau.approval",
					StateKey:  "agent/gated",
					// RoomAlias empty → checks configRoomID
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "agent/gated", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	// Put the approval event in the config room.
	matrixState.setStateEvent(configRoomID, "m.bureau.approval", "agent/gated", map[string]string{
		"approved_by": "bureau-admin",
	})

	daemon, createdPrincipals, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	createdPrincipals.mu.Lock()
	defer createdPrincipals.mu.Unlock()

	if len(createdPrincipals.names) != 1 || createdPrincipals.names[0] != "agent/gated" {
		t.Errorf("expected create-sandbox for agent/gated, got %v", createdPrincipals.names)
	}
}

// TestReconcile_StartConditionUnresolvableAlias verifies that when the
// room alias in a StartCondition doesn't resolve, the principal is deferred.
func TestReconcile_StartConditionUnresolvableAlias(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!templates:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	matrixState := newStartConditionTestState(t, configRoomID, templateRoomID, machineName)

	// Principal references a room alias that doesn't exist.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "agent/orphan",
				Template:  "bureau/templates:test-template",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType: schema.EventTypeWorkspaceReady,
					StateKey:  "",
					RoomAlias: "#nonexistent/room:test.local",
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "agent/orphan", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, createdPrincipals, cleanup := newStartConditionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	createdPrincipals.mu.Lock()
	defer createdPrincipals.mu.Unlock()

	if len(createdPrincipals.names) != 0 {
		t.Errorf("expected no create-sandbox calls (alias unresolvable), got %v", createdPrincipals.names)
	}
	if daemon.running["agent/orphan"] {
		t.Error("principal should not be running when room alias is unresolvable")
	}
}

// --- Test helpers ---

// createdPrincipalTracker is a thread-safe tracker for principals passed
// to create-sandbox IPC calls.
type createdPrincipalTracker struct {
	mu    sync.Mutex
	names []string
}

// newStartConditionTestState creates a mock Matrix state with a base template.
// The caller sets MachineConfig and StartCondition per test.
func newStartConditionTestState(t *testing.T, configRoomID, templateRoomID, machineName string) *mockMatrixState {
	t.Helper()

	state := newMockMatrixState()

	state.setRoomAlias("#bureau/templates:test.local", templateRoomID)
	state.setStateEvent(templateRoomID, schema.EventTypeTemplate, "test-template", schema.TemplateContent{
		Command: []string{"/bin/echo", "hello"},
	})

	return state
}

// newStartConditionTestDaemon creates a Daemon backed by a mock Matrix server
// and a mock launcher. Returns the daemon, a tracker for created principals,
// and a cleanup function.
func newStartConditionTestDaemon(t *testing.T, matrixState *mockMatrixState, configRoomID, serverName, machineName string) (*Daemon, *createdPrincipalTracker, func()) {
	t.Helper()

	matrixServer := httptest.NewServer(matrixState.handler())

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken("@"+machineName+":"+serverName, "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	tracker := &createdPrincipalTracker{}
	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		if request.Action == "create-sandbox" {
			tracker.mu.Lock()
			tracker.names = append(tracker.names, request.Principal)
			tracker.mu.Unlock()
			return launcherIPCResponse{OK: true, ProxyPID: 99999}
		}
		return launcherIPCResponse{OK: true}
	})

	daemon := &Daemon{
		session:             session,
		machineName:         machineName,
		serverName:          serverName,
		configRoomID:        configRoomID,
		launcherSocket:      launcherSocket,
		running:             make(map[string]bool),
		lastSpecs:           make(map[string]*schema.SandboxSpec),
		services:            make(map[string]*schema.Service),
		proxyRoutes:         make(map[string]string),
		adminSocketPathFunc: func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") },
		layoutWatchers:      make(map[string]*layoutWatcher),
		logger:              slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		prefetchFunc: func(ctx context.Context, storePath string) error {
			return nil
		},
	}

	cleanup := func() {
		daemon.stopAllLayoutWatchers()
		listener.Close()
		session.Close()
		matrixServer.Close()
	}

	return daemon, tracker, cleanup
}
