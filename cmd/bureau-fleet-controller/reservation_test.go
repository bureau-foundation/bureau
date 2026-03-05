// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/messaging"
)

// newReservationTestController creates a FleetController with a
// fakeConfigStore and a fake clock, suitable for reservation tests.
func newReservationTestController(t *testing.T) (*FleetController, *fakeConfigStore) {
	t.Helper()
	fc, store := newExecuteTestController(t)
	fc.clock = clock.Fake(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	return fc, store
}

// testMachineLocalpart returns the full machine localpart for a short
// machine name in the standard test fleet (bureau/fleet/prod).
func testMachineLocalpart(name string) string {
	return "bureau/fleet/prod/machine/" + name
}

// testOpsRoomID returns a deterministic ops room ID for a machine name.
func testOpsRoomID(name string) ref.RoomID {
	return mustRoomID("!ops-" + name + ":local")
}

// makeRelayTicketEvent constructs a ticket state event for a
// resource_request relay ticket targeting a machine.
func makeRelayTicketEvent(ticketID string, machineName string, status ticket.TicketStatus, priority int, maxDuration string, mode schema.ReservationMode, originServerTS int64) messaging.Event {
	content := ticket.TicketContent{
		Version:  5,
		Title:    "Reserve " + machineName,
		Status:   status,
		Priority: priority,
		Type:     ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{
						Type:   schema.ResourceMachine,
						Target: machineName,
					},
					Mode:   mode,
					Status: schema.ClaimPending,
				},
			},
			MaxDuration: maxDuration,
		},
	}

	contentMap := mustContentMap(content)
	return messaging.Event{
		Type:           schema.EventTypeTicket,
		StateKey:       stringPtr(ticketID),
		Content:        contentMap,
		OriginServerTS: originServerTS,
	}
}

// makeRelayLinkEvent constructs a relay link state event for an ops room.
func makeRelayLinkEvent(ticketID string, requester ref.Entity) messaging.Event {
	content := schema.RelayLink{
		OriginRoom:   mustRoomID("!workspace:local"),
		OriginTicket: "origin-" + ticketID,
		Requester:    requester,
	}
	contentMap := mustContentMap(content)
	return messaging.Event{
		Type:     schema.EventTypeRelayLink,
		StateKey: stringPtr(ticketID),
		Content:  contentMap,
	}
}

// mustContentMap converts a struct to map[string]any via JSON round-trip.
func mustContentMap(value any) map[string]any {
	data, err := json.Marshal(value)
	if err != nil {
		panic("mustContentMap marshal: " + err.Error())
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		panic("mustContentMap unmarshal: " + err.Error())
	}
	return result
}

// --- sortReservationQueue tests ---

func TestSortReservationQueueByPriority(t *testing.T) {
	queue := []queuedReservation{
		{relayTicketID: "p3", priority: 3, createdAt: time.Unix(100, 0)},
		{relayTicketID: "p0", priority: 0, createdAt: time.Unix(200, 0)},
		{relayTicketID: "p1", priority: 1, createdAt: time.Unix(150, 0)},
	}

	sortReservationQueue(queue)

	if queue[0].relayTicketID != "p0" {
		t.Errorf("queue[0] = %q, want p0 (priority 0)", queue[0].relayTicketID)
	}
	if queue[1].relayTicketID != "p1" {
		t.Errorf("queue[1] = %q, want p1 (priority 1)", queue[1].relayTicketID)
	}
	if queue[2].relayTicketID != "p3" {
		t.Errorf("queue[2] = %q, want p3 (priority 3)", queue[2].relayTicketID)
	}
}

func TestSortReservationQueueSamePriorityByTime(t *testing.T) {
	queue := []queuedReservation{
		{relayTicketID: "later", priority: 2, createdAt: time.Unix(200, 0)},
		{relayTicketID: "earlier", priority: 2, createdAt: time.Unix(100, 0)},
		{relayTicketID: "middle", priority: 2, createdAt: time.Unix(150, 0)},
	}

	sortReservationQueue(queue)

	if queue[0].relayTicketID != "earlier" {
		t.Errorf("queue[0] = %q, want earlier", queue[0].relayTicketID)
	}
	if queue[1].relayTicketID != "middle" {
		t.Errorf("queue[1] = %q, want middle", queue[1].relayTicketID)
	}
	if queue[2].relayTicketID != "later" {
		t.Errorf("queue[2] = %q, want later", queue[2].relayTicketID)
	}
}

// --- classifyOpsRoom tests ---

