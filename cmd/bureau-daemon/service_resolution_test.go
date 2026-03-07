// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ipc"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/workspace"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestReconcile_ServiceMountsResolved verifies that when a principal's
// template declares RequiredServices, the daemon resolves them to
// ServiceMounts on the IPC request. The service binding comes from an
// m.bureau.service_binding state event in the config room.
func TestReconcile_ServiceMountsResolved(t *testing.T) {
	t.Parallel()

	const (
		configRoomID   = "!config:test.local"
		templateRoomID = "!template:test.local"
	)

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	machineName := daemon.machine.UserID().StateKey()

	matrixState := newMockMatrixState()
	matrixState.setRoomAlias(daemon.fleet.Namespace().TemplateRoomAlias(), templateRoomID)

	// Template that requires the "ticket" service.
	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "service-consumer", schema.TemplateContent{
		Command:          []string{"/bin/echo", "hello"},
		RequiredServices: []string{"ticket"},
	})

	// Bind "ticket" service to a provider principal in the config room.
	ticketEntity := testEntity(t, daemon.fleet, "service/ticket")
	matrixState.setStateEvent(configRoomID, schema.EventTypeServiceBinding, "ticket", schema.ServiceBindingContent{
		Principal: ticketEntity,
	})

	// Register the service in the in-memory directory so that
	// resolveServiceMounts can check locality (local vs remote).
	daemon.services[ticketEntity.UserID().StateKey()] = &schema.Service{
		Principal: ticketEntity,
		Machine:   daemon.machine,
		Endpoints: map[string]string{"cbor": "service.sock"},
	}

	// Principal assignment.
	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, daemon.fleet, "iree/amdgpu/pm"),
				Template:  "bureau/template:service-consumer",
				AutoStart: true,
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID().StateKey(), schema.Credentials{
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
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
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
		if request.Action == ipc.ActionCreateSandbox {
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

	daemon.runDir = principal.DefaultRunDir
	daemon.fleetRunDir = daemon.fleet.RunDir(daemon.runDir)
	daemon.stateDir = t.TempDir()
	daemon.session = session
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.tokenSigningPrivateKey = signingKey
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
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

	if !daemon.isAlive(testEntity(t, daemon.fleet, "iree/amdgpu/pm")) {
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
	expectedSocket := ticketEntity.ServiceSocketPath(daemon.fleetRunDir)
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
	)

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	machineName := daemon.machine.UserID().StateKey()

	matrixState := newMockMatrixState()
	matrixState.setRoomAlias(daemon.fleet.Namespace().TemplateRoomAlias(), templateRoomID)

	// Template requires "ticket" but no m.bureau.service_binding exists.
	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "needs-ticket", schema.TemplateContent{
		Command:          []string{"/bin/echo", "hello"},
		RequiredServices: []string{"ticket"},
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, daemon.fleet, "iree/amdgpu/pm"),
				Template:  "bureau/template:needs-ticket",
				AutoStart: true,
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID().StateKey(), schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	tracker, cleanup := newServiceResolutionTestDaemon(t, daemon, matrixState, configRoomID)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	// Principal should NOT be running — the required service could not be resolved.
	if daemon.isAlive(testEntity(t, daemon.fleet, "iree/amdgpu/pm")) {
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
	)

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	machineName := daemon.machine.UserID().StateKey()

	matrixState := newMockMatrixState()
	matrixState.setRoomAlias(daemon.fleet.Namespace().TemplateRoomAlias(), templateRoomID)
	matrixState.setRoomAlias(ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"), workspaceRoomID)

	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "ws-consumer", schema.TemplateContent{
		Command:          []string{"/bin/echo", "hello"},
		RequiredServices: []string{"ticket"},
	})

	// Bind "ticket" in config room to one principal.
	globalTicketEntity := testEntity(t, daemon.fleet, "service/ticket/global")
	matrixState.setStateEvent(configRoomID, schema.EventTypeServiceBinding, "ticket", schema.ServiceBindingContent{
		Principal: globalTicketEntity,
	})

	// Bind "ticket" in workspace room to a different principal. This should win.
	wsTicketEntity := testEntity(t, daemon.fleet, "service/ticket/workspace")
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeServiceBinding, "ticket", schema.ServiceBindingContent{
		Principal: wsTicketEntity,
	})

	// Register both service principals in the in-memory directory.
	daemon.services[globalTicketEntity.UserID().StateKey()] = &schema.Service{
		Principal: globalTicketEntity,
		Machine:   daemon.machine,
		Endpoints: map[string]string{"cbor": "service.sock"},
	}
	daemon.services[wsTicketEntity.UserID().StateKey()] = &schema.Service{
		Principal: wsTicketEntity,
		Machine:   daemon.machine,
		Endpoints: map[string]string{"cbor": "service.sock"},
	}

	// Workspace event so the StartCondition resolves and gives us the workspace room ID.
	matrixState.setStateEvent(workspaceRoomID, schema.EventTypeWorkspace, "", workspace.WorkspaceState{
		Status:    workspace.WorkspaceStatusActive,
		Project:   "iree",
		Machine:   machineName,
		UpdatedAt: "2026-02-10T00:00:00Z",
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, daemon.fleet, "iree/amdgpu/pm"),
				Template:  "bureau/template:ws-consumer",
				AutoStart: true,
				StartCondition: &schema.StartCondition{
					EventType:    schema.EventTypeWorkspace,
					StateKey:     "",
					RoomAlias:    ref.MustParseRoomAlias("#iree/amdgpu/inference:test.local"),
					ContentMatch: schema.ContentMatch{"status": schema.Eq("active")},
				},
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID().StateKey(), schema.Credentials{
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
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
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
		if request.Action == ipc.ActionCreateSandbox {
			capturedMounts = request.ServiceMounts
		}
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	_, signingKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	daemon.runDir = principal.DefaultRunDir
	daemon.fleetRunDir = daemon.fleet.RunDir(daemon.runDir)
	daemon.stateDir = t.TempDir()
	daemon.session = session
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.tokenSigningPrivateKey = signingKey
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
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

	if !daemon.isAlive(testEntity(t, daemon.fleet, "iree/amdgpu/pm")) {
		t.Fatal("principal should be running")
	}

	mountsMu.Lock()
	defer mountsMu.Unlock()

	if len(capturedMounts) != 1 {
		t.Fatalf("expected 1 service mount, got %d", len(capturedMounts))
	}

	// Workspace room binding should win over config room binding.
	expectedSocket := wsTicketEntity.ServiceSocketPath(daemon.fleetRunDir)
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
	)

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	machineName := daemon.machine.UserID().StateKey()

	matrixState := newMockMatrixState()
	matrixState.setRoomAlias(daemon.fleet.Namespace().TemplateRoomAlias(), templateRoomID)

	// Template requires both "ticket" and "rag" services.
	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "multi-service", schema.TemplateContent{
		Command:          []string{"/bin/echo", "hello"},
		RequiredServices: []string{"ticket", "rag"},
	})

	// Bind both services in the config room.
	ticketEntity := testEntity(t, daemon.fleet, "service/ticket")
	matrixState.setStateEvent(configRoomID, schema.EventTypeServiceBinding, "ticket", schema.ServiceBindingContent{
		Principal: ticketEntity,
	})
	ragEntity := testEntity(t, daemon.fleet, "service/rag")
	matrixState.setStateEvent(configRoomID, schema.EventTypeServiceBinding, "rag", schema.ServiceBindingContent{
		Principal: ragEntity,
	})

	// Register both services in the in-memory directory.
	daemon.services[ticketEntity.UserID().StateKey()] = &schema.Service{
		Principal: ticketEntity,
		Machine:   daemon.machine,
		Endpoints: map[string]string{"cbor": "service.sock"},
	}
	daemon.services[ragEntity.UserID().StateKey()] = &schema.Service{
		Principal: ragEntity,
		Machine:   daemon.machine,
		Endpoints: map[string]string{"cbor": "service.sock"},
	}

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, daemon.fleet, "iree/amdgpu/pm"),
				Template:  "bureau/template:multi-service",
				AutoStart: true,
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID().StateKey(), schema.Credentials{
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
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
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
		if request.Action == ipc.ActionCreateSandbox {
			capturedMounts = request.ServiceMounts
		}
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	_, signingKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	daemon.runDir = principal.DefaultRunDir
	daemon.fleetRunDir = daemon.fleet.RunDir(daemon.runDir)
	daemon.stateDir = t.TempDir()
	daemon.session = session
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.tokenSigningPrivateKey = signingKey
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
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

	if !daemon.isAlive(testEntity(t, daemon.fleet, "iree/amdgpu/pm")) {
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

	ticketSocket := ticketEntity.ServiceSocketPath(daemon.fleetRunDir)
	if mountsByRole["ticket"] != ticketSocket {
		t.Errorf("ticket socket = %q, want %q", mountsByRole["ticket"], ticketSocket)
	}

	ragSocket := ragEntity.ServiceSocketPath(daemon.fleetRunDir)
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
	)

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	machineName := daemon.machine.UserID().StateKey()

	matrixState := newMockMatrixState()
	matrixState.setRoomAlias(daemon.fleet.Namespace().TemplateRoomAlias(), templateRoomID)

	// Template requires both "ticket" and "rag", but only "ticket" is bound.
	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "partial-services", schema.TemplateContent{
		Command:          []string{"/bin/echo", "hello"},
		RequiredServices: []string{"ticket", "rag"},
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeServiceBinding, "ticket", schema.ServiceBindingContent{
		Principal: testEntity(t, daemon.fleet, "service/ticket"),
	})
	// "rag" is intentionally NOT bound.

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, daemon.fleet, "iree/amdgpu/pm"),
				Template:  "bureau/template:partial-services",
				AutoStart: true,
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID().StateKey(), schema.Credentials{
		Ciphertext: "encrypted-test-credentials",
	})

	tracker, cleanup := newServiceResolutionTestDaemon(t, daemon, matrixState, configRoomID)
	defer cleanup()

	if err := daemon.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if daemon.isAlive(testEntity(t, daemon.fleet, "iree/amdgpu/pm")) {
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
	)

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	machineName := daemon.machine.UserID().StateKey()

	matrixState := newMockMatrixState()
	matrixState.setRoomAlias(daemon.fleet.Namespace().TemplateRoomAlias(), templateRoomID)

	// Template with no RequiredServices.
	matrixState.setStateEvent(templateRoomID, schema.EventTypeTemplate, "plain-template", schema.TemplateContent{
		Command: []string{"/bin/echo", "hello"},
	})

	matrixState.setStateEvent(configRoomID, schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Principal: testEntity(t, daemon.fleet, "iree/amdgpu/pm"),
				Template:  "bureau/template:plain-template",
				AutoStart: true,
			},
		},
	})
	matrixState.setStateEvent(configRoomID, schema.EventTypeCredentials, testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID().StateKey(), schema.Credentials{
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
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
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
		if request.Action == ipc.ActionCreateSandbox {
			capturedMounts = request.ServiceMounts
		}
		return launcherIPCResponse{OK: true, ProxyPID: 99999}
	})
	t.Cleanup(func() { listener.Close() })

	_, signingKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	daemon.runDir = principal.DefaultRunDir
	daemon.fleetRunDir = daemon.fleet.RunDir(daemon.runDir)
	daemon.stateDir = t.TempDir()
	daemon.session = session
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.tokenSigningPrivateKey = signingKey
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
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

	if !daemon.isAlive(testEntity(t, daemon.fleet, "iree/amdgpu/pm")) {
		t.Fatal("principal should be running (no required services)")
	}

	mountsMu.Lock()
	defer mountsMu.Unlock()

	if len(capturedMounts) != 0 {
		t.Errorf("expected no service mounts, got %v", capturedMounts)
	}
}

