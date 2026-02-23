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
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
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
func newTestServer(t *testing.T, rooms map[ref.RoomID]*roomState, opts testServerOpts) *testEnv {
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

	testServiceRef := mustParseService(t, "@bureau/fleet/prod/service/ticket/test:bureau.local")
	testMachineRef := mustParseMachine(t, "@bureau/fleet/prod/machine/test:bureau.local")
	ts := &TicketService{
		writer:      writer,
		clock:       testClock,
		startedAt:   time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		service:     testServiceRef,
		machine:     testMachineRef,
		rooms:       rooms,
		subscribers: make(map[ref.RoomID][]*subscriber),
		logger:      logger,
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
			Subject:   mustParseUserID(t, "@bureau/fleet/prod/"+subject+":bureau.local"),
			Machine:   mustParseMachine(t, "@bureau/fleet/prod/machine/test:bureau.local"),
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

func testServer(t *testing.T, rooms map[ref.RoomID]*roomState) (*service.ServiceClient, func()) {
	t.Helper()
	env := newTestServer(t, rooms, testServerOpts{noWriter: true})
	return env.client, env.cleanup
}

func testServerNoGrants(t *testing.T, rooms map[ref.RoomID]*roomState) (*service.ServiceClient, func()) {
	t.Helper()
	env := newTestServer(t, rooms, testServerOpts{noWriter: true, noGrants: true})
	return env.client, env.cleanup
}

func testServerWithGrants(t *testing.T, rooms map[ref.RoomID]*roomState, grants []servicetoken.Grant) (*service.ServiceClient, func()) {
	t.Helper()
	env := newTestServer(t, rooms, testServerOpts{noWriter: true, grants: grants})
	return env.client, env.cleanup
}

func testMutationServerWithGrants(t *testing.T, rooms map[ref.RoomID]*roomState, grants []servicetoken.Grant) *testEnv {
	t.Helper()
	return newTestServer(t, rooms, testServerOpts{grants: grants})
}

// mustParseUserID parses a raw Matrix user ID for test use.
func mustParseUserID(t *testing.T, raw string) ref.UserID {
	t.Helper()
	userID, err := ref.ParseUserID(raw)
	if err != nil {
		t.Fatalf("ParseUserID(%q): %v", raw, err)
	}
	return userID
}

// mustParseMachine parses a raw machine Matrix user ID for test use.
func mustParseMachine(t *testing.T, raw string) ref.Machine {
	t.Helper()
	machine, err := ref.ParseMachineUserID(raw)
	if err != nil {
		t.Fatalf("ParseMachineUserID(%q): %v", raw, err)
	}
	return machine
}

// mustParseService parses a raw service Matrix user ID for test use.
func mustParseService(t *testing.T, raw string) ref.Service {
	t.Helper()
	service, err := ref.ParseServiceUserID(raw)
	if err != nil {
		t.Fatalf("ParseServiceUserID(%q): %v", raw, err)
	}
	return service
}

// mintToken creates a signed test token with specific grants. The
// subject is an account localpart (e.g. "agent/tester"), expanded
// to a full user ID under bureau/fleet/prod on bureau.local.
// Timestamps are relative to testClockEpoch.
func mintToken(t *testing.T, privateKey ed25519.PrivateKey, subject string, grants []servicetoken.Grant) []byte {
	t.Helper()
	token := &servicetoken.Token{
		Subject:   mustParseUserID(t, "@bureau/fleet/prod/"+subject+":bureau.local"),
		Machine:   mustParseMachine(t, "@bureau/fleet/prod/machine/test:bureau.local"),
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
func ticketFixture(title, status string) ticket.TicketContent {
	return ticket.TicketContent{
		Version:   ticket.TicketContentVersion,
		Title:     title,
		Status:    status,
		Priority:  2,
		Type:      "task",
		CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
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
func sampleRooms() map[ref.RoomID]*roomState {
	roomA := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {
			Version:   1,
			Title:     "implement login",
			Body:      "add OAuth support",
			Status:    "open",
			Priority:  1,
			Type:      "feature",
			Labels:    []string{"auth", "frontend"},
			Assignee:  ref.MustParseUserID("@agent/coder:bureau.local"),
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

	roomB := newTrackedRoom(map[string]ticket.TicketContent{
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

	return map[ref.RoomID]*roomState{
		testRoomID("!roomA:local"): roomA,
		testRoomID("!roomB:local"): roomB,
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
		{"list-rooms", map[string]any{}},
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
		{"add-note", map[string]any{"ticket": "tkt-1"}},
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

// Verify codec does not need explicit cbor tags on the types. This
// ensures the schema types (json-tagged) round-trip correctly through
// our CBOR wire protocol.
func TestSchemaContentRoundTripViaCBOR(t *testing.T) {
	original := ticket.TicketContent{
		Version:   1,
		Title:     "round-trip test",
		Body:      "body text",
		Status:    "open",
		Priority:  2,
		Type:      "task",
		Labels:    []string{"a", "b"},
		Assignee:  ref.MustParseUserID("@agent/test:bureau.local"),
		Parent:    "parent-1",
		BlockedBy: []string{"dep-1"},
		CreatedBy: ref.MustParseUserID("@creator:bureau.local"),
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T00:00:00Z",
		Notes: []ticket.TicketNote{
			{ID: "n-1", Author: ref.MustParseUserID("@author:bureau.local"), Body: "note body", CreatedAt: "2026-01-01T00:00:00Z"},
		},
	}

	data, err := codec.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded ticket.TicketContent
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
	EventType ref.EventType
	StateKey  string
	Content   any
}

func (f *fakeWriter) SendStateEvent(_ context.Context, roomID ref.RoomID, eventType ref.EventType, stateKey string, content any) (ref.EventID, error) {
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
	return ref.MustParseEventID("$event-" + stateKey), nil
}

// testEnv holds the server, client, and writer for mutation tests.
type testEnv struct {
	client  *service.ServiceClient
	writer  *fakeWriter
	service *TicketService
	clock   *clock.FakeClock
	cleanup func()
}

func testMutationServer(t *testing.T, rooms map[ref.RoomID]*roomState) *testEnv {
	t.Helper()
	return newTestServer(t, rooms, testServerOpts{})
}

func testMutationServerWithTimers(t *testing.T, rooms map[ref.RoomID]*roomState) *testEnv {
	t.Helper()
	return newTestServer(t, rooms, testServerOpts{withTimers: true})
}

// mutationRooms returns a room with tickets in various states suitable
// for mutation testing.
func mutationRooms() map[ref.RoomID]*roomState {
	room := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-open": {
			Version:   1,
			Title:     "open ticket",
			Status:    "open",
			Priority:  2,
			Type:      "task",
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		"tkt-closed": {
			Version:     1,
			Title:       "closed ticket",
			Status:      "closed",
			Priority:    2,
			Type:        "bug",
			CreatedBy:   ref.MustParseUserID("@agent/creator:bureau.local"),
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
			Assignee:  ref.MustParseUserID("@agent/worker:bureau.local"),
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-02T00:00:00Z",
		},
		"tkt-gated": {
			Version:  1,
			Title:    "gated ticket",
			Status:   "open",
			Priority: 2,
			Type:     "task",
			Gates: []ticket.TicketGate{
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
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
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
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		"tkt-review": {
			Version:  ticket.TicketContentVersion,
			Title:    "ticket in review",
			Status:   "review",
			Priority: 2,
			Type:     "task",
			Assignee: ref.MustParseUserID("@agent/worker:bureau.local"),
			Review: &ticket.TicketReview{
				Reviewers: []ticket.ReviewerEntry{
					{
						UserID:      ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
						Disposition: "pending",
					},
					{
						UserID:      ref.MustParseUserID("@agent/reviewer:bureau.local"),
						Disposition: "pending",
					},
				},
			},
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-03T00:00:00Z",
		},
		"tkt-blocked": {
			Version:   1,
			Title:     "blocked ticket",
			Status:    "blocked",
			Priority:  2,
			Type:      "task",
			CreatedBy: ref.MustParseUserID("@agent/creator:bureau.local"),
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-02T00:00:00Z",
		},
	})

	return map[ref.RoomID]*roomState{
		testRoomID("!room:bureau.local"): room,
	}
}