func TestClassifyOpsRoomFromResourceRequestTicket(t *testing.T) {
	fc := newTestFleetController(t)

	roomID := testOpsRoomID("gpu-box")
	content := &ticket.TicketContent{
		Type: ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{
						Type:   schema.ResourceMachine,
						Target: "gpu-box",
					},
					Mode:   schema.ModeExclusive,
					Status: schema.ClaimPending,
				},
			},
			MaxDuration: "2h",
		},
	}

	machineLocalpart, classified := fc.classifyOpsRoom(roomID, content)
	if !classified {
		t.Fatal("expected room to be classified as ops room")
	}
	if machineLocalpart != testMachineLocalpart("gpu-box") {
		t.Errorf("machine localpart = %q, want %q", machineLocalpart, testMachineLocalpart("gpu-box"))
	}

	// Verify the maps were populated.
	if fc.opsRooms[testMachineLocalpart("gpu-box")] != roomID {
		t.Error("opsRooms should map machine to room ID")
	}
	if fc.opsRoomMachines[roomID] != testMachineLocalpart("gpu-box") {
		t.Error("opsRoomMachines should map room ID to machine")
	}
}

func TestClassifyOpsRoomIgnoresNonResourceRequest(t *testing.T) {
	fc := newTestFleetController(t)

	content := &ticket.TicketContent{
		Type: "bug",
	}

	_, classified := fc.classifyOpsRoom(testOpsRoomID("x"), content)
	if classified {
		t.Error("non-resource_request ticket should not classify a room")
	}
}

func TestClassifyOpsRoomIgnoresNonMachineResource(t *testing.T) {
	fc := newTestFleetController(t)

	content := &ticket.TicketContent{
		Type: ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{
						Type:   schema.ResourceQuota,
						Target: "api-budget",
					},
					Mode:   schema.ModeExclusive,
					Status: schema.ClaimPending,
				},
			},
			MaxDuration: "1h",
		},
	}

	_, classified := fc.classifyOpsRoom(testOpsRoomID("x"), content)
	if classified {
		t.Error("quota resource_request should not classify a room as machine ops room")
	}
}

func TestClassifyOpsRoomIdempotent(t *testing.T) {
	fc := newTestFleetController(t)

	roomID := testOpsRoomID("gpu-box")
	content := &ticket.TicketContent{
		Type: ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{
						Type:   schema.ResourceMachine,
						Target: "gpu-box",
					},
					Mode:   schema.ModeExclusive,
					Status: schema.ClaimPending,
				},
			},
			MaxDuration: "2h",
		},
	}

	// Classify twice — second call should not overwrite.
	fc.classifyOpsRoom(roomID, content)
	fc.classifyOpsRoom(roomID, content)

	if len(fc.opsRooms) != 1 {
		t.Errorf("expected 1 ops room mapping, got %d", len(fc.opsRooms))
	}
}

// --- processRelayLinkEvent tests ---

func TestProcessRelayLinkEvent(t *testing.T) {
	fc := newTestFleetController(t)

	roomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")
	event := makeRelayLinkEvent("tkt-1", holder)

	fc.processRelayLinkEvent(roomID, event)

	key := opsTicketKey{roomID: roomID, ticketID: "tkt-1"}
	relayLink, exists := fc.relayLinks[key]
	if !exists {
		t.Fatal("relay link should be stored")
	}
	if relayLink.Requester != holder {
		t.Errorf("relay link requester = %v, want %v", relayLink.Requester, holder)
	}
	if relayLink.OriginTicket != "origin-tkt-1" {
		t.Errorf("relay link origin ticket = %q, want origin-tkt-1", relayLink.OriginTicket)
	}
}

func TestProcessRelayLinkEventIgnoresEmptyContent(t *testing.T) {
	fc := newTestFleetController(t)

	event := messaging.Event{
		Type:     schema.EventTypeRelayLink,
		StateKey: stringPtr("tkt-1"),
		Content:  map[string]any{},
	}

	fc.processRelayLinkEvent(testOpsRoomID("gpu-box"), event)

	if len(fc.relayLinks) != 0 {
		t.Error("empty content should not create relay link entry")
	}
}

// --- enqueueReservation tests ---

func TestEnqueueReservation(t *testing.T) {
	fc, _ := newReservationTestController(t)

	opsRoomID := testOpsRoomID("gpu-box")
	machineLocalpart := testMachineLocalpart("gpu-box")
	holder := testEntity(t, "agent/builder")

	// Register relay link so holder can be resolved.
	fc.relayLinks[opsTicketKey{roomID: opsRoomID, ticketID: "tkt-1"}] = schema.RelayLink{
		Requester: holder,
	}

	content := &ticket.TicketContent{
		Priority: 2,
		Type:     ticket.TypeResourceRequest,
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

	fc.enqueueReservation(machineLocalpart, opsRoomID, "tkt-1", content, time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC).UnixMilli())

	reservation := fc.reservations[machineLocalpart]
	if reservation == nil {
		t.Fatal("reservation should exist after enqueue")
	}
	if len(reservation.queue) != 1 {
		t.Fatalf("queue length = %d, want 1", len(reservation.queue))
	}

	queued := reservation.queue[0]
	if queued.relayTicketID != "tkt-1" {
		t.Errorf("ticket ID = %q, want tkt-1", queued.relayTicketID)
	}
	if queued.holder != holder {
		t.Errorf("holder = %v, want %v", queued.holder, holder)
	}
	if queued.mode != schema.ModeExclusive {
		t.Errorf("mode = %q, want exclusive", queued.mode)
	}
	if queued.priority != 2 {
		t.Errorf("priority = %d, want 2", queued.priority)
	}
	if queued.maxDuration != 2*time.Hour {
		t.Errorf("max duration = %v, want 2h", queued.maxDuration)
	}
}