// newServiceResolutionTestDaemon configures a pre-created Daemon for
// service resolution tests using the shared mock launcher pattern.
// The daemon must already have machine and fleet set via testMachineSetup.
func newServiceResolutionTestDaemon(t *testing.T, daemon *Daemon, matrixState *mockMatrixState, configRoomID string) (*principalTracker, func()) {
	t.Helper()

	matrixServer := httptest.NewServer(matrixState.handler())

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
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
		case ipc.ActionCreateSandbox:
			tracker.created = append(tracker.created, request.Principal)
			return launcherIPCResponse{OK: true, ProxyPID: 99999}
		case ipc.ActionDestroySandbox:
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

	daemon.runDir = principal.DefaultRunDir
	daemon.fleetRunDir = daemon.fleet.RunDir(daemon.runDir)
	daemon.stateDir = t.TempDir()
	daemon.session = session
	daemon.configRoomID = mustRoomID(configRoomID)
	daemon.launcherSocket = launcherSocket
	daemon.tokenSigningPrivateKey = tokenSigningPrivateKey
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.adminSocketPathFunc = func(principal ref.Entity) string {
		return filepath.Join(socketDir, principal.AccountLocalpart()+".admin.sock")
	}
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

	return tracker, cleanup
}

// TestParseServiceSpec verifies that service specs are correctly split
// into role and endpoint parts.
func TestParseServiceSpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		spec         string
		wantRole     string
		wantEndpoint string
	}{
		{spec: "ticket", wantRole: "ticket", wantEndpoint: ""},
		{spec: "model:http", wantRole: "model", wantEndpoint: "http"},
		{spec: "stt/whisper", wantRole: "stt/whisper", wantEndpoint: ""},
		{spec: "stt/whisper:grpc", wantRole: "stt/whisper", wantEndpoint: "grpc"},
	}

	for _, test := range tests {
		role, endpoint := parseServiceSpec(test.spec)
		if role != test.wantRole {
			t.Errorf("parseServiceSpec(%q) role = %q, want %q", test.spec, role, test.wantRole)
		}
		if endpoint != test.wantEndpoint {
			t.Errorf("parseServiceSpec(%q) endpoint = %q, want %q", test.spec, endpoint, test.wantEndpoint)
		}
	}
}

