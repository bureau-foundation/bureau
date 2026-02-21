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
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestAuthorizeObserveDefaultDeny(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	// No allowances in the authorization index — everything should be denied.
	authz := daemon.authorizeObserve(ref.MustParseUserID("@ben:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), "readwrite")
	if authz.Allowed {
		t.Error("expected default deny when target has no allowances")
	}
}

func TestAuthorizeObserveNoAllowanceDeny(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	// Target principal exists in the index but has no allowances.
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{})
	authz := daemon.authorizeObserve(ref.MustParseUserID("@ben:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), "readwrite")
	if authz.Allowed {
		t.Error("expected deny when target has no observation allowances")
	}
}

func TestAuthorizeObserveAllowAllObservers(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"**:**"}},
		},
	})
	authz := daemon.authorizeObserve(ref.MustParseUserID("@ben:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), "readonly")
	if !authz.Allowed {
		t.Error("expected allow with ** observer allowance")
	}
	if authz.GrantedMode != "readonly" {
		t.Errorf("GrantedMode = %q, want readonly", authz.GrantedMode)
	}
}

func TestAuthorizeObserveSpecificAllowanceOverridesWildcard(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	// Only ops/alice is allowed.
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/alice:**"}},
		},
	})

	// ben does not match ops/alice.
	authz := daemon.authorizeObserve(ref.MustParseUserID("@ben:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), "readonly")
	if authz.Allowed {
		t.Error("expected deny: ben not in observation allowances")
	}

	// alice matches.
	authz = daemon.authorizeObserve(ref.MustParseUserID("@ops/alice:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), "readonly")
	if !authz.Allowed {
		t.Error("expected allow: ops/alice matches observation allowance")
	}
}

func TestAuthorizeObserveModeDowngrade(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"**:**"}},
			{Actions: []string{"observe/read-write"}, Actors: []string{"ops/**:**"}},
		},
	})

	// ben requests readwrite but is not in observe/read-write allowance —
	// should be downgraded to readonly.
	authz := daemon.authorizeObserve(ref.MustParseUserID("@ben:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), "readwrite")
	if !authz.Allowed {
		t.Fatal("expected allow")
	}
	if authz.GrantedMode != "readonly" {
		t.Errorf("GrantedMode = %q, want readonly (downgraded)", authz.GrantedMode)
	}

	// ops/alice requests readwrite and matches observe/read-write — should get it.
	authz = daemon.authorizeObserve(ref.MustParseUserID("@ops/alice:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), "readwrite")
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
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"**:**"}},
			{Actions: []string{"observe/read-write"}, Actors: []string{"**:**"}},
		},
	})

	// Even though observer has readwrite permission, requesting readonly
	// should stay readonly.
	authz := daemon.authorizeObserve(ref.MustParseUserID("@ben:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), "readonly")
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
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/*:**"}},
		},
	})

	// ops/alice matches ops/*
	authz := daemon.authorizeObserve(ref.MustParseUserID("@ops/alice:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), "readonly")
	if !authz.Allowed {
		t.Error("expected allow: ops/alice matches ops/*")
	}

	// ops/team/lead does NOT match ops/* (single segment only).
	authz = daemon.authorizeObserve(ref.MustParseUserID("@ops/team/lead:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), "readonly")
	if authz.Allowed {
		t.Error("expected deny: ops/team/lead should not match ops/* (single segment)")
	}

	// iree/builder does NOT match ops/*.
	authz = daemon.authorizeObserve(ref.MustParseUserID("@iree/builder:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), "readonly")
	if authz.Allowed {
		t.Error("expected deny: iree/builder does not match ops/*")
	}
}

func TestAuthorizeObserveTargetNotInIndex(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	// Target not in the authorization index at all.
	authz := daemon.authorizeObserve(ref.MustParseUserID("@ben:bureau.local"), testEntity(t, daemon.fleet, "unknown/principal").UserID(), "readonly")
	if authz.Allowed {
		t.Error("expected deny: target not in authorization index")
	}
}

func TestAuthorizeObserveAllowanceDenialOverrides(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/**:**"}},
		},
		AllowanceDenials: []schema.AllowanceDenial{
			{Actions: []string{"observe"}, Actors: []string{"ops/untrusted:**"}},
		},
	})

	// ops/alice is allowed.
	authz := daemon.authorizeObserve(ref.MustParseUserID("@ops/alice:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), "readonly")
	if !authz.Allowed {
		t.Error("expected allow: ops/alice should pass allowance")
	}

	// ops/untrusted is denied by allowance denial.
	authz = daemon.authorizeObserve(ref.MustParseUserID("@ops/untrusted:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), "readonly")
	if authz.Allowed {
		t.Error("expected deny: ops/untrusted should be blocked by allowance denial")
	}
}

func TestAuthorizeListFiltering(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/**:**"}},
		},
	})
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/builder").UserID(), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"**:**"}},
		},
	})

	// ben can only see builder, not pm.
	if daemon.authorizeList(ref.MustParseUserID("@ben:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID()) {
		t.Error("ben should not be authorized to list iree/amdgpu/pm")
	}
	if !daemon.authorizeList(ref.MustParseUserID("@ben:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/builder").UserID()) {
		t.Error("ben should be authorized to list iree/amdgpu/builder")
	}

	// ops/alice can see both.
	if !daemon.authorizeList(ref.MustParseUserID("@ops/alice:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID()) {
		t.Error("ops/alice should be authorized to list iree/amdgpu/pm")
	}
	if !daemon.authorizeList(ref.MustParseUserID("@ops/alice:bureau.local"), testEntity(t, daemon.fleet, "iree/amdgpu/builder").UserID()) {
		t.Error("ops/alice should be authorized to list iree/amdgpu/builder")
	}
}

// mockConn is a minimal net.Conn that tracks whether Close was called.
// Used to verify that enforceObserveAllowanceChange terminates sessions
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
// below (enforceObserveAllowanceChange only calls Close), but providing
// them avoids nil-pointer panics if the test setup changes.
func (c *mockConn) LocalAddr() net.Addr              { return &net.UnixAddr{Name: "mock"} }
func (c *mockConn) RemoteAddr() net.Addr             { return &net.UnixAddr{Name: "mock"} }
func (c *mockConn) SetDeadline(time.Time) error      { return nil }
func (c *mockConn) SetReadDeadline(time.Time) error  { return nil }
func (c *mockConn) SetWriteDeadline(time.Time) error { return nil }
func (c *mockConn) Read([]byte) (int, error)         { return 0, nil }
func (c *mockConn) Write(b []byte) (int, error)      { return len(b), nil }

func TestEnforceObserveAllowanceChangeRevoke(t *testing.T) {
	t.Parallel()

	connection := &mockConn{}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// Allowance only permits ops/alice. The session below is for
	// ops/bob, who is not in the allowance.
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/alice:**"}},
		},
	})
	daemon.observeSessions = []*activeObserveSession{
		{
			principal:   testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(),
			observer:    ref.MustParseUserID("@ops/bob:bureau.local"),
			grantedMode: "readonly",
			clientConn:  connection,
		},
	}

	daemon.enforceObserveAllowanceChange(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID())

	if !connection.wasClosed() {
		t.Error("expected connection to be closed: ops/bob is not in observation allowances")
	}
}

func TestEnforceObserveAllowanceChangeRetain(t *testing.T) {
	t.Parallel()

	connection := &mockConn{}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// Allowance permits ops/** — ops/bob is still authorized.
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/**:**"}},
		},
	})
	daemon.observeSessions = []*activeObserveSession{
		{
			principal:   testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(),
			observer:    ref.MustParseUserID("@ops/bob:bureau.local"),
			grantedMode: "readonly",
			clientConn:  connection,
		},
	}

	daemon.enforceObserveAllowanceChange(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID())

	if connection.wasClosed() {
		t.Error("expected connection to remain open: ops/bob matches observation allowance")
	}
}

func TestEnforceObserveAllowanceChangeModeDowngrade(t *testing.T) {
	t.Parallel()

	connection := &mockConn{}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// Allowance still permits ops/** for observation, but
	// observe/read-write is now limited to ops/alice. The session was
	// granted readwrite for ops/bob, who would now only get readonly.
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/**:**"}},
			{Actions: []string{"observe/read-write"}, Actors: []string{"ops/alice:**"}},
		},
	})
	daemon.observeSessions = []*activeObserveSession{
		{
			principal:   testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(),
			observer:    ref.MustParseUserID("@ops/bob:bureau.local"),
			grantedMode: "readwrite",
			clientConn:  connection,
		},
	}

	daemon.enforceObserveAllowanceChange(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID())

	if !connection.wasClosed() {
		t.Error("expected connection to be closed: ops/bob's readwrite session would now be downgraded to readonly")
	}
}

func TestEnforceObserveAllowanceChangeOtherPrincipalUntouched(t *testing.T) {
	t.Parallel()

	targetConnection := &mockConn{}
	otherConnection := &mockConn{}

	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// pm has empty allowances — revokes all sessions.
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{})
	// builder allows ops/**
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/builder").UserID(), schema.AuthorizationPolicy{
		Allowances: []schema.Allowance{
			{Actions: []string{"observe"}, Actors: []string{"ops/**:**"}},
		},
	})
	daemon.observeSessions = []*activeObserveSession{
		{
			principal:   testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(),
			observer:    ref.MustParseUserID("@ops/bob:bureau.local"),
			grantedMode: "readonly",
			clientConn:  targetConnection,
		},
		{
			principal:   testEntity(t, daemon.fleet, "iree/amdgpu/builder").UserID(),
			observer:    ref.MustParseUserID("@ops/bob:bureau.local"),
			grantedMode: "readonly",
			clientConn:  otherConnection,
		},
	}

	// Only enforce on pm — builder's session should be untouched.
	daemon.enforceObserveAllowanceChange(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID())

	if !targetConnection.wasClosed() {
		t.Error("expected pm connection to be closed")
	}
	if otherConnection.wasClosed() {
		t.Error("expected builder connection to remain open (different principal)")
	}
}

func TestEnforceObserveAllowanceChangeNoSessions(t *testing.T) {
	t.Parallel()

	// Verify no panic when there are no sessions.
	daemon, _ := newTestDaemon(t)
	daemon.runDir = principal.DefaultRunDir
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	daemon.authorizationIndex.SetPrincipal(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID(), schema.AuthorizationPolicy{})

	// Should not panic with nil observeSessions.
	daemon.enforceObserveAllowanceChange(testEntity(t, daemon.fleet, "iree/amdgpu/pm").UserID())
}
