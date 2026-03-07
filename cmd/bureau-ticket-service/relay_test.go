// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/stewardshipindex"
	"github.com/bureau-foundation/bureau/messaging"
)

// --- Pure function tests ---

func TestIsRelayReady(t *testing.T) {
	tests := []struct {
		name    string
		content ticket.TicketContent
		want    bool
	}{
		{
			name: "ready: resource_request with reservation and no gates",
			content: ticket.TicketContent{
				Type:   ticket.TypeResourceRequest,
				Status: ticket.StatusOpen,
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"}, Mode: schema.ModeExclusive},
					},
					MaxDuration: "2h",
				},
			},
			want: true,
		},
		{
			name: "ready: all existing gates satisfied",
			content: ticket.TicketContent{
				Type:   ticket.TypeResourceRequest,
				Status: ticket.StatusOpen,
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"}, Mode: schema.ModeExclusive},
					},
					MaxDuration: "2h",
				},
				Gates: []ticket.TicketGate{
					{ID: "ci-pass", Type: ticket.GatePipeline, Status: ticket.GateSatisfied},
				},
			},
			want: true,
		},
		{
			name: "not ready: wrong ticket type",
			content: ticket.TicketContent{
				Type:   ticket.TypeTask,
				Status: ticket.StatusOpen,
			},
			want: false,
		},
		{
			name: "not ready: nil reservation",
			content: ticket.TicketContent{
				Type:   ticket.TypeResourceRequest,
				Status: ticket.StatusOpen,
			},
			want: false,
		},
		{
			name: "not ready: empty claims",
			content: ticket.TicketContent{
				Type:   ticket.TypeResourceRequest,
				Status: ticket.StatusOpen,
				Reservation: &ticket.ReservationContent{
					Claims:      []ticket.ResourceClaim{},
					MaxDuration: "2h",
				},
			},
			want: false,
		},
		{
			name: "not ready: not open",
			content: ticket.TicketContent{
				Type:   ticket.TypeResourceRequest,
				Status: ticket.StatusClosed,
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"}, Mode: schema.ModeExclusive},
					},
					MaxDuration: "2h",
				},
			},
			want: false,
		},
		{
			name: "not ready: unsatisfied gate",
			content: ticket.TicketContent{
				Type:   ticket.TypeResourceRequest,
				Status: ticket.StatusOpen,
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"}, Mode: schema.ModeExclusive},
					},
					MaxDuration: "2h",
				},
				Gates: []ticket.TicketGate{
					{ID: "ci-pass", Type: ticket.GatePipeline, Status: ticket.GatePending},
				},
			},
			want: false,
		},
		{
			name: "not ready: mix of satisfied and pending gates",
			content: ticket.TicketContent{
				Type:   ticket.TypeResourceRequest,
				Status: ticket.StatusOpen,
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"}, Mode: schema.ModeExclusive},
					},
					MaxDuration: "2h",
				},
				Gates: []ticket.TicketGate{
					{ID: "ci-pass", Type: ticket.GatePipeline, Status: ticket.GateSatisfied},
					{ID: "review", Type: ticket.GateHuman, Status: ticket.GatePending},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRelayReady(&tt.content)
			if got != tt.want {
				t.Errorf("isRelayReady() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsRelayed(t *testing.T) {
	tests := []struct {
		name    string
		content ticket.TicketContent
		want    bool
	}{
		{
			name: "relayed: has reservation state_event gate",
			content: ticket.TicketContent{
				Gates: []ticket.TicketGate{
					{ID: "rsv-0", Type: ticket.GateStateEvent, EventType: schema.EventTypeReservation},
				},
			},
			want: true,
		},
		{
			name: "relayed: reservation gate among other gates",
			content: ticket.TicketContent{
				Gates: []ticket.TicketGate{
					{ID: "ci-pass", Type: ticket.GatePipeline, Status: ticket.GateSatisfied},
					{ID: "rsv-0", Type: ticket.GateStateEvent, EventType: schema.EventTypeReservation},
				},
			},
			want: true,
		},
		{
			name:    "not relayed: no gates",
			content: ticket.TicketContent{},
			want:    false,
		},
		{
			name: "not relayed: non-reservation state_event gate",
			content: ticket.TicketContent{
				Gates: []ticket.TicketGate{
					{ID: "deploy-ready", Type: ticket.GateStateEvent, EventType: ref.EventType("m.bureau.deploy_result")},
				},
			},
			want: false,
		},
		{
			name: "not relayed: pipeline gate only",
			content: ticket.TicketContent{
				Gates: []ticket.TicketGate{
					{ID: "ci-pass", Type: ticket.GatePipeline},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRelayed(&tt.content)
			if got != tt.want {
				t.Errorf("isRelayed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFleetFromRoomAlias(t *testing.T) {
	tests := []struct {
		name      string
		alias     string
		wantFleet string
		wantErr   bool
	}{
		{
			name:      "workspace room",
			alias:     "#bureau/fleet/prod/workspace/my-project:bureau.local",
			wantFleet: "bureau/fleet/prod",
		},
		{
			name:      "machine room",
			alias:     "#bureau/fleet/prod/machine/gpu-box:bureau.local",
			wantFleet: "bureau/fleet/prod",
		},
		{
			name:      "service room",
			alias:     "#bureau/fleet/prod/service/ticket/main:bureau.local",
			wantFleet: "bureau/fleet/prod",
		},
		{
			name:      "different namespace and fleet",
			alias:     "#myorg/fleet/staging/workspace/test:bureau.local",
			wantFleet: "myorg/fleet/staging",
		},
		{
			name:      "fleet room itself",
			alias:     "#bureau/fleet/prod:bureau.local",
			wantFleet: "bureau/fleet/prod",
		},
		{
			name:    "no fleet segment",
			alias:   "#bureau/workspace/my-project:bureau.local",
			wantErr: true,
		},
		{
			name:    "empty alias",
			alias:   "",
			wantErr: true,
		},
		{
			name:    "invalid alias format",
			alias:   "not-an-alias",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fleet, err := fleetFromRoomAlias(tt.alias)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got fleet %q", fleet)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fleet.String() != tt.wantFleet {
				t.Errorf("fleet = %q, want %q", fleet, tt.wantFleet)
			}
		})
	}
}

// --- Relay lifecycle tests ---

// relayTestService creates a TicketService configured for relay
// lifecycle testing. It includes a writer for observing state event
// writes, an alias resolver, and pre-populated rooms. The session
// field provides GetStateEvent for relay policy fetching.
func relayTestService(t *testing.T) (*TicketService, *fakeWriterForGates, *fakeSessionForRelay) {
	t.Helper()

	writer := &fakeWriterForGates{}
	session := &fakeSessionForRelay{
		policies: make(map[ref.RoomID]json.RawMessage),
	}
	resolver := &fakeAliasResolver{
		aliases: map[string]string{},
	}

	testServiceRef := mustParseService(t, "@bureau/fleet/prod/service/ticket/test:bureau.local")

	ts := &TicketService{
		writer:           writer,
		session:          session,
		resolver:         resolver,
		clock:            clock.Fake(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
		service:          testServiceRef,
		startedAt:        time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:            make(map[ref.RoomID]*roomState),
		aliasCache:       make(map[ref.RoomAlias]ref.RoomID),
		crossRoomWatches: make(map[crossRoomWatchKey][]crossRoomWatch),
		relayEntries:     make(map[string]*relayEntry),
		subscribers:      make(map[ref.RoomID][]*subscriber),
		membersByRoom:    make(map[ref.RoomID]map[ref.UserID]roomMember),
		stewardshipIndex: stewardshipindex.NewIndex(),
		digestTimers:     make(map[digestKey]*digestEntry),
		digestNotify:     make(chan struct{}, 1),
		logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	return ts, writer, session
}

// fakeSessionForRelay implements messaging.Session with only the
// methods the relay code uses (GetStateEvent). All other methods
// panic to catch unintended usage.
type fakeSessionForRelay struct {
	messaging.Session // embed to satisfy the interface

	// policies maps room ID → JSON-encoded RelayPolicy. If absent,
	// GetStateEvent returns M_NOT_FOUND.
	policies map[ref.RoomID]json.RawMessage
}

func (f *fakeSessionForRelay) GetStateEvent(_ context.Context, roomID ref.RoomID, eventType ref.EventType, _ string) (json.RawMessage, error) {
	if eventType == schema.EventTypeRelayPolicy {
		raw, exists := f.policies[roomID]
		if !exists {
			return nil, &messaging.MatrixError{Code: messaging.ErrCodeNotFound, Message: "not found"}
		}
		return raw, nil
	}
	return nil, fmt.Errorf("fakeSessionForRelay: unexpected event type %s", eventType)
}

func (f *fakeSessionForRelay) UserID() ref.UserID {
	return ref.MustParseUserID("@bureau/fleet/prod/service/ticket/test:bureau.local")
}

func (f *fakeSessionForRelay) ResolveAlias(_ context.Context, alias ref.RoomAlias) (ref.RoomID, error) {
	return ref.RoomID{}, fmt.Errorf("fakeSessionForRelay: unexpected ResolveAlias(%s)", alias)
}

func (f *fakeSessionForRelay) SendStateEvent(_ context.Context, roomID ref.RoomID, eventType ref.EventType, stateKey string, content any) (ref.EventID, error) {
	return ref.MustParseEventID("$event-" + stateKey), nil
}

// setPolicy configures a relay policy for a room.
func (f *fakeSessionForRelay) setPolicy(roomID ref.RoomID, policy schema.RelayPolicy) {
	raw, err := json.Marshal(policy)
	if err != nil {
		panic(fmt.Sprintf("setPolicy: marshal: %v", err))
	}
	f.policies[roomID] = raw
}

// --- Workspace closure cascade tests ---

func TestCloseRelayTicketsForOrigin(t *testing.T) {
	ts, writer, _ := relayTestService(t)

	opsRoomA := testRoomID("!ops-a:bureau.local")
	opsRoomB := testRoomID("!ops-b:bureau.local")

	// Set up tracked ops rooms with relay tickets.
	ts.rooms[opsRoomA] = newTrackedRoom(map[string]ticket.TicketContent{
		"relay-a": {
			Version:   1,
			Title:     "Resource request: machine/gpu-1",
			Status:    ticket.StatusOpen,
			Priority:  2,
			Type:      ticket.TypeResourceRequest,
			CreatedBy: ts.service.UserID(),
			CreatedAt: "2026-01-15T12:00:00Z",
			UpdatedAt: "2026-01-15T12:00:00Z",
			Reservation: &ticket.ReservationContent{
				Claims:      []ticket.ResourceClaim{{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-1"}, Mode: schema.ModeExclusive}},
				MaxDuration: "2h",
			},
		},
	})
	ts.rooms[opsRoomB] = newTrackedRoom(map[string]ticket.TicketContent{
		"relay-b": {
			Version:   1,
			Title:     "Resource request: machine/gpu-2",
			Status:    ticket.StatusOpen,
			Priority:  2,
			Type:      ticket.TypeResourceRequest,
			CreatedBy: ts.service.UserID(),
			CreatedAt: "2026-01-15T12:00:00Z",
			UpdatedAt: "2026-01-15T12:00:00Z",
			Reservation: &ticket.ReservationContent{
				Claims:      []ticket.ResourceClaim{{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-2"}, Mode: schema.ModeExclusive}},
				MaxDuration: "2h",
			},
		},
	})

	ts.relayEntries["ws-tkt-1"] = &relayEntry{
		originRoom:   testRoomID("!workspace:bureau.local"),
		originTicket: "ws-tkt-1",
		relayTickets: map[ref.RoomID]relayTicketRef{
			opsRoomA: {ticketID: "relay-a", claimIndex: 0},
			opsRoomB: {ticketID: "relay-b", claimIndex: 1},
		},
	}

	ts.closeRelayTicketsForOrigin(context.Background(), "ws-tkt-1")

	// Both relay tickets should be closed with "origin closed".
	if len(writer.events) != 2 {
		t.Fatalf("expected 2 writes, got %d", len(writer.events))
	}
	for _, event := range writer.events {
		content, ok := event.Content.(ticket.TicketContent)
		if !ok {
			t.Fatalf("expected TicketContent, got %T", event.Content)
		}
		if content.Status != ticket.StatusClosed {
			t.Errorf("relay ticket status = %q, want %q", content.Status, ticket.StatusClosed)
		}
		if content.CloseReason != "origin closed" {
			t.Errorf("close reason = %q, want %q", content.CloseReason, "origin closed")
		}
	}

	// Relay entry should be removed.
	if _, exists := ts.relayEntries["ws-tkt-1"]; exists {
		t.Error("relay entry should be removed after origin closure")
	}
}

func TestCloseRelayTicketsSkipsAlreadyClosed(t *testing.T) {
	ts, writer, _ := relayTestService(t)

	opsRoom := testRoomID("!ops:bureau.local")
	ts.rooms[opsRoom] = newTrackedRoom(map[string]ticket.TicketContent{
		"relay-1": {
			Version:     1,
			Title:       "Resource request: machine/gpu-1",
			Status:      ticket.StatusClosed,
			Priority:    2,
			Type:        ticket.TypeResourceRequest,
			CreatedBy:   ts.service.UserID(),
			CreatedAt:   "2026-01-15T12:00:00Z",
			UpdatedAt:   "2026-01-15T12:00:00Z",
			ClosedAt:    "2026-01-15T12:05:00Z",
			CloseReason: "previously closed",
			Reservation: &ticket.ReservationContent{
				Claims:      []ticket.ResourceClaim{{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-1"}, Mode: schema.ModeExclusive}},
				MaxDuration: "2h",
			},
		},
	})

	ts.relayEntries["ws-tkt-1"] = &relayEntry{
		originRoom:   testRoomID("!workspace:bureau.local"),
		originTicket: "ws-tkt-1",
		relayTickets: map[ref.RoomID]relayTicketRef{
			opsRoom: {ticketID: "relay-1", claimIndex: 0},
		},
	}

	ts.closeRelayTicketsForOrigin(context.Background(), "ws-tkt-1")

	// No writes should happen — ticket was already closed.
	if len(writer.events) != 0 {
		t.Fatalf("expected 0 writes for already-closed relay ticket, got %d", len(writer.events))
	}
}

func TestCloseRelayTicketsNoOpWhenNoEntry(t *testing.T) {
	ts, writer, _ := relayTestService(t)

	ts.closeRelayTicketsForOrigin(context.Background(), "nonexistent-ticket")

	if len(writer.events) != 0 {
		t.Fatalf("expected 0 writes for nonexistent entry, got %d", len(writer.events))
	}
}

// --- Denial cascade tests ---

func TestHandleRelayTicketClosedCascadesDenial(t *testing.T) {
	ts, writer, _ := relayTestService(t)

	originRoom := testRoomID("!workspace:bureau.local")
	opsRoomA := testRoomID("!ops-a:bureau.local")
	opsRoomB := testRoomID("!ops-b:bureau.local")

	// Set up origin room with the source ticket.
	ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
		"ws-tkt-1": {
			Version:   1,
			Title:     "Need GPU cluster",
			Status:    ticket.StatusOpen,
			Priority:  1,
			Type:      ticket.TypeResourceRequest,
			CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
			CreatedAt: "2026-01-15T12:00:00Z",
			UpdatedAt: "2026-01-15T12:00:00Z",
			Reservation: &ticket.ReservationContent{
				Claims: []ticket.ResourceClaim{
					{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-1"}, Mode: schema.ModeExclusive},
					{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-2"}, Mode: schema.ModeExclusive},
				},
				MaxDuration: "2h",
			},
		},
	})

	// Set up ops rooms with relay tickets.
	ts.rooms[opsRoomA] = newTrackedRoom(map[string]ticket.TicketContent{
		"relay-a": {
			Version:     1,
			Title:       "Resource request: machine/gpu-1",
			Status:      ticket.StatusClosed,
			Priority:    1,
			Type:        ticket.TypeResourceRequest,
			CreatedBy:   ts.service.UserID(),
			CreatedAt:   "2026-01-15T12:00:00Z",
			UpdatedAt:   "2026-01-15T12:01:00Z",
			ClosedAt:    "2026-01-15T12:01:00Z",
			CloseReason: "resource unavailable",
			Reservation: &ticket.ReservationContent{
				Claims:      []ticket.ResourceClaim{{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-1"}, Mode: schema.ModeExclusive}},
				MaxDuration: "2h",
			},
		},
	})
	ts.rooms[opsRoomB] = newTrackedRoom(map[string]ticket.TicketContent{
		"relay-b": {
			Version:   1,
			Title:     "Resource request: machine/gpu-2",
			Status:    ticket.StatusOpen,
			Priority:  1,
			Type:      ticket.TypeResourceRequest,
			CreatedBy: ts.service.UserID(),
			CreatedAt: "2026-01-15T12:00:00Z",
			UpdatedAt: "2026-01-15T12:00:00Z",
			Reservation: &ticket.ReservationContent{
				Claims:      []ticket.ResourceClaim{{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-2"}, Mode: schema.ModeExclusive}},
				MaxDuration: "2h",
			},
		},
	})

	// Register the relay entry.
	ts.relayEntries["ws-tkt-1"] = &relayEntry{
		originRoom:   originRoom,
		originTicket: "ws-tkt-1",
		relayTickets: map[ref.RoomID]relayTicketRef{
			opsRoomA: {ticketID: "relay-a", claimIndex: 0},
			opsRoomB: {ticketID: "relay-b", claimIndex: 1},
		},
	}

	// Simulate relay-a being closed by the ops side (denial).
	relayContent, _ := ts.rooms[opsRoomA].index.Get("relay-a")
	ts.handleRelayTicketClosed(context.Background(), opsRoomA, "relay-a", relayContent)

	// Expect: relay-b closed (denial cascade) + origin ticket closed.
	if len(writer.events) < 2 {
		t.Fatalf("expected at least 2 writes (cascade relay-b + close origin), got %d", len(writer.events))
	}

	// Check that relay-b was closed with cascade reason.
	relayBContent, exists := ts.rooms[opsRoomB].index.Get("relay-b")
	if !exists {
		t.Fatal("relay-b should still exist in index")
	}
	if relayBContent.Status != ticket.StatusClosed {
		t.Errorf("relay-b status = %q, want closed", relayBContent.Status)
	}
	if relayBContent.CloseReason != "denial cascade: resource unavailable" {
		t.Errorf("relay-b close reason = %q, want %q", relayBContent.CloseReason, "denial cascade: resource unavailable")
	}

	// Check that the origin ticket was closed.
	originContent, exists := ts.rooms[originRoom].index.Get("ws-tkt-1")
	if !exists {
		t.Fatal("origin ticket should still exist in index")
	}
	if originContent.Status != ticket.StatusClosed {
		t.Errorf("origin ticket status = %q, want closed", originContent.Status)
	}
	if originContent.CloseReason != "reservation denied: resource unavailable" {
		t.Errorf("origin ticket close reason = %q, want %q", originContent.CloseReason, "reservation denied: resource unavailable")
	}

	// Relay entry should be cleaned up.
	if _, exists := ts.relayEntries["ws-tkt-1"]; exists {
		t.Error("relay entry should be removed after denial cascade")
	}
}

func TestHandleRelayTicketClosedIgnoresOriginClosed(t *testing.T) {
	ts, writer, _ := relayTestService(t)

	opsRoom := testRoomID("!ops:bureau.local")
	originRoom := testRoomID("!workspace:bureau.local")

	ts.rooms[opsRoom] = newTrackedRoom(map[string]ticket.TicketContent{
		"relay-1": {
			Version:     1,
			Title:       "Resource request: machine/gpu-1",
			Status:      ticket.StatusClosed,
			Priority:    2,
			Type:        ticket.TypeResourceRequest,
			CreatedBy:   ts.service.UserID(),
			CreatedAt:   "2026-01-15T12:00:00Z",
			UpdatedAt:   "2026-01-15T12:05:00Z",
			ClosedAt:    "2026-01-15T12:05:00Z",
			CloseReason: "origin closed",
			Reservation: &ticket.ReservationContent{
				Claims:      []ticket.ResourceClaim{{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-1"}, Mode: schema.ModeExclusive}},
				MaxDuration: "2h",
			},
		},
	})

	ts.relayEntries["ws-tkt-1"] = &relayEntry{
		originRoom:   originRoom,
		originTicket: "ws-tkt-1",
		relayTickets: map[ref.RoomID]relayTicketRef{
			opsRoom: {ticketID: "relay-1", claimIndex: 0},
		},
	}

	relayContent, _ := ts.rooms[opsRoom].index.Get("relay-1")
	ts.handleRelayTicketClosed(context.Background(), opsRoom, "relay-1", relayContent)

	// No cascade should happen — the closure was initiated from origin side.
	if len(writer.events) != 0 {
		t.Fatalf("expected 0 writes for origin-closed relay ticket, got %d", len(writer.events))
	}

	// Relay entry should still exist (origin closure path handles cleanup).
	if _, exists := ts.relayEntries["ws-tkt-1"]; !exists {
		t.Error("relay entry should still exist after origin-closed (cleanup happens in origin closure path)")
	}
}

func TestHandleRelayTicketClosedIgnoresUntracked(t *testing.T) {
	ts, writer, _ := relayTestService(t)

	opsRoom := testRoomID("!ops:bureau.local")
	ts.rooms[opsRoom] = newTrackedRoom(map[string]ticket.TicketContent{
		"some-ticket": {
			Version:   1,
			Title:     "Some ticket",
			Status:    ticket.StatusClosed,
			Priority:  2,
			Type:      ticket.TypeTask,
			CreatedAt: "2026-01-15T12:00:00Z",
			UpdatedAt: "2026-01-15T12:00:00Z",
		},
	})

	// No relay entries registered for this ticket.
	content, _ := ts.rooms[opsRoom].index.Get("some-ticket")
	ts.handleRelayTicketClosed(context.Background(), opsRoom, "some-ticket", content)

	if len(writer.events) != 0 {
		t.Fatalf("expected 0 writes for untracked ticket, got %d", len(writer.events))
	}
}

// --- Relay entry rebuild tests ---

func TestRebuildRelayEntries(t *testing.T) {
	ts, _, _ := relayTestService(t)

	originRoom := testRoomID("!workspace:bureau.local")
	opsRoom := testRoomID("!ops:bureau.local")

	opsAlias := ref.MustParseRoomAlias("#bureau/fleet/prod/machine/gpu-box/ops:bureau.local")

	// Pre-populate alias cache (rebuild calls resolveAliasWithCache).
	ts.aliasCache[opsAlias] = opsRoom

	// Workspace room with a relayed resource_request ticket.
	ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
		"ws-tkt-1": {
			Version:   1,
			Title:     "Need GPU",
			Status:    ticket.StatusOpen,
			Priority:  2,
			Type:      ticket.TypeResourceRequest,
			CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
			CreatedAt: "2026-01-15T12:00:00Z",
			UpdatedAt: "2026-01-15T12:00:00Z",
			Reservation: &ticket.ReservationContent{
				Claims: []ticket.ResourceClaim{
					{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"}, Mode: schema.ModeExclusive},
				},
				MaxDuration: "2h",
			},
			Gates: []ticket.TicketGate{
				{
					ID:        "rsv-0",
					Type:      ticket.GateStateEvent,
					Status:    ticket.GatePending,
					EventType: schema.EventTypeReservation,
					RoomAlias: opsAlias,
				},
			},
		},
		// Non-reservation ticket should be ignored.
		"ws-tkt-2": {
			Version:   1,
			Title:     "Regular task",
			Status:    ticket.StatusOpen,
			Priority:  2,
			Type:      ticket.TypeTask,
			CreatedAt: "2026-01-15T12:00:00Z",
			UpdatedAt: "2026-01-15T12:00:00Z",
		},
	})

	// Ops room with the relay ticket created by the service.
	ts.rooms[opsRoom] = newTrackedRoom(map[string]ticket.TicketContent{
		"relay-1": {
			Version:   1,
			Title:     "Resource request: machine/gpu-box",
			Status:    ticket.StatusOpen,
			Priority:  2,
			Type:      ticket.TypeResourceRequest,
			CreatedBy: ts.service.UserID(),
			CreatedAt: "2026-01-15T12:00:00Z",
			UpdatedAt: "2026-01-15T12:00:00Z",
			Reservation: &ticket.ReservationContent{
				Claims:      []ticket.ResourceClaim{{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"}, Mode: schema.ModeExclusive}},
				MaxDuration: "2h",
			},
		},
	})

	ts.rebuildRelayEntries()

	if len(ts.relayEntries) != 1 {
		t.Fatalf("expected 1 relay entry, got %d", len(ts.relayEntries))
	}

	entry, exists := ts.relayEntries["ws-tkt-1"]
	if !exists {
		t.Fatal("expected relay entry for ws-tkt-1")
	}
	if entry.originRoom != originRoom {
		t.Errorf("origin room = %q, want %q", entry.originRoom, originRoom)
	}
	if entry.originTicket != "ws-tkt-1" {
		t.Errorf("origin ticket = %q, want %q", entry.originTicket, "ws-tkt-1")
	}
	relayRef, exists := entry.relayTickets[opsRoom]
	if !exists {
		t.Fatal("expected relay ticket in ops room")
	}
	if relayRef.ticketID != "relay-1" {
		t.Errorf("relay ticket ID = %q, want %q", relayRef.ticketID, "relay-1")
	}
	if relayRef.claimIndex != 0 {
		t.Errorf("relay claim index = %d, want 0", relayRef.claimIndex)
	}
}

func TestRebuildRelayEntriesSkipsClosedRelay(t *testing.T) {
	ts, _, _ := relayTestService(t)

	originRoom := testRoomID("!workspace:bureau.local")
	opsRoom := testRoomID("!ops:bureau.local")

	opsAlias := ref.MustParseRoomAlias("#bureau/fleet/prod/machine/gpu-box/ops:bureau.local")
	ts.aliasCache[opsAlias] = opsRoom

	ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
		"ws-tkt-1": {
			Version:   1,
			Title:     "Need GPU",
			Status:    ticket.StatusOpen,
			Priority:  2,
			Type:      ticket.TypeResourceRequest,
			CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
			CreatedAt: "2026-01-15T12:00:00Z",
			UpdatedAt: "2026-01-15T12:00:00Z",
			Reservation: &ticket.ReservationContent{
				Claims: []ticket.ResourceClaim{
					{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"}, Mode: schema.ModeExclusive},
				},
				MaxDuration: "2h",
			},
			Gates: []ticket.TicketGate{
				{
					ID:        "rsv-0",
					Type:      ticket.GateStateEvent,
					Status:    ticket.GatePending,
					EventType: schema.EventTypeReservation,
					RoomAlias: opsAlias,
				},
			},
		},
	})

	// Relay ticket already closed — should not be tracked.
	ts.rooms[opsRoom] = newTrackedRoom(map[string]ticket.TicketContent{
		"relay-1": {
			Version:     1,
			Title:       "Resource request: machine/gpu-box",
			Status:      ticket.StatusClosed,
			Priority:    2,
			Type:        ticket.TypeResourceRequest,
			CreatedBy:   ts.service.UserID(),
			CreatedAt:   "2026-01-15T12:00:00Z",
			UpdatedAt:   "2026-01-15T12:01:00Z",
			ClosedAt:    "2026-01-15T12:01:00Z",
			CloseReason: "denied",
			Reservation: &ticket.ReservationContent{
				Claims:      []ticket.ResourceClaim{{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"}, Mode: schema.ModeExclusive}},
				MaxDuration: "2h",
			},
		},
	})

	ts.rebuildRelayEntries()

	// Closed relay ticket should not produce a relay entry.
	if len(ts.relayEntries) != 0 {
		t.Fatalf("expected 0 relay entries (relay ticket is closed), got %d", len(ts.relayEntries))
	}
}

func TestRebuildRelayEntriesSkipsNonRelayed(t *testing.T) {
	ts, _, _ := relayTestService(t)

	originRoom := testRoomID("!workspace:bureau.local")

	// Resource request without reservation gates (not yet relayed).
	ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
		"ws-tkt-1": {
			Version:   1,
			Title:     "Need GPU",
			Status:    ticket.StatusOpen,
			Priority:  2,
			Type:      ticket.TypeResourceRequest,
			CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
			CreatedAt: "2026-01-15T12:00:00Z",
			UpdatedAt: "2026-01-15T12:00:00Z",
			Reservation: &ticket.ReservationContent{
				Claims: []ticket.ResourceClaim{
					{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"}, Mode: schema.ModeExclusive},
				},
				MaxDuration: "2h",
			},
			// No reservation gates — not yet relayed.
		},
	})

	ts.rebuildRelayEntries()

	if len(ts.relayEntries) != 0 {
		t.Fatalf("expected 0 relay entries (ticket not relayed), got %d", len(ts.relayEntries))
	}
}

// --- Relay initiation tests ---

func TestCheckAndInitiateRelayCreatesRelayTickets(t *testing.T) {
	ts, writer, session := relayTestService(t)

	originRoom := testRoomID("!workspace:bureau.local")
	opsRoom := testRoomID("!ops-gpu:bureau.local")

	opsAlias := ref.MustParseRoomAlias("#bureau/fleet/prod/machine/gpu-box/ops:bureau.local")

	// Set up alias resolution.
	ts.resolver = &fakeAliasResolver{
		aliases: map[string]string{
			opsAlias.String(): opsRoom.String(),
		},
	}

	// Workspace room needs a fleet-parseable alias.
	originState := newTrackedRoom(nil)
	originState.alias = "#bureau/fleet/prod/workspace/my-project:bureau.local"
	ts.rooms[originRoom] = originState

	// Ops room with ticket config (must be tracked).
	ts.rooms[opsRoom] = newTrackedRoom(nil)

	// Set up relay policy allowing fleet members.
	session.setPolicy(opsRoom, schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{Match: schema.RelayMatchFleetMember, Fleet: "bureau/fleet/prod"},
		},
	})

	content := ticket.TicketContent{
		Version:   ticket.TicketContentVersion,
		Title:     "Need GPU",
		Status:    ticket.StatusOpen,
		Priority:  2,
		Type:      ticket.TypeResourceRequest,
		CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
		CreatedAt: "2026-01-15T12:00:00Z",
		UpdatedAt: "2026-01-15T12:00:00Z",
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
					Mode:     schema.ModeExclusive,
					Status:   schema.ClaimPending,
				},
			},
			MaxDuration: "2h",
		},
	}

	ts.checkAndInitiateRelay(context.Background(), originRoom, originState, "ws-tkt-1", content)

	// Expect: relay ticket created (1 write) + relay link published
	// (1 write) + origin ticket updated with gate (1 write) = 3
	// writes.
	if len(writer.events) < 3 {
		t.Fatalf("expected at least 3 writes, got %d: %+v", len(writer.events), writer.events)
	}

	// Verify the relay ticket was written to the ops room.
	var relayTicketWrite *writtenEventForGates
	for index := range writer.events {
		event := &writer.events[index]
		if event.RoomID == opsRoom.String() && event.EventType == schema.EventTypeTicket {
			relayTicketWrite = event
			break
		}
	}
	if relayTicketWrite == nil {
		t.Fatal("no relay ticket write found in ops room")
	}
	relayContent, ok := relayTicketWrite.Content.(ticket.TicketContent)
	if !ok {
		t.Fatalf("relay ticket content is %T, want TicketContent", relayTicketWrite.Content)
	}
	if relayContent.Type != ticket.TypeResourceRequest {
		t.Errorf("relay ticket type = %q, want %q", relayContent.Type, ticket.TypeResourceRequest)
	}
	if relayContent.CreatedBy != ts.service.UserID() {
		t.Errorf("relay ticket creator = %q, want %q", relayContent.CreatedBy, ts.service.UserID())
	}

	// Verify the relay link was published.
	var relayLinkWrite *writtenEventForGates
	for index := range writer.events {
		event := &writer.events[index]
		if event.EventType == schema.EventTypeRelayLink {
			relayLinkWrite = event
			break
		}
	}
	if relayLinkWrite == nil {
		t.Fatal("no relay link write found")
	}

	// Verify the origin ticket was updated with a reservation gate.
	var originWrite *writtenEventForGates
	for index := range writer.events {
		event := &writer.events[index]
		if event.RoomID == originRoom.String() && event.EventType == schema.EventTypeTicket {
			originWrite = event
			break
		}
	}
	if originWrite == nil {
		t.Fatal("no origin ticket update found")
	}
	updatedContent, ok := originWrite.Content.(ticket.TicketContent)
	if !ok {
		t.Fatalf("origin ticket content is %T, want TicketContent", originWrite.Content)
	}

	// Find the reservation gate.
	var reservationGate *ticket.TicketGate
	for index := range updatedContent.Gates {
		gate := &updatedContent.Gates[index]
		if gate.Type == ticket.GateStateEvent && gate.EventType == schema.EventTypeReservation {
			reservationGate = gate
			break
		}
	}
	if reservationGate == nil {
		t.Fatal("origin ticket should have a reservation state_event gate")
	}
	if reservationGate.RoomAlias != opsAlias {
		t.Errorf("gate room alias = %q, want %q", reservationGate.RoomAlias, opsAlias)
	}

	// Verify relay entry was tracked.
	if len(ts.relayEntries) != 1 {
		t.Fatalf("expected 1 relay entry, got %d", len(ts.relayEntries))
	}
	entry, exists := ts.relayEntries["ws-tkt-1"]
	if !exists {
		t.Fatal("expected relay entry for ws-tkt-1")
	}
	if _, exists := entry.relayTickets[opsRoom]; !exists {
		t.Error("expected relay ticket in ops room")
	}
}

func TestCheckAndInitiateRelayDeniedByPolicy(t *testing.T) {
	ts, writer, session := relayTestService(t)

	originRoom := testRoomID("!workspace:bureau.local")
	opsRoom := testRoomID("!ops-gpu:bureau.local")

	opsAlias := ref.MustParseRoomAlias("#bureau/fleet/prod/machine/gpu-box/ops:bureau.local")

	ts.resolver = &fakeAliasResolver{
		aliases: map[string]string{
			opsAlias.String(): opsRoom.String(),
		},
	}

	originState := newTrackedRoom(nil)
	originState.alias = "#bureau/fleet/prod/workspace/my-project:bureau.local"
	ts.rooms[originRoom] = originState

	ts.rooms[opsRoom] = newTrackedRoom(nil)

	// Policy denies: requires specific room, not fleet_member.
	session.setPolicy(opsRoom, schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{Match: schema.RelayMatchRoom, Room: testRoomID("!other-room:bureau.local")},
		},
	})

	content := ticket.TicketContent{
		Version:   ticket.TicketContentVersion,
		Title:     "Need GPU",
		Status:    ticket.StatusOpen,
		Priority:  2,
		Type:      ticket.TypeResourceRequest,
		CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
		CreatedAt: "2026-01-15T12:00:00Z",
		UpdatedAt: "2026-01-15T12:00:00Z",
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"}, Mode: schema.ModeExclusive, Status: schema.ClaimPending},
			},
			MaxDuration: "2h",
		},
	}

	ts.checkAndInitiateRelay(context.Background(), originRoom, originState, "ws-tkt-1", content)

	// Should write a failure note to the origin ticket.
	foundFailureNote := false
	for _, event := range writer.events {
		if event.RoomID == originRoom.String() && event.EventType == schema.EventTypeTicket {
			if content, ok := event.Content.(ticket.TicketContent); ok {
				for _, note := range content.Notes {
					if note.Body != "" {
						foundFailureNote = true
					}
				}
			}
		}
	}
	if !foundFailureNote {
		t.Error("expected a failure note on the origin ticket after policy denial")
	}

	// No relay entries should be created.
	if len(ts.relayEntries) != 0 {
		t.Errorf("expected 0 relay entries after denial, got %d", len(ts.relayEntries))
	}
}

func TestCheckAndInitiateRelaySkipsNonResourceRequest(t *testing.T) {
	ts, writer, _ := relayTestService(t)

	content := ticket.TicketContent{
		Version:   1,
		Title:     "Regular task",
		Status:    ticket.StatusOpen,
		Priority:  2,
		Type:      ticket.TypeTask,
		CreatedBy: ref.MustParseUserID("@test:bureau.local"),
		CreatedAt: "2026-01-15T12:00:00Z",
		UpdatedAt: "2026-01-15T12:00:00Z",
	}

	state := newTrackedRoom(nil)
	ts.checkAndInitiateRelay(context.Background(), testRoomID("!room:bureau.local"), state, "tkt-1", content)

	if len(writer.events) != 0 {
		t.Fatalf("expected no writes for non-resource_request ticket, got %d", len(writer.events))
	}
}

func TestCheckAndInitiateRelaySkipsAlreadyRelayed(t *testing.T) {
	ts, writer, _ := relayTestService(t)

	opsAlias := ref.MustParseRoomAlias("#bureau/fleet/prod/machine/gpu-box/ops:bureau.local")

	content := ticket.TicketContent{
		Version:   1,
		Title:     "Already relayed",
		Status:    ticket.StatusOpen,
		Priority:  2,
		Type:      ticket.TypeResourceRequest,
		CreatedBy: ref.MustParseUserID("@test:bureau.local"),
		CreatedAt: "2026-01-15T12:00:00Z",
		UpdatedAt: "2026-01-15T12:00:00Z",
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"}, Mode: schema.ModeExclusive},
			},
			MaxDuration: "2h",
		},
		Gates: []ticket.TicketGate{
			// Reservation gate exists — already relayed.
			{ID: "rsv-0", Type: ticket.GateStateEvent, EventType: schema.EventTypeReservation, RoomAlias: opsAlias},
		},
	}

	state := newTrackedRoom(nil)
	ts.checkAndInitiateRelay(context.Background(), testRoomID("!room:bureau.local"), state, "tkt-1", content)

	if len(writer.events) != 0 {
		t.Fatalf("expected no writes for already-relayed ticket, got %d", len(writer.events))
	}
}

func TestCheckAndInitiateRelayMultiClaim(t *testing.T) {
	ts, writer, session := relayTestService(t)

	originRoom := testRoomID("!workspace:bureau.local")
	opsRoomA := testRoomID("!ops-gpu1:bureau.local")
	opsRoomB := testRoomID("!ops-gpu2:bureau.local")

	opsAliasA := ref.MustParseRoomAlias("#bureau/fleet/prod/machine/gpu-1/ops:bureau.local")
	opsAliasB := ref.MustParseRoomAlias("#bureau/fleet/prod/machine/gpu-2/ops:bureau.local")

	ts.resolver = &fakeAliasResolver{
		aliases: map[string]string{
			opsAliasA.String(): opsRoomA.String(),
			opsAliasB.String(): opsRoomB.String(),
		},
	}

	originState := newTrackedRoom(nil)
	originState.alias = "#bureau/fleet/prod/workspace/multi-gpu:bureau.local"
	ts.rooms[originRoom] = originState
	ts.rooms[opsRoomA] = newTrackedRoom(nil)
	ts.rooms[opsRoomB] = newTrackedRoom(nil)

	// Both ops rooms allow fleet members.
	fleetPolicy := schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{Match: schema.RelayMatchFleetMember, Fleet: "bureau/fleet/prod"},
		},
	}
	session.setPolicy(opsRoomA, fleetPolicy)
	session.setPolicy(opsRoomB, fleetPolicy)

	content := ticket.TicketContent{
		Version:   ticket.TicketContentVersion,
		Title:     "Need two GPUs",
		Status:    ticket.StatusOpen,
		Priority:  1,
		Type:      ticket.TypeResourceRequest,
		CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
		CreatedAt: "2026-01-15T12:00:00Z",
		UpdatedAt: "2026-01-15T12:00:00Z",
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-1"}, Mode: schema.ModeExclusive, Status: schema.ClaimPending},
				{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-2"}, Mode: schema.ModeExclusive, Status: schema.ClaimPending},
			},
			MaxDuration: "2h",
		},
	}

	ts.checkAndInitiateRelay(context.Background(), originRoom, originState, "ws-multi", content)

	// Expect writes for 2 relay tickets + 2 relay links + 1 origin ticket update = 5.
	if len(writer.events) < 5 {
		t.Fatalf("expected at least 5 writes for multi-claim relay, got %d", len(writer.events))
	}

	// Verify relay entry tracks both ops rooms.
	entry, exists := ts.relayEntries["ws-multi"]
	if !exists {
		t.Fatal("expected relay entry for ws-multi")
	}
	if len(entry.relayTickets) != 2 {
		t.Errorf("expected 2 relay tickets, got %d", len(entry.relayTickets))
	}
	if _, exists := entry.relayTickets[opsRoomA]; !exists {
		t.Error("expected relay ticket in ops-gpu1")
	}
	if _, exists := entry.relayTickets[opsRoomB]; !exists {
		t.Error("expected relay ticket in ops-gpu2")
	}

	// Workspace ticket should have 2 reservation gates.
	var originUpdate *writtenEventForGates
	for index := range writer.events {
		event := &writer.events[index]
		if event.RoomID == originRoom.String() && event.EventType == schema.EventTypeTicket {
			originUpdate = event
		}
	}
	if originUpdate == nil {
		t.Fatal("no origin ticket update found")
	}
	updatedContent, ok := originUpdate.Content.(ticket.TicketContent)
	if !ok {
		t.Fatalf("origin ticket content is %T, want TicketContent", originUpdate.Content)
	}

	reservationGateCount := 0
	for index := range updatedContent.Gates {
		if updatedContent.Gates[index].Type == ticket.GateStateEvent && updatedContent.Gates[index].EventType == schema.EventTypeReservation {
			reservationGateCount++
		}
	}
	if reservationGateCount != 2 {
		t.Errorf("expected 2 reservation gates, got %d", reservationGateCount)
	}
}

func TestCheckAndInitiateRelayNoPolicy(t *testing.T) {
	ts, writer, _ := relayTestService(t)

	originRoom := testRoomID("!workspace:bureau.local")
	opsRoom := testRoomID("!ops-gpu:bureau.local")

	opsAlias := ref.MustParseRoomAlias("#bureau/fleet/prod/machine/gpu-box/ops:bureau.local")

	ts.resolver = &fakeAliasResolver{
		aliases: map[string]string{
			opsAlias.String(): opsRoom.String(),
		},
	}

	originState := newTrackedRoom(nil)
	originState.alias = "#bureau/fleet/prod/workspace/my-project:bureau.local"
	ts.rooms[originRoom] = originState
	ts.rooms[opsRoom] = newTrackedRoom(nil)

	// No policy configured — session returns M_NOT_FOUND.
	// fetchRelayPolicy returns nil, which Evaluate treats as deny.

	content := ticket.TicketContent{
		Version:   ticket.TicketContentVersion,
		Title:     "Need GPU",
		Status:    ticket.StatusOpen,
		Priority:  2,
		Type:      ticket.TypeResourceRequest,
		CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
		CreatedAt: "2026-01-15T12:00:00Z",
		UpdatedAt: "2026-01-15T12:00:00Z",
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"}, Mode: schema.ModeExclusive, Status: schema.ClaimPending},
			},
			MaxDuration: "2h",
		},
	}

	ts.checkAndInitiateRelay(context.Background(), originRoom, originState, "ws-tkt-1", content)

	// Nil policy → deny → failure note on origin ticket.
	foundFailureNote := false
	for _, event := range writer.events {
		if event.RoomID == originRoom.String() && event.EventType == schema.EventTypeTicket {
			if ticketContent, ok := event.Content.(ticket.TicketContent); ok {
				for _, note := range ticketContent.Notes {
					if note.Body != "" {
						foundFailureNote = true
					}
				}
			}
		}
	}
	if !foundFailureNote {
		t.Error("expected a failure note when no relay policy is configured")
	}

	if len(ts.relayEntries) != 0 {
		t.Errorf("expected 0 relay entries, got %d", len(ts.relayEntries))
	}
}

// --- fetchRelayPolicy tests ---

func TestFetchRelayPolicyReturnsPolicy(t *testing.T) {
	ts, _, session := relayTestService(t)

	opsRoom := testRoomID("!ops:bureau.local")
	session.setPolicy(opsRoom, schema.RelayPolicy{
		Sources: []schema.RelaySource{
			{Match: schema.RelayMatchFleetMember, Fleet: "bureau/fleet/prod"},
		},
		AllowedTypes: []string{"resource_request"},
	})

	policy, err := ts.fetchRelayPolicy(context.Background(), opsRoom)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if policy == nil {
		t.Fatal("expected non-nil policy")
	}
	if len(policy.Sources) != 1 {
		t.Errorf("expected 1 source, got %d", len(policy.Sources))
	}
	if len(policy.AllowedTypes) != 1 || policy.AllowedTypes[0] != "resource_request" {
		t.Errorf("allowed types = %v, want [resource_request]", policy.AllowedTypes)
	}
}

func TestFetchRelayPolicyReturnsNilForNotFound(t *testing.T) {
	ts, _, _ := relayTestService(t)

	opsRoom := testRoomID("!ops:bureau.local")

	policy, err := ts.fetchRelayPolicy(context.Background(), opsRoom)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if policy != nil {
		t.Fatalf("expected nil policy for non-existent event, got %+v", policy)
	}
}

// --- isNotFoundError tests ---

func TestIsNotFoundError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "M_NOT_FOUND",
			err:  &messaging.MatrixError{Code: messaging.ErrCodeNotFound, Message: "not found"},
			want: true,
		},
		{
			name: "other matrix error",
			err:  &messaging.MatrixError{Code: "M_FORBIDDEN", Message: "forbidden"},
			want: false,
		},
		{
			name: "non-matrix error",
			err:  fmt.Errorf("some error"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isNotFoundError(tt.err)
			if got != tt.want {
				t.Errorf("isNotFoundError() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Claim status tracking tests ---

func TestFindRelayEntryByOpsRoom(t *testing.T) {
	ts, _, _ := relayTestService(t)

	opsRoomA := testRoomID("!ops-a:bureau.local")
	opsRoomB := testRoomID("!ops-b:bureau.local")

	ts.relayEntries["ws-tkt-1"] = &relayEntry{
		originRoom:   testRoomID("!workspace:bureau.local"),
		originTicket: "ws-tkt-1",
		relayTickets: map[ref.RoomID]relayTicketRef{
			opsRoomA: {ticketID: "relay-a", claimIndex: 0},
			opsRoomB: {ticketID: "relay-b", claimIndex: 1},
		},
	}

	t.Run("found", func(t *testing.T) {
		entry, originTicketID, claimIndex := ts.findRelayEntryByOpsRoom(opsRoomA, "relay-a")
		if entry == nil {
			t.Fatal("expected non-nil entry")
		}
		if originTicketID != "ws-tkt-1" {
			t.Errorf("origin ticket = %q, want ws-tkt-1", originTicketID)
		}
		if claimIndex != 0 {
			t.Errorf("claim index = %d, want 0", claimIndex)
		}
	})

	t.Run("found second claim", func(t *testing.T) {
		entry, originTicketID, claimIndex := ts.findRelayEntryByOpsRoom(opsRoomB, "relay-b")
		if entry == nil {
			t.Fatal("expected non-nil entry")
		}
		if originTicketID != "ws-tkt-1" {
			t.Errorf("origin ticket = %q, want ws-tkt-1", originTicketID)
		}
		if claimIndex != 1 {
			t.Errorf("claim index = %d, want 1", claimIndex)
		}
	})

	t.Run("wrong ticket ID", func(t *testing.T) {
		entry, _, _ := ts.findRelayEntryByOpsRoom(opsRoomA, "wrong-id")
		if entry != nil {
			t.Error("expected nil entry for wrong ticket ID")
		}
	})

	t.Run("wrong room", func(t *testing.T) {
		entry, _, _ := ts.findRelayEntryByOpsRoom(testRoomID("!unknown:bureau.local"), "relay-a")
		if entry != nil {
			t.Error("expected nil entry for unknown room")
		}
	})
}

func TestUpdateClaimStatus(t *testing.T) {
	t.Run("normal update", func(t *testing.T) {
		ts, writer, _ := relayTestService(t)

		originRoom := testRoomID("!workspace:bureau.local")
		ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
			"ws-tkt-1": {
				Version:   ticket.TicketContentVersion,
				Title:     "Need GPU",
				Status:    ticket.StatusOpen,
				Priority:  2,
				Type:      ticket.TypeResourceRequest,
				CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
				CreatedAt: "2026-01-15T12:00:00Z",
				UpdatedAt: "2026-01-15T12:00:00Z",
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{
							Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
							Mode:     schema.ModeExclusive,
							Status:   schema.ClaimPending,
							StatusAt: "2026-01-15T12:00:00Z",
						},
					},
					MaxDuration: "2h",
				},
			},
		})

		entry := &relayEntry{
			originRoom:   originRoom,
			originTicket: "ws-tkt-1",
			relayTickets: map[ref.RoomID]relayTicketRef{},
		}

		ts.updateClaimStatus(context.Background(), entry, "ws-tkt-1", 0, schema.ClaimApproved, "")

		// Verify the origin ticket was updated.
		if len(writer.events) != 1 {
			t.Fatalf("expected 1 write, got %d", len(writer.events))
		}
		content, ok := writer.events[0].Content.(ticket.TicketContent)
		if !ok {
			t.Fatalf("expected TicketContent, got %T", writer.events[0].Content)
		}
		if content.Reservation.Claims[0].Status != schema.ClaimApproved {
			t.Errorf("claim status = %q, want %q", content.Reservation.Claims[0].Status, schema.ClaimApproved)
		}
		if content.Reservation.Claims[0].StatusAt == "" {
			t.Error("claim StatusAt should be set")
		}
	})

	t.Run("skips terminal state", func(t *testing.T) {
		ts, writer, _ := relayTestService(t)

		originRoom := testRoomID("!workspace:bureau.local")
		ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
			"ws-tkt-1": {
				Version:   ticket.TicketContentVersion,
				Title:     "Need GPU",
				Status:    ticket.StatusOpen,
				Priority:  2,
				Type:      ticket.TypeResourceRequest,
				CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
				CreatedAt: "2026-01-15T12:00:00Z",
				UpdatedAt: "2026-01-15T12:00:00Z",
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{
							Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
							Mode:     schema.ModeExclusive,
							Status:   schema.ClaimDenied,
							StatusAt: "2026-01-15T12:01:00Z",
						},
					},
					MaxDuration: "2h",
				},
			},
		})

		entry := &relayEntry{
			originRoom:   originRoom,
			originTicket: "ws-tkt-1",
			relayTickets: map[ref.RoomID]relayTicketRef{},
		}

		// Trying to update a denied claim to approved should be a no-op.
		ts.updateClaimStatus(context.Background(), entry, "ws-tkt-1", 0, schema.ClaimApproved, "")

		if len(writer.events) != 0 {
			t.Fatalf("expected 0 writes for terminal state, got %d", len(writer.events))
		}
	})

	t.Run("skips same status", func(t *testing.T) {
		ts, writer, _ := relayTestService(t)

		originRoom := testRoomID("!workspace:bureau.local")
		ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
			"ws-tkt-1": {
				Version:   ticket.TicketContentVersion,
				Title:     "Need GPU",
				Status:    ticket.StatusOpen,
				Priority:  2,
				Type:      ticket.TypeResourceRequest,
				CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
				CreatedAt: "2026-01-15T12:00:00Z",
				UpdatedAt: "2026-01-15T12:00:00Z",
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{
							Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
							Mode:     schema.ModeExclusive,
							Status:   schema.ClaimApproved,
							StatusAt: "2026-01-15T12:01:00Z",
						},
					},
					MaxDuration: "2h",
				},
			},
		})

		entry := &relayEntry{
			originRoom:   originRoom,
			originTicket: "ws-tkt-1",
			relayTickets: map[ref.RoomID]relayTicketRef{},
		}

		ts.updateClaimStatus(context.Background(), entry, "ws-tkt-1", 0, schema.ClaimApproved, "")

		if len(writer.events) != 0 {
			t.Fatalf("expected 0 writes for same status, got %d", len(writer.events))
		}
	})

	t.Run("updates with reason", func(t *testing.T) {
		ts, writer, _ := relayTestService(t)

		originRoom := testRoomID("!workspace:bureau.local")
		ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
			"ws-tkt-1": {
				Version:   ticket.TicketContentVersion,
				Title:     "Need GPU",
				Status:    ticket.StatusOpen,
				Priority:  2,
				Type:      ticket.TypeResourceRequest,
				CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
				CreatedAt: "2026-01-15T12:00:00Z",
				UpdatedAt: "2026-01-15T12:00:00Z",
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{
							Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
							Mode:     schema.ModeExclusive,
							Status:   schema.ClaimGranted,
							StatusAt: "2026-01-15T12:01:00Z",
						},
					},
					MaxDuration: "2h",
				},
			},
		})

		entry := &relayEntry{
			originRoom:   originRoom,
			originTicket: "ws-tkt-1",
			relayTickets: map[ref.RoomID]relayTicketRef{},
		}

		ts.updateClaimStatus(context.Background(), entry, "ws-tkt-1", 0, schema.ClaimPreempted, "duration exceeded")

		if len(writer.events) != 1 {
			t.Fatalf("expected 1 write, got %d", len(writer.events))
		}
		content := writer.events[0].Content.(ticket.TicketContent)
		claim := content.Reservation.Claims[0]
		if claim.Status != schema.ClaimPreempted {
			t.Errorf("claim status = %q, want %q", claim.Status, schema.ClaimPreempted)
		}
		if claim.StatusReason != "duration exceeded" {
			t.Errorf("claim reason = %q, want %q", claim.StatusReason, "duration exceeded")
		}
	})

	t.Run("out of range claim index", func(t *testing.T) {
		ts, writer, _ := relayTestService(t)

		originRoom := testRoomID("!workspace:bureau.local")
		ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
			"ws-tkt-1": {
				Version:   ticket.TicketContentVersion,
				Title:     "Need GPU",
				Status:    ticket.StatusOpen,
				Priority:  2,
				Type:      ticket.TypeResourceRequest,
				CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
				CreatedAt: "2026-01-15T12:00:00Z",
				UpdatedAt: "2026-01-15T12:00:00Z",
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{
							Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
							Mode:     schema.ModeExclusive,
							Status:   schema.ClaimPending,
							StatusAt: "2026-01-15T12:00:00Z",
						},
					},
					MaxDuration: "2h",
				},
			},
		})

		entry := &relayEntry{
			originRoom:   originRoom,
			originTicket: "ws-tkt-1",
			relayTickets: map[ref.RoomID]relayTicketRef{},
		}

		// Claim index 5 doesn't exist — should be a no-op.
		ts.updateClaimStatus(context.Background(), entry, "ws-tkt-1", 5, schema.ClaimApproved, "")

		if len(writer.events) != 0 {
			t.Fatalf("expected 0 writes for out-of-range claim, got %d", len(writer.events))
		}
	})
}