func TestEnqueueReservationSkipsDuplicate(t *testing.T) {
	fc, _ := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")

	content := &ticket.TicketContent{
		Priority: 2,
		Type:     ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
					Mode:     schema.ModeExclusive,
					Status:   schema.ClaimPending,
				},
			},
			MaxDuration: "1h",
		},
	}

	timestamp := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
	fc.enqueueReservation(machineLocalpart, opsRoomID, "tkt-1", content, timestamp)
	fc.enqueueReservation(machineLocalpart, opsRoomID, "tkt-1", content, timestamp)

	if len(fc.reservations[machineLocalpart].queue) != 1 {
		t.Errorf("duplicate enqueue should be ignored, got queue length %d", len(fc.reservations[machineLocalpart].queue))
	}
}

func TestEnqueueReservationSkipsActiveTicket(t *testing.T) {
	fc, _ := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")

	// Pre-populate an active reservation with the same ticket ID.
	fc.reservations[machineLocalpart] = &machineReservation{
		active: &activeReservation{
			relayTicketID: "tkt-1",
			opsRoomID:     opsRoomID,
		},
	}

	content := &ticket.TicketContent{
		Priority: 2,
		Type:     ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
					Mode:     schema.ModeExclusive,
					Status:   schema.ClaimPending,
				},
			},
			MaxDuration: "1h",
		},
	}

	fc.enqueueReservation(machineLocalpart, opsRoomID, "tkt-1", content, 1740826800000) // 2025-03-01T10:00:00Z

	if len(fc.reservations[machineLocalpart].queue) != 0 {
		t.Error("should not enqueue a ticket that is already active")
	}
}

func TestEnqueueReservationRejectsInvalidDuration(t *testing.T) {
	fc, _ := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")

	content := &ticket.TicketContent{
		Priority: 2,
		Type:     ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
					Mode:     schema.ModeExclusive,
					Status:   schema.ClaimPending,
				},
			},
			MaxDuration: "not-a-duration",
		},
	}

	fc.enqueueReservation(machineLocalpart, opsRoomID, "tkt-bad", content, 1740826800000) // 2025-03-01T10:00:00Z

	if _, exists := fc.reservations[machineLocalpart]; exists {
		if len(fc.reservations[machineLocalpart].queue) != 0 {
			t.Error("invalid duration should prevent enqueue")
		}
	}
}

// --- Grant flow tests ---

func TestGrantReservation(t *testing.T) {
	fc, store := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineLocalpart] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineLocalpart

	// Seed a relay ticket in the store so setRelayTicketInProgress
	// can read-modify-write it.
	store.seedState(opsRoomID.String(), "tkt-1", &ticket.TicketContent{
		Version:  5,
		Title:    "Reserve gpu-box",
		Status:   ticket.StatusOpen,
		Priority: 1,
		Type:     ticket.TypeResourceRequest,
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
	})

	reservation := &machineReservation{
		queue: []queuedReservation{
			{
				relayTicketID: "tkt-1",
				opsRoomID:     opsRoomID,
				holder:        holder,
				mode:          schema.ModeExclusive,
				priority:      1,
				maxDuration:   2 * time.Hour,
				createdAt:     time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			},
		},
	}
	fc.reservations[machineLocalpart] = reservation

	fc.grantReservation(context.Background(), machineLocalpart, reservation)

	// Queue should be empty after grant.
	if len(reservation.queue) != 0 {
		t.Errorf("queue should be empty after grant, got %d", len(reservation.queue))
	}

	// Active reservation should be set.
	if reservation.active == nil {
		t.Fatal("active reservation should be set after grant")
	}
	if reservation.active.relayTicketID != "tkt-1" {
		t.Errorf("active ticket = %q, want tkt-1", reservation.active.relayTicketID)
	}
	if reservation.active.holder != holder {
		t.Errorf("active holder = %v, want %v", reservation.active.holder, holder)
	}
	if reservation.active.mode != schema.ModeExclusive {
		t.Errorf("active mode = %q, want exclusive", reservation.active.mode)
	}

	// Check the Matrix writes. We expect:
	// 1. Ticket set to in_progress (read-modify-write)
	// 2. Machine drain published (exclusive mode)
	// 3. Reservation grant published
	if len(store.writes) < 3 {
		t.Fatalf("expected at least 3 writes, got %d", len(store.writes))
	}

	// Verify ticket was set to in_progress.
	ticketWrite := store.writes[0]
	if ticketWrite.EventType != schema.EventTypeTicket {
		t.Errorf("write[0] event type = %q, want m.bureau.ticket", ticketWrite.EventType)
	}
	ticketRaw := store.latestState(opsRoomID.String(), "tkt-1")
	var ticketContent ticket.TicketContent
	if err := json.Unmarshal(ticketRaw, &ticketContent); err != nil {
		t.Fatalf("unmarshal ticket: %v", err)
	}
	if ticketContent.Status != ticket.StatusInProgress {
		t.Errorf("ticket status = %q, want in_progress", ticketContent.Status)
	}

	// Verify drain was published.
	drainWrite := store.writes[1]
	if drainWrite.EventType != schema.EventTypeMachineDrain {
		t.Errorf("write[1] event type = %q, want m.bureau.machine_drain", drainWrite.EventType)
	}

	// Verify reservation grant was published.
	reservationWrite := store.writes[2]
	if reservationWrite.EventType != schema.EventTypeReservation {
		t.Errorf("write[2] event type = %q, want m.bureau.reservation", reservationWrite.EventType)
	}
	if reservationWrite.StateKey != holder.Localpart() {
		t.Errorf("reservation state key = %q, want %q", reservationWrite.StateKey, holder.Localpart())
	}

	// Verify reservation grant content.
	grantRaw := store.latestState(opsRoomID.String(), holder.Localpart())
	var grant schema.ReservationGrant
	if err := json.Unmarshal(grantRaw, &grant); err != nil {
		t.Fatalf("unmarshal grant: %v", err)
	}
	if grant.RelayTicket != "tkt-1" {
		t.Errorf("grant relay ticket = %q, want tkt-1", grant.RelayTicket)
	}
	if grant.Mode != schema.ModeExclusive {
		t.Errorf("grant mode = %q, want exclusive", grant.Mode)
	}
	if grant.Holder != holder {
		t.Errorf("grant holder = %v, want %v", grant.Holder, holder)
	}
	if grant.Resource.Type != schema.ResourceMachine {
		t.Errorf("grant resource type = %q, want machine", grant.Resource.Type)
	}
}

