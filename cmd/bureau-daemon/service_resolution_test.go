// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestReconcile_ServiceMountsResolved verifies that when a principal's
// template declares RequiredServices, the daemon resolves them to
// ServiceMounts on the IPC request. The service binding comes from an
// m.bureau.room_service state event in the config room.
func TestReconcile_ServiceMountsResolved(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	matrixState := newMockMatrixState()
	matrixState.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), templateRoomID)

	// Template that requires the "ticket" service.
	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "service-consumer", schema.TemplateContent{
		Command:          []string{"/bin/echo", "hello"},
		RequiredServices: []string{"ticket"},
	})

	// Bind "ticket" service to a provider principal in the config room.
	matrixState.setStateEvent(configRoomID, schema.EventTypeRoomService, "ticket", schema.RoomServiceContent{
		Principal: "service/ticket",
	})

	// Principal assignment.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/template:service-consumer",
				AutoStart: true,
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	// Custom mock launcher that captures ServiceMounts.
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

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
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	var capturedMounts []launcherServiceMount
	var capturedTokenDir string
	var mountsMu sync.Mutex

	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		mountsMu.Lock()
		defer mountsMu.Unlock()
		if request.Action == "create-sandbox" {
			capturedMounts = request.ServiceMounts
			capturedTokenDir = request.TokenDirectory
		}
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	_, signingKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.stateDir = t.TempDir()
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.tokenSigningPrivateKey = signingKey
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.adminSocketPathFunc = func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") }
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return nil
	}
	t.Cleanup(func() {
		daemon.stopAllHealthMonitors()
		daemon.stopAllLayoutWatchers()
	})

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if !daemon.running["iree/amdgpu/pm"] {
		t.Fatal("principal should be running")
	}

	mountsMu.Lock()
	defer mountsMu.Unlock()

	if len(capturedMounts) != 1 {
		t.Fatalf("expected 1 service mount, got %d", len(capturedMounts))
	}
	if capturedMounts[0].Role != "ticket" {
		t.Errorf("mount role = %q, want %q", capturedMounts[0].Role, "ticket")
	}
	expectedSocket := principal.RunDirSocketPath(principal.DefaultRunDir, "service/ticket")
	if capturedMounts[0].SocketPath != expectedSocket {
		t.Errorf("mount socket = %q, want %q", capturedMounts[0].SocketPath, expectedSocket)
	}

	// Token directory should be set when RequiredServices are present.
	// It may be empty if the authorization index has no grants for this
	// principal, but the path should still be populated since the daemon
	// creates the directory unconditionally for any principal with
	// RequiredServices.
	if capturedTokenDir == "" {
		t.Error("expected TokenDirectory to be set in IPC request")
	}
}