// TestResolveEndpointSocket verifies that the daemon correctly resolves
// endpoint-specific socket paths from the in-memory service directory.
func TestResolveEndpointSocket(t *testing.T) {
	t.Parallel()

	t.Run("default cbor endpoint", func(t *testing.T) {
		t.Parallel()
		daemon, _ := newTestDaemon(t)
		daemon.machine, daemon.fleet = testMachineSetup(t, "workstation", "bureau.local")
		daemon.fleetRunDir = daemon.fleet.RunDir(daemon.runDir)

		serviceRef, err := ref.NewService(daemon.fleet, "stt/whisper")
		if err != nil {
			t.Fatalf("construct service ref: %v", err)
		}
		principal := serviceRef.Entity()

		// Empty endpoint returns the canonical ServiceSocketPath.
		socketPath, err := daemon.resolveEndpointSocket(principal, "")
		if err != nil {
			t.Fatalf("resolveEndpointSocket empty: %v", err)
		}
		wantPath := principal.ServiceSocketPath(daemon.fleetRunDir)
		if socketPath != wantPath {
			t.Errorf("socket = %q, want %q", socketPath, wantPath)
		}

		// Explicit "cbor" also returns the canonical path.
		socketPath, err = daemon.resolveEndpointSocket(principal, "cbor")
		if err != nil {
			t.Fatalf("resolveEndpointSocket cbor: %v", err)
		}
		if socketPath != wantPath {
			t.Errorf("socket = %q, want %q", socketPath, wantPath)
		}
	})

	t.Run("named endpoint via symlink", func(t *testing.T) {
		t.Parallel()
		daemon, _ := newTestDaemon(t)
		daemon.machine, daemon.fleet = testMachineSetup(t, "workstation", "bureau.local")

		// Use a real temp dir for fleetRunDir so we can create symlinks.
		runDir := t.TempDir()
		daemon.runDir = runDir
		daemon.fleetRunDir = daemon.fleet.RunDir(runDir)

		serviceRef, err := ref.NewService(daemon.fleet, "model/gpt")
		if err != nil {
			t.Fatalf("construct service ref: %v", err)
		}
		servicePrincipal := serviceRef.Entity()

		// Register the service with multiple endpoints.
		daemon.services[servicePrincipal.UserID().StateKey()] = &schema.Service{
			Principal: servicePrincipal,
			Machine:   daemon.machine,
			Endpoints: map[string]string{
				"cbor": "service.sock",
				"http": "http.sock",
			},
		}

		// Set up the filesystem: create listen dir, symlink from
		// ServiceSocketPath → listenDir/service.sock.
		listenDir := t.TempDir()
		cborSocket := filepath.Join(listenDir, "service.sock")
		httpSocket := filepath.Join(listenDir, "http.sock")
		os.WriteFile(cborSocket, nil, 0600)
		os.WriteFile(httpSocket, nil, 0600)

		symlinkPath := servicePrincipal.ServiceSocketPath(daemon.fleetRunDir)
		if err := os.MkdirAll(filepath.Dir(symlinkPath), 0755); err != nil {
			t.Fatalf("create symlink parent dir: %v", err)
		}
		if err := os.Symlink(cborSocket, symlinkPath); err != nil {
			t.Fatalf("create symlink: %v", err)
		}

		socketPath, err := daemon.resolveEndpointSocket(servicePrincipal, "http")
		if err != nil {
			t.Fatalf("resolveEndpointSocket http: %v", err)
		}
		if socketPath != httpSocket {
			t.Errorf("socket = %q, want %q", socketPath, httpSocket)
		}
	})

	t.Run("unknown endpoint errors", func(t *testing.T) {
		t.Parallel()
		daemon, _ := newTestDaemon(t)
		daemon.machine, daemon.fleet = testMachineSetup(t, "workstation", "bureau.local")
		daemon.fleetRunDir = daemon.fleet.RunDir(daemon.runDir)

		serviceRef, err := ref.NewService(daemon.fleet, "ticket")
		if err != nil {
			t.Fatalf("construct service ref: %v", err)
		}
		principal := serviceRef.Entity()

		daemon.services[principal.UserID().StateKey()] = &schema.Service{
			Principal: principal,
			Machine:   daemon.machine,
			Endpoints: map[string]string{"cbor": "service.sock"},
		}

		_, err = daemon.resolveEndpointSocket(principal, "grpc")
		if err == nil {
			t.Fatal("expected error for unknown endpoint, got nil")
		}
		if !strings.Contains(err.Error(), "does not declare endpoint") {
			t.Errorf("error = %v, want 'does not declare endpoint'", err)
		}
	})

	t.Run("service not in directory", func(t *testing.T) {
		t.Parallel()
		daemon, _ := newTestDaemon(t)
		daemon.machine, daemon.fleet = testMachineSetup(t, "workstation", "bureau.local")
		daemon.fleetRunDir = daemon.fleet.RunDir(daemon.runDir)

		serviceRef, err := ref.NewService(daemon.fleet, "unknown/service")
		if err != nil {
			t.Fatalf("construct service ref: %v", err)
		}
		principal := serviceRef.Entity()

		_, err = daemon.resolveEndpointSocket(principal, "http")
		if err == nil {
			t.Fatal("expected error for missing service, got nil")
		}
		if !strings.Contains(err.Error(), "not yet in the service directory") {
			t.Errorf("error = %v, want 'not yet in the service directory'", err)
		}
	})
}