func TestMirrorRelayTicketStatus(t *testing.T) {
	t.Run("in_progress maps to approved", func(t *testing.T) {
		ts, writer, _ := relayTestService(t)

		originRoom := testRoomID("!workspace:bureau.local")
		opsRoom := testRoomID("!ops:bureau.local")

		ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
			"ws-tkt-1": {
				Version:   ticket.TicketContentVersion,
				Title:     "Need GPU",
				Status:    ticket.StatusOpen,
				Priority:  2,
				Type:      ticket.TypeResourceRequest,
				CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
				CreatedAt: "2026-01-15T12:00:00Z",
				UpdatedAt: "2026-01-15T12:00:00Z",
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{
							Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
							Mode:     schema.ModeExclusive,
							Status:   schema.ClaimPending,
							StatusAt: "2026-01-15T12:00:00Z",
						},
					},
					MaxDuration: "2h",
				},
			},
		})

		ts.relayEntries["ws-tkt-1"] = &relayEntry{
			originRoom:   originRoom,
			originTicket: "ws-tkt-1",
			relayTickets: map[ref.RoomID]relayTicketRef{
				opsRoom: {ticketID: "relay-1", claimIndex: 0},
			},
		}

		relayContent := ticket.TicketContent{
			Status: ticket.StatusInProgress,
		}

		ts.mirrorRelayTicketStatus(context.Background(), opsRoom, "relay-1", relayContent)

		if len(writer.events) != 1 {
			t.Fatalf("expected 1 write, got %d", len(writer.events))
		}
		content := writer.events[0].Content.(ticket.TicketContent)
		if content.Reservation.Claims[0].Status != schema.ClaimApproved {
			t.Errorf("claim status = %q, want %q", content.Reservation.Claims[0].Status, schema.ClaimApproved)
		}
	})

	t.Run("closed after grant maps to preempted", func(t *testing.T) {
		ts, writer, _ := relayTestService(t)

		originRoom := testRoomID("!workspace:bureau.local")
		opsRoom := testRoomID("!ops:bureau.local")

		ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
			"ws-tkt-1": {
				Version:   ticket.TicketContentVersion,
				Title:     "Need GPU",
				Status:    ticket.StatusOpen,
				Priority:  2,
				Type:      ticket.TypeResourceRequest,
				CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
				CreatedAt: "2026-01-15T12:00:00Z",
				UpdatedAt: "2026-01-15T12:00:00Z",
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{
							Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
							Mode:     schema.ModeExclusive,
							Status:   schema.ClaimGranted,
							StatusAt: "2026-01-15T12:01:00Z",
						},
					},
					MaxDuration: "2h",
				},
			},
		})

		ts.relayEntries["ws-tkt-1"] = &relayEntry{
			originRoom:   originRoom,
			originTicket: "ws-tkt-1",
			relayTickets: map[ref.RoomID]relayTicketRef{
				opsRoom: {ticketID: "relay-1", claimIndex: 0},
			},
		}

		relayContent := ticket.TicketContent{
			Status:      ticket.StatusClosed,
			CloseReason: "duration exceeded",
		}

		ts.mirrorRelayTicketStatus(context.Background(), opsRoom, "relay-1", relayContent)

		// Preemption now produces two writes:
		// 1. Claim status update (preempted)
		// 2. Ticket closure (preempted: reason)
		// (Relay ticket closure would be a third write, but the ops
		// room is not tracked in ts.rooms so closeRelayTicketsForOrigin
		// skips it. The full cascade is exercised in integration tests.)
		if len(writer.events) != 2 {
			t.Fatalf("expected 2 writes, got %d", len(writer.events))
		}

		// First write: claim status updated to preempted.
		claimUpdate := writer.events[0].Content.(ticket.TicketContent)
		if claimUpdate.Reservation.Claims[0].Status != schema.ClaimPreempted {
			t.Errorf("claim status = %q, want %q", claimUpdate.Reservation.Claims[0].Status, schema.ClaimPreempted)
		}
		if claimUpdate.Reservation.Claims[0].StatusReason != "duration exceeded" {
			t.Errorf("claim reason = %q, want %q", claimUpdate.Reservation.Claims[0].StatusReason, "duration exceeded")
		}

		// Second write: ticket closed with preemption reason.
		// This event also carries the preempted claim status from
		// the first write — closeTicketOnPreemption re-reads the
		// ticket from the index after the claim update.
		ticketClosure := writer.events[1].Content.(ticket.TicketContent)
		if ticketClosure.Status != ticket.StatusClosed {
			t.Errorf("ticket status = %q, want %q", ticketClosure.Status, ticket.StatusClosed)
		}
		if ticketClosure.CloseReason != "preempted: duration exceeded" {
			t.Errorf("close reason = %q, want %q", ticketClosure.CloseReason, "preempted: duration exceeded")
		}

		// Relay entry should be cleaned up.
		if _, exists := ts.relayEntries["ws-tkt-1"]; exists {
			t.Error("relay entry should be deleted after preemption")
		}
	})

	t.Run("origin closed is no-op", func(t *testing.T) {
		ts, writer, _ := relayTestService(t)

		originRoom := testRoomID("!workspace:bureau.local")
		opsRoom := testRoomID("!ops:bureau.local")

		ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
			"ws-tkt-1": {
				Version:   ticket.TicketContentVersion,
				Title:     "Need GPU",
				Status:    ticket.StatusOpen,
				Priority:  2,
				Type:      ticket.TypeResourceRequest,
				CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
				CreatedAt: "2026-01-15T12:00:00Z",
				UpdatedAt: "2026-01-15T12:00:00Z",
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{
							Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
							Mode:     schema.ModeExclusive,
							Status:   schema.ClaimPending,
							StatusAt: "2026-01-15T12:00:00Z",
						},
					},
					MaxDuration: "2h",
				},
			},
		})

		ts.relayEntries["ws-tkt-1"] = &relayEntry{
			originRoom:   originRoom,
			originTicket: "ws-tkt-1",
			relayTickets: map[ref.RoomID]relayTicketRef{
				opsRoom: {ticketID: "relay-1", claimIndex: 0},
			},
		}

		relayContent := ticket.TicketContent{
			Status:      ticket.StatusClosed,
			CloseReason: "origin closed",
		}

		ts.mirrorRelayTicketStatus(context.Background(), opsRoom, "relay-1", relayContent)

		if len(writer.events) != 0 {
			t.Fatalf("expected 0 writes for origin-closed, got %d", len(writer.events))
		}
	})

	t.Run("untracked relay ticket is no-op", func(t *testing.T) {
		ts, writer, _ := relayTestService(t)

		opsRoom := testRoomID("!ops:bureau.local")

		relayContent := ticket.TicketContent{
			Status: ticket.StatusInProgress,
		}

		ts.mirrorRelayTicketStatus(context.Background(), opsRoom, "unknown-relay", relayContent)

		if len(writer.events) != 0 {
			t.Fatalf("expected 0 writes for untracked relay, got %d", len(writer.events))
		}
	})

	t.Run("closed without grant defers to denial cascade", func(t *testing.T) {
		ts, writer, _ := relayTestService(t)

		originRoom := testRoomID("!workspace:bureau.local")
		opsRoom := testRoomID("!ops:bureau.local")

		ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
			"ws-tkt-1": {
				Version:   ticket.TicketContentVersion,
				Title:     "Need GPU",
				Status:    ticket.StatusOpen,
				Priority:  2,
				Type:      ticket.TypeResourceRequest,
				CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
				CreatedAt: "2026-01-15T12:00:00Z",
				UpdatedAt: "2026-01-15T12:00:00Z",
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{
							Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
							Mode:     schema.ModeExclusive,
							Status:   schema.ClaimApproved,
							StatusAt: "2026-01-15T12:01:00Z",
						},
					},
					MaxDuration: "2h",
				},
			},
		})

		ts.relayEntries["ws-tkt-1"] = &relayEntry{
			originRoom:   originRoom,
			originTicket: "ws-tkt-1",
			relayTickets: map[ref.RoomID]relayTicketRef{
				opsRoom: {ticketID: "relay-1", claimIndex: 0},
			},
		}

		// Closed without prior grant — the claim was approved but not
		// granted. mirrorRelayTicketStatus should not update the claim
		// (the denial cascade in handleRelayTicketClosed handles this).
		relayContent := ticket.TicketContent{
			Status:      ticket.StatusClosed,
			CloseReason: "resource unavailable",
		}

		ts.mirrorRelayTicketStatus(context.Background(), opsRoom, "relay-1", relayContent)

		if len(writer.events) != 0 {
			t.Fatalf("expected 0 writes for non-granted closure (denial cascade handles it), got %d", len(writer.events))
		}
	})
}

