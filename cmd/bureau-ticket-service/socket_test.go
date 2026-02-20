// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/ticket"
)

// testClockEpoch is the fixed time used by the fake clock in ticket
// service tests. Token timestamps and service clock share this epoch.
var testClockEpoch = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

// --- Test infrastructure ---

// testServerOpts configures a test TicketService. Fields left at
// their zero values select sensible defaults (fakeWriter, ticket/*
// grants, no timers, short token expiry).
type testServerOpts struct {
	// grants overrides the token grants. Default: ticket/*.
	// Set to an empty non-nil slice for no grants.
	grants []servicetoken.Grant
	// noGrants mints a token with no grants at all.
	noGrants bool
	// withTimers starts the timer loop and uses a 30-day token
	// so clock advancement doesn't expire the token.
	withTimers bool
	// noWriter skips creating a fakeWriter (for read-only tests).
	noWriter bool
}

// newTestServer creates a TicketService with a running socket server
// and returns a testEnv. The testEnv always carries a client, service
// reference, clock, and cleanup function. The writer is nil when
// opts.noWriter is set.
func newTestServer(t *testing.T, rooms map[string]*roomState, opts testServerOpts) *testEnv {
	t.Helper()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}

	testClock := clock.Fake(testClockEpoch)
	authConfig := &service.AuthConfig{
		PublicKey: publicKey,
		Audience:  "ticket",
		Blacklist: servicetoken.NewBlacklist(),
		Clock:     testClock,
	}

	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "ticket.sock")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := service.NewSocketServer(socketPath, logger, authConfig)

	var writer *fakeWriter
	if !opts.noWriter {
		writer = &fakeWriter{notify: make(chan struct{}, 64)}
	}

	ts := &TicketService{
		writer:        writer,
		clock:         testClock,
		startedAt:     time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		principalName: "service/ticket/test",
		serverName:    "bureau.local",
		rooms:         rooms,
		logger:        logger,
	}
	if opts.withTimers {
		ts.timerNotify = make(chan struct{}, 1)
	}
	ts.registerActions(server)

	if opts.withTimers {
		ts.rebuildTimerHeap()
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	if opts.withTimers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ts.startTimerLoop(ctx)
		}()
	}

	// Determine grants and subject.
	grants := opts.grants
	subject := "agent/tester"
	if opts.noGrants {
		grants = nil
		subject = "agent/unauthorized"
	} else if grants == nil {
		grants = []servicetoken.Grant{{Actions: []string{"ticket/*"}}}
	}

	// Timer tests need long-lived tokens so clock advancement
	// doesn't cause expiration.
	var tokenBytes []byte
	if opts.withTimers {
		token := &servicetoken.Token{
			Subject:   subject,
			Machine:   "machine/test",
			Audience:  "ticket",
			Grants:    grants,
			ID:        "test-token-long",
			IssuedAt:  testClockEpoch.Add(-5 * time.Minute).Unix(),
			ExpiresAt: testClockEpoch.Add(30 * 24 * time.Hour).Unix(),
		}
		tokenBytes, err = servicetoken.Mint(privateKey, token)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
	} else {
		tokenBytes = mintToken(t, privateKey, subject, grants)
	}
	client := service.NewServiceClientFromToken(socketPath, tokenBytes)

	return &testEnv{
		client:  client,
		writer:  writer,
		service: ts,
		clock:   testClock,
		cleanup: func() {
			cancel()
			wg.Wait()
		},
	}
}

// Legacy wrappers — these delegate to newTestServer for backward
// compatibility with the large number of existing callers. New tests
// should call newTestServer directly.

func testServer(t *testing.T, rooms map[string]*roomState) (*service.ServiceClient, func()) {
	t.Helper()
	env := newTestServer(t, rooms, testServerOpts{noWriter: true})
	return env.client, env.cleanup
}

func testServerNoGrants(t *testing.T, rooms map[string]*roomState) (*service.ServiceClient, func()) {
	t.Helper()
	env := newTestServer(t, rooms, testServerOpts{noWriter: true, noGrants: true})
	return env.client, env.cleanup
}

func testServerWithGrants(t *testing.T, rooms map[string]*roomState, grants []servicetoken.Grant) (*service.ServiceClient, func()) {
	t.Helper()
	env := newTestServer(t, rooms, testServerOpts{noWriter: true, grants: grants})
	return env.client, env.cleanup
}

func testMutationServerWithGrants(t *testing.T, rooms map[string]*roomState, grants []servicetoken.Grant) *testEnv {
	t.Helper()
	return newTestServer(t, rooms, testServerOpts{grants: grants})
}

// mintToken creates a signed test token with specific grants.
// Timestamps are relative to testClockEpoch.
func mintToken(t *testing.T, privateKey ed25519.PrivateKey, subject string, grants []servicetoken.Grant) []byte {
	t.Helper()
	token := &servicetoken.Token{
		Subject:   subject,
		Machine:   "machine/test",
		Audience:  "ticket",
		Grants:    grants,
		ID:        "test-token",
		IssuedAt:  testClockEpoch.Add(-5 * time.Minute).Unix(),
		ExpiresAt: testClockEpoch.Add(5 * time.Minute).Unix(),
	}
	tokenBytes, err := servicetoken.Mint(privateKey, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return tokenBytes
}

// waitForSocket polls until the socket file exists.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	for range 500 {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond) //nolint:realclock — filesystem polling for socket existence
	}
	t.Fatalf("socket %s did not appear within timeout", path)
}

// ticketFixture returns a minimal valid TicketContent. The title and
// status are parameterized; everything else gets sensible defaults.
func ticketFixture(title, status string) schema.TicketContent {
	return schema.TicketContent{
		Version:   schema.TicketContentVersion,
		Title:     title,
		Status:    status,
		Priority:  2,
		Type:      "task",
		CreatedBy: "@agent/tester:bureau.local",
		CreatedAt: "2026-01-15T12:00:00Z",
		UpdatedAt: "2026-01-15T12:00:00Z",
	}
}

// requireServiceError asserts that err is a *service.ServiceError.
func requireServiceError(t *testing.T, err error) *service.ServiceError {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var serviceErr *service.ServiceError
	if !errors.As(err, &serviceErr) {
		t.Fatalf("expected *ServiceError, got %T: %v", err, err)
	}
	return serviceErr
}

// sampleRooms returns a two-room setup for testing. Room A has 3
// tickets (2 open, 1 closed); room B has 1 ticket with a dependency.
func sampleRooms() map[string]*roomState {
	roomA := newTrackedRoom(map[string]schema.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "implement login",
			Body:      "add OAuth support",
			Status:    "open",
			Priority:  1,
			Type:      "feature",
			Labels:    []string{"auth", "frontend"},
			Assignee:  "@agent/coder:bureau.local",
			CreatedAt: "2026-01-01T00:00:00Z",
		},
		"tkt-2": {
			Version:   1,
			Title:     "fix database timeout",
			Status:    "closed",
			Priority:  0,
			Type:      "bug",
			Labels:    []string{"backend"},
			ClosedAt:  "2026-01-02T00:00:00Z",
			CreatedAt: "2026-01-01T12:00:00Z",
		},
		"tkt-3": {
			Version:   1,
			Title:     "write tests",
			Status:    "open",
			Priority:  2,
			Type:      "task",
			Parent:    "tkt-1",
			BlockedBy: []string{"tkt-2"},
			CreatedAt: "2026-01-02T00:00:00Z",
		},
	})

	roomB := newTrackedRoom(map[string]schema.TicketContent{
		"tkt-10": {
			Version:   1,
			Title:     "deploy service",
			Body:      "roll out to production",
			Status:    "open",
			Priority:  1,
			Type:      "task",
			CreatedAt: "2026-01-03T00:00:00Z",
		},
	})

	return map[string]*roomState{
		"!roomA:local": roomA,
		"!roomB:local": roomB,
	}
}

// --- Authorization tests ---

func TestQueryActionsRequireGrant(t *testing.T) {
	rooms := sampleRooms()

	actions := []struct {
		name   string
		fields map[string]any
	}{
		// Query actions.
		{"list", map[string]any{"room": "!roomA:local"}},
		{"ready", map[string]any{"room": "!roomA:local"}},
		{"blocked", map[string]any{"room": "!roomA:local"}},
		{"ranked", map[string]any{"room": "!roomA:local"}},
		{"show", map[string]any{"ticket": "tkt-1"}},
		{"children", map[string]any{"ticket": "tkt-1"}},
		{"grep", map[string]any{"pattern": "login"}},
		{"stats", map[string]any{"room": "!roomA:local"}},
		{"deps", map[string]any{"ticket": "tkt-3"}},
		{"epic-health", map[string]any{"ticket": "tkt-1"}},
		// Mutation actions.
		{"create", map[string]any{"room": "!roomA:local"}},
		{"update", map[string]any{"ticket": "tkt-1"}},
		{"close", map[string]any{"ticket": "tkt-1"}},
		{"reopen", map[string]any{"ticket": "tkt-1"}},
		{"batch-create", map[string]any{"room": "!roomA:local"}},
		{"resolve-gate", map[string]any{"ticket": "tkt-1"}},
		{"update-gate", map[string]any{"ticket": "tkt-1"}},
	}

	client, cleanup := testServerNoGrants(t, rooms)
	defer cleanup()

	ctx := context.Background()
	for _, action := range actions {
		t.Run(action.name, func(t *testing.T) {
			err := client.Call(ctx, action.name, action.fields, nil)
			serviceErr := requireServiceError(t, err)
			if serviceErr.Action != action.name {
				t.Errorf("error action: got %q, want %q", serviceErr.Action, action.name)
			}
		})
	}
}

// --- List tests ---