// TestResolveRemoteServiceMount verifies that resolveRemoteServiceMount
// creates a tunnel socket when given a remote service with transport
// configured and a known peer address.
func TestResolveRemoteServiceMount(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "workstation", "bureau.local")
	daemon.runDir = t.TempDir()
	daemon.fleetRunDir = daemon.fleet.RunDir(daemon.runDir)
	daemon.operatorsGID = -1
	daemon.transportDialer = &testTCPDialer{}

	// Create a remote machine ref.
	remoteMachine, err := ref.NewMachine(daemon.fleet, "gpu-box")
	if err != nil {
		t.Fatalf("construct remote machine: %v", err)
	}

	serviceRef, err := ref.NewService(daemon.fleet, "model/gpt")
	if err != nil {
		t.Fatalf("construct service ref: %v", err)
	}
	servicePrincipal := serviceRef.Entity()

	service := &schema.Service{
		Principal: servicePrincipal,
		Machine:   remoteMachine,
		Endpoints: map[string]string{
			"cbor": "service.sock",
			"http": "http.sock",
		},
	}

	// Set peer address so transport can find the remote machine.
	daemon.peerAddresses[remoteMachine.UserID().String()] = "127.0.0.1:9999"

	t.Run("default endpoint", func(t *testing.T) {
		socketPath, err := daemon.resolveRemoteServiceMount(servicePrincipal, service, "model", "")
		if err != nil {
			t.Fatalf("resolveRemoteServiceMount: %v", err)
		}

		wantSocket := filepath.Join(daemon.runDir, "tunnel", "service-model.sock")
		if socketPath != wantSocket {
			t.Errorf("socket = %q, want %q", socketPath, wantSocket)
		}

		// Tunnel should be tracked.
		if _, ok := daemon.tunnels["service/model"]; !ok {
			t.Error("expected tunnel 'service/model' to exist")
		}
		if !daemon.activeServiceTunnels["service/model"] {
			t.Error("expected tunnel 'service/model' to be marked active")
		}

		daemon.stopTunnel("service/model")
	})

	t.Run("named endpoint", func(t *testing.T) {
		socketPath, err := daemon.resolveRemoteServiceMount(servicePrincipal, service, "model", "http")
		if err != nil {
			t.Fatalf("resolveRemoteServiceMount: %v", err)
		}

		wantSocket := filepath.Join(daemon.runDir, "tunnel", "service-model-http.sock")
		if socketPath != wantSocket {
			t.Errorf("socket = %q, want %q", socketPath, wantSocket)
		}

		if _, ok := daemon.tunnels["service/model-http"]; !ok {
			t.Error("expected tunnel 'service/model-http' to exist")
		}
		if !daemon.activeServiceTunnels["service/model-http"] {
			t.Error("expected tunnel 'service/model-http' to be marked active")
		}

		daemon.stopTunnel("service/model-http")
	})

	t.Run("unknown endpoint", func(t *testing.T) {
		_, err := daemon.resolveRemoteServiceMount(servicePrincipal, service, "model", "grpc")
		if err == nil {
			t.Fatal("expected error for unknown endpoint")
		}
		if !strings.Contains(err.Error(), "does not declare endpoint") {
			t.Errorf("error = %v, want 'does not declare endpoint'", err)
		}
	})

	t.Run("role with slashes sanitized in socket filename", func(t *testing.T) {
		// Roles like "stt/whisper" should produce flat socket filenames
		// ("service-stt-whisper.sock") not nested directory paths.
		socketPath, err := daemon.resolveRemoteServiceMount(servicePrincipal, service, "stt/whisper", "")
		if err != nil {
			t.Fatalf("resolveRemoteServiceMount: %v", err)
		}

		wantSocket := filepath.Join(daemon.runDir, "tunnel", "service-stt-whisper.sock")
		if socketPath != wantSocket {
			t.Errorf("socket = %q, want %q", socketPath, wantSocket)
		}

		// Tunnel map key preserves slashes (it's a map key, not a path).
		if _, ok := daemon.tunnels["service/stt/whisper"]; !ok {
			t.Error("expected tunnel 'service/stt/whisper' to exist")
		}

		daemon.stopTunnel("service/stt/whisper")
	})
}