func TestGrantReservationInclusiveNoDrain(t *testing.T) {
	fc, store := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineLocalpart] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineLocalpart

	store.seedState(opsRoomID.String(), "tkt-1", &ticket.TicketContent{
		Version:  5,
		Status:   ticket.StatusOpen,
		Priority: 1,
		Type:     ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
					Mode:     schema.ModeInclusive,
					Status:   schema.ClaimPending,
				},
			},
			MaxDuration: "1h",
		},
	})

	reservation := &machineReservation{
		queue: []queuedReservation{
			{
				relayTicketID: "tkt-1",
				opsRoomID:     opsRoomID,
				holder:        holder,
				mode:          schema.ModeInclusive,
				priority:      1,
				maxDuration:   time.Hour,
				createdAt:     time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			},
		},
	}
	fc.reservations[machineLocalpart] = reservation

	fc.grantReservation(context.Background(), machineLocalpart, reservation)

	// For inclusive mode, we expect only 2 writes: ticket in_progress
	// and reservation grant. No drain.
	drainWritten := false
	for _, write := range store.writes {
		if write.EventType == schema.EventTypeMachineDrain {
			drainWritten = true
		}
	}
	if drainWritten {
		t.Error("inclusive mode should not publish a machine drain")
	}
}

// --- Release tests ---

func TestReleaseReservationOnTicketClosed(t *testing.T) {
	fc, store := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineLocalpart] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineLocalpart

	// Set up an active exclusive reservation.
	fc.reservations[machineLocalpart] = &machineReservation{
		active: &activeReservation{
			relayTicketID: "tkt-1",
			opsRoomID:     opsRoomID,
			holder:        holder,
			mode:          schema.ModeExclusive,
			grantedAt:     time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			expiresAt:     time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
			priority:      1,
		},
	}

	// Simulate ticket closure.
	fc.handleRelayTicketClosed(context.Background(), machineLocalpart, opsRoomID, "tkt-1", "Work completed")

	reservation := fc.reservations[machineLocalpart]
	if reservation.active != nil {
		t.Error("active reservation should be nil after release")
	}

	// Verify state events were cleared (empty content written for
	// reservation and drain).
	reservationCleared := false
	drainCleared := false
	for _, write := range store.writes {
		if write.EventType == schema.EventTypeReservation {
			reservationCleared = true
		}
		if write.EventType == schema.EventTypeMachineDrain {
			drainCleared = true
		}
	}
	if !reservationCleared {
		t.Error("reservation state event should be cleared on release")
	}
	if !drainCleared {
		t.Error("drain state event should be cleared on exclusive release")
	}
}

