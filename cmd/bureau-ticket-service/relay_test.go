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

func TestCloseRelayTicketsForWorkspace(t *testing.T) {
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
		workspaceRoom:   testRoomID("!workspace:bureau.local"),
		workspaceTicket: "ws-tkt-1",
		relayTickets: map[ref.RoomID]relayTicketRef{
			opsRoomA: {ticketID: "relay-a", claimIndex: 0},
			opsRoomB: {ticketID: "relay-b", claimIndex: 1},
		},
	}

	ts.closeRelayTicketsForWorkspace(context.Background(), "ws-tkt-1")

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
		t.Error("relay entry should be removed after workspace closure")
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
		workspaceRoom:   testRoomID("!workspace:bureau.local"),
		workspaceTicket: "ws-tkt-1",
		relayTickets: map[ref.RoomID]relayTicketRef{
			opsRoom: {ticketID: "relay-1", claimIndex: 0},
		},
	}

	ts.closeRelayTicketsForWorkspace(context.Background(), "ws-tkt-1")

	// No writes should happen — ticket was already closed.
	if len(writer.events) != 0 {
		t.Fatalf("expected 0 writes for already-closed relay ticket, got %d", len(writer.events))
	}
}

func TestCloseRelayTicketsNoOpWhenNoEntry(t *testing.T) {
	ts, writer, _ := relayTestService(t)

	ts.closeRelayTicketsForWorkspace(context.Background(), "nonexistent-ticket")

	if len(writer.events) != 0 {
		t.Fatalf("expected 0 writes for nonexistent entry, got %d", len(writer.events))
	}
}

// --- Denial cascade tests ---