func TestMirrorReservationGrant(t *testing.T) {
	t.Run("mirrors grant to claim", func(t *testing.T) {
		ts, writer, _ := relayTestService(t)

		originRoom := testRoomID("!workspace:bureau.local")
		opsRoom := testRoomID("!ops:bureau.local")

		ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
			"ws-tkt-1": {
				Version:   ticket.TicketContentVersion,
				Title:     "Need GPU",
				Status:    ticket.StatusOpen,
				Priority:  2,
				Type:      ticket.TypeResourceRequest,
				CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
				CreatedAt: "2026-01-15T12:00:00Z",
				UpdatedAt: "2026-01-15T12:00:00Z",
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{
							Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
							Mode:     schema.ModeExclusive,
							Status:   schema.ClaimApproved,
							StatusAt: "2026-01-15T12:01:00Z",
						},
					},
					MaxDuration: "2h",
				},
			},
		})

		ts.relayEntries["ws-tkt-1"] = &relayEntry{
			originRoom:   originRoom,
			originTicket: "ws-tkt-1",
			relayTickets: map[ref.RoomID]relayTicketRef{
				opsRoom: {ticketID: "relay-1", claimIndex: 0},
			},
		}

		ts.mirrorReservationGrant(context.Background(), opsRoom, "relay-1")

		if len(writer.events) != 1 {
			t.Fatalf("expected 1 write, got %d", len(writer.events))
		}
		content := writer.events[0].Content.(ticket.TicketContent)
		if content.Reservation.Claims[0].Status != schema.ClaimGranted {
			t.Errorf("claim status = %q, want %q", content.Reservation.Claims[0].Status, schema.ClaimGranted)
		}
	})

	t.Run("untracked relay is no-op", func(t *testing.T) {
		ts, writer, _ := relayTestService(t)

		ts.mirrorReservationGrant(context.Background(), testRoomID("!unknown:bureau.local"), "unknown-relay")

		if len(writer.events) != 0 {
			t.Fatalf("expected 0 writes for untracked relay, got %d", len(writer.events))
		}
	})

	t.Run("multi-claim grant updates correct claim", func(t *testing.T) {
		ts, writer, _ := relayTestService(t)

		originRoom := testRoomID("!workspace:bureau.local")
		opsRoomA := testRoomID("!ops-a:bureau.local")
		opsRoomB := testRoomID("!ops-b:bureau.local")

		ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
			"ws-tkt-1": {
				Version:   ticket.TicketContentVersion,
				Title:     "Need two GPUs",
				Status:    ticket.StatusOpen,
				Priority:  1,
				Type:      ticket.TypeResourceRequest,
				CreatedBy: ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
				CreatedAt: "2026-01-15T12:00:00Z",
				UpdatedAt: "2026-01-15T12:00:00Z",
				Reservation: &ticket.ReservationContent{
					Claims: []ticket.ResourceClaim{
						{
							Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-1"},
							Mode:     schema.ModeExclusive,
							Status:   schema.ClaimApproved,
							StatusAt: "2026-01-15T12:01:00Z",
						},
						{
							Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-2"},
							Mode:     schema.ModeExclusive,
							Status:   schema.ClaimApproved,
							StatusAt: "2026-01-15T12:01:00Z",
						},
					},
					MaxDuration: "2h",
				},
			},
		})

		ts.relayEntries["ws-tkt-1"] = &relayEntry{
			originRoom:   originRoom,
			originTicket: "ws-tkt-1",
			relayTickets: map[ref.RoomID]relayTicketRef{
				opsRoomA: {ticketID: "relay-a", claimIndex: 0},
				opsRoomB: {ticketID: "relay-b", claimIndex: 1},
			},
		}

		// Grant only claim 1 (gpu-2 in ops-b).
		ts.mirrorReservationGrant(context.Background(), opsRoomB, "relay-b")

		if len(writer.events) != 1 {
			t.Fatalf("expected 1 write, got %d", len(writer.events))
		}
		content := writer.events[0].Content.(ticket.TicketContent)

		// Claim 0 (gpu-1) should still be approved.
		if content.Reservation.Claims[0].Status != schema.ClaimApproved {
			t.Errorf("claim[0] status = %q, want %q", content.Reservation.Claims[0].Status, schema.ClaimApproved)
		}
		// Claim 1 (gpu-2) should now be granted.
		if content.Reservation.Claims[1].Status != schema.ClaimGranted {
			t.Errorf("claim[1] status = %q, want %q", content.Reservation.Claims[1].Status, schema.ClaimGranted)
		}
	})
}