// TestResolveRemoteServiceMount_NoTransport verifies that resolving a
// remote service without transport configured returns an error.
func TestResolveRemoteServiceMount_NoTransport(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "workstation", "bureau.local")
	daemon.runDir = t.TempDir()
	// transportDialer is nil (not configured).

	remoteMachine, err := ref.NewMachine(daemon.fleet, "gpu-box")
	if err != nil {
		t.Fatalf("construct remote machine: %v", err)
	}
	serviceRef, err := ref.NewService(daemon.fleet, "ticket")
	if err != nil {
		t.Fatalf("construct service ref: %v", err)
	}

	service := &schema.Service{
		Principal: serviceRef.Entity(),
		Machine:   remoteMachine,
		Endpoints: map[string]string{"cbor": "service.sock"},
	}

	_, err = daemon.resolveRemoteServiceMount(serviceRef.Entity(), service, "ticket", "")
	if err == nil {
		t.Fatal("expected error when transport is not configured")
	}
	if !strings.Contains(err.Error(), "transport is not configured") {
		t.Errorf("error = %v, want 'transport is not configured'", err)
	}
}

// TestResolveRemoteServiceMount_NoPeerAddress verifies that resolving a
// remote service without a known peer address returns an error.
func TestResolveRemoteServiceMount_NoPeerAddress(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "workstation", "bureau.local")
	daemon.runDir = t.TempDir()
	daemon.transportDialer = &testTCPDialer{}
	// peerAddresses is empty (no address known for remote machine).

	remoteMachine, err := ref.NewMachine(daemon.fleet, "gpu-box")
	if err != nil {
		t.Fatalf("construct remote machine: %v", err)
	}
	serviceRef, err := ref.NewService(daemon.fleet, "ticket")
	if err != nil {
		t.Fatalf("construct service ref: %v", err)
	}

	service := &schema.Service{
		Principal: serviceRef.Entity(),
		Machine:   remoteMachine,
		Endpoints: map[string]string{"cbor": "service.sock"},
	}

	_, err = daemon.resolveRemoteServiceMount(serviceRef.Entity(), service, "ticket", "")
	if err == nil {
		t.Fatal("expected error when no peer address is known")
	}
	if !strings.Contains(err.Error(), "no transport address is known") {
		t.Errorf("error = %v, want 'no transport address is known'", err)
	}
}

