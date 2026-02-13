// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestAuthorizeObserveDefaultDeny(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	// No lastConfig — everything should be denied.
	authz := daemon.authorizeObserve("@ben:bureau.local", "iree/amdgpu/pm", "readwrite")
	if authz.Allowed {
		t.Error("expected default deny when lastConfig is nil")
	}
}

func TestAuthorizeObserveNoPolicyDeny(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.lastConfig = &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{Localpart: "iree/amdgpu/pm", AutoStart: true},
		},
		// No DefaultObservePolicy and no per-principal ObservePolicy.
	}
	authz := daemon.authorizeObserve("@ben:bureau.local", "iree/amdgpu/pm", "readwrite")
	if authz.Allowed {
		t.Error("expected deny when no ObservePolicy exists at any level")
	}
}

func TestAuthorizeObserveDefaultPolicyAllows(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.lastConfig = &schema.MachineConfig{
		DefaultObservePolicy: &schema.ObservePolicy{
			AllowedObservers: []string{"**"},
		},
		Principals: []schema.PrincipalAssignment{
			{Localpart: "iree/amdgpu/pm", AutoStart: true},
		},
	}
	authz := daemon.authorizeObserve("@ben:bureau.local", "iree/amdgpu/pm", "readonly")
	if !authz.Allowed {
		t.Error("expected allow with ** default policy")
	}
	if authz.GrantedMode != "readonly" {
		t.Errorf("GrantedMode = %q, want readonly", authz.GrantedMode)
	}
}

func TestAuthorizeObservePrincipalPolicyOverridesDefault(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.lastConfig = &schema.MachineConfig{
		DefaultObservePolicy: &schema.ObservePolicy{
			AllowedObservers: []string{"**"},
		},
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				AutoStart: true,
				ObservePolicy: &schema.ObservePolicy{
					// Principal-level policy only allows specific observer.
					AllowedObservers: []string{"ops/alice"},
				},
			},
		},
	}

	// ben is not in the principal's policy, even though default allows **.
	authz := daemon.authorizeObserve("@ben:bureau.local", "iree/amdgpu/pm", "readonly")
	if authz.Allowed {
		t.Error("expected deny: principal policy overrides default, ben not in AllowedObservers")
	}

	// alice matches the principal's policy.
	authz = daemon.authorizeObserve("@ops/alice:bureau.local", "iree/amdgpu/pm", "readonly")
	if !authz.Allowed {
		t.Error("expected allow: alice matches principal policy")
	}
}

func TestAuthorizeObserveModeDowngrade(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.lastConfig = &schema.MachineConfig{
		DefaultObservePolicy: &schema.ObservePolicy{
			AllowedObservers:   []string{"**"},
			ReadWriteObservers: []string{"ops/**"},
		},
		Principals: []schema.PrincipalAssignment{
			{Localpart: "iree/amdgpu/pm", AutoStart: true},
		},
	}

	// ben requests readwrite but is not in ReadWriteObservers — should
	// be downgraded to readonly.
	authz := daemon.authorizeObserve("@ben:bureau.local", "iree/amdgpu/pm", "readwrite")
	if !authz.Allowed {
		t.Fatal("expected allow")
	}
	if authz.GrantedMode != "readonly" {
		t.Errorf("GrantedMode = %q, want readonly (downgraded)", authz.GrantedMode)
	}

	// ops/alice requests readwrite and is in ReadWriteObservers — should get it.
	authz = daemon.authorizeObserve("@ops/alice:bureau.local", "iree/amdgpu/pm", "readwrite")
	if !authz.Allowed {
		t.Fatal("expected allow")
	}
	if authz.GrantedMode != "readwrite" {
		t.Errorf("GrantedMode = %q, want readwrite", authz.GrantedMode)
	}
}

func TestAuthorizeObserveReadonlyNotUpgraded(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.lastConfig = &schema.MachineConfig{
		DefaultObservePolicy: &schema.ObservePolicy{
			AllowedObservers:   []string{"**"},
			ReadWriteObservers: []string{"**"},
		},
		Principals: []schema.PrincipalAssignment{
			{Localpart: "iree/amdgpu/pm", AutoStart: true},
		},
	}

	// Even though observer has readwrite permission, requesting readonly
	// should stay readonly.
	authz := daemon.authorizeObserve("@ben:bureau.local", "iree/amdgpu/pm", "readonly")
	if !authz.Allowed {
		t.Fatal("expected allow")
	}
	if authz.GrantedMode != "readonly" {
		t.Errorf("GrantedMode = %q, want readonly (not upgraded)", authz.GrantedMode)
	}
}