// --- Drain status tests ---

func TestPublishDrainStatusWithActiveRelays(t *testing.T) {
	t.Parallel()

	ts, writer, _ := relayTestService(t)

	opsRoom := testRoomID("!ops-gpu:bureau.local")
	originRoom := testRoomID("!workspace:bureau.local")

	// Set up the ops room with ticket management and two relay tickets.
	ts.rooms[opsRoom] = newTrackedRoom(map[string]ticket.TicketContent{
		"relay-1": {
			Type:   ticket.TypeResourceRequest,
			Status: ticket.StatusOpen,
		},
		"relay-2": {
			Type:   ticket.TypeResourceRequest,
			Status: ticket.StatusInProgress,
		},
	})
	ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{
		"origin-1": {
			Type:   ticket.TypeResourceRequest,
			Status: ticket.StatusOpen,
		},
	})

	ts.relayEntries["origin-1"] = &relayEntry{
		originRoom:   originRoom,
		originTicket: "origin-1",
		relayTickets: map[ref.RoomID]relayTicketRef{
			opsRoom: {ticketID: "relay-1", claimIndex: 0},
		},
	}
	// relay-2 is from a different origin (simulates another relay in the same ops room).
	ts.relayEntries["origin-other"] = &relayEntry{
		originRoom:   testRoomID("!workspace-2:bureau.local"),
		originTicket: "origin-other",
		relayTickets: map[ref.RoomID]relayTicketRef{
			opsRoom: {ticketID: "relay-2", claimIndex: 0},
		},
	}

	// Build a drain event with an active drain.
	drainStateKey := ""
	drainEvent := messaging.Event{
		Type:     schema.EventTypeMachineDrain,
		StateKey: &drainStateKey,
		Content:  drainContentMap(t, "@bureau/fleet/prod/agent/benchmark:bureau.local"),
	}

	ts.publishDrainStatus(context.Background(), opsRoom, ts.rooms[opsRoom], drainEvent)

	// Should have published one drain_status event.
	if len(writer.events) != 1 {
		t.Fatalf("expected 1 drain_status write, got %d", len(writer.events))
	}

	written := writer.events[0]
	if written.EventType != schema.EventTypeDrainStatus {
		t.Errorf("event type = %s, want %s", written.EventType, schema.EventTypeDrainStatus)
	}
	if written.StateKey != ts.service.Localpart() {
		t.Errorf("state key = %q, want %q", written.StateKey, ts.service.Localpart())
	}

	status, ok := written.Content.(schema.DrainStatusContent)
	if !ok {
		t.Fatalf("content type = %T, want schema.DrainStatusContent", written.Content)
	}
	if !status.Acknowledged {
		t.Error("drain_status should be acknowledged")
	}
	if status.InFlight != 2 {
		t.Errorf("InFlight = %d, want 2 (two active relay tickets)", status.InFlight)
	}
	if status.DrainedAt != "" {
		t.Errorf("DrainedAt should be empty when InFlight > 0, got %q", status.DrainedAt)
	}
}