func TestHandleRelayTicketClosedCascadesDenial(t *testing.T) {
	ts, writer, _ := relayTestService(t)

	workspaceRoom := testRoomID("!workspace:bureau.local")
	opsRoomA := testRoomID("!ops-a:bureau.local")
	opsRoomB := testRoomID("!ops-b:bureau.local")

	// Set up workspace room with the source ticket.
	ts.rooms[workspaceRoom] = newTrackedRoom(map[string]ticket.TicketContent{
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
		workspaceRoom:   workspaceRoom,
		workspaceTicket: "ws-tkt-1",
		relayTickets: map[ref.RoomID]relayTicketRef{
			opsRoomA: {ticketID: "relay-a", claimIndex: 0},
			opsRoomB: {ticketID: "relay-b", claimIndex: 1},
		},
	}

	// Simulate relay-a being closed by the ops side (denial).
	relayContent, _ := ts.rooms[opsRoomA].index.Get("relay-a")
	ts.handleRelayTicketClosed(context.Background(), opsRoomA, "relay-a", relayContent)

	// Expect: relay-b closed (denial cascade) + workspace ticket closed.
	if len(writer.events) < 2 {
		t.Fatalf("expected at least 2 writes (cascade relay-b + close workspace), got %d", len(writer.events))
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

	// Check that the workspace ticket was closed.
	wsContent, exists := ts.rooms[workspaceRoom].index.Get("ws-tkt-1")
	if !exists {
		t.Fatal("workspace ticket should still exist in index")
	}
	if wsContent.Status != ticket.StatusClosed {
		t.Errorf("workspace ticket status = %q, want closed", wsContent.Status)
	}
	if wsContent.CloseReason != "reservation denied: resource unavailable" {
		t.Errorf("workspace close reason = %q, want %q", wsContent.CloseReason, "reservation denied: resource unavailable")
	}

	// Relay entry should be cleaned up.
	if _, exists := ts.relayEntries["ws-tkt-1"]; exists {
		t.Error("relay entry should be removed after denial cascade")
	}
}

func TestHandleRelayTicketClosedIgnoresOriginClosed(t *testing.T) {
	ts, writer, _ := relayTestService(t)

	opsRoom := testRoomID("!ops:bureau.local")
	workspaceRoom := testRoomID("!workspace:bureau.local")

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
		workspaceRoom:   workspaceRoom,
		workspaceTicket: "ws-tkt-1",
		relayTickets: map[ref.RoomID]relayTicketRef{
			opsRoom: {ticketID: "relay-1", claimIndex: 0},
		},
	}

	relayContent, _ := ts.rooms[opsRoom].index.Get("relay-1")
	ts.handleRelayTicketClosed(context.Background(), opsRoom, "relay-1", relayContent)

	// No cascade should happen — the closure was initiated from workspace side.
	if len(writer.events) != 0 {
		t.Fatalf("expected 0 writes for origin-closed relay ticket, got %d", len(writer.events))
	}

	// Relay entry should still exist (workspace closure path handles cleanup).
	if _, exists := ts.relayEntries["ws-tkt-1"]; !exists {
		t.Error("relay entry should still exist after origin-closed (cleanup happens in workspace path)")
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

	workspaceRoom := testRoomID("!workspace:bureau.local")
	opsRoom := testRoomID("!ops:bureau.local")

	opsAlias := ref.MustParseRoomAlias("#bureau/fleet/prod/machine/gpu-box/ops:bureau.local")

	// Pre-populate alias cache (rebuild calls resolveAliasWithCache).
	ts.aliasCache[opsAlias] = opsRoom

	// Workspace room with a relayed resource_request ticket.
	ts.rooms[workspaceRoom] = newTrackedRoom(map[string]ticket.TicketContent{
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
	if entry.workspaceRoom != workspaceRoom {
		t.Errorf("workspace room = %q, want %q", entry.workspaceRoom, workspaceRoom)
	}
	if entry.workspaceTicket != "ws-tkt-1" {
		t.Errorf("workspace ticket = %q, want %q", entry.workspaceTicket, "ws-tkt-1")
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

	workspaceRoom := testRoomID("!workspace:bureau.local")
	opsRoom := testRoomID("!ops:bureau.local")

	opsAlias := ref.MustParseRoomAlias("#bureau/fleet/prod/machine/gpu-box/ops:bureau.local")
	ts.aliasCache[opsAlias] = opsRoom

	ts.rooms[workspaceRoom] = newTrackedRoom(map[string]ticket.TicketContent{
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

	workspaceRoom := testRoomID("!workspace:bureau.local")

	// Resource request without reservation gates (not yet relayed).
	ts.rooms[workspaceRoom] = newTrackedRoom(map[string]ticket.TicketContent{
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

	workspaceRoom := testRoomID("!workspace:bureau.local")
	opsRoom := testRoomID("!ops-gpu:bureau.local")

	opsAlias := ref.MustParseRoomAlias("#bureau/fleet/prod/machine/gpu-box/ops:bureau.local")

	// Set up alias resolution.
	ts.resolver = &fakeAliasResolver{
		aliases: map[string]string{
			opsAlias.String(): opsRoom.String(),
		},
	}

	// Workspace room needs a fleet-parseable alias.
	wsState := newTrackedRoom(nil)
	wsState.alias = "#bureau/fleet/prod/workspace/my-project:bureau.local"
	ts.rooms[workspaceRoom] = wsState

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

	ts.checkAndInitiateRelay(context.Background(), workspaceRoom, wsState, "ws-tkt-1", content)

	// Expect: relay ticket created (1 write) + relay link published
	// (1 write) + workspace ticket updated with gate (1 write) = 3
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

	// Verify the workspace ticket was updated with a reservation gate.
	var workspaceWrite *writtenEventForGates
	for index := range writer.events {
		event := &writer.events[index]
		if event.RoomID == workspaceRoom.String() && event.EventType == schema.EventTypeTicket {
			workspaceWrite = event
			break
		}
	}
	if workspaceWrite == nil {
		t.Fatal("no workspace ticket update found")
	}
	updatedContent, ok := workspaceWrite.Content.(ticket.TicketContent)
	if !ok {
		t.Fatalf("workspace ticket content is %T, want TicketContent", workspaceWrite.Content)
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
		t.Fatal("workspace ticket should have a reservation state_event gate")
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

	workspaceRoom := testRoomID("!workspace:bureau.local")
	opsRoom := testRoomID("!ops-gpu:bureau.local")

	opsAlias := ref.MustParseRoomAlias("#bureau/fleet/prod/machine/gpu-box/ops:bureau.local")

	ts.resolver = &fakeAliasResolver{
		aliases: map[string]string{
			opsAlias.String(): opsRoom.String(),
		},
	}

	wsState := newTrackedRoom(nil)
	wsState.alias = "#bureau/fleet/prod/workspace/my-project:bureau.local"
	ts.rooms[workspaceRoom] = wsState

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

	ts.checkAndInitiateRelay(context.Background(), workspaceRoom, wsState, "ws-tkt-1", content)

	// Should write a failure note to the workspace ticket.
	foundFailureNote := false
	for _, event := range writer.events {
		if event.RoomID == workspaceRoom.String() && event.EventType == schema.EventTypeTicket {
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
		t.Error("expected a failure note on the workspace ticket after policy denial")
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

	workspaceRoom := testRoomID("!workspace:bureau.local")
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

	wsState := newTrackedRoom(nil)
	wsState.alias = "#bureau/fleet/prod/workspace/multi-gpu:bureau.local"
	ts.rooms[workspaceRoom] = wsState
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

	ts.checkAndInitiateRelay(context.Background(), workspaceRoom, wsState, "ws-multi", content)

	// Expect writes for 2 relay tickets + 2 relay links + 1 workspace update = 5.
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
	var wsUpdate *writtenEventForGates
	for index := range writer.events {
		event := &writer.events[index]
		if event.RoomID == workspaceRoom.String() && event.EventType == schema.EventTypeTicket {
			wsUpdate = event
		}
	}
	if wsUpdate == nil {
		t.Fatal("no workspace ticket update found")
	}
	updatedContent, ok := wsUpdate.Content.(ticket.TicketContent)
	if !ok {
		t.Fatalf("workspace content is %T, want TicketContent", wsUpdate.Content)
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

	workspaceRoom := testRoomID("!workspace:bureau.local")
	opsRoom := testRoomID("!ops-gpu:bureau.local")

	opsAlias := ref.MustParseRoomAlias("#bureau/fleet/prod/machine/gpu-box/ops:bureau.local")

	ts.resolver = &fakeAliasResolver{
		aliases: map[string]string{
			opsAlias.String(): opsRoom.String(),
		},
	}

	wsState := newTrackedRoom(nil)
	wsState.alias = "#bureau/fleet/prod/workspace/my-project:bureau.local"
	ts.rooms[workspaceRoom] = wsState
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

	ts.checkAndInitiateRelay(context.Background(), workspaceRoom, wsState, "ws-tkt-1", content)

	// Nil policy → deny → failure note on workspace ticket.
	foundFailureNote := false
	for _, event := range writer.events {
		if event.RoomID == workspaceRoom.String() && event.EventType == schema.EventTypeTicket {
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
		workspaceRoom:   testRoomID("!workspace:bureau.local"),
		workspaceTicket: "ws-tkt-1",
		relayTickets: map[ref.RoomID]relayTicketRef{
			opsRoomA: {ticketID: "relay-a", claimIndex: 0},
			opsRoomB: {ticketID: "relay-b", claimIndex: 1},
		},
	}

	t.Run("found", func(t *testing.T) {
		entry, workspaceTicketID, claimIndex := ts.findRelayEntryByOpsRoom(opsRoomA, "relay-a")
		if entry == nil {
			t.Fatal("expected non-nil entry")
		}
		if workspaceTicketID != "ws-tkt-1" {
			t.Errorf("workspace ticket = %q, want ws-tkt-1", workspaceTicketID)
		}
		if claimIndex != 0 {
			t.Errorf("claim index = %d, want 0", claimIndex)
		}
	})

	t.Run("found second claim", func(t *testing.T) {
		entry, workspaceTicketID, claimIndex := ts.findRelayEntryByOpsRoom(opsRoomB, "relay-b")
		if entry == nil {
			t.Fatal("expected non-nil entry")
		}
		if workspaceTicketID != "ws-tkt-1" {
			t.Errorf("workspace ticket = %q, want ws-tkt-1", workspaceTicketID)
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

		workspaceRoom := testRoomID("!workspace:bureau.local")
		ts.rooms[workspaceRoom] = newTrackedRoom(map[string]ticket.TicketContent{
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
			workspaceRoom:   workspaceRoom,
			workspaceTicket: "ws-tkt-1",
			relayTickets:    map[ref.RoomID]relayTicketRef{},
		}

		ts.updateClaimStatus(context.Background(), entry, "ws-tkt-1", 0, schema.ClaimApproved, "")

		// Verify the workspace ticket was updated.
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

		workspaceRoom := testRoomID("!workspace:bureau.local")
		ts.rooms[workspaceRoom] = newTrackedRoom(map[string]ticket.TicketContent{
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
			workspaceRoom:   workspaceRoom,
			workspaceTicket: "ws-tkt-1",
			relayTickets:    map[ref.RoomID]relayTicketRef{},
		}

		// Trying to update a denied claim to approved should be a no-op.
		ts.updateClaimStatus(context.Background(), entry, "ws-tkt-1", 0, schema.ClaimApproved, "")

		if len(writer.events) != 0 {
			t.Fatalf("expected 0 writes for terminal state, got %d", len(writer.events))
		}
	})

	t.Run("skips same status", func(t *testing.T) {
		ts, writer, _ := relayTestService(t)

		workspaceRoom := testRoomID("!workspace:bureau.local")
		ts.rooms[workspaceRoom] = newTrackedRoom(map[string]ticket.TicketContent{
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
			workspaceRoom:   workspaceRoom,
			workspaceTicket: "ws-tkt-1",
			relayTickets:    map[ref.RoomID]relayTicketRef{},
		}

		ts.updateClaimStatus(context.Background(), entry, "ws-tkt-1", 0, schema.ClaimApproved, "")

		if len(writer.events) != 0 {
			t.Fatalf("expected 0 writes for same status, got %d", len(writer.events))
		}
	})

	t.Run("updates with reason", func(t *testing.T) {
		ts, writer, _ := relayTestService(t)

		workspaceRoom := testRoomID("!workspace:bureau.local")
		ts.rooms[workspaceRoom] = newTrackedRoom(map[string]ticket.TicketContent{
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
			workspaceRoom:   workspaceRoom,
			workspaceTicket: "ws-tkt-1",
			relayTickets:    map[ref.RoomID]relayTicketRef{},
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

		workspaceRoom := testRoomID("!workspace:bureau.local")
		ts.rooms[workspaceRoom] = newTrackedRoom(map[string]ticket.TicketContent{
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
			workspaceRoom:   workspaceRoom,
			workspaceTicket: "ws-tkt-1",
			relayTickets:    map[ref.RoomID]relayTicketRef{},
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

		workspaceRoom := testRoomID("!workspace:bureau.local")
		opsRoom := testRoomID("!ops:bureau.local")

		ts.rooms[workspaceRoom] = newTrackedRoom(map[string]ticket.TicketContent{
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
			workspaceRoom:   workspaceRoom,
			workspaceTicket: "ws-tkt-1",
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

		workspaceRoom := testRoomID("!workspace:bureau.local")
		opsRoom := testRoomID("!ops:bureau.local")

		ts.rooms[workspaceRoom] = newTrackedRoom(map[string]ticket.TicketContent{
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
			workspaceRoom:   workspaceRoom,
			workspaceTicket: "ws-tkt-1",
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
		// room is not tracked in ts.rooms so closeRelayTicketsForWorkspace
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

		workspaceRoom := testRoomID("!workspace:bureau.local")
		opsRoom := testRoomID("!ops:bureau.local")

		ts.rooms[workspaceRoom] = newTrackedRoom(map[string]ticket.TicketContent{
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
			workspaceRoom:   workspaceRoom,
			workspaceTicket: "ws-tkt-1",
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

		workspaceRoom := testRoomID("!workspace:bureau.local")
		opsRoom := testRoomID("!ops:bureau.local")

		ts.rooms[workspaceRoom] = newTrackedRoom(map[string]ticket.TicketContent{
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
			workspaceRoom:   workspaceRoom,
			workspaceTicket: "ws-tkt-1",
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

		workspaceRoom := testRoomID("!workspace:bureau.local")
		opsRoom := testRoomID("!ops:bureau.local")

		ts.rooms[workspaceRoom] = newTrackedRoom(map[string]ticket.TicketContent{
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
			workspaceRoom:   workspaceRoom,
			workspaceTicket: "ws-tkt-1",
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

		workspaceRoom := testRoomID("!workspace:bureau.local")
		opsRoomA := testRoomID("!ops-a:bureau.local")
		opsRoomB := testRoomID("!ops-b:bureau.local")

		ts.rooms[workspaceRoom] = newTrackedRoom(map[string]ticket.TicketContent{
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
			workspaceRoom:   workspaceRoom,
			workspaceTicket: "ws-tkt-1",
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