func TestAuthorizeObserveGlobPatterns(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.lastConfig = &schema.MachineConfig{
		DefaultObservePolicy: &schema.ObservePolicy{
			AllowedObservers: []string{"ops/*"},
		},
		Principals: []schema.PrincipalAssignment{
			{Localpart: "iree/amdgpu/pm", AutoStart: true},
		},
	}

	// ops/alice matches ops/*
	authz := daemon.authorizeObserve("@ops/alice:bureau.local", "iree/amdgpu/pm", "readonly")
	if !authz.Allowed {
		t.Error("expected allow: ops/alice matches ops/*")
	}

	// ops/team/lead does NOT match ops/* (single segment only).
	authz = daemon.authorizeObserve("@ops/team/lead:bureau.local", "iree/amdgpu/pm", "readonly")
	if authz.Allowed {
		t.Error("expected deny: ops/team/lead should not match ops/* (single segment)")
	}

	// iree/builder does NOT match ops/*.
	authz = daemon.authorizeObserve("@iree/builder:bureau.local", "iree/amdgpu/pm", "readonly")
	if authz.Allowed {
		t.Error("expected deny: iree/builder does not match ops/*")
	}
}

func TestAuthorizeObserveUnknownPrincipal(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.lastConfig = &schema.MachineConfig{
		DefaultObservePolicy: &schema.ObservePolicy{
			AllowedObservers: []string{"**"},
		},
		// No principals listed — an unknown principal still uses
		// the default policy.
	}

	authz := daemon.authorizeObserve("@ben:bureau.local", "unknown/principal", "readonly")
	if !authz.Allowed {
		t.Error("expected allow: unknown principal should fall back to default policy")
	}
}

func TestAuthorizeListFiltering(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.lastConfig = &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				AutoStart: true,
				ObservePolicy: &schema.ObservePolicy{
					AllowedObservers: []string{"ops/**"},
				},
			},
			{
				Localpart: "iree/amdgpu/builder",
				AutoStart: true,
				ObservePolicy: &schema.ObservePolicy{
					AllowedObservers: []string{"**"},
				},
			},
		},
	}

	// ben can only see builder, not pm.
	if daemon.authorizeList("@ben:bureau.local", "iree/amdgpu/pm") {
		t.Error("ben should not be authorized to list iree/amdgpu/pm")
	}
	if !daemon.authorizeList("@ben:bureau.local", "iree/amdgpu/builder") {
		t.Error("ben should be authorized to list iree/amdgpu/builder")
	}

	// ops/alice can see both.
	if !daemon.authorizeList("@ops/alice:bureau.local", "iree/amdgpu/pm") {
		t.Error("ops/alice should be authorized to list iree/amdgpu/pm")
	}
	if !daemon.authorizeList("@ops/alice:bureau.local", "iree/amdgpu/builder") {
		t.Error("ops/alice should be authorized to list iree/amdgpu/builder")
	}
}

func TestFindObservePolicyFallback(t *testing.T) {
	t.Parallel()

	defaultPolicy := &schema.ObservePolicy{
		AllowedObservers: []string{"**"},
	}
	principalPolicy := &schema.ObservePolicy{
		AllowedObservers: []string{"ops/**"},
	}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.lastConfig = &schema.MachineConfig{
		DefaultObservePolicy: defaultPolicy,
		Principals: []schema.PrincipalAssignment{
			{
				Localpart:     "with-policy",
				ObservePolicy: principalPolicy,
			},
			{
				Localpart: "without-policy",
			},
		},
	}

	// Principal with its own policy should use it.
	result := daemon.findObservePolicy("with-policy")
	if result != principalPolicy {
		t.Error("expected principal-level policy for with-policy")
	}

	// Principal without its own policy should fall back to default.
	result = daemon.findObservePolicy("without-policy")
	if result != defaultPolicy {
		t.Error("expected default policy for without-policy")
	}

	// Unknown principal should also fall back to default.
	result = daemon.findObservePolicy("unknown")
	if result != defaultPolicy {
		t.Error("expected default policy for unknown principal")
	}
}

// mockConn is a minimal net.Conn that tracks whether Close was called.
// Used to verify that enforceObservePolicyChange terminates sessions
// by closing the client connection.
type mockConn struct {
	net.Conn // embed to satisfy interface; unused methods will panic

	mu     sync.Mutex
	closed bool
}

func (c *mockConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *mockConn) wasClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Satisfy the net.Conn interface methods that bridgeConnections or
// other callers might touch. These are never called in the unit tests
// below (enforceObservePolicyChange only calls Close), but providing
// them avoids nil-pointer panics if the test setup changes.
func (c *mockConn) LocalAddr() net.Addr              { return &net.UnixAddr{Name: "mock"} }
func (c *mockConn) RemoteAddr() net.Addr             { return &net.UnixAddr{Name: "mock"} }
func (c *mockConn) SetDeadline(time.Time) error      { return nil }
func (c *mockConn) SetReadDeadline(time.Time) error  { return nil }
func (c *mockConn) SetWriteDeadline(time.Time) error { return nil }
func (c *mockConn) Read([]byte) (int, error)         { return 0, nil }
func (c *mockConn) Write(b []byte) (int, error)      { return len(b), nil }