func TestPublishDrainStatusZeroInFlight(t *testing.T) {
	t.Parallel()

	ts, writer, _ := relayTestService(t)

	opsRoom := testRoomID("!ops-gpu:bureau.local")

	// Ops room with ticket management but no active relay tickets.
	ts.rooms[opsRoom] = newTrackedRoom(map[string]ticket.TicketContent{})

	drainStateKey := ""
	drainEvent := messaging.Event{
		Type:     schema.EventTypeMachineDrain,
		StateKey: &drainStateKey,
		Content:  drainContentMap(t, "@bureau/fleet/prod/agent/benchmark:bureau.local"),
	}

	ts.publishDrainStatus(context.Background(), opsRoom, ts.rooms[opsRoom], drainEvent)

	if len(writer.events) != 1 {
		t.Fatalf("expected 1 drain_status write, got %d", len(writer.events))
	}

	status, ok := writer.events[0].Content.(schema.DrainStatusContent)
	if !ok {
		t.Fatalf("content type = %T, want schema.DrainStatusContent", writer.events[0].Content)
	}
	if !status.Acknowledged {
		t.Error("drain_status should be acknowledged")
	}
	if status.InFlight != 0 {
		t.Errorf("InFlight = %d, want 0", status.InFlight)
	}
	expectedDrainedAt := ts.clock.Now().UTC().Format(time.RFC3339)
	if status.DrainedAt != expectedDrainedAt {
		t.Errorf("DrainedAt = %q, want %q", status.DrainedAt, expectedDrainedAt)
	}
}