// TestReconcile_ServiceMountsUnresolvable verifies that when a required
// service has no binding in any accessible room, the principal is skipped
// (not created). No silent fallback.
func TestReconcile_ServiceMountsUnresolvable(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	matrixState := newMockMatrixState()
	matrixState.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), templateRoomID)

	// Template requires "ticket" but no m.bureau.room_service binding exists.
	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "needs-ticket", schema.TemplateContent{
		Command:          []string{"/bin/echo", "hello"},
		RequiredServices: []string{"ticket"},
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/template:needs-ticket",
				AutoStart: true,
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newServiceResolutionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	// Principal should NOT be running — the required service could not be resolved.
	if daemon.running["iree/amdgpu/pm"] {
		t.Error("principal should not be running when a required service is unresolvable")
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if len(tracker.created) != 0 {
		t.Errorf("expected no create-sandbox calls, got %v", tracker.created)
	}
}

// TestReconcile_ServiceMountsWorkspaceRoom verifies that service bindings
// in a workspace room take priority over the config room. The daemon checks
// more specific scopes first.
func TestReconcile_ServiceMountsWorkspaceRoom(t *testing.T) {
	t.Parallel()

	const (
		configRoomID    = "!config:test.local"
		templateRoomID  = "!template:test.local"
		workspaceRoomID = "!workspace:test.local"
		serverName      = "test.local"
		machineName     = "machine/test"
	)

	matrixState := newMockMatrixState()
	matrixState.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), templateRoomID)
	matrixState.setRoomAlias("#iree/amdgpu/inference:test.local", workspaceRoomID)

	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "ws-consumer", schema.TemplateContent{
		Command:          []string{"/bin/echo", "hello"},
		RequiredServices: []string{"ticket"},
	})

	// Bind "ticket" in config room to one principal.
	matrixState.setStateEvent(configRoomID, schema.EventTypeRoomService, "ticket", schema.RoomServiceContent{
		Principal: "service/ticket/global",
	})

	// Bind "ticket" in workspace room to a different principal. This should win.
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeRoomService, "ticket", schema.RoomServiceContent{
		Principal: "service/ticket/workspace",
	})

	// Workspace event so the StartCondition resolves and gives us the workspace room ID.
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", schema.WorkspaceState{
		Status:    "active",
		Project:   "iree",
		Machine:   machineName,
		UpdatedAt: "2026-02-10T00:00:00Z",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/template:ws-consumer",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    "#iree/amdgpu/inference:test.local",
					ContentMatch: schema.ContentMatch{"status": schema.Eq("active")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	// Custom mock launcher to capture ServiceMounts.
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

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
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	var capturedMounts []launcherServiceMount
	var mountsMu sync.Mutex

	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		mountsMu.Lock()
		defer mountsMu.Unlock()
		if request.Action == "create-sandbox" {
			capturedMounts = request.ServiceMounts
		}
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	_, signingKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.stateDir = t.TempDir()
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.tokenSigningPrivateKey = signingKey
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.adminSocketPathFunc = func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") }
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return nil
	}
	t.Cleanup(func() {
		daemon.stopAllHealthMonitors()
		daemon.stopAllLayoutWatchers()
	})

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if !daemon.running["iree/amdgpu/pm"] {
		t.Fatal("principal should be running")
	}

	mountsMu.Lock()
	defer mountsMu.Unlock()

	if len(capturedMounts) != 1 {
		t.Fatalf("expected 1 service mount, got %d", len(capturedMounts))
	}

	// Workspace room binding should win over config room binding.
	expectedSocket := principal.RunDirSocketPath(principal.DefaultRunDir, "service/ticket/workspace")
	if capturedMounts[0].SocketPath != expectedSocket {
		t.Errorf("mount socket = %q, want %q (workspace binding should win)",
			capturedMounts[0].SocketPath, expectedSocket)
	}
}

// TestReconcile_ServiceMountsMultipleServices verifies that multiple
// required services are all resolved and passed as ServiceMounts.
func TestReconcile_ServiceMountsMultipleServices(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	matrixState := newMockMatrixState()
	matrixState.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), templateRoomID)

	// Template requires both "ticket" and "rag" services.
	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "multi-service", schema.TemplateContent{
		Command:          []string{"/bin/echo", "hello"},
		RequiredServices: []string{"ticket", "rag"},
	})

	// Bind both services in the config room.
	matrixState.setStateEvent(configRoomID, schema.EventTypeRoomService, "ticket", schema.RoomServiceContent{
		Principal: "service/ticket",
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeRoomService, "rag", schema.RoomServiceContent{
		Principal: "service/rag",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/template:multi-service",
				AutoStart: true,
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	// Custom mock launcher to capture ServiceMounts.
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

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
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	var capturedMounts []launcherServiceMount
	var mountsMu sync.Mutex

	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		mountsMu.Lock()
		defer mountsMu.Unlock()
		if request.Action == "create-sandbox" {
			capturedMounts = request.ServiceMounts
		}
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	_, signingKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.stateDir = t.TempDir()
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.tokenSigningPrivateKey = signingKey
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.adminSocketPathFunc = func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") }
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return nil
	}
	t.Cleanup(func() {
		daemon.stopAllHealthMonitors()
		daemon.stopAllLayoutWatchers()
	})

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if !daemon.running["iree/amdgpu/pm"] {
		t.Fatal("principal should be running")
	}

	mountsMu.Lock()
	defer mountsMu.Unlock()

	if len(capturedMounts) != 2 {
		t.Fatalf("expected 2 service mounts, got %d", len(capturedMounts))
	}

	// Build a map for order-independent comparison.
	mountsByRole := make(map[string]string, len(capturedMounts))
	for _, mount := range capturedMounts {
		mountsByRole[mount.Role] = mount.SocketPath
	}

	ticketSocket := principal.RunDirSocketPath(principal.DefaultRunDir, "service/ticket")
	if mountsByRole["ticket"] != ticketSocket {
		t.Errorf("ticket socket = %q, want %q", mountsByRole["ticket"], ticketSocket)
	}

	ragSocket := principal.RunDirSocketPath(principal.DefaultRunDir, "service/rag")
	if mountsByRole["rag"] != ragSocket {
		t.Errorf("rag socket = %q, want %q", mountsByRole["rag"], ragSocket)
	}
}

// TestReconcile_ServiceMountsPartialFailure verifies that when one of
// multiple required services is unresolvable, the entire principal is
// skipped (all-or-nothing, no partial binding).
func TestReconcile_ServiceMountsPartialFailure(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	matrixState := newMockMatrixState()
	matrixState.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), templateRoomID)

	// Template requires both "ticket" and "rag", but only "ticket" is bound.
	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "partial-services", schema.TemplateContent{
		Command:          []string{"/bin/echo", "hello"},
		RequiredServices: []string{"ticket", "rag"},
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeRoomService, "ticket", schema.RoomServiceContent{
		Principal: "service/ticket",
	})
	// "rag" is intentionally NOT bound.

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/template:partial-services",
				AutoStart: true,
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	daemon, tracker, cleanup := newServiceResolutionTestDaemon(t, matrixState, configRoomID, serverName, machineName)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if daemon.running["iree/amdgpu/pm"] {
		t.Error("principal should not be running when any required service is unresolvable")
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if len(tracker.created) != 0 {
		t.Errorf("expected no create-sandbox calls, got %v", tracker.created)
	}
}