func TestReleaseReservationAdvancesQueue(t *testing.T) {
	fc, store := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder1 := testEntity(t, "agent/builder")
	holder2 := testEntity(t, "agent/tester")

	fc.opsRooms[machineLocalpart] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineLocalpart

	// Seed the next ticket for the grant flow.
	store.seedState(opsRoomID.String(), "tkt-2", &ticket.TicketContent{
		Version:  5,
		Status:   ticket.StatusOpen,
		Priority: 2,
		Type:     ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
					Mode:     schema.ModeInclusive,
					Status:   schema.ClaimPending,
				},
			},
			MaxDuration: "1h",
		},
	})

	fc.reservations[machineLocalpart] = &machineReservation{
		active: &activeReservation{
			relayTicketID: "tkt-1",
			opsRoomID:     opsRoomID,
			holder:        holder1,
			mode:          schema.ModeInclusive,
			grantedAt:     time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			expiresAt:     time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
			priority:      1,
		},
		queue: []queuedReservation{
			{
				relayTicketID: "tkt-2",
				opsRoomID:     opsRoomID,
				holder:        holder2,
				mode:          schema.ModeInclusive,
				priority:      2,
				maxDuration:   time.Hour,
				createdAt:     time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC),
			},
		},
	}

	fc.handleRelayTicketClosed(context.Background(), machineLocalpart, opsRoomID, "tkt-1", "Done")

	reservation := fc.reservations[machineLocalpart]
	if reservation.active == nil {
		t.Fatal("next queued reservation should be granted")
	}
	if reservation.active.relayTicketID != "tkt-2" {
		t.Errorf("active ticket = %q, want tkt-2", reservation.active.relayTicketID)
	}
	if reservation.active.holder != holder2 {
		t.Errorf("active holder = %v, want %v", reservation.active.holder, holder2)
	}
}

func TestHandleRelayTicketClosedRemovesFromQueue(t *testing.T) {
	fc, _ := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")

	fc.reservations[machineLocalpart] = &machineReservation{
		queue: []queuedReservation{
			{relayTicketID: "tkt-1", opsRoomID: opsRoomID},
			{relayTicketID: "tkt-2", opsRoomID: opsRoomID},
			{relayTicketID: "tkt-3", opsRoomID: opsRoomID},
		},
	}

	fc.handleRelayTicketClosed(context.Background(), machineLocalpart, opsRoomID, "tkt-2", "Cancelled")

	queue := fc.reservations[machineLocalpart].queue
	if len(queue) != 2 {
		t.Fatalf("queue length = %d, want 2", len(queue))
	}
	for _, entry := range queue {
		if entry.relayTicketID == "tkt-2" {
			t.Error("tkt-2 should have been removed from queue")
		}
	}
}

// --- Preemption tests ---

func TestPreemptReservation(t *testing.T) {
	fc, store := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder1 := testEntity(t, "agent/builder")
	holder2 := testEntity(t, "agent/critical-task")

	fc.opsRooms[machineLocalpart] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineLocalpart

	// Seed the active ticket (for closure read-modify-write).
	store.seedState(opsRoomID.String(), "tkt-low", &ticket.TicketContent{
		Version:  5,
		Status:   ticket.StatusInProgress,
		Priority: 3,
		Type:     ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
					Mode:     schema.ModeExclusive,
					Status:   schema.ClaimGranted,
				},
			},
			MaxDuration: "2h",
		},
	})

	// Seed the preempting ticket (for grant read-modify-write).
	store.seedState(opsRoomID.String(), "tkt-high", &ticket.TicketContent{
		Version:  5,
		Status:   ticket.StatusOpen,
		Priority: 0,
		Type:     ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
					Mode:     schema.ModeExclusive,
					Status:   schema.ClaimPending,
				},
			},
			MaxDuration: "1h",
		},
	})

	reservation := &machineReservation{
		active: &activeReservation{
			relayTicketID: "tkt-low",
			opsRoomID:     opsRoomID,
			holder:        holder1,
			mode:          schema.ModeExclusive,
			grantedAt:     time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			expiresAt:     time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
			priority:      3,
		},
		queue: []queuedReservation{
			{
				relayTicketID: "tkt-high",
				opsRoomID:     opsRoomID,
				holder:        holder2,
				mode:          schema.ModeExclusive,
				priority:      0,
				maxDuration:   time.Hour,
				createdAt:     time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC),
			},
		},
	}
	fc.reservations[machineLocalpart] = reservation

	fc.preemptReservation(context.Background(), machineLocalpart, reservation)

	// The preempted ticket should have been closed.
	closedTicketRaw := store.latestState(opsRoomID.String(), "tkt-low")
	var closedTicket ticket.TicketContent
	if err := json.Unmarshal(closedTicketRaw, &closedTicket); err != nil {
		t.Fatalf("unmarshal closed ticket: %v", err)
	}
	if closedTicket.Status != ticket.StatusClosed {
		t.Errorf("preempted ticket status = %q, want closed", closedTicket.Status)
	}
	if closedTicket.CloseReason != "Preempted by higher priority request" {
		t.Errorf("close reason = %q, want 'Preempted by higher priority request'", closedTicket.CloseReason)
	}

	// The high priority ticket should now be active.
	if reservation.active == nil {
		t.Fatal("active reservation should be set after preemption")
	}
	if reservation.active.relayTicketID != "tkt-high" {
		t.Errorf("active ticket = %q, want tkt-high", reservation.active.relayTicketID)
	}
	if reservation.active.holder != holder2 {
		t.Errorf("active holder = %v, want %v", reservation.active.holder, holder2)
	}
}