func TestHandleListReturnsAllTickets(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result []entryWithRoom
	err := client.Call(context.Background(), "list", map[string]any{
		"room": "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 3 {
		t.Fatalf("got %d entries, want 3", len(result))
	}
}

func TestHandleListFilterByStatus(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result []entryWithRoom
	err := client.Call(context.Background(), "list", map[string]any{
		"room":   "!roomA:local",
		"status": "open",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("got %d entries, want 2 open tickets", len(result))
	}
	for _, entry := range result {
		if entry.Content.Status != "open" {
			t.Errorf("ticket %s has status %q, want open", entry.ID, entry.Content.Status)
		}
	}
}

func TestHandleListFilterByPriority(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	priority := 0
	var result []entryWithRoom
	err := client.Call(context.Background(), "list", map[string]any{
		"room":     "!roomA:local",
		"priority": priority,
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("got %d entries, want 1 critical ticket", len(result))
	}
	if result[0].ID != "tkt-2" {
		t.Errorf("got ticket %s, want tkt-2", result[0].ID)
	}
}

func TestHandleListFilterByLabel(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result []entryWithRoom
	err := client.Call(context.Background(), "list", map[string]any{
		"room":  "!roomA:local",
		"label": "backend",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("got %d entries, want 1", len(result))
	}
	if result[0].ID != "tkt-2" {
		t.Errorf("got ticket %s, want tkt-2", result[0].ID)
	}
}

func TestHandleListMissingRoom(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "list", nil, nil)
	requireServiceError(t, err)
}

func TestHandleListUnknownRoom(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "list", map[string]any{
		"room": "!nonexistent:local",
	}, nil)
	requireServiceError(t, err)
}

// --- Ready tests ---

func TestHandleReady(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result []entryWithRoom
	err := client.Call(context.Background(), "ready", map[string]any{
		"room": "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// tkt-1 is open with no blockers → ready.
	// tkt-2 is closed → not ready.
	// tkt-3 is open but blocked by tkt-2 (which is closed) → ready.
	if len(result) != 2 {
		t.Fatalf("got %d ready tickets, want 2", len(result))
	}
}

// --- Blocked tests ---

func TestHandleBlocked(t *testing.T) {
	// Create a room where tkt-3 is blocked by tkt-1 (which is open).
	rooms := map[string]*roomState{
		"!room:local": newTrackedRoom(map[string]schema.TicketContent{
			"tkt-1": {Version: 1, Title: "blocker", Status: "open", Priority: 1, CreatedAt: "2026-01-01T00:00:00Z"},
			"tkt-3": {Version: 1, Title: "blocked", Status: "open", Priority: 2, BlockedBy: []string{"tkt-1"}, CreatedAt: "2026-01-02T00:00:00Z"},
		}),
	}

	client, cleanup := testServer(t, rooms)
	defer cleanup()

	var result []entryWithRoom
	err := client.Call(context.Background(), "blocked", map[string]any{
		"room": "!room:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("got %d blocked tickets, want 1", len(result))
	}
	if result[0].ID != "tkt-3" {
		t.Errorf("got ticket %s, want tkt-3", result[0].ID)
	}
}

// --- Show tests ---

func TestHandleShow(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result showResponse
	err := client.Call(context.Background(), "show", map[string]any{
		"ticket": "tkt-1",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.ID != "tkt-1" {
		t.Errorf("id: got %q, want tkt-1", result.ID)
	}
	if result.Room != "!roomA:local" {
		t.Errorf("room: got %q, want !roomA:local", result.Room)
	}
	if result.Content.Title != "implement login" {
		t.Errorf("title: got %q, want 'implement login'", result.Content.Title)
	}
	if result.Content.Body != "add OAuth support" {
		t.Errorf("body: got %q, want 'add OAuth support'", result.Content.Body)
	}
	if result.Content.Assignee != "@agent/coder:bureau.local" {
		t.Errorf("assignee: got %q", result.Content.Assignee)
	}
}

func TestHandleShowComputedFields(t *testing.T) {
	// tkt-3 is a child of tkt-1 and blocked by tkt-2.
	// tkt-1 should show: blocks=[] (nothing blocked by it in blocked_by),
	// child_total=1, child_closed=0.
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result showResponse
	err := client.Call(context.Background(), "show", map[string]any{
		"ticket": "tkt-1",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// tkt-1 has one child (tkt-3).
	if result.ChildTotal != 1 {
		t.Errorf("child_total: got %d, want 1", result.ChildTotal)
	}
	if result.ChildClosed != 0 {
		t.Errorf("child_closed: got %d, want 0", result.ChildClosed)
	}

	// Show tkt-2 — tkt-3 is blocked by it, so Blocks should include tkt-3.
	var result2 showResponse
	err = client.Call(context.Background(), "show", map[string]any{
		"ticket": "tkt-2",
		"room":   "!roomA:local",
	}, &result2)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(result2.Blocks) != 1 || result2.Blocks[0] != "tkt-3" {
		t.Errorf("blocks: got %v, want [tkt-3]", result2.Blocks)
	}
}

func TestHandleShowNotFound(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "show", map[string]any{
		"ticket": "nonexistent",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleShowMissingTicketField(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "show", nil, nil)
	requireServiceError(t, err)
}

// --- Children tests ---

func TestHandleChildren(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result childrenResponse
	err := client.Call(context.Background(), "children", map[string]any{
		"ticket": "tkt-1",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Parent != "tkt-1" {
		t.Errorf("parent: got %q, want tkt-1", result.Parent)
	}
	if len(result.Children) != 1 {
		t.Fatalf("got %d children, want 1", len(result.Children))
	}
	if result.Children[0].ID != "tkt-3" {
		t.Errorf("child: got %q, want tkt-3", result.Children[0].ID)
	}
	if result.ChildTotal != 1 {
		t.Errorf("child_total: got %d, want 1", result.ChildTotal)
	}
	if result.ChildClosed != 0 {
		t.Errorf("child_closed: got %d, want 0", result.ChildClosed)
	}
}

func TestHandleChildrenNoChildren(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result childrenResponse
	err := client.Call(context.Background(), "children", map[string]any{
		"ticket": "tkt-2",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result.Children) != 0 {
		t.Fatalf("got %d children, want 0", len(result.Children))
	}
}

// --- Grep tests ---

func TestHandleGrepRoomScoped(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result []entryWithRoom
	err := client.Call(context.Background(), "grep", map[string]any{
		"pattern": "login",
		"room":    "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("got %d results, want 1", len(result))
	}
	if result[0].ID != "tkt-1" {
		t.Errorf("got ticket %s, want tkt-1", result[0].ID)
	}
}

func TestHandleGrepCrossRoom(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	// "deploy" only appears in room B.
	var result []entryWithRoom
	err := client.Call(context.Background(), "grep", map[string]any{
		"pattern": "deploy",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("got %d results, want 1", len(result))
	}
	if result[0].ID != "tkt-10" {
		t.Errorf("got ticket %s, want tkt-10", result[0].ID)
	}
	if result[0].Room != "!roomB:local" {
		t.Errorf("room: got %q, want !roomB:local", result[0].Room)
	}
}

func TestHandleGrepBodySearch(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	// "OAuth" is in tkt-1's body, not title.
	var result []entryWithRoom
	err := client.Call(context.Background(), "grep", map[string]any{
		"pattern": "OAuth",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("got %d results, want 1", len(result))
	}
	if result[0].ID != "tkt-1" {
		t.Errorf("got ticket %s, want tkt-1", result[0].ID)
	}
}

func TestHandleGrepNoResults(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result []entryWithRoom
	err := client.Call(context.Background(), "grep", map[string]any{
		"pattern": "xyzzy",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 0 {
		t.Fatalf("got %d results, want 0", len(result))
	}
}

func TestHandleGrepInvalidRegex(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "grep", map[string]any{
		"pattern": "[invalid",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleGrepMissingPattern(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "grep", nil, nil)
	requireServiceError(t, err)
}

// --- Stats tests ---

func TestHandleStats(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result ticket.Stats
	err := client.Call(context.Background(), "stats", map[string]any{
		"room": "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Total != 3 {
		t.Errorf("total: got %d, want 3", result.Total)
	}
	if result.ByStatus["open"] != 2 {
		t.Errorf("open: got %d, want 2", result.ByStatus["open"])
	}
	if result.ByStatus["closed"] != 1 {
		t.Errorf("closed: got %d, want 1", result.ByStatus["closed"])
	}
}

// --- Deps tests ---

func TestHandleDeps(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result depsResponse
	err := client.Call(context.Background(), "deps", map[string]any{
		"ticket": "tkt-3",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Ticket != "tkt-3" {
		t.Errorf("ticket: got %q, want tkt-3", result.Ticket)
	}
	if len(result.Deps) != 1 || result.Deps[0] != "tkt-2" {
		t.Errorf("deps: got %v, want [tkt-2]", result.Deps)
	}
}

func TestHandleDepsNoDependencies(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	var result depsResponse
	err := client.Call(context.Background(), "deps", map[string]any{
		"ticket": "tkt-1",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result.Deps) != 0 {
		t.Errorf("deps: got %v, want empty", result.Deps)
	}
}

func TestHandleDepsNotFound(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "deps", map[string]any{
		"ticket": "nonexistent",
	}, nil)
	requireServiceError(t, err)
}

// --- Ranked tests ---

func TestHandleRanked(t *testing.T) {
	// Set up a room with scoring-relevant structure: one ticket
	// that unblocks another (high leverage) and one that doesn't.
	rooms := map[string]*roomState{
		"!room:local": newTrackedRoom(map[string]schema.TicketContent{
			"tkt-blocker": {
				Version:   1,
				Title:     "high leverage blocker",
				Status:    "closed",
				Priority:  2,
				Type:      "task",
				CreatedAt: "2026-01-01T00:00:00Z",
				ClosedAt:  "2026-01-10T00:00:00Z",
			},
			"tkt-ready-a": {
				Version:   1,
				Title:     "ready ticket A",
				Status:    "open",
				Priority:  2,
				Type:      "task",
				BlockedBy: []string{"tkt-blocker"},
				CreatedAt: "2026-01-01T00:00:00Z",
			},
			"tkt-ready-b": {
				Version:   1,
				Title:     "ready ticket B unblocks downstream",
				Status:    "open",
				Priority:  2,
				Type:      "task",
				CreatedAt: "2026-01-01T00:00:00Z",
			},
			"tkt-blocked": {
				Version:   1,
				Title:     "blocked by B",
				Status:    "open",
				Priority:  0,
				Type:      "bug",
				BlockedBy: []string{"tkt-ready-b"},
				CreatedAt: "2026-01-01T00:00:00Z",
			},
		}),
	}

	client, cleanup := testServer(t, rooms)
	defer cleanup()

	var result []rankedEntryResponse
	err := client.Call(context.Background(), "ranked", map[string]any{
		"room": "!room:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// tkt-ready-a and tkt-ready-b are ready. tkt-ready-b unblocks
	// a P0 (high leverage + borrowed priority) so it should rank first.
	if len(result) != 2 {
		t.Fatalf("got %d ranked entries, want 2", len(result))
	}
	if result[0].ID != "tkt-ready-b" {
		t.Errorf("first ranked entry: got %q, want tkt-ready-b", result[0].ID)
	}
	if result[0].Score.UnblockCount != 1 {
		t.Errorf("tkt-ready-b UnblockCount: got %d, want 1", result[0].Score.UnblockCount)
	}
	if result[0].Score.BorrowedPriority != 0 {
		t.Errorf("tkt-ready-b BorrowedPriority: got %d, want 0", result[0].Score.BorrowedPriority)
	}
	if result[1].ID != "tkt-ready-a" {
		t.Errorf("second ranked entry: got %q, want tkt-ready-a", result[1].ID)
	}
}

func TestHandleRankedEmpty(t *testing.T) {
	rooms := map[string]*roomState{
		"!room:local": newTrackedRoom(map[string]schema.TicketContent{
			"tkt-closed": {
				Version:   1,
				Title:     "all done",
				Status:    "closed",
				Priority:  2,
				Type:      "task",
				CreatedAt: "2026-01-01T00:00:00Z",
			},
		}),
	}

	client, cleanup := testServer(t, rooms)
	defer cleanup()

	var result []rankedEntryResponse
	err := client.Call(context.Background(), "ranked", map[string]any{
		"room": "!room:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("got %d ranked entries, want 0", len(result))
	}
}

func TestHandleRankedMissingRoom(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "ranked", nil, nil)
	requireServiceError(t, err)
}

// --- Epic health tests ---

func TestHandleEpicHealth(t *testing.T) {
	rooms := map[string]*roomState{
		"!room:local": newTrackedRoom(map[string]schema.TicketContent{
			"tkt-epic": {
				Version:   1,
				Title:     "the epic",
				Status:    "open",
				Priority:  1,
				Type:      "epic",
				CreatedAt: "2026-01-01T00:00:00Z",
			},
			"tkt-done": {
				Version:   1,
				Title:     "done child",
				Status:    "closed",
				Priority:  2,
				Type:      "task",
				Parent:    "tkt-epic",
				CreatedAt: "2026-01-01T00:00:00Z",
				ClosedAt:  "2026-01-05T00:00:00Z",
			},
			"tkt-ready": {
				Version:   1,
				Title:     "ready child",
				Status:    "open",
				Priority:  2,
				Type:      "task",
				Parent:    "tkt-epic",
				CreatedAt: "2026-01-02T00:00:00Z",
			},
			"tkt-blocked-child": {
				Version:   1,
				Title:     "blocked child",
				Status:    "open",
				Priority:  2,
				Type:      "task",
				Parent:    "tkt-epic",
				BlockedBy: []string{"tkt-ready"},
				CreatedAt: "2026-01-02T00:00:00Z",
			},
		}),
	}

	client, cleanup := testServer(t, rooms)
	defer cleanup()

	var result epicHealthResponse
	err := client.Call(context.Background(), "epic-health", map[string]any{
		"ticket": "tkt-epic",
		"room":   "!room:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Ticket != "tkt-epic" {
		t.Errorf("ticket: got %q, want tkt-epic", result.Ticket)
	}
	if result.Health.TotalChildren != 3 {
		t.Errorf("TotalChildren: got %d, want 3", result.Health.TotalChildren)
	}
	if result.Health.ClosedChildren != 1 {
		t.Errorf("ClosedChildren: got %d, want 1", result.Health.ClosedChildren)
	}
	if result.Health.ReadyChildren != 1 {
		t.Errorf("ReadyChildren: got %d, want 1", result.Health.ReadyChildren)
	}
	if result.Health.CriticalDepth != 1 {
		t.Errorf("CriticalDepth: got %d, want 1", result.Health.CriticalDepth)
	}
}

func TestHandleEpicHealthNotFound(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "epic-health", map[string]any{
		"ticket": "nonexistent",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleEpicHealthMissingTicketField(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	err := client.Call(context.Background(), "epic-health", nil, nil)
	requireServiceError(t, err)
}

// --- Show with score tests ---

func TestHandleShowIncludesScore(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	// tkt-1 is open — should include a score.
	var result showResponse
	err := client.Call(context.Background(), "show", map[string]any{
		"ticket": "tkt-1",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Score == nil {
		t.Fatal("expected non-nil Score for open ticket")
	}
}

func TestHandleShowClosedTicketOmitsScore(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	// tkt-2 is closed — score should be nil.
	var result showResponse
	err := client.Call(context.Background(), "show", map[string]any{
		"ticket": "tkt-2",
		"room":   "!roomA:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Score != nil {
		t.Errorf("expected nil Score for closed ticket, got %+v", result.Score)
	}
}

// --- Cross-room show test ---

func TestHandleShowCrossRoom(t *testing.T) {
	client, cleanup := testServer(t, sampleRooms())
	defer cleanup()

	// tkt-10 is in room B.
	var result showResponse
	err := client.Call(context.Background(), "show", map[string]any{
		"ticket": "tkt-10",
		"room":   "!roomB:local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Room != "!roomB:local" {
		t.Errorf("room: got %q, want !roomB:local", result.Room)
	}
	if result.Content.Title != "deploy service" {
		t.Errorf("title: got %q, want 'deploy service'", result.Content.Title)
	}
}

// Verify codec does not need explicit cbor tags on the types. This
// ensures the schema types (json-tagged) round-trip correctly through
// our CBOR wire protocol.
func TestSchemaContentRoundTripViaCBOR(t *testing.T) {
	original := schema.TicketContent{
		Version:   1,
		Title:     "round-trip test",
		Body:      "body text",
		Status:    "open",
		Priority:  2,
		Type:      "task",
		Labels:    []string{"a", "b"},
		Assignee:  "@agent/test:bureau.local",
		Parent:    "parent-1",
		BlockedBy: []string{"dep-1"},
		CreatedBy: "@creator:bureau.local",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T00:00:00Z",
		Notes: []schema.TicketNote{
			{ID: "n-1", Author: "@author:bureau.local", Body: "note body", CreatedAt: "2026-01-01T00:00:00Z"},
		},
	}

	data, err := codec.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded schema.TicketContent
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Title != original.Title {
		t.Errorf("title: got %q, want %q", decoded.Title, original.Title)
	}
	if decoded.Body != original.Body {
		t.Errorf("body: got %q, want %q", decoded.Body, original.Body)
	}
	if decoded.Assignee != original.Assignee {
		t.Errorf("assignee: got %q, want %q", decoded.Assignee, original.Assignee)
	}
	if len(decoded.Labels) != 2 {
		t.Errorf("labels: got %v, want [a b]", decoded.Labels)
	}
	if len(decoded.Notes) != 1 || decoded.Notes[0].Body != "note body" {
		t.Errorf("notes: got %v", decoded.Notes)
	}
}

// --- Mutation test infrastructure ---

// fakeWriter records state events written by mutation handlers.
type fakeWriter struct {
	mu     sync.Mutex
	events []writtenEvent
	// notify receives after each write. Tests block on this to
	// synchronize with the timer loop goroutine. Buffered.
	notify chan struct{}
}

type writtenEvent struct {
	RoomID    string
	EventType string
	StateKey  string
	Content   any
}

func (f *fakeWriter) SendStateEvent(_ context.Context, roomID ref.RoomID, eventType, stateKey string, content any) (string, error) {
	f.mu.Lock()
	f.events = append(f.events, writtenEvent{
		RoomID:    roomID.String(),
		EventType: eventType,
		StateKey:  stateKey,
		Content:   content,
	})
	f.mu.Unlock()
	if f.notify != nil {
		select {
		case f.notify <- struct{}{}:
		default:
		}
	}
	return "$event-" + stateKey, nil
}

// testEnv holds the server, client, and writer for mutation tests.
type testEnv struct {
	client  *service.ServiceClient
	writer  *fakeWriter
	service *TicketService
	clock   *clock.FakeClock
	cleanup func()
}

func testMutationServer(t *testing.T, rooms map[string]*roomState) *testEnv {
	t.Helper()
	return newTestServer(t, rooms, testServerOpts{})
}

func testMutationServerWithTimers(t *testing.T, rooms map[string]*roomState) *testEnv {
	t.Helper()
	return newTestServer(t, rooms, testServerOpts{withTimers: true})
}

// mutationRooms returns a room with tickets in various states suitable
// for mutation testing.
func mutationRooms() map[string]*roomState {
	room := newTrackedRoom(map[string]schema.TicketContent{
		"tkt-open": {
			Version:   1,
			Title:     "open ticket",
			Status:    "open",
			Priority:  2,
			Type:      "task",
			CreatedBy: "@agent/creator:bureau.local",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		"tkt-closed": {
			Version:     1,
			Title:       "closed ticket",
			Status:      "closed",
			Priority:    2,
			Type:        "bug",
			CreatedBy:   "@agent/creator:bureau.local",
			CreatedAt:   "2026-01-01T00:00:00Z",
			UpdatedAt:   "2026-01-02T00:00:00Z",
			ClosedAt:    "2026-01-02T00:00:00Z",
			CloseReason: "fixed",
		},
		"tkt-inprog": {
			Version:   1,
			Title:     "in-progress ticket",
			Status:    "in_progress",
			Priority:  1,
			Type:      "feature",
			Assignee:  "@agent/worker:bureau.local",
			CreatedBy: "@agent/creator:bureau.local",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-02T00:00:00Z",
		},
		"tkt-gated": {
			Version:  1,
			Title:    "gated ticket",
			Status:   "open",
			Priority: 2,
			Type:     "task",
			Gates: []schema.TicketGate{
				{
					ID:          "human-review",
					Type:        "human",
					Status:      "pending",
					Description: "needs human review",
					CreatedAt:   "2026-01-01T00:00:00Z",
				},
				{
					ID:          "ci-pass",
					Type:        "pipeline",
					Status:      "pending",
					PipelineRef: "build/main",
					CreatedAt:   "2026-01-01T00:00:00Z",
				},
			},
			CreatedBy: "@agent/creator:bureau.local",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		"tkt-dep": {
			Version:   1,
			Title:     "dependency ticket",
			Status:    "open",
			Priority:  2,
			Type:      "task",
			BlockedBy: []string{"tkt-open"},
			CreatedBy: "@agent/creator:bureau.local",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
	})

	return map[string]*roomState{
		"!room:bureau.local": room,
	}
}

// --- Create tests ---

func TestHandleCreate(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "new feature",
		"type":     "feature",
		"priority": 1,
		"labels":   []string{"backend"},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Room != "!room:bureau.local" {
		t.Errorf("room: got %q, want !room:bureau.local", result.Room)
	}
	if result.ID == "" {
		t.Fatal("expected non-empty ticket ID")
	}
	if len(result.ID) < 8 { // "tkt-" + at least 4 hex chars
		t.Errorf("ticket ID too short: %q", result.ID)
	}

	// Verify ticket exists in the index.
	content, exists := env.service.rooms["!room:bureau.local"].index.Get(result.ID)
	if !exists {
		t.Fatalf("ticket %s not in index after create", result.ID)
	}
	if content.Title != "new feature" {
		t.Errorf("title: got %q, want 'new feature'", content.Title)
	}
	if content.Status != "open" {
		t.Errorf("status: got %q, want 'open'", content.Status)
	}
	if content.CreatedBy != "@agent/tester:bureau.local" {
		t.Errorf("created_by: got %q, want '@agent/tester:bureau.local'", content.CreatedBy)
	}

	// Verify state event was written to Matrix.
	env.writer.mu.Lock()
	defer env.writer.mu.Unlock()
	if len(env.writer.events) != 1 {
		t.Fatalf("expected 1 written event, got %d", len(env.writer.events))
	}
	event := env.writer.events[0]
	if event.RoomID != "!room:bureau.local" {
		t.Errorf("event room: got %q", event.RoomID)
	}
	if event.StateKey != result.ID {
		t.Errorf("event state_key: got %q, want %q", event.StateKey, result.ID)
	}
}

func TestHandleCreateWithBlockedBy(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":       "!room:bureau.local",
		"title":      "blocked task",
		"type":       "task",
		"priority":   2,
		"blocked_by": []string{"tkt-open"},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	content, _ := env.service.rooms["!room:bureau.local"].index.Get(result.ID)
	if len(content.BlockedBy) != 1 || content.BlockedBy[0] != "tkt-open" {
		t.Errorf("blocked_by: got %v, want [tkt-open]", content.BlockedBy)
	}
}

func TestHandleCreateBlockedByNotFound(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":       "!room:bureau.local",
		"title":      "bad deps",
		"type":       "task",
		"priority":   2,
		"blocked_by": []string{"nonexistent"},
	}, nil)
	requireServiceError(t, err)
}

func TestHandleCreateParentNotFound(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "orphan subtask",
		"type":     "task",
		"priority": 2,
		"parent":   "nonexistent",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleCreateMissingTitle(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"type":     "task",
		"priority": 2,
	}, nil)
	requireServiceError(t, err)
}

func TestHandleCreateUnknownRoom(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!nonexistent:bureau.local",
		"title":    "test",
		"type":     "task",
		"priority": 2,
	}, nil)
	requireServiceError(t, err)
}

// --- Update tests ---

func TestHandleUpdateTitle(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"title":  "updated title",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Title != "updated title" {
		t.Errorf("title: got %q, want 'updated title'", result.Content.Title)
	}
	if result.Content.Status != "open" {
		t.Errorf("status: got %q, want 'open'", result.Content.Status)
	}
	if result.Content.UpdatedAt != "2026-01-15T12:00:00Z" {
		t.Errorf("updated_at: got %q, want '2026-01-15T12:00:00Z'", result.Content.UpdatedAt)
	}
}

func TestHandleUpdateClaimTicket(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket":   "tkt-open",
		"room":     "!room:bureau.local",
		"status":   "in_progress",
		"assignee": "@agent/tester:bureau.local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != "in_progress" {
		t.Errorf("status: got %q, want 'in_progress'", result.Content.Status)
	}
	if result.Content.Assignee != "@agent/tester:bureau.local" {
		t.Errorf("assignee: got %q", result.Content.Assignee)
	}
}

func TestHandleUpdateContentionRejected(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// tkt-inprog is already in_progress. Trying to claim it should fail.
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket":   "tkt-inprog",
		"status":   "in_progress",
		"assignee": "@agent/tester:bureau.local",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if serviceErr.Action != "update" {
		t.Errorf("action: got %q, want 'update'", serviceErr.Action)
	}
}

func TestHandleUpdateUnclaim(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-inprog",
		"room":   "!room:bureau.local",
		"status": "open",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != "open" {
		t.Errorf("status: got %q, want 'open'", result.Content.Status)
	}
	if result.Content.Assignee != "" {
		t.Errorf("assignee should be auto-cleared, got %q", result.Content.Assignee)
	}
}

func TestHandleUpdateInProgressRequiresAssignee(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// Trying to claim without providing assignee.
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-open",
		"status": "in_progress",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleUpdateAssigneeRequiresInProgress(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// Trying to set assignee on an open ticket without changing status.
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket":   "tkt-open",
		"assignee": "@agent/tester:bureau.local",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleUpdateCycleDetection(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// tkt-dep already blocks on tkt-open. Making tkt-open block on
	// tkt-dep would create a cycle.
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket":     "tkt-open",
		"blocked_by": []string{"tkt-dep"},
	}, nil)
	requireServiceError(t, err)
}

func TestHandleUpdateCloseViaUpdate(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"status": "closed",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != "closed" {
		t.Errorf("status: got %q, want 'closed'", result.Content.Status)
	}
	if result.Content.ClosedAt == "" {
		t.Error("closed_at should be auto-set")
	}
	if result.Content.Assignee != "" {
		t.Errorf("assignee should be empty, got %q", result.Content.Assignee)
	}
}

func TestHandleUpdateNotFound(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "nonexistent",
		"title":  "test",
	}, nil)
	requireServiceError(t, err)
}

// --- Close tests ---

func TestHandleClose(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"reason": "done",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != "closed" {
		t.Errorf("status: got %q, want 'closed'", result.Content.Status)
	}
	if result.Content.CloseReason != "done" {
		t.Errorf("close_reason: got %q, want 'done'", result.Content.CloseReason)
	}
	if result.Content.ClosedAt == "" {
		t.Error("closed_at should be set")
	}
}

func TestHandleCloseInProgress(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// Closing an in_progress ticket should work and clear the assignee.
	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-inprog",
		"room":   "!room:bureau.local",
		"reason": "agent finished",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Assignee != "" {
		t.Errorf("assignee should be cleared, got %q", result.Content.Assignee)
	}
}

func TestHandleCloseAlreadyClosed(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-closed",
		"room":   "!room:bureau.local",
		"reason": "duplicate",
	}, nil)
	// closed → closed is a no-op status transition, not an error.
	// But we check via the validateStatusTransition behavior: same
	// status is allowed for all except in_progress.
	if err != nil {
		t.Fatalf("expected no error for closing already-closed ticket, got: %v", err)
	}
}

// --- Close with recurring gate re-arm tests ---

// recurringRooms returns rooms with tickets that have recurring timer
// gates, suitable for testing the auto-rearm behavior of handleClose.
func recurringRooms() map[string]*roomState {
	room := newTrackedRoom(map[string]schema.TicketContent{
		"tkt-recurring-schedule": {
			Version:   1,
			Title:     "daily standup",
			Status:    "open",
			Priority:  2,
			Type:      "task",
			CreatedBy: "@agent/creator:bureau.local",
			CreatedAt: "2026-01-10T00:00:00Z",
			UpdatedAt: "2026-01-10T00:00:00Z",
			Gates: []schema.TicketGate{
				{
					ID:        "daily",
					Type:      "timer",
					Status:    "satisfied",
					Duration:  "1h",
					Target:    "2026-01-15T07:00:00Z",
					Schedule:  "0 7 * * *",
					CreatedAt: "2026-01-10T00:00:00Z",
					FireCount: 4,
				},
			},
		},
		"tkt-recurring-interval": {
			Version:   1,
			Title:     "polling task",
			Status:    "open",
			Priority:  2,
			Type:      "task",
			CreatedBy: "@agent/creator:bureau.local",
			CreatedAt: "2026-01-10T00:00:00Z",
			UpdatedAt: "2026-01-10T00:00:00Z",
			Gates: []schema.TicketGate{
				{
					ID:        "poll",
					Type:      "timer",
					Status:    "satisfied",
					Duration:  "4h",
					Target:    "2026-01-15T08:00:00Z",
					Interval:  "4h",
					CreatedAt: "2026-01-10T00:00:00Z",
					FireCount: 1,
				},
			},
		},
		"tkt-recurring-exhausted": {
			Version:   1,
			Title:     "limited recurring",
			Status:    "open",
			Priority:  2,
			Type:      "task",
			CreatedBy: "@agent/creator:bureau.local",
			CreatedAt: "2026-01-10T00:00:00Z",
			UpdatedAt: "2026-01-10T00:00:00Z",
			Gates: []schema.TicketGate{
				{
					ID:             "limited",
					Type:           "timer",
					Status:         "satisfied",
					Duration:       "1h",
					Target:         "2026-01-15T07:00:00Z",
					Schedule:       "0 7 * * *",
					CreatedAt:      "2026-01-10T00:00:00Z",
					FireCount:      4,
					MaxOccurrences: 5,
				},
			},
		},
		"tkt-non-recurring": {
			Version:   1,
			Title:     "normal ticket",
			Status:    "open",
			Priority:  2,
			Type:      "task",
			CreatedBy: "@agent/creator:bureau.local",
			CreatedAt: "2026-01-10T00:00:00Z",
			UpdatedAt: "2026-01-10T00:00:00Z",
		},
	})

	return map[string]*roomState{
		"!room:bureau.local": room,
	}
}

func TestHandleCloseRecurringScheduleRearms(t *testing.T) {
	env := testMutationServer(t, recurringRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-recurring-schedule",
		"room":   "!room:bureau.local",
		"reason": "done",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Ticket should reopen instead of closing.
	if result.Content.Status != "open" {
		t.Errorf("status: got %q, want 'open' (re-armed)", result.Content.Status)
	}
	if result.Content.ClosedAt != "" {
		t.Errorf("closed_at should be empty (re-armed), got %q", result.Content.ClosedAt)
	}
	if result.Content.CloseReason != "" {
		t.Errorf("close_reason should be empty (re-armed), got %q", result.Content.CloseReason)
	}
	if result.Content.Assignee != "" {
		t.Errorf("assignee should be cleared, got %q", result.Content.Assignee)
	}

	// Gate should be re-armed with a new target.
	gate := result.Content.Gates[0]
	if gate.Status != "pending" {
		t.Errorf("gate status: got %q, want 'pending'", gate.Status)
	}
	// testClockEpoch is 2026-01-15T12:00:00Z. Next cron occurrence
	// for "0 7 * * *" after that is 2026-01-16T07:00:00Z.
	if gate.Target != "2026-01-16T07:00:00Z" {
		t.Errorf("gate target: got %q, want '2026-01-16T07:00:00Z'", gate.Target)
	}
	if gate.FireCount != 5 {
		t.Errorf("gate fire_count: got %d, want 5", gate.FireCount)
	}
	if gate.LastFiredAt != "2026-01-15T12:00:00Z" {
		t.Errorf("gate last_fired_at: got %q, want '2026-01-15T12:00:00Z'", gate.LastFiredAt)
	}
	if gate.SatisfiedAt != "" {
		t.Errorf("gate satisfied_at should be cleared, got %q", gate.SatisfiedAt)
	}
	if gate.SatisfiedBy != "" {
		t.Errorf("gate satisfied_by should be cleared, got %q", gate.SatisfiedBy)
	}
}

func TestHandleCloseRecurringIntervalRearms(t *testing.T) {
	env := testMutationServer(t, recurringRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-recurring-interval",
		"room":   "!room:bureau.local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != "open" {
		t.Errorf("status: got %q, want 'open' (re-armed)", result.Content.Status)
	}

	gate := result.Content.Gates[0]
	if gate.Status != "pending" {
		t.Errorf("gate status: got %q, want 'pending'", gate.Status)
	}
	// testClockEpoch + 4h = 2026-01-15T16:00:00Z
	if gate.Target != "2026-01-15T16:00:00Z" {
		t.Errorf("gate target: got %q, want '2026-01-15T16:00:00Z'", gate.Target)
	}
	if gate.FireCount != 2 {
		t.Errorf("gate fire_count: got %d, want 2", gate.FireCount)
	}
}

func TestHandleCloseRecurringExhaustedClosesNormally(t *testing.T) {
	env := testMutationServer(t, recurringRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-recurring-exhausted",
		"room":   "!room:bureau.local",
		"reason": "final",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// MaxOccurrences reached — ticket should close normally.
	if result.Content.Status != "closed" {
		t.Errorf("status: got %q, want 'closed' (exhausted)", result.Content.Status)
	}
	if result.Content.ClosedAt == "" {
		t.Error("closed_at should be set for normal close")
	}
	if result.Content.CloseReason != "final" {
		t.Errorf("close_reason: got %q, want 'final'", result.Content.CloseReason)
	}

	// Gate metadata should reflect the final fire.
	gate := result.Content.Gates[0]
	if gate.FireCount != 5 {
		t.Errorf("gate fire_count: got %d, want 5", gate.FireCount)
	}
}

func TestHandleCloseEndRecurrenceClosesNormally(t *testing.T) {
	env := testMutationServer(t, recurringRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket":         "tkt-recurring-schedule",
		"room":           "!room:bureau.local",
		"reason":         "manual stop",
		"end_recurrence": true,
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// EndRecurrence strips recurring gates and closes normally.
	if result.Content.Status != "closed" {
		t.Errorf("status: got %q, want 'closed'", result.Content.Status)
	}
	if result.Content.CloseReason != "manual stop" {
		t.Errorf("close_reason: got %q, want 'manual stop'", result.Content.CloseReason)
	}
	if len(result.Content.Gates) != 0 {
		t.Errorf("gates should be empty after end_recurrence, got %d", len(result.Content.Gates))
	}
}

func TestHandleCloseNonRecurringClosesNormally(t *testing.T) {
	env := testMutationServer(t, recurringRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-non-recurring",
		"room":   "!room:bureau.local",
		"reason": "done",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != "closed" {
		t.Errorf("status: got %q, want 'closed'", result.Content.Status)
	}
}

// --- Reopen tests ---

func TestHandleReopen(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "reopen", map[string]any{
		"ticket": "tkt-closed",
		"room":   "!room:bureau.local",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Content.Status != "open" {
		t.Errorf("status: got %q, want 'open'", result.Content.Status)
	}
	if result.Content.ClosedAt != "" {
		t.Errorf("closed_at should be cleared, got %q", result.Content.ClosedAt)
	}
	if result.Content.CloseReason != "" {
		t.Errorf("close_reason should be cleared, got %q", result.Content.CloseReason)
	}
}

func TestHandleReopenNotClosed(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "reopen", map[string]any{
		"ticket": "tkt-open",
	}, nil)
	requireServiceError(t, err)
}

// --- Batch create tests ---

func TestHandleBatchCreate(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result batchCreateResponse
	err := env.client.Call(context.Background(), "batch-create", map[string]any{
		"room": "!room:bureau.local",
		"tickets": []map[string]any{
			{"ref": "a", "title": "first task", "type": "task", "priority": 2},
			{"ref": "b", "title": "second task", "type": "task", "priority": 2, "blocked_by": []string{"a"}},
			{"ref": "c", "title": "third task", "type": "task", "priority": 2, "blocked_by": []string{"b"}},
		},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Room != "!room:bureau.local" {
		t.Errorf("room: got %q", result.Room)
	}
	if len(result.Refs) != 3 {
		t.Fatalf("expected 3 refs, got %d", len(result.Refs))
	}

	// Verify symbolic refs were resolved correctly.
	idA := result.Refs["a"]
	idB := result.Refs["b"]
	idC := result.Refs["c"]

	index := env.service.rooms["!room:bureau.local"].index
	contentB, exists := index.Get(idB)
	if !exists {
		t.Fatalf("ticket B (%s) not in index", idB)
	}
	if len(contentB.BlockedBy) != 1 || contentB.BlockedBy[0] != idA {
		t.Errorf("B.blocked_by: got %v, want [%s]", contentB.BlockedBy, idA)
	}

	contentC, exists := index.Get(idC)
	if !exists {
		t.Fatalf("ticket C (%s) not in index", idC)
	}
	if len(contentC.BlockedBy) != 1 || contentC.BlockedBy[0] != idB {
		t.Errorf("C.blocked_by: got %v, want [%s]", contentC.BlockedBy, idB)
	}

	// Verify all three state events were written.
	env.writer.mu.Lock()
	defer env.writer.mu.Unlock()
	if len(env.writer.events) != 3 {
		t.Errorf("expected 3 written events, got %d", len(env.writer.events))
	}
}

func TestHandleBatchCreateDuplicateRef(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "batch-create", map[string]any{
		"room": "!room:bureau.local",
		"tickets": []map[string]any{
			{"ref": "a", "title": "first", "type": "task", "priority": 2},
			{"ref": "a", "title": "duplicate", "type": "task", "priority": 2},
		},
	}, nil)
	requireServiceError(t, err)
}

func TestHandleBatchCreateInvalidRef(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "batch-create", map[string]any{
		"room": "!room:bureau.local",
		"tickets": []map[string]any{
			{"ref": "a", "title": "task", "type": "task", "priority": 2, "blocked_by": []string{"nonexistent"}},
		},
	}, nil)
	requireServiceError(t, err)
}

func TestHandleBatchCreateWithExistingDep(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// blocked_by references an existing ticket (not a symbolic ref).
	var result batchCreateResponse
	err := env.client.Call(context.Background(), "batch-create", map[string]any{
		"room": "!room:bureau.local",
		"tickets": []map[string]any{
			{"ref": "a", "title": "depends on existing", "type": "task", "priority": 2, "blocked_by": []string{"tkt-open"}},
		},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	idA := result.Refs["a"]
	content, _ := env.service.rooms["!room:bureau.local"].index.Get(idA)
	if len(content.BlockedBy) != 1 || content.BlockedBy[0] != "tkt-open" {
		t.Errorf("blocked_by: got %v, want [tkt-open]", content.BlockedBy)
	}
}

// --- Resolve gate tests ---

func TestHandleResolveGate(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "resolve-gate", map[string]any{
		"ticket": "tkt-gated",
		"room":   "!room:bureau.local",
		"gate":   "human-review",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Find the human-review gate in the response.
	var gate *schema.TicketGate
	for i := range result.Content.Gates {
		if result.Content.Gates[i].ID == "human-review" {
			gate = &result.Content.Gates[i]
			break
		}
	}
	if gate == nil {
		t.Fatal("human-review gate not found in response")
	}
	if gate.Status != "satisfied" {
		t.Errorf("gate status: got %q, want 'satisfied'", gate.Status)
	}
	if gate.SatisfiedBy != "@agent/tester:bureau.local" {
		t.Errorf("satisfied_by: got %q", gate.SatisfiedBy)
	}
	if gate.SatisfiedAt == "" {
		t.Error("satisfied_at should be set")
	}
}

func TestHandleResolveGateNotHuman(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// ci-pass is a pipeline gate, not human.
	err := env.client.Call(context.Background(), "resolve-gate", map[string]any{
		"ticket": "tkt-gated",
		"gate":   "ci-pass",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleResolveGateAlreadySatisfied(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// First resolve succeeds.
	err := env.client.Call(context.Background(), "resolve-gate", map[string]any{
		"ticket": "tkt-gated",
		"room":   "!room:bureau.local",
		"gate":   "human-review",
	}, nil)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	// Second resolve should fail.
	err = env.client.Call(context.Background(), "resolve-gate", map[string]any{
		"ticket": "tkt-gated",
		"room":   "!room:bureau.local",
		"gate":   "human-review",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleResolveGateNotFound(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "resolve-gate", map[string]any{
		"ticket": "tkt-gated",
		"gate":   "nonexistent",
	}, nil)
	requireServiceError(t, err)
}

// --- Update gate tests ---

func TestHandleUpdateGate(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "update-gate", map[string]any{
		"ticket":       "tkt-gated",
		"room":         "!room:bureau.local",
		"gate":         "ci-pass",
		"status":       "satisfied",
		"satisfied_by": "$pipeline-event-123",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var gate *schema.TicketGate
	for i := range result.Content.Gates {
		if result.Content.Gates[i].ID == "ci-pass" {
			gate = &result.Content.Gates[i]
			break
		}
	}
	if gate == nil {
		t.Fatal("ci-pass gate not found in response")
	}
	if gate.Status != "satisfied" {
		t.Errorf("gate status: got %q, want 'satisfied'", gate.Status)
	}
	if gate.SatisfiedBy != "$pipeline-event-123" {
		t.Errorf("satisfied_by: got %q, want '$pipeline-event-123'", gate.SatisfiedBy)
	}
}

func TestHandleUpdateGateNotFound(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "update-gate", map[string]any{
		"ticket": "tkt-gated",
		"gate":   "nonexistent",
		"status": "satisfied",
	}, nil)
	requireServiceError(t, err)
}

func TestHandleUpdateGateMissingStatus(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "update-gate", map[string]any{
		"ticket": "tkt-gated",
		"gate":   "ci-pass",
	}, nil)
	requireServiceError(t, err)
}

// --- Fine-grained grant enforcement tests ---
//
// These verify that close and reopen operations require dedicated
// grants (ticket/close, ticket/reopen) separate from ticket/update.
// A principal with ticket/update but not ticket/close cannot close
// tickets — this enables the asymmetric permission model where PMs
// close tickets and workers only update them.

func TestCloseRequiresCloseGrant(t *testing.T) {
	// Token has ticket/update but NOT ticket/close.
	env := testMutationServerWithGrants(t, mutationRooms(), []servicetoken.Grant{
		{Actions: []string{"ticket/update"}},
	})
	defer env.cleanup()

	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"reason": "done",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if serviceErr.Action != "close" {
		t.Errorf("action: got %q, want 'close'", serviceErr.Action)
	}
}

func TestReopenRequiresReopenGrant(t *testing.T) {
	// Token has ticket/update but NOT ticket/reopen.
	env := testMutationServerWithGrants(t, mutationRooms(), []servicetoken.Grant{
		{Actions: []string{"ticket/update"}},
	})
	defer env.cleanup()

	err := env.client.Call(context.Background(), "reopen", map[string]any{
		"ticket": "tkt-closed",
		"room":   "!room:bureau.local",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if serviceErr.Action != "reopen" {
		t.Errorf("action: got %q, want 'reopen'", serviceErr.Action)
	}
}

func TestUpdateToClosedRequiresCloseGrant(t *testing.T) {
	// Token has ticket/update but NOT ticket/close. Closing via the
	// update action (status: "closed") should still be denied.
	env := testMutationServerWithGrants(t, mutationRooms(), []servicetoken.Grant{
		{Actions: []string{"ticket/update"}},
	})
	defer env.cleanup()

	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"status": "closed",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if serviceErr.Action != "update" {
		t.Errorf("action: got %q, want 'update'", serviceErr.Action)
	}
}

func TestUpdateFromClosedRequiresReopenGrant(t *testing.T) {
	// Token has ticket/update but NOT ticket/reopen. Reopening via
	// the update action (status: "open" on a closed ticket) should
	// still be denied.
	env := testMutationServerWithGrants(t, mutationRooms(), []servicetoken.Grant{
		{Actions: []string{"ticket/update"}},
	})
	defer env.cleanup()

	err := env.client.Call(context.Background(), "update", map[string]any{
		"ticket": "tkt-closed",
		"room":   "!room:bureau.local",
		"status": "open",
	}, nil)
	serviceErr := requireServiceError(t, err)
	if serviceErr.Action != "update" {
		t.Errorf("action: got %q, want 'update'", serviceErr.Action)
	}
}

func TestCloseAllowedWithCloseGrant(t *testing.T) {
	// Token has both ticket/update and ticket/close. Close should work.
	env := testMutationServerWithGrants(t, mutationRooms(), []servicetoken.Grant{
		{Actions: []string{"ticket/update", "ticket/close"}},
	})
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "close", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"reason": "done",
	}, &result)
	if err != nil {
		t.Fatalf("close with ticket/close grant should succeed: %v", err)
	}
	if result.Content.Status != "closed" {
		t.Errorf("status: got %q, want 'closed'", result.Content.Status)
	}
}

func TestReopenAllowedWithReopenGrant(t *testing.T) {
	// Token has both ticket/update and ticket/reopen. Reopen should work.
	env := testMutationServerWithGrants(t, mutationRooms(), []servicetoken.Grant{
		{Actions: []string{"ticket/update", "ticket/reopen"}},
	})
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "reopen", map[string]any{
		"ticket": "tkt-closed",
		"room":   "!room:bureau.local",
	}, &result)
	if err != nil {
		t.Fatalf("reopen with ticket/reopen grant should succeed: %v", err)
	}
	if result.Content.Status != "open" {
		t.Errorf("status: got %q, want 'open'", result.Content.Status)
	}
}

func TestHandleCreateWithSchedule(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "daily health check",
		"type":     "chore",
		"priority": 3,
		"schedule": "0 7 * * *",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	content, exists := env.service.rooms["!room:bureau.local"].index.Get(result.ID)
	if !exists {
		t.Fatalf("ticket %s not in index after create", result.ID)
	}

	if len(content.Gates) != 1 {
		t.Fatalf("expected 1 gate, got %d", len(content.Gates))
	}
	gate := content.Gates[0]
	if gate.ID != "schedule" {
		t.Errorf("gate ID: got %q, want 'schedule'", gate.ID)
	}
	if gate.Type != "timer" {
		t.Errorf("gate Type: got %q, want 'timer'", gate.Type)
	}
	if gate.Schedule != "0 7 * * *" {
		t.Errorf("gate Schedule: got %q, want '0 7 * * *'", gate.Schedule)
	}
	if gate.Status != "pending" {
		t.Errorf("gate Status: got %q, want 'pending'", gate.Status)
	}
	// testClockEpoch is 2026-01-15T12:00:00Z. Next 7am is 2026-01-16T07:00:00Z.
	if gate.Target != "2026-01-16T07:00:00Z" {
		t.Errorf("gate Target: got %q, want '2026-01-16T07:00:00Z'", gate.Target)
	}
}

func TestHandleCreateWithInterval(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "rotate API keys",
		"type":     "chore",
		"priority": 3,
		"interval": "720h",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	content, exists := env.service.rooms["!room:bureau.local"].index.Get(result.ID)
	if !exists {
		t.Fatalf("ticket %s not in index after create", result.ID)
	}

	if len(content.Gates) != 1 {
		t.Fatalf("expected 1 gate, got %d", len(content.Gates))
	}
	gate := content.Gates[0]
	if gate.ID != "interval" {
		t.Errorf("gate ID: got %q, want 'interval'", gate.ID)
	}
	if gate.Interval != "720h" {
		t.Errorf("gate Interval: got %q, want '720h'", gate.Interval)
	}
	// testClockEpoch + 720h = 2026-01-15T12:00:00Z + 30 days = 2026-02-14T12:00:00Z.
	if gate.Target != "2026-02-14T12:00:00Z" {
		t.Errorf("gate Target: got %q, want '2026-02-14T12:00:00Z'", gate.Target)
	}
}

func TestHandleCreateWithScheduleAndExistingGates(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	// Create with both an explicit gate and a convenience schedule.
	// The schedule gate should be appended alongside the explicit gate.
	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "reviewed health check",
		"type":     "chore",
		"priority": 2,
		"schedule": "0 7 * * *",
		"gates": []map[string]any{
			{
				"id":     "approval",
				"type":   "human",
				"status": "pending",
			},
		},
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	content, exists := env.service.rooms["!room:bureau.local"].index.Get(result.ID)
	if !exists {
		t.Fatalf("ticket %s not in index after create", result.ID)
	}

	if len(content.Gates) != 2 {
		t.Fatalf("expected 2 gates (explicit + schedule), got %d", len(content.Gates))
	}
	if content.Gates[0].ID != "approval" {
		t.Errorf("first gate ID: got %q, want 'approval'", content.Gates[0].ID)
	}
	if content.Gates[1].ID != "schedule" {
		t.Errorf("second gate ID: got %q, want 'schedule'", content.Gates[1].ID)
	}
}

func TestHandleCreateScheduleAndIntervalMutuallyExclusive(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "bad request",
		"type":     "task",
		"priority": 2,
		"schedule": "0 7 * * *",
		"interval": "4h",
	}, nil)
	if err == nil {
		t.Fatal("expected error for mutually exclusive schedule and interval")
	}
}

func TestHandleCreateInvalidSchedule(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "bad schedule",
		"type":     "task",
		"priority": 2,
		"schedule": "not-a-cron",
	}, nil)
	if err == nil {
		t.Fatal("expected error for invalid cron expression")
	}
}

func TestHandleCreateInvalidInterval(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":     "!room:bureau.local",
		"title":    "bad interval",
		"type":     "task",
		"priority": 2,
		"interval": "not-a-duration",
	}, nil)
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

// --- handleDefer tests ---

func TestHandleDeferWithFor(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"for":    "24h",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.ID != "tkt-open" {
		t.Errorf("ID: got %q, want 'tkt-open'", result.ID)
	}

	// Verify the defer gate was created.
	gateIndex := findGate(result.Content.Gates, "defer")
	if gateIndex < 0 {
		t.Fatal("expected 'defer' gate on ticket")
	}
	gate := result.Content.Gates[gateIndex]
	if gate.Type != "timer" {
		t.Errorf("gate Type: got %q, want 'timer'", gate.Type)
	}
	if gate.Status != "pending" {
		t.Errorf("gate Status: got %q, want 'pending'", gate.Status)
	}
	// testClockEpoch + 24h = 2026-01-16T12:00:00Z.
	if gate.Target != "2026-01-16T12:00:00Z" {
		t.Errorf("gate Target: got %q, want '2026-01-16T12:00:00Z'", gate.Target)
	}
}

func TestHandleDeferWithUntil(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"until":  "2026-02-01T09:00:00Z",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	gateIndex := findGate(result.Content.Gates, "defer")
	if gateIndex < 0 {
		t.Fatal("expected 'defer' gate on ticket")
	}
	gate := result.Content.Gates[gateIndex]
	if gate.Target != "2026-02-01T09:00:00Z" {
		t.Errorf("gate Target: got %q, want '2026-02-01T09:00:00Z'", gate.Target)
	}
}

func TestHandleDeferUpdatesExistingGate(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	ctx := context.Background()

	// First defer.
	err := env.client.Call(ctx, "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"for":    "1h",
	}, nil)
	if err != nil {
		t.Fatalf("first defer: %v", err)
	}

	// Second defer should update the same gate, not create a second one.
	var result mutationResponse
	err = env.client.Call(ctx, "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"for":    "48h",
	}, &result)
	if err != nil {
		t.Fatalf("second defer: %v", err)
	}

	// Should still be exactly one defer gate.
	deferCount := 0
	for _, gate := range result.Content.Gates {
		if gate.ID == "defer" {
			deferCount++
		}
	}
	if deferCount != 1 {
		t.Fatalf("expected exactly 1 defer gate, got %d", deferCount)
	}

	gateIndex := findGate(result.Content.Gates, "defer")
	gate := result.Content.Gates[gateIndex]
	// testClockEpoch + 48h = 2026-01-17T12:00:00Z.
	if gate.Target != "2026-01-17T12:00:00Z" {
		t.Errorf("gate Target: got %q, want '2026-01-17T12:00:00Z'", gate.Target)
	}
	if gate.Status != "pending" {
		t.Errorf("gate Status: got %q, want 'pending'", gate.Status)
	}
}

func TestHandleDeferRequiresUntilOrFor(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
	}, nil)
	if err == nil {
		t.Fatal("expected error when neither until nor for is provided")
	}
}

func TestHandleDeferUntilAndForMutuallyExclusive(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"until":  "2026-02-01T09:00:00Z",
		"for":    "24h",
	}, nil)
	if err == nil {
		t.Fatal("expected error for mutually exclusive until and for")
	}
}

func TestHandleDeferUntilInThePast(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"until":  "2025-01-01T00:00:00Z",
	}, nil)
	if err == nil {
		t.Fatal("expected error for until time in the past")
	}
}

func TestHandleDeferClosedTicketSucceeds(t *testing.T) {
	// Deferring a closed ticket is allowed — the defer gate is added
	// and becomes relevant when the ticket is reopened. This avoids
	// forcing callers to reopen before deferring.
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "defer", map[string]any{
		"ticket": "tkt-closed",
		"room":   "!room:bureau.local",
		"for":    "24h",
	}, &result)
	if err != nil {
		t.Fatalf("defer on closed ticket should succeed: %v", err)
	}

	gateIndex := findGate(result.Content.Gates, "defer")
	if gateIndex < 0 {
		t.Fatal("expected 'defer' gate on closed ticket")
	}
}

func TestHandleCreateWithDeferFor(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result createResponse
	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":      "!room:bureau.local",
		"title":     "deferred task",
		"type":      "task",
		"priority":  3,
		"defer_for": "24h",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	content, exists := env.service.rooms["!room:bureau.local"].index.Get(result.ID)
	if !exists {
		t.Fatalf("ticket %s not in index after create", result.ID)
	}

	gateIndex := findGate(content.Gates, "defer")
	if gateIndex < 0 {
		t.Fatal("expected 'defer' gate on ticket")
	}
	gate := content.Gates[gateIndex]
	// testClockEpoch + 24h = 2026-01-16T12:00:00Z.
	if gate.Target != "2026-01-16T12:00:00Z" {
		t.Errorf("gate Target: got %q, want '2026-01-16T12:00:00Z'", gate.Target)
	}
}

func TestHandleCreateWithDeferUntilAndDeferForMutuallyExclusive(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	err := env.client.Call(context.Background(), "create", map[string]any{
		"room":        "!room:bureau.local",
		"title":       "bad request",
		"type":        "task",
		"priority":    2,
		"defer_until": "2026-02-01T00:00:00Z",
		"defer_for":   "24h",
	}, nil)
	if err == nil {
		t.Fatal("expected error for mutually exclusive defer_until and defer_for")
	}
}

func TestWildcardGrantCoversCloseAndReopen(t *testing.T) {
	// Token has ticket/* which should match ticket/close and
	// ticket/reopen. This verifies that the existing tests using
	// ticket/* continue to work with the new grant checks.
	env := testMutationServerWithGrants(t, mutationRooms(), []servicetoken.Grant{
		{Actions: []string{"ticket/*"}},
	})
	defer env.cleanup()

	ctx := context.Background()

	// Close should succeed.
	err := env.client.Call(ctx, "close", map[string]any{
		"ticket": "tkt-open",
		"room":   "!room:bureau.local",
		"reason": "done",
	}, nil)
	if err != nil {
		t.Fatalf("close with ticket/* grant should succeed: %v", err)
	}

	// Reopen should succeed (tkt-closed is already closed).
	err = env.client.Call(ctx, "reopen", map[string]any{
		"ticket": "tkt-closed",
		"room":   "!room:bureau.local",
	}, nil)
	if err != nil {
		t.Fatalf("reopen with ticket/* grant should succeed: %v", err)
	}
}

// --- upcoming-gates tests ---

func TestHandleUpcomingGates(t *testing.T) {
	rooms := map[string]*roomState{
		"!room1:bureau.local": newTrackedRoom(map[string]schema.TicketContent{
			"tkt-timer1": {
				Version:   1,
				Title:     "scheduled check",
				Status:    "open",
				Priority:  2,
				Type:      "chore",
				CreatedBy: "@agent/creator:bureau.local",
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
				Gates: []schema.TicketGate{
					{
						ID:       "schedule",
						Type:     "timer",
						Status:   "pending",
						Schedule: "0 7 * * *",
						Target:   "2026-01-16T07:00:00Z",
					},
				},
			},
			"tkt-timer2": {
				Version:   1,
				Title:     "interval poll",
				Status:    "open",
				Priority:  3,
				Type:      "chore",
				CreatedBy: "@agent/creator:bureau.local",
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
				Gates: []schema.TicketGate{
					{
						ID:       "interval",
						Type:     "timer",
						Status:   "pending",
						Interval: "4h",
						Target:   "2026-01-15T16:00:00Z",
					},
				},
			},
			"tkt-satisfied": {
				Version:   1,
				Title:     "already fired",
				Status:    "open",
				Priority:  2,
				Type:      "task",
				CreatedBy: "@agent/creator:bureau.local",
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
				Gates: []schema.TicketGate{
					{
						ID:          "done",
						Type:        "timer",
						Status:      "satisfied",
						Target:      "2026-01-14T00:00:00Z",
						SatisfiedAt: "2026-01-14T00:00:00Z",
					},
				},
			},
		}),
	}

	env := testMutationServer(t, rooms)
	defer env.cleanup()

	var results []upcomingGateEntry
	err := env.client.Call(context.Background(), "upcoming-gates", map[string]any{}, &results)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Should return 2 entries (timer1 and timer2). The satisfied gate
	// on tkt-satisfied should be excluded.
	if len(results) != 2 {
		t.Fatalf("expected 2 upcoming gates, got %d", len(results))
	}

	// Results should be sorted by target time: timer2 (16:00) before timer1 (07:00 next day).
	if results[0].TicketID != "tkt-timer2" {
		t.Errorf("first entry: got %q, want 'tkt-timer2'", results[0].TicketID)
	}
	if results[1].TicketID != "tkt-timer1" {
		t.Errorf("second entry: got %q, want 'tkt-timer1'", results[1].TicketID)
	}

	// Check metadata.
	if results[0].GateID != "interval" {
		t.Errorf("first gate ID: got %q, want 'interval'", results[0].GateID)
	}
	if results[0].Interval != "4h" {
		t.Errorf("first interval: got %q, want '4h'", results[0].Interval)
	}
	if results[0].Title != "interval poll" {
		t.Errorf("first title: got %q, want 'interval poll'", results[0].Title)
	}
	// testClockEpoch is 2026-01-15T12:00:00Z, target is 2026-01-15T16:00:00Z = 4h ahead.
	if results[0].UntilFire != "4h" {
		t.Errorf("first until_fire: got %q, want '4h'", results[0].UntilFire)
	}

	if results[1].Schedule != "0 7 * * *" {
		t.Errorf("second schedule: got %q, want '0 7 * * *'", results[1].Schedule)
	}
	// 2026-01-16T07:00:00Z - 2026-01-15T12:00:00Z = 19h.
	if results[1].UntilFire != "19h" {
		t.Errorf("second until_fire: got %q, want '19h'", results[1].UntilFire)
	}
}

func TestHandleUpcomingGatesRoomFilter(t *testing.T) {
	rooms := map[string]*roomState{
		"!room1:bureau.local": newTrackedRoom(map[string]schema.TicketContent{
			"tkt-a": {
				Version: 1, Title: "room1 task", Status: "open",
				Priority: 2, Type: "chore",
				CreatedBy: "@agent/creator:bureau.local",
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
				Gates: []schema.TicketGate{
					{ID: "t", Type: "timer", Status: "pending", Target: "2026-01-16T00:00:00Z"},
				},
			},
		}),
		"!room2:bureau.local": newTrackedRoom(map[string]schema.TicketContent{
			"tkt-b": {
				Version: 1, Title: "room2 task", Status: "open",
				Priority: 2, Type: "chore",
				CreatedBy: "@agent/creator:bureau.local",
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
				Gates: []schema.TicketGate{
					{ID: "t", Type: "timer", Status: "pending", Target: "2026-01-17T00:00:00Z"},
				},
			},
		}),
	}

	env := testMutationServer(t, rooms)
	defer env.cleanup()

	var results []upcomingGateEntry
	err := env.client.Call(context.Background(), "upcoming-gates", map[string]any{
		"room": "!room1:bureau.local",
	}, &results)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 gate from room1, got %d", len(results))
	}
	if results[0].TicketID != "tkt-a" {
		t.Errorf("ticket ID: got %q, want 'tkt-a'", results[0].TicketID)
	}
}

func TestHandleUpcomingGatesEmpty(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var results []upcomingGateEntry
	err := env.client.Call(context.Background(), "upcoming-gates", map[string]any{}, &results)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// mutationRooms has a tkt-gated ticket but its gates are human/pipeline type, not timer.
	if len(results) != 0 {
		t.Errorf("expected 0 upcoming gates from mutationRooms, got %d", len(results))
	}
}

// --- Timer lifecycle tests ---

// waitForGateStatus blocks until the named gate on the ticket reaches
// the expected status. It synchronizes with the timer loop goroutine
// by waiting for fakeWriter write notifications — each notification
// means a state event was written, so the test checks whether the
// write produced the expected gate status. If the gate never reaches
// the expected status, Go's test timeout catches the hang.
func waitForGateStatus(t *testing.T, env *testEnv, room, ticketID, gateID, expectedStatus string) schema.TicketContent {
	t.Helper()

	for {
		<-env.writer.notify

		var result mutationResponse
		err := env.client.Call(context.Background(), "show", map[string]any{
			"ticket": ticketID,
			"room":   room,
		}, &result)
		if err != nil {
			t.Fatalf("show %s: %v", ticketID, err)
		}

		for _, gate := range result.Content.Gates {
			if gate.ID == gateID && gate.Status == expectedStatus {
				return result.Content
			}
		}
	}
}

func TestTimerLifecycleScheduleFireAndRearm(t *testing.T) {
	// Full lifecycle: create ticket with schedule → gate fires →
	// close ticket → auto-rearm → gate fires again → end-recurrence.
	env := testMutationServerWithTimers(t, mutationRooms())
	defer env.cleanup()

	ctx := context.Background()
	room := "!room:bureau.local"

	// Step 1: Create a ticket with a cron schedule (daily at 7am UTC).
	// testClockEpoch is 2026-01-15T12:00:00Z, so first target is
	// 2026-01-16T07:00:00Z (19 hours from now).
	var createResult createResponse
	err := env.client.Call(ctx, "create", map[string]any{
		"room":     room,
		"title":    "daily check",
		"type":     "chore",
		"priority": 3,
		"schedule": "0 7 * * *",
	}, &createResult)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ticketID := createResult.ID

	// Verify the initial gate state.
	var showResult mutationResponse
	err = env.client.Call(ctx, "show", map[string]any{
		"ticket": ticketID,
		"room":   room,
	}, &showResult)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if len(showResult.Content.Gates) != 1 {
		t.Fatalf("expected 1 gate, got %d", len(showResult.Content.Gates))
	}
	gate := showResult.Content.Gates[0]
	if gate.Target != "2026-01-16T07:00:00Z" {
		t.Fatalf("initial target: got %q, want '2026-01-16T07:00:00Z'", gate.Target)
	}

	// Step 2: Advance clock past the first target. The timer loop
	// should fire the gate.
	env.clock.Advance(20 * time.Hour) // Now: 2026-01-16T08:00:00Z

	content := waitForGateStatus(t, env, room, ticketID, "schedule", "satisfied")
	if content.Gates[0].SatisfiedBy != "timer" {
		t.Errorf("satisfied_by: got %q, want 'timer'", content.Gates[0].SatisfiedBy)
	}

	// Step 3: Close the ticket. Since it has a recurring schedule gate,
	// it should auto-rearm: reopen as "open" with a new target.
	var closeResult mutationResponse
	err = env.client.Call(ctx, "close", map[string]any{
		"ticket": ticketID,
		"room":   room,
	}, &closeResult)
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	// After rearm, status should be "open" (not "closed").
	if closeResult.Content.Status != "open" {
		t.Fatalf("status after rearm: got %q, want 'open'", closeResult.Content.Status)
	}
	rearmedGate := closeResult.Content.Gates[0]
	if rearmedGate.Status != "pending" {
		t.Errorf("gate status after rearm: got %q, want 'pending'", rearmedGate.Status)
	}
	if rearmedGate.FireCount != 1 {
		t.Errorf("fire_count after rearm: got %d, want 1", rearmedGate.FireCount)
	}
	// Next 7am after 2026-01-16T08:00:00Z is 2026-01-17T07:00:00Z.
	if rearmedGate.Target != "2026-01-17T07:00:00Z" {
		t.Errorf("rearmed target: got %q, want '2026-01-17T07:00:00Z'", rearmedGate.Target)
	}

	// Step 4: Advance to the second occurrence and verify it fires again.
	env.clock.Advance(24 * time.Hour) // Now: 2026-01-17T08:00:00Z

	waitForGateStatus(t, env, room, ticketID, "schedule", "satisfied")

	// Step 5: Close with --end-recurrence to permanently close.
	// Use a fresh result variable to avoid stale slice values from
	// the previous close response (CBOR omitempty skips empty slices,
	// so decoding into a pre-used struct leaves old values).
	var endResult mutationResponse
	err = env.client.Call(ctx, "close", map[string]any{
		"ticket":         ticketID,
		"room":           room,
		"end_recurrence": true,
		"reason":         "no longer needed",
	}, &endResult)
	if err != nil {
		t.Fatalf("close with end_recurrence: %v", err)
	}
	if endResult.Content.Status != "closed" {
		t.Errorf("status after end-recurrence: got %q, want 'closed'", endResult.Content.Status)
	}
	// Recurring gates should be stripped.
	for _, gate := range endResult.Content.Gates {
		if gate.Schedule != "" || gate.Interval != "" {
			t.Errorf("recurring gate %q was not stripped", gate.ID)
		}
	}
}

func TestTimerLifecycleIntervalFireAndRearm(t *testing.T) {
	env := testMutationServerWithTimers(t, mutationRooms())
	defer env.cleanup()

	ctx := context.Background()
	room := "!room:bureau.local"

	// Create a ticket with a 4-hour interval.
	// testClockEpoch is 2026-01-15T12:00:00Z, target: 16:00:00Z.
	var createResult createResponse
	err := env.client.Call(ctx, "create", map[string]any{
		"room":     room,
		"title":    "periodic poll",
		"type":     "chore",
		"priority": 3,
		"interval": "4h",
	}, &createResult)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ticketID := createResult.ID

	// Advance past the first target.
	env.clock.Advance(5 * time.Hour) // Now: 2026-01-15T17:00:00Z

	content := waitForGateStatus(t, env, room, ticketID, "interval", "satisfied")
	if content.Gates[0].SatisfiedBy != "timer" {
		t.Errorf("satisfied_by: got %q, want 'timer'", content.Gates[0].SatisfiedBy)
	}

	// Close the ticket. Interval rearm: next target is now + 4h.
	var closeResult mutationResponse
	err = env.client.Call(ctx, "close", map[string]any{
		"ticket": ticketID,
		"room":   room,
	}, &closeResult)
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	if closeResult.Content.Status != "open" {
		t.Fatalf("status after rearm: got %q, want 'open'", closeResult.Content.Status)
	}
	rearmedGate := closeResult.Content.Gates[0]
	// Rearm target: 2026-01-15T17:00:00Z + 4h = 2026-01-15T21:00:00Z.
	if rearmedGate.Target != "2026-01-15T21:00:00Z" {
		t.Errorf("rearmed target: got %q, want '2026-01-15T21:00:00Z'", rearmedGate.Target)
	}
}

func TestTimerLifecycleMaxOccurrences(t *testing.T) {
	// A recurring gate with max_occurrences=2 should stop rearming
	// after 2 fires.
	rooms := map[string]*roomState{
		"!room:bureau.local": newTrackedRoom(map[string]schema.TicketContent{
			"tkt-max": {
				Version:   1,
				Title:     "limited recurring",
				Status:    "open",
				Priority:  2,
				Type:      "chore",
				CreatedBy: "@agent/creator:bureau.local",
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
				Gates: []schema.TicketGate{
					{
						ID:             "interval",
						Type:           "timer",
						Status:         "pending",
						Interval:       "1h",
						Target:         "2026-01-15T13:00:00Z",
						MaxOccurrences: 2,
						CreatedAt:      "2026-01-01T00:00:00Z",
					},
				},
			},
		}),
	}

	env := testMutationServerWithTimers(t, rooms)
	defer env.cleanup()

	ctx := context.Background()
	room := "!room:bureau.local"

	// Fire the first occurrence.
	env.clock.Advance(2 * time.Hour) // Now: 2026-01-15T14:00:00Z
	waitForGateStatus(t, env, room, "tkt-max", "interval", "satisfied")

	// Close — should rearm (fire_count becomes 1, max is 2).
	var closeResult mutationResponse
	err := env.client.Call(ctx, "close", map[string]any{
		"ticket": "tkt-max",
		"room":   room,
	}, &closeResult)
	if err != nil {
		t.Fatalf("first close: %v", err)
	}
	if closeResult.Content.Status != "open" {
		t.Fatalf("expected rearm after first fire, got status %q", closeResult.Content.Status)
	}

	// Fire the second occurrence.
	env.clock.Advance(2 * time.Hour) // Now: 2026-01-15T16:00:00Z
	waitForGateStatus(t, env, room, "tkt-max", "interval", "satisfied")

	// Close — should NOT rearm (fire_count becomes 2 == max_occurrences).
	err = env.client.Call(ctx, "close", map[string]any{
		"ticket": "tkt-max",
		"room":   room,
	}, &closeResult)
	if err != nil {
		t.Fatalf("second close: %v", err)
	}
	if closeResult.Content.Status != "closed" {
		t.Fatalf("expected closed after max occurrences, got status %q", closeResult.Content.Status)
	}
}

func TestTimerLifecycleDeferCreateAndFire(t *testing.T) {
	env := testMutationServerWithTimers(t, mutationRooms())
	defer env.cleanup()

	ctx := context.Background()
	room := "!room:bureau.local"

	// Defer tkt-open for 2 hours.
	var deferResult mutationResponse
	err := env.client.Call(ctx, "defer", map[string]any{
		"ticket": "tkt-open",
		"room":   room,
		"for":    "2h",
	}, &deferResult)
	if err != nil {
		t.Fatalf("defer: %v", err)
	}

	// Verify the gate is pending.
	gateIndex := findGate(deferResult.Content.Gates, "defer")
	if gateIndex < 0 {
		t.Fatal("expected 'defer' gate")
	}
	if deferResult.Content.Gates[gateIndex].Status != "pending" {
		t.Fatalf("gate status: got %q, want 'pending'", deferResult.Content.Gates[gateIndex].Status)
	}

	// Advance past the defer target.
	env.clock.Advance(3 * time.Hour) // Now: 2026-01-15T15:00:00Z

	content := waitForGateStatus(t, env, room, "tkt-open", "defer", "satisfied")
	if content.Gates[gateIndex].SatisfiedBy != "timer" {
		t.Errorf("satisfied_by: got %q, want 'timer'", content.Gates[gateIndex].SatisfiedBy)
	}
}

// --- Deadline tests ---

func TestDeadlineCRUD(t *testing.T) {
	room := "!deadline-room:bureau.local"

	t.Run("CreateWithDeadline", func(t *testing.T) {
		rooms := map[string]*roomState{room: newTrackedRoom(nil)}
		env := testMutationServer(t, rooms)
		defer env.cleanup()

		var result createResponse
		err := env.client.Call(context.Background(), "create", map[string]any{
			"room": room, "title": "Ship v2 API", "type": "task", "priority": 2,
			"deadline": "2026-03-01T00:00:00Z",
		}, &result)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		content, exists := rooms[room].index.Get(result.ID)
		if !exists {
			t.Fatalf("ticket %s not in index", result.ID)
		}
		if content.Deadline != "2026-03-01T00:00:00Z" {
			t.Errorf("deadline: got %q, want %q", content.Deadline, "2026-03-01T00:00:00Z")
		}
	})

	t.Run("CreateWithInvalidDeadline", func(t *testing.T) {
		rooms := map[string]*roomState{room: newTrackedRoom(nil)}
		env := testMutationServer(t, rooms)
		defer env.cleanup()

		var result createResponse
		err := env.client.Call(context.Background(), "create", map[string]any{
			"room": room, "title": "Bad deadline", "type": "task", "priority": 2,
			"deadline": "not-a-date",
		}, &result)
		if err == nil {
			t.Fatal("expected error for invalid deadline")
		}
	})

	t.Run("UpdateSetDeadline", func(t *testing.T) {
		rooms := map[string]*roomState{room: newTrackedRoom(map[string]schema.TicketContent{
			"tkt-1": ticketFixture("No deadline yet", "open"),
		})}
		env := testMutationServer(t, rooms)
		defer env.cleanup()

		var result mutationResponse
		err := env.client.Call(context.Background(), "update", map[string]any{
			"ticket": "tkt-1", "room": room, "deadline": "2026-04-01T00:00:00Z",
		}, &result)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if result.Content.Deadline != "2026-04-01T00:00:00Z" {
			t.Errorf("deadline: got %q, want %q", result.Content.Deadline, "2026-04-01T00:00:00Z")
		}
	})

	t.Run("UpdateClearDeadline", func(t *testing.T) {
		fixture := ticketFixture("Has deadline", "open")
		fixture.Deadline = "2026-04-01T00:00:00Z"
		rooms := map[string]*roomState{room: newTrackedRoom(map[string]schema.TicketContent{
			"tkt-1": fixture,
		})}
		env := testMutationServer(t, rooms)
		defer env.cleanup()

		var result mutationResponse
		err := env.client.Call(context.Background(), "update", map[string]any{
			"ticket": "tkt-1", "room": room, "deadline": "",
		}, &result)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if result.Content.Deadline != "" {
			t.Errorf("deadline should be cleared, got %q", result.Content.Deadline)
		}
	})
}

// --- Deadline timer lifecycle tests ---

func TestDeadlineFireAddsNote(t *testing.T) {
	room := "!deadline-room:bureau.local"
	// Deadline 2 hours from test epoch.
	deadline := testClockEpoch.Add(2 * time.Hour).UTC().Format(time.RFC3339)

	rooms := map[string]*roomState{
		room: newTrackedRoom(nil),
	}
	env := testMutationServerWithTimers(t, rooms)
	defer env.cleanup()

	ctx := context.Background()
	var result createResponse
	err := env.client.Call(ctx, "create", map[string]any{
		"room":     room,
		"title":    "Ticket with deadline",
		"type":     "task",
		"priority": 2,
		"deadline": deadline,
	}, &result)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Advance past the deadline. The AfterFunc fires synchronously,
	// the timer loop goroutine processes it and writes.
	env.clock.Advance(3 * time.Hour)

	// Wait for the deadline note write. The create RPC also wrote,
	// so we loop until we see a note (skipping the create write).
	for {
		<-env.writer.notify
		var showResult showResponse
		if err := env.client.Call(ctx, "show", map[string]any{
			"ticket": result.ID,
			"room":   room,
		}, &showResult); err != nil {
			t.Fatalf("show: %v", err)
		}
		if len(showResult.Content.Notes) > 0 {
			if len(showResult.Content.Notes) != 1 {
				t.Fatalf("expected 1 note, got %d", len(showResult.Content.Notes))
			}
			expectedBody := "Deadline passed: " + deadline
			if showResult.Content.Notes[0].Body != expectedBody {
				t.Errorf("note body: got %q, want %q", showResult.Content.Notes[0].Body, expectedBody)
			}
			return
		}
	}
}

func TestDeadlineStaleEntryIgnored(t *testing.T) {
	// Update a deadline from 2h to 10h. Advance past both at once.
	// The stale (2h) heap entry should be skipped by lazy deletion
	// in fireDeadlineLocked (target mismatch). Only the new (10h)
	// deadline should produce a note. Asserting exactly 1 note
	// proves the stale entry didn't fire.
	room := "!deadline-room:bureau.local"
	originalDeadline := testClockEpoch.Add(2 * time.Hour).UTC().Format(time.RFC3339)
	newDeadline := testClockEpoch.Add(10 * time.Hour).UTC().Format(time.RFC3339)

	rooms := map[string]*roomState{
		room: newTrackedRoom(nil),
	}
	env := testMutationServerWithTimers(t, rooms)
	defer env.cleanup()

	ctx := context.Background()
	var result createResponse
	err := env.client.Call(ctx, "create", map[string]any{
		"room":     room,
		"title":    "Deadline will change",
		"type":     "task",
		"priority": 2,
		"deadline": originalDeadline,
	}, &result)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Update the deadline to later.
	var updateResult mutationResponse
	err = env.client.Call(ctx, "update", map[string]any{
		"ticket":   result.ID,
		"room":     room,
		"deadline": newDeadline,
	}, &updateResult)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	// Advance past both deadlines. The stale 2h entry fires first
	// but is skipped (target != content.Deadline). The 10h entry
	// fires and writes a note.
	env.clock.Advance(11 * time.Hour)

	// Wait for the deadline note write.
	for {
		<-env.writer.notify
		var showResult showResponse
		if err := env.client.Call(ctx, "show", map[string]any{
			"ticket": result.ID,
			"room":   room,
		}, &showResult); err != nil {
			t.Fatalf("show: %v", err)
		}
		if len(showResult.Content.Notes) > 0 {
			if len(showResult.Content.Notes) != 1 {
				t.Fatalf("expected exactly 1 note (stale entry should not fire), got %d", len(showResult.Content.Notes))
			}
			expectedBody := "Deadline passed: " + newDeadline
			if showResult.Content.Notes[0].Body != expectedBody {
				t.Errorf("note body: got %q, want %q", showResult.Content.Notes[0].Body, expectedBody)
			}
			return
		}
	}
}
