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
		{"list", map[string]any{"room": "!roomA:local"}},
		{"ready", map[string]any{"room": "!roomA:local"}},
		{"blocked", map[string]any{"room": "!roomA:local"}},
		{"show", map[string]any{"ticket": "tkt-1"}},
		{"children", map[string]any{"ticket": "tkt-1"}},
		{"grep", map[string]any{"pattern": "login"}},
		{"stats", map[string]any{"room": "!roomA:local"}},
		{"deps", map[string]any{"ticket": "tkt-3"}},
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