func TestAdvanceReservationQueuesPreempts(t *testing.T) {
	fc, store := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder1 := testEntity(t, "agent/builder")
	holder2 := testEntity(t, "agent/critical")

	fc.opsRooms[machineLocalpart] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineLocalpart

	store.seedState(opsRoomID.String(), "tkt-low", &ticket.TicketContent{
		Version:  5,
		Status:   ticket.StatusInProgress,
		Priority: 4,
		Type:     ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
					Mode:     schema.ModeInclusive,
					Status:   schema.ClaimGranted,
				},
			},
			MaxDuration: "2h",
		},
	})
	store.seedState(opsRoomID.String(), "tkt-high", &ticket.TicketContent{
		Version:  5,
		Status:   ticket.StatusOpen,
		Priority: 1,
		Type:     ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
					Mode:     schema.ModeInclusive,
					Status:   schema.ClaimPending,
				},
			},
			MaxDuration: "1h",
		},
	})

	fc.reservations[machineLocalpart] = &machineReservation{
		active: &activeReservation{
			relayTicketID: "tkt-low",
			opsRoomID:     opsRoomID,
			holder:        holder1,
			mode:          schema.ModeInclusive,
			grantedAt:     time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			expiresAt:     time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
			priority:      4,
		},
		queue: []queuedReservation{
			{
				relayTicketID: "tkt-high",
				opsRoomID:     opsRoomID,
				holder:        holder2,
				mode:          schema.ModeInclusive,
				priority:      1,
				maxDuration:   time.Hour,
				createdAt:     time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC),
			},
		},
	}

	fc.advanceReservationQueues(context.Background())

	reservation := fc.reservations[machineLocalpart]
	if reservation.active == nil {
		t.Fatal("active reservation should exist after preemption")
	}
	if reservation.active.relayTicketID != "tkt-high" {
		t.Errorf("active ticket = %q, want tkt-high (preempted tkt-low)", reservation.active.relayTicketID)
	}
}

func TestAdvanceReservationQueuesNoPreemptSamePriority(t *testing.T) {
	fc, _ := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder1 := testEntity(t, "agent/builder")
	holder2 := testEntity(t, "agent/also-builder")

	fc.reservations[machineLocalpart] = &machineReservation{
		active: &activeReservation{
			relayTicketID: "tkt-1",
			opsRoomID:     opsRoomID,
			holder:        holder1,
			mode:          schema.ModeInclusive,
			grantedAt:     time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			expiresAt:     time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
			priority:      2,
		},
		queue: []queuedReservation{
			{
				relayTicketID: "tkt-2",
				opsRoomID:     opsRoomID,
				holder:        holder2,
				mode:          schema.ModeInclusive,
				priority:      2,
				maxDuration:   time.Hour,
			},
		},
	}

	fc.advanceReservationQueues(context.Background())

	// Same priority should not preempt.
	if fc.reservations[machineLocalpart].active.relayTicketID != "tkt-1" {
		t.Error("same priority should not trigger preemption")
	}
}

func TestAdvanceReservationQueuesGrantsWhenNoActive(t *testing.T) {
	fc, store := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineLocalpart] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineLocalpart

	store.seedState(opsRoomID.String(), "tkt-1", &ticket.TicketContent{
		Version:  5,
		Status:   ticket.StatusOpen,
		Priority: 2,
		Type:     ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
					Mode:     schema.ModeInclusive,
					Status:   schema.ClaimPending,
				},
			},
			MaxDuration: "1h",
		},
	})

	fc.reservations[machineLocalpart] = &machineReservation{
		queue: []queuedReservation{
			{
				relayTicketID: "tkt-1",
				opsRoomID:     opsRoomID,
				holder:        holder,
				mode:          schema.ModeInclusive,
				priority:      2,
				maxDuration:   time.Hour,
				createdAt:     time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			},
		},
	}

	fc.advanceReservationQueues(context.Background())

	reservation := fc.reservations[machineLocalpart]
	if reservation.active == nil {
		t.Fatal("queued reservation should be granted when no active")
	}
	if reservation.active.relayTicketID != "tkt-1" {
		t.Errorf("active ticket = %q, want tkt-1", reservation.active.relayTicketID)
	}
}

// --- Duration watchdog tests ---

func TestCheckReservationExpiry(t *testing.T) {
	fc, store := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineLocalpart] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineLocalpart

	// Seed the ticket for closure.
	store.seedState(opsRoomID.String(), "tkt-1", &ticket.TicketContent{
		Version:  5,
		Status:   ticket.StatusInProgress,
		Priority: 1,
		Type:     ticket.TypeResourceRequest,
		Reservation: &ticket.ReservationContent{
			Claims: []ticket.ResourceClaim{
				{
					Resource: schema.ResourceRef{Type: schema.ResourceMachine, Target: "gpu-box"},
					Mode:     schema.ModeExclusive,
					Status:   schema.ClaimGranted,
				},
			},
			MaxDuration: "1h",
		},
	})

	fc.reservations[machineLocalpart] = &machineReservation{
		active: &activeReservation{
			relayTicketID: "tkt-1",
			opsRoomID:     opsRoomID,
			holder:        holder,
			mode:          schema.ModeExclusive,
			grantedAt:     time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			expiresAt:     time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC),
			priority:      1,
		},
	}

	// Clock is at 12:00 — well past the 11:00 expiry.
	fc.checkReservationExpiry(context.Background())

	// Ticket should be closed with "Duration exceeded".
	ticketRaw := store.latestState(opsRoomID.String(), "tkt-1")
	var ticketContent ticket.TicketContent
	if err := json.Unmarshal(ticketRaw, &ticketContent); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ticketContent.Status != ticket.StatusClosed {
		t.Errorf("ticket status = %q, want closed", ticketContent.Status)
	}
	if ticketContent.CloseReason != "Duration exceeded" {
		t.Errorf("close reason = %q, want 'Duration exceeded'", ticketContent.CloseReason)
	}

	// Active reservation should be cleared.
	if fc.reservations[machineLocalpart].active != nil {
		t.Error("active reservation should be nil after expiry")
	}
}

