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
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/ticket"
)

// --- Test infrastructure ---

// testServer creates a TicketService with a running socket server and
// returns a ServiceClient connected via a real minted token. The
// token carries ticket/* grants so all query actions are authorized.
// Call the returned cleanup function to shut down the server.
func testServer(t *testing.T, rooms map[string]*roomState) (*service.ServiceClient, func()) {
	t.Helper()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}

	authConfig := &service.AuthConfig{
		PublicKey: publicKey,
		Audience:  "ticket",
		Blacklist: servicetoken.NewBlacklist(),
	}

	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "ticket.sock")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := service.NewSocketServer(socketPath, logger, authConfig)

	ts := &TicketService{
		clock:     clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:     rooms,
		logger:    logger,
	}
	ts.registerActions(server)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	// Mint a token with ticket/* grants.
	tokenBytes := mintToken(t, privateKey, "agent/tester", []servicetoken.Grant{
		{Actions: []string{"ticket/*"}},
	})
	client := service.NewServiceClientFromToken(socketPath, tokenBytes)

	cleanup := func() {
		cancel()
		wg.Wait()
	}
	return client, cleanup
}

// testServerNoGrants is like testServer but the token carries no
// grants — all grant checks should fail.
func testServerNoGrants(t *testing.T, rooms map[string]*roomState) (*service.ServiceClient, func()) {
	t.Helper()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}

	authConfig := &service.AuthConfig{
		PublicKey: publicKey,
		Audience:  "ticket",
		Blacklist: servicetoken.NewBlacklist(),
	}

	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "ticket.sock")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := service.NewSocketServer(socketPath, logger, authConfig)

	ts := &TicketService{
		clock:     clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:     rooms,
		logger:    logger,
	}
	ts.registerActions(server)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	// Mint a token with no grants.
	tokenBytes := mintToken(t, privateKey, "agent/unauthorized", nil)
	client := service.NewServiceClientFromToken(socketPath, tokenBytes)

	cleanup := func() {
		cancel()
		wg.Wait()
	}
	return client, cleanup
}

// mintToken creates a signed test token with specific grants.
func mintToken(t *testing.T, privateKey ed25519.PrivateKey, subject string, grants []servicetoken.Grant) []byte {
	t.Helper()
	token := &servicetoken.Token{
		Subject:   subject,
		Machine:   "machine/test",
		Audience:  "ticket",
		Grants:    grants,
		ID:        "test-token",
		IssuedAt:  1735689600, // 2025-01-01T00:00:00Z
		ExpiresAt: 4070908800, // 2099-01-01T00:00:00Z
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
}

type writtenEvent struct {
	RoomID    string
	EventType string
	StateKey  string
	Content   any
}

func (f *fakeWriter) SendStateEvent(_ context.Context, roomID, eventType, stateKey string, content any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, writtenEvent{
		RoomID:    roomID,
		EventType: eventType,
		StateKey:  stateKey,
		Content:   content,
	})
	return "$event-" + stateKey, nil
}

// testEnv holds the server, client, and writer for mutation tests.
type testEnv struct {
	client  *service.ServiceClient
	writer  *fakeWriter
	service *TicketService
	cleanup func()
}

// testMutationServer creates a TicketService with a fakeWriter for
// mutation testing. Returns a testEnv with the client, writer, service,
// and cleanup function.
func testMutationServer(t *testing.T, rooms map[string]*roomState) *testEnv {
	t.Helper()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}

	authConfig := &service.AuthConfig{
		PublicKey: publicKey,
		Audience:  "ticket",
		Blacklist: servicetoken.NewBlacklist(),
	}

	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "ticket.sock")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := service.NewSocketServer(socketPath, logger, authConfig)

	writer := &fakeWriter{}
	ts := &TicketService{
		writer:     writer,
		clock:      clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		startedAt:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		serverName: "bureau.local",
		rooms:      rooms,
		logger:     logger,
	}
	ts.registerActions(server)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	tokenBytes := mintToken(t, privateKey, "agent/tester", []servicetoken.Grant{
		{Actions: []string{"ticket/*"}},
	})
	client := service.NewServiceClientFromToken(socketPath, tokenBytes)

	cleanup := func() {
		cancel()
		wg.Wait()
	}
	return &testEnv{
		client:  client,
		writer:  writer,
		service: ts,
		cleanup: cleanup,
	}
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
		"reason": "duplicate",
	}, nil)
	// closed → closed is a no-op status transition, not an error.
	// But we check via the validateStatusTransition behavior: same
	// status is allowed for all except in_progress.
	if err != nil {
		t.Fatalf("expected no error for closing already-closed ticket, got: %v", err)
	}
}

// --- Reopen tests ---

func TestHandleReopen(t *testing.T) {
	env := testMutationServer(t, mutationRooms())
	defer env.cleanup()

	var result mutationResponse
	err := env.client.Call(context.Background(), "reopen", map[string]any{
		"ticket": "tkt-closed",
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
		"gate":   "human-review",
	}, nil)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	// Second resolve should fail.
	err = env.client.Call(context.Background(), "resolve-gate", map[string]any{
		"ticket": "tkt-gated",
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