func TestPublishDrainStatusClearedDrain(t *testing.T) {
	t.Parallel()

	ts, writer, _ := relayTestService(t)

	opsRoom := testRoomID("!ops-gpu:bureau.local")
	ts.rooms[opsRoom] = newTrackedRoom(map[string]ticket.TicketContent{})

	// Cleared drain: empty content.
	drainStateKey := ""
	drainEvent := messaging.Event{
		Type:     schema.EventTypeMachineDrain,
		StateKey: &drainStateKey,
		Content:  map[string]any{},
	}

	ts.publishDrainStatus(context.Background(), opsRoom, ts.rooms[opsRoom], drainEvent)

	if len(writer.events) != 1 {
		t.Fatalf("expected 1 drain_status clear write, got %d", len(writer.events))
	}

	written := writer.events[0]
	if written.EventType != schema.EventTypeDrainStatus {
		t.Errorf("event type = %s, want %s", written.EventType, schema.EventTypeDrainStatus)
	}

	// Cleared drain should publish empty JSON object.
	raw, ok := written.Content.(json.RawMessage)
	if !ok {
		t.Fatalf("cleared drain_status content type = %T, want json.RawMessage", written.Content)
	}
	if string(raw) != "{}" {
		t.Errorf("cleared drain_status content = %s, want {}", raw)
	}
}