// TestCleanupServiceTunnels verifies that service tunnels not in the
// activeServiceTunnels set are stopped, while non-service tunnels and
// active service tunnels are preserved.
func TestCleanupServiceTunnels(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.machine, daemon.fleet = testMachineSetup(t, "workstation", "bureau.local")
	daemon.runDir = t.TempDir()
	daemon.operatorsGID = -1
	daemon.transportDialer = &testTCPDialer{}

	socketDir := testutil.SocketDir(t)

	// Create three tunnels: one active service, one stale service, one non-service.
	activePath := filepath.Join(socketDir, "active.sock")
	stalePath := filepath.Join(socketDir, "stale.sock")
	upstreamPath := filepath.Join(socketDir, "upstream.sock")

	if err := daemon.startTunnel("service/model", "service/model/gpt", "", "127.0.0.1:9999", activePath); err != nil {
		t.Fatalf("start active tunnel: %v", err)
	}
	if err := daemon.startTunnel("service/ticket", "service/ticket", "", "127.0.0.1:9999", stalePath); err != nil {
		t.Fatalf("start stale tunnel: %v", err)
	}
	if err := daemon.startTunnel("upstream", "service/artifact", "", "127.0.0.1:9999", upstreamPath); err != nil {
		t.Fatalf("start upstream tunnel: %v", err)
	}

	if len(daemon.tunnels) != 3 {
		t.Fatalf("expected 3 tunnels, got %d", len(daemon.tunnels))
	}

	// Mark only "service/model" as active.
	daemon.activeServiceTunnels["service/model"] = true

	daemon.cleanupServiceTunnels()

	// "service/model" should remain (active).
	if _, ok := daemon.tunnels["service/model"]; !ok {
		t.Error("active service tunnel 'service/model' should not be removed")
	}
	// "service/ticket" should be removed (stale).
	if _, ok := daemon.tunnels["service/ticket"]; ok {
		t.Error("stale service tunnel 'service/ticket' should be removed")
	}
	// "upstream" should remain (not a service tunnel).
	if _, ok := daemon.tunnels["upstream"]; !ok {
		t.Error("non-service tunnel 'upstream' should not be removed")
	}

	// Clean up remaining tunnels.
	daemon.stopAllTunnels()
}