// TestReconcile_NoServiceMountsWithoutRequiredServices verifies that
// principals without RequiredServices get no ServiceMounts (empty field).
func TestReconcile_NoServiceMountsWithoutRequiredServices(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
		serverName     = "test.local"
		machineName    = "machine/test"
	)

	matrixState := newMockMatrixState()
	matrixState.setRoomAlias(schema.FullRoomAlias(schema.RoomAliasTemplate, "test.local"), templateRoomID)

	// Template with no RequiredServices.
	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "plain-template", schema.TemplateContent{
		Command: []string{"/bin/echo", "hello"},
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "bureau/template:plain-template",
				AutoStart: true,
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, "iree/amdgpu/pm", schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	// Custom mock launcher to capture ServiceMounts.
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

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
	t.Cleanup(func() { session.Close() })

	socketDir := testutil.SocketDir(t)
	launcherSocket := filepath.Join(socketDir, "launcher.sock")

	var capturedMounts []launcherServiceMount
	var mountsMu sync.Mutex

	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		mountsMu.Lock()
		defer mountsMu.Unlock()
		if request.Action == "create-sandbox" {
			capturedMounts = request.ServiceMounts
		}
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	_, signingKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.stateDir = t.TempDir()
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.tokenSigningPrivateKey = signingKey
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.adminSocketPathFunc = func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") }
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return nil
	}
	t.Cleanup(func() {
		daemon.stopAllHealthMonitors()
		daemon.stopAllLayoutWatchers()
	})

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if !daemon.running["iree/amdgpu/pm"] {
		t.Fatal("principal should be running (no required services)")
	}

	mountsMu.Lock()
	defer mountsMu.Unlock()

	if len(capturedMounts) != 0 {
		t.Errorf("expected no service mounts, got %v", capturedMounts)
	}
}

// newServiceResolutionTestDaemon creates a Daemon for service resolution
// tests using the shared mock launcher pattern.
func newServiceResolutionTestDaemon(t *testing.T, matrixState *mockMatrixState, configRoomID, serverName, machineName string) (*Daemon, *principalTracker, func()) {
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

	tracker := &principalTracker{}
	listener := startMockLauncher(t, launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		switch request.Action {
		case "create-sandbox":
			tracker.created = append(tracker.created, request.Principal)
			return launcherIPCResponse{OK: true, ProxyPID: 99999}
		case "destroy-sandbox":
			tracker.destroyed = append(tracker.destroyed, request.Principal)
			return launcherIPCResponse{OK: true}
		default:
			return launcherIPCResponse{OK: true}
		}
	})

	_, tokenSigningPrivateKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.stateDir = t.TempDir()
	daemon.session = session
	daemon.machineName = machineName
	daemon.serverName = serverName
	daemon.configRoomID = configRoomID
	daemon.launcherSocket = launcherSocket
	daemon.tokenSigningPrivateKey = tokenSigningPrivateKey
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.adminSocketPathFunc = func(localpart string) string { return filepath.Join(socketDir, localpart+".admin.sock") }
	daemon.prefetchFunc = func(ctx context.Context, storePath string) error {
		return nil
	}

	cleanup := func() {
		daemon.stopAllHealthMonitors()
		daemon.stopAllLayoutWatchers()
		listener.Close()
		session.Close()
		matrixServer.Close()
	}

	return daemon, tracker, cleanup
}