func TestPublishDrainStatusExcludesClosedRelayTickets(t *testing.T) {
	t.Parallel()

	ts, writer, _ := relayTestService(t)

	opsRoom := testRoomID("!ops-gpu:bureau.local")
	originRoom := testRoomID("!workspace:bureau.local")

	// One open relay, one closed relay.
	ts.rooms[opsRoom] = newTrackedRoom(map[string]ticket.TicketContent{
		"relay-open": {
			Type:   ticket.TypeResourceRequest,
			Status: ticket.StatusOpen,
		},
		"relay-closed": {
			Type:   ticket.TypeResourceRequest,
			Status: ticket.StatusClosed,
		},
	})
	ts.rooms[originRoom] = newTrackedRoom(map[string]ticket.TicketContent{})

	ts.relayEntries["origin-1"] = &relayEntry{
		originRoom:   originRoom,
		originTicket: "origin-1",
		relayTickets: map[ref.RoomID]relayTicketRef{
			opsRoom: {ticketID: "relay-open", claimIndex: 0},
		},
	}
	ts.relayEntries["origin-2"] = &relayEntry{
		originRoom:   originRoom,
		originTicket: "origin-2",
		relayTickets: map[ref.RoomID]relayTicketRef{
			opsRoom: {ticketID: "relay-closed", claimIndex: 0},
		},
	}

	drainStateKey := ""
	drainEvent := messaging.Event{
		Type:     schema.EventTypeMachineDrain,
		StateKey: &drainStateKey,
		Content:  drainContentMap(t, "@bureau/fleet/prod/agent/benchmark:bureau.local"),
	}

	ts.publishDrainStatus(context.Background(), opsRoom, ts.rooms[opsRoom], drainEvent)

	if len(writer.events) != 1 {
		t.Fatalf("expected 1 write, got %d", len(writer.events))
	}

	status := writer.events[0].Content.(schema.DrainStatusContent)
	if status.InFlight != 1 {
		t.Errorf("InFlight = %d, want 1 (only the open relay ticket)", status.InFlight)
	}
}

// drainContentMap builds a drain event content map with a valid
// MachineDrainContent for testing.
func drainContentMap(t *testing.T, holderUserID string) map[string]any {
	t.Helper()
	holder, err := ref.ParseEntityUserID(holderUserID)
	if err != nil {
		t.Fatalf("ParseEntityUserID: %v", err)
	}
	drain := schema.MachineDrainContent{
		Services:          []string{"service/ticket/test"},
		ReservationHolder: holder,
		RequestedAt:       "2026-03-01T12:00:00Z",
	}
	data, err := json.Marshal(drain)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return result
}