func TestCheckReservationExpiryNotYetExpired(t *testing.T) {
	fc, store := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineLocalpart] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineLocalpart

	fc.reservations[machineLocalpart] = &machineReservation{
		active: &activeReservation{
			relayTicketID: "tkt-1",
			opsRoomID:     opsRoomID,
			holder:        holder,
			mode:          schema.ModeExclusive,
			grantedAt:     time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			// Expires at 14:00 — clock is at 12:00.
			expiresAt: time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC),
			priority:  1,
		},
	}

	fc.checkReservationExpiry(context.Background())

	// No writes — not yet expired.
	if len(store.writes) != 0 {
		t.Errorf("expected 0 writes for non-expired reservation, got %d", len(store.writes))
	}
	if fc.reservations[machineLocalpart].active == nil {
		t.Error("active reservation should still be present")
	}
}

// --- processOpsRoomTicketEvent integration tests ---

func TestProcessOpsRoomTicketEventClassifiesAndEnqueues(t *testing.T) {
	fc, _ := newReservationTestController(t)

	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	// Register relay link first so holder is resolved.
	fc.relayLinks[opsTicketKey{roomID: opsRoomID, ticketID: "tkt-1"}] = schema.RelayLink{
		Requester: holder,
	}

	event := makeRelayTicketEvent("tkt-1", "gpu-box", ticket.StatusOpen, 2, "2h", schema.ModeExclusive,
		time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC).UnixMilli())

	fc.processOpsRoomTicketEvent(context.Background(), opsRoomID, event)

	// Room should be classified as ops room.
	machineLocalpart := testMachineLocalpart("gpu-box")
	if fc.opsRooms[machineLocalpart] != opsRoomID {
		t.Error("room should be classified as ops room")
	}

	// Ticket should be enqueued.
	reservation := fc.reservations[machineLocalpart]
	if reservation == nil {
		t.Fatal("reservation should exist")
	}
	if len(reservation.queue) != 1 {
		t.Fatalf("queue length = %d, want 1", len(reservation.queue))
	}
	if reservation.queue[0].relayTicketID != "tkt-1" {
		t.Errorf("queued ticket = %q, want tkt-1", reservation.queue[0].relayTicketID)
	}
}

func TestProcessOpsRoomTicketEventClosedReleasesActive(t *testing.T) {
	fc, store := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineLocalpart] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineLocalpart
	fc.reservations[machineLocalpart] = &machineReservation{
		active: &activeReservation{
			relayTicketID: "tkt-1",
			opsRoomID:     opsRoomID,
			holder:        holder,
			mode:          schema.ModeInclusive,
			grantedAt:     time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			expiresAt:     time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
			priority:      2,
		},
	}

	event := makeRelayTicketEvent("tkt-1", "gpu-box", ticket.StatusClosed, 2, "2h", schema.ModeInclusive,
		time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC).UnixMilli())
	// Manually add close_reason to content.
	event.Content["close_reason"] = "Pipeline completed"

	fc.processOpsRoomTicketEvent(context.Background(), opsRoomID, event)

	if fc.reservations[machineLocalpart].active != nil {
		t.Error("active reservation should be released after ticket closure")
	}

	// Verify reservation grant was cleared.
	reservationCleared := false
	for _, write := range store.writes {
		if write.EventType == schema.EventTypeReservation {
			reservationCleared = true
		}
	}
	if !reservationCleared {
		t.Error("reservation state event should be cleared")
	}
}

func TestProcessOpsRoomTicketEventEmptyContentHandled(t *testing.T) {
	fc, _ := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")

	fc.opsRooms[machineLocalpart] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineLocalpart

	fc.reservations[machineLocalpart] = &machineReservation{
		queue: []queuedReservation{
			{relayTicketID: "tkt-1", opsRoomID: opsRoomID},
		},
	}

	// Empty content = state cleared.
	event := messaging.Event{
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  map[string]any{},
	}

	fc.processOpsRoomTicketEvent(context.Background(), opsRoomID, event)

	// Should be removed from queue.
	if len(fc.reservations[machineLocalpart].queue) != 0 {
		t.Error("cleared ticket should be removed from queue")
	}
}