func TestEnforceObservePolicyChangeRevoke(t *testing.T) {
	t.Parallel()

	connection := &mockConn{}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// Policy initially allowed ops/** to observe, but has now been
	// changed to only allow ops/alice. The session below is for
	// ops/bob, who is no longer in AllowedObservers.
	daemon.lastConfig = &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				AutoStart: true,
				ObservePolicy: &schema.ObservePolicy{
					AllowedObservers: []string{"ops/alice"},
				},
			},
		},
	}
	daemon.observeSessions = []*activeObserveSession{
		{
			principal:   "iree/amdgpu/pm",
			observer:    "@ops/bob:bureau.local",
			grantedMode: "readonly",
			clientConn:  connection,
		},
	}

	daemon.enforceObservePolicyChange("iree/amdgpu/pm")

	if !connection.wasClosed() {
		t.Error("expected connection to be closed: ops/bob is no longer in AllowedObservers")
	}
}

func TestEnforceObservePolicyChangeRetain(t *testing.T) {
	t.Parallel()

	connection := &mockConn{}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// Policy allows ops/** — ops/bob is still authorized.
	daemon.lastConfig = &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				AutoStart: true,
				ObservePolicy: &schema.ObservePolicy{
					AllowedObservers: []string{"ops/**"},
				},
			},
		},
	}
	daemon.observeSessions = []*activeObserveSession{
		{
			principal:   "iree/amdgpu/pm",
			observer:    "@ops/bob:bureau.local",
			grantedMode: "readonly",
			clientConn:  connection,
		},
	}

	daemon.enforceObservePolicyChange("iree/amdgpu/pm")

	if connection.wasClosed() {
		t.Error("expected connection to remain open: ops/bob is still in AllowedObservers")
	}
}

func TestEnforceObservePolicyChangeModeDowngrade(t *testing.T) {
	t.Parallel()

	connection := &mockConn{}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// Policy still allows ops/** to observe, but ReadWriteObservers
	// has been narrowed to only ops/alice. The session below was
	// granted readwrite for ops/bob, who now would only get readonly.
	daemon.lastConfig = &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				AutoStart: true,
				ObservePolicy: &schema.ObservePolicy{
					AllowedObservers:   []string{"ops/**"},
					ReadWriteObservers: []string{"ops/alice"},
				},
			},
		},
	}
	daemon.observeSessions = []*activeObserveSession{
		{
			principal:   "iree/amdgpu/pm",
			observer:    "@ops/bob:bureau.local",
			grantedMode: "readwrite",
			clientConn:  connection,
		},
	}

	daemon.enforceObservePolicyChange("iree/amdgpu/pm")

	if !connection.wasClosed() {
		t.Error("expected connection to be closed: ops/bob's readwrite session would now be downgraded to readonly")
	}
}

func TestEnforceObservePolicyChangeOtherPrincipalUntouched(t *testing.T) {
	t.Parallel()

	targetConnection := &mockConn{}
	otherConnection := &mockConn{}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.lastConfig = &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				AutoStart: true,
				ObservePolicy: &schema.ObservePolicy{
					// Nobody allowed — revokes all sessions.
					AllowedObservers: []string{},
				},
			},
			{
				Localpart: "iree/amdgpu/builder",
				AutoStart: true,
				ObservePolicy: &schema.ObservePolicy{
					AllowedObservers: []string{"ops/**"},
				},
			},
		},
	}
	daemon.observeSessions = []*activeObserveSession{
		{
			principal:   "iree/amdgpu/pm",
			observer:    "@ops/bob:bureau.local",
			grantedMode: "readonly",
			clientConn:  targetConnection,
		},
		{
			principal:   "iree/amdgpu/builder",
			observer:    "@ops/bob:bureau.local",
			grantedMode: "readonly",
			clientConn:  otherConnection,
		},
	}

	// Only enforce on pm — builder's session should be untouched.
	daemon.enforceObservePolicyChange("iree/amdgpu/pm")

	if !targetConnection.wasClosed() {
		t.Error("expected pm connection to be closed")
	}
	if otherConnection.wasClosed() {
		t.Error("expected builder connection to remain open (different principal)")
	}
}

func TestEnforceObservePolicyChangeNoSessions(t *testing.T) {
	t.Parallel()

	// Verify no panic when there are no sessions.
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.lastConfig = &schema.MachineConfig{
		Principals: []schema.PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				AutoStart: true,
			},
		},
	}

	// Should not panic with nil observeSessions.
	daemon.enforceObservePolicyChange("iree/amdgpu/pm")
}