// --- processLeave ops room tests ---

func TestProcessLeaveOpsRoom(t *testing.T) {
	fc := newTestFleetController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")

	fc.opsRooms[machineLocalpart] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineLocalpart
	fc.reservations[machineLocalpart] = &machineReservation{
		active: &activeReservation{relayTicketID: "tkt-1"},
		queue:  []queuedReservation{{relayTicketID: "tkt-2"}},
	}
	fc.relayLinks[opsTicketKey{roomID: opsRoomID, ticketID: "tkt-1"}] = schema.RelayLink{}
	fc.relayLinks[opsTicketKey{roomID: opsRoomID, ticketID: "tkt-2"}] = schema.RelayLink{}
	// Also add a relay link for a different room to verify it's preserved.
	otherRoomID := mustRoomID("!other-ops:local")
	fc.relayLinks[opsTicketKey{roomID: otherRoomID, ticketID: "tkt-3"}] = schema.RelayLink{}

	fc.processLeave(opsRoomID)

	if _, exists := fc.opsRooms[machineLocalpart]; exists {
		t.Error("opsRooms should be cleaned up after leave")
	}
	if _, exists := fc.opsRoomMachines[opsRoomID]; exists {
		t.Error("opsRoomMachines should be cleaned up after leave")
	}
	if _, exists := fc.reservations[machineLocalpart]; exists {
		t.Error("reservations should be cleaned up after leave")
	}

	// Relay links for this room should be gone.
	if _, exists := fc.relayLinks[opsTicketKey{roomID: opsRoomID, ticketID: "tkt-1"}]; exists {
		t.Error("relay link for tkt-1 should be removed")
	}
	if _, exists := fc.relayLinks[opsTicketKey{roomID: opsRoomID, ticketID: "tkt-2"}]; exists {
		t.Error("relay link for tkt-2 should be removed")
	}

	// Relay links for other rooms should be preserved.
	if _, exists := fc.relayLinks[opsTicketKey{roomID: otherRoomID, ticketID: "tkt-3"}]; !exists {
		t.Error("relay link for other room should be preserved")
	}
}

// --- machineResourceRef tests ---

func TestMachineResourceRef(t *testing.T) {
	fc := newTestFleetController(t)

	resourceRef := fc.machineResourceRef(testMachineLocalpart("gpu-box"))
	if resourceRef.Type != schema.ResourceMachine {
		t.Errorf("type = %q, want machine", resourceRef.Type)
	}
	if resourceRef.Target != "gpu-box" {
		t.Errorf("target = %q, want gpu-box", resourceRef.Target)
	}
}

// --- Full sync cycle integration test ---

func TestHandleSyncProcessesReservationLifecycle(t *testing.T) {
	fc, store := newReservationTestController(t)

	machineLocalpart := testMachineLocalpart("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	// Seed the relay ticket for the grant's read-modify-write.
	store.seedState(opsRoomID.String(), "tkt-1", &ticket.TicketContent{
		Version:  5,
		Status:   ticket.StatusOpen,
		Priority: 1,
		Type:     ticket.TypeResourceRequest,
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
	})

	// Simulate a sync batch that delivers both a relay link and a
	// resource_request ticket in the same ops room.
	relayLinkEvent := makeRelayLinkEvent("tkt-1", holder)
	ticketEvent := makeRelayTicketEvent("tkt-1", "gpu-box", ticket.StatusOpen, 1, "2h", schema.ModeExclusive,
		time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC).UnixMilli())

	response := &messaging.SyncResponse{
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				opsRoomID: {
					State: messaging.StateSection{
						Events: []messaging.Event{relayLinkEvent, ticketEvent},
					},
				},
			},
		},
	}

	fc.handleSync(context.Background(), response)

	// After handleSync:
	// 1. First pass indexed the relay link.
	// 2. Second pass processed the ticket, classified the ops room,
	//    and enqueued the reservation.
	// 3. advanceReservationQueues granted the reservation.

	if _, exists := fc.opsRooms[machineLocalpart]; !exists {
		t.Error("ops room should be classified after sync")
	}

	reservation := fc.reservations[machineLocalpart]
	if reservation == nil {
		t.Fatal("reservation should exist after sync")
	}
	if reservation.active == nil {
		t.Fatal("reservation should be granted (queue head with no active)")
	}
	if reservation.active.relayTicketID != "tkt-1" {
		t.Errorf("active ticket = %q, want tkt-1", reservation.active.relayTicketID)
	}
	if reservation.active.holder != holder {
		t.Errorf("active holder = %v, want %v", reservation.active.holder, holder)
	}

	// Verify the ticket was set to in_progress.
	ticketRaw := store.latestState(opsRoomID.String(), "tkt-1")
	var ticketContent ticket.TicketContent
	if err := json.Unmarshal(ticketRaw, &ticketContent); err != nil {
		t.Fatalf("unmarshal ticket: %v", err)
	}
	if ticketContent.Status != ticket.StatusInProgress {
		t.Errorf("ticket status = %q, want in_progress", ticketContent.Status)
	}
}
