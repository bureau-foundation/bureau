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

// testMachineStateKey returns the machine state_key string for a short
// machine name in the standard test fleet. State keys use the
// "localpart:server" format (without '@' prefix) for Matrix room
// version 10+ compatibility.
func testMachineStateKey(name string) string {
	return testMachineUserID(name).StateKey()
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

	machineUserID, classified := fc.classifyOpsRoom(roomID, content)
	if !classified {
		t.Fatal("expected room to be classified as ops room")
	}
	if machineUserID != testMachineUserID("gpu-box") {
		t.Errorf("machine user ID = %v, want %v", machineUserID, testMachineUserID("gpu-box"))
	}

	// Verify the maps were populated.
	if fc.opsRooms[testMachineUserID("gpu-box")] != roomID {
		t.Error("opsRooms should map machine to room ID")
	}
	if fc.opsRoomMachines[roomID] != testMachineUserID("gpu-box") {
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
	machineUserID := testMachineUserID("gpu-box")
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

	fc.enqueueReservation(machineUserID, opsRoomID, "tkt-1", content, time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC).UnixMilli())

	reservation := fc.reservations[machineUserID]
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

	machineUserID := testMachineUserID("gpu-box")
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
	fc.enqueueReservation(machineUserID, opsRoomID, "tkt-1", content, timestamp)
	fc.enqueueReservation(machineUserID, opsRoomID, "tkt-1", content, timestamp)

	if len(fc.reservations[machineUserID].queue) != 1 {
		t.Errorf("duplicate enqueue should be ignored, got queue length %d", len(fc.reservations[machineUserID].queue))
	}
}

func TestEnqueueReservationSkipsActiveTicket(t *testing.T) {
	fc, _ := newReservationTestController(t)

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")

	// Pre-populate an active reservation with the same ticket ID.
	fc.reservations[machineUserID] = &machineReservation{
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

	fc.enqueueReservation(machineUserID, opsRoomID, "tkt-1", content, 1740826800000) // 2025-03-01T10:00:00Z

	if len(fc.reservations[machineUserID].queue) != 0 {
		t.Error("should not enqueue a ticket that is already active")
	}
}

func TestEnqueueReservationRejectsInvalidDuration(t *testing.T) {
	fc, _ := newReservationTestController(t)

	machineUserID := testMachineUserID("gpu-box")
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

	fc.enqueueReservation(machineUserID, opsRoomID, "tkt-bad", content, 1740826800000) // 2025-03-01T10:00:00Z

	if _, exists := fc.reservations[machineUserID]; exists {
		if len(fc.reservations[machineUserID].queue) != 0 {
			t.Error("invalid duration should prevent enqueue")
		}
	}
}

// --- Grant flow tests ---

func TestGrantReservation(t *testing.T) {
	fc, store := newReservationTestController(t)

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID

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
	fc.reservations[machineUserID] = reservation

	fc.startDrain(context.Background(), machineUserID, reservation)

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
	if reservationWrite.StateKey != holder.UserID().StateKey() {
		t.Errorf("reservation state key = %q, want %q", reservationWrite.StateKey, holder.UserID().StateKey())
	}

	// Verify reservation grant content.
	grantRaw := store.latestState(opsRoomID.String(), holder.UserID().StateKey())
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

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID

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
	fc.reservations[machineUserID] = reservation

	fc.startDrain(context.Background(), machineUserID, reservation)

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

// --- Drain-wait tests ---

func TestDrainWaitGrantAfterAcknowledgment(t *testing.T) {
	fc, store := newReservationTestController(t)
	fc.drainGracePeriod = 30 * time.Second

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID

	// Place a fleet-managed service on the machine so the drain
	// has something to wait for. The PrincipalAssignment must have a
	// real Entity so that fleetManagedServicesOnMachine returns the
	// full Matrix localpart (used as the drain_status state_key).
	serviceEntity := testEntity(t, "service/stt/whisper")
	fc.services[testServiceUserID("service/stt/whisper")] = &fleetServiceState{
		instances: map[ref.UserID]*schema.PrincipalAssignment{
			machineUserID: {Principal: serviceEntity},
		},
	}

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
	fc.reservations[machineUserID] = reservation

	fc.startDrain(context.Background(), machineUserID, reservation)

	// Drain should be pending, not yet granted.
	if reservation.pending == nil {
		t.Fatal("expected pending drain after startDrain")
	}
	if reservation.active != nil {
		t.Fatal("reservation should not be active while drain is pending")
	}

	// Verify ticket was set to in_progress and drain was published,
	// but no reservation grant yet.
	if len(store.writes) != 2 {
		t.Fatalf("expected 2 writes (ticket + drain), got %d", len(store.writes))
	}
	if store.writes[0].EventType != schema.EventTypeTicket {
		t.Errorf("write[0] = %s, want m.bureau.ticket", store.writes[0].EventType)
	}
	if store.writes[1].EventType != schema.EventTypeMachineDrain {
		t.Errorf("write[1] = %s, want m.bureau.machine_drain", store.writes[1].EventType)
	}

	// Simulate the service acknowledging the drain with InFlight == 0.
	// The state_key is in "localpart:server" format (without '@' prefix).
	drainStatusEvent := messaging.Event{
		Type:     schema.EventTypeDrainStatus,
		StateKey: stringPtr(serviceEntity.UserID().StateKey()),
		Content: mustContentMap(schema.DrainStatusContent{
			Acknowledged: true,
			InFlight:     0,
			DrainedAt:    "2026-03-01T12:00:05Z",
		}),
	}
	fc.processDrainStatusEvent(opsRoomID, drainStatusEvent)

	// Run checkDrainCompletion — should now complete the grant.
	fc.checkDrainCompletion(context.Background())

	if reservation.pending != nil {
		t.Error("pending drain should be cleared after acknowledgment")
	}
	if reservation.active == nil {
		t.Fatal("reservation should be active after drain completion")
	}
	if reservation.active.relayTicketID != "tkt-1" {
		t.Errorf("active ticket = %q, want tkt-1", reservation.active.relayTicketID)
	}

	// Verify reservation grant was published (third write).
	grantWritten := false
	for _, write := range store.writes {
		if write.EventType == schema.EventTypeReservation {
			grantWritten = true
		}
	}
	if !grantWritten {
		t.Error("reservation grant should have been published after drain completion")
	}
}

func TestDrainWaitTimeoutGrantsAnyway(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	fc, store := newReservationTestController(t)
	fc.clock = fakeClock
	fc.drainGracePeriod = 10 * time.Second

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID

	// Fleet-managed service that will NOT acknowledge.
	serviceEntity := testEntity(t, "service/stt/whisper")
	fc.services[testServiceUserID("service/stt/whisper")] = &fleetServiceState{
		instances: map[ref.UserID]*schema.PrincipalAssignment{
			machineUserID: {Principal: serviceEntity},
		},
	}

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
	fc.reservations[machineUserID] = reservation

	fc.startDrain(context.Background(), machineUserID, reservation)

	// Before timeout: drain is pending.
	if reservation.pending == nil {
		t.Fatal("expected pending drain")
	}

	// Advance clock past grace period but check before that — still pending.
	fakeClock.Advance(5 * time.Second)
	fc.checkDrainCompletion(context.Background())
	if reservation.pending == nil {
		t.Fatal("drain should still be pending before grace period expires")
	}

	// Advance past the grace period.
	fakeClock.Advance(6 * time.Second) // total 11s > 10s grace
	fc.checkDrainCompletion(context.Background())

	if reservation.pending != nil {
		t.Error("pending drain should be cleared after timeout")
	}
	if reservation.active == nil {
		t.Fatal("reservation should be granted after timeout despite no acknowledgment")
	}
}

func TestDrainWaitNoServicesGrantsImmediately(t *testing.T) {
	fc, store := newReservationTestController(t)
	fc.drainGracePeriod = 30 * time.Second

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID

	// No fleet-managed services on this machine.

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
	fc.reservations[machineUserID] = reservation

	fc.startDrain(context.Background(), machineUserID, reservation)

	// With no services to wait for, the drain publishes but
	// checkDrainCompletion should complete immediately since the
	// empty services map is trivially "all drained".
	if reservation.pending == nil {
		// It's also acceptable for startDrain to enter pending state
		// with an empty service map, which checkDrainCompletion will
		// complete on the next cycle.
		if reservation.active == nil {
			t.Fatal("expected either pending or active after startDrain with no services")
		}
		return
	}

	// Pending with empty services — checkDrainCompletion should grant.
	fc.checkDrainCompletion(context.Background())

	if reservation.pending != nil {
		t.Error("pending should be cleared — no services to wait for")
	}
	if reservation.active == nil {
		t.Fatal("reservation should be granted when no services need draining")
	}

	// Verify drain was published even though grant was immediate.
	drainWritten := false
	for _, write := range store.writes {
		if write.EventType == schema.EventTypeMachineDrain {
			drainWritten = true
		}
	}
	if !drainWritten {
		t.Error("exclusive mode should publish drain even when no services to wait for")
	}

	// Verify grant was published.
	grantWritten := false
	for _, write := range store.writes {
		if write.EventType == schema.EventTypeReservation {
			grantWritten = true
		}
	}
	if !grantWritten {
		t.Error("reservation grant should have been published")
	}
}

func TestDrainPreemptionDuringPending(t *testing.T) {
	fc, store := newReservationTestController(t)
	fc.drainGracePeriod = 30 * time.Second

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holderP3 := testEntity(t, "agent/low-priority")
	holderP1 := testEntity(t, "agent/high-priority")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID

	// Fleet-managed service on the machine.
	serviceEntity := testEntity(t, "service/stt/whisper")
	fc.services[testServiceUserID("service/stt/whisper")] = &fleetServiceState{
		instances: map[ref.UserID]*schema.PrincipalAssignment{
			machineUserID: {Principal: serviceEntity},
		},
	}

	// Seed both tickets.
	store.seedState(opsRoomID.String(), "tkt-p3", &ticket.TicketContent{
		Version:  5,
		Status:   ticket.StatusOpen,
		Priority: 3,
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
	store.seedState(opsRoomID.String(), "tkt-p1", &ticket.TicketContent{
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

	// Start with P3 draining.
	reservation := &machineReservation{
		queue: []queuedReservation{
			{
				relayTicketID: "tkt-p3",
				opsRoomID:     opsRoomID,
				holder:        holderP3,
				mode:          schema.ModeExclusive,
				priority:      3,
				maxDuration:   time.Hour,
				createdAt:     time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			},
		},
	}
	fc.reservations[machineUserID] = reservation

	fc.startDrain(context.Background(), machineUserID, reservation)

	if reservation.pending == nil {
		t.Fatal("expected P3 drain to be pending")
	}
	if reservation.pending.queued.relayTicketID != "tkt-p3" {
		t.Errorf("pending ticket = %q, want tkt-p3", reservation.pending.queued.relayTicketID)
	}

	// Now P1 arrives in the queue.
	reservation.queue = append(reservation.queue, queuedReservation{
		relayTicketID: "tkt-p1",
		opsRoomID:     opsRoomID,
		holder:        holderP1,
		mode:          schema.ModeExclusive,
		priority:      1,
		maxDuration:   2 * time.Hour,
		createdAt:     time.Date(2026, 3, 1, 10, 5, 0, 0, time.UTC),
	})

	// advanceReservationQueues should preempt the P3 drain and start P1.
	fc.advanceReservationQueues(context.Background())

	// P3's ticket should be closed.
	closedP3 := false
	for _, write := range store.writes {
		if write.EventType == schema.EventTypeTicket && write.StateKey == "tkt-p3" {
			var content ticket.TicketContent
			raw, _ := json.Marshal(write.Content)
			json.Unmarshal(raw, &content)
			if content.Status == ticket.StatusClosed {
				closedP3 = true
			}
		}
	}
	if !closedP3 {
		t.Error("P3 relay ticket should be closed after preemption")
	}

	// P1 should now be the pending drain.
	if reservation.pending == nil {
		t.Fatal("expected P1 drain to be pending after preemption")
	}
	if reservation.pending.queued.relayTicketID != "tkt-p1" {
		t.Errorf("pending ticket = %q, want tkt-p1", reservation.pending.queued.relayTicketID)
	}
}

func TestDrainServicesListPopulated(t *testing.T) {
	fc, store := newReservationTestController(t)
	fc.drainGracePeriod = 30 * time.Second

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID

	// Two fleet-managed services on this machine.
	whisperEntity := testEntity(t, "service/stt/whisper")
	buildbarnEntity := testEntity(t, "service/buildbarn/worker")
	fc.services[testServiceUserID("service/stt/whisper")] = &fleetServiceState{
		instances: map[ref.UserID]*schema.PrincipalAssignment{
			machineUserID: {Principal: whisperEntity},
		},
	}
	fc.services[testServiceUserID("service/buildbarn/worker")] = &fleetServiceState{
		instances: map[ref.UserID]*schema.PrincipalAssignment{
			machineUserID: {Principal: buildbarnEntity},
		},
	}

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
	fc.reservations[machineUserID] = reservation

	fc.startDrain(context.Background(), machineUserID, reservation)

	// Find the drain write and verify Services is populated.
	for _, write := range store.writes {
		if write.EventType != schema.EventTypeMachineDrain {
			continue
		}
		raw, err := json.Marshal(write.Content)
		if err != nil {
			t.Fatalf("marshal drain content: %v", err)
		}
		var drain schema.MachineDrainContent
		if err := json.Unmarshal(raw, &drain); err != nil {
			t.Fatalf("unmarshal drain content: %v", err)
		}
		if len(drain.Services) != 2 {
			t.Errorf("drain services count = %d, want 2", len(drain.Services))
		}
		// Verify both services are listed (order may vary).
		serviceSet := make(map[ref.UserID]bool)
		for _, service := range drain.Services {
			serviceSet[service] = true
		}
		if !serviceSet[whisperEntity.UserID()] {
			t.Errorf("drain services should include %s", whisperEntity.UserID())
		}
		if !serviceSet[buildbarnEntity.UserID()] {
			t.Errorf("drain services should include %s", buildbarnEntity.UserID())
		}
		return
	}
	t.Error("no machine drain write found")
}

// --- Release tests ---

func TestReleaseReservationOnTicketClosed(t *testing.T) {
	fc, store := newReservationTestController(t)

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID

	// Set up an active exclusive reservation.
	fc.reservations[machineUserID] = &machineReservation{
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
	fc.handleRelayTicketClosed(context.Background(), machineUserID, opsRoomID, "tkt-1", "Work completed")

	reservation := fc.reservations[machineUserID]
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

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder1 := testEntity(t, "agent/builder")
	holder2 := testEntity(t, "agent/tester")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID

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

	fc.reservations[machineUserID] = &machineReservation{
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

	fc.handleRelayTicketClosed(context.Background(), machineUserID, opsRoomID, "tkt-1", "Done")

	reservation := fc.reservations[machineUserID]
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

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")

	fc.reservations[machineUserID] = &machineReservation{
		queue: []queuedReservation{
			{relayTicketID: "tkt-1", opsRoomID: opsRoomID},
			{relayTicketID: "tkt-2", opsRoomID: opsRoomID},
			{relayTicketID: "tkt-3", opsRoomID: opsRoomID},
		},
	}

	fc.handleRelayTicketClosed(context.Background(), machineUserID, opsRoomID, "tkt-2", "Cancelled")

	queue := fc.reservations[machineUserID].queue
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

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder1 := testEntity(t, "agent/builder")
	holder2 := testEntity(t, "agent/critical-task")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID

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
	fc.reservations[machineUserID] = reservation

	fc.preemptReservation(context.Background(), machineUserID, reservation)

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

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder1 := testEntity(t, "agent/builder")
	holder2 := testEntity(t, "agent/critical")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID

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

	fc.reservations[machineUserID] = &machineReservation{
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

	reservation := fc.reservations[machineUserID]
	if reservation.active == nil {
		t.Fatal("active reservation should exist after preemption")
	}
	if reservation.active.relayTicketID != "tkt-high" {
		t.Errorf("active ticket = %q, want tkt-high (preempted tkt-low)", reservation.active.relayTicketID)
	}
}

func TestAdvanceReservationQueuesNoPreemptSamePriority(t *testing.T) {
	fc, _ := newReservationTestController(t)

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder1 := testEntity(t, "agent/builder")
	holder2 := testEntity(t, "agent/also-builder")

	fc.reservations[machineUserID] = &machineReservation{
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
	if fc.reservations[machineUserID].active.relayTicketID != "tkt-1" {
		t.Error("same priority should not trigger preemption")
	}
}

func TestAdvanceReservationQueuesGrantsWhenNoActive(t *testing.T) {
	fc, store := newReservationTestController(t)

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID

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

	fc.reservations[machineUserID] = &machineReservation{
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

	reservation := fc.reservations[machineUserID]
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

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID

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

	fc.reservations[machineUserID] = &machineReservation{
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
	if fc.reservations[machineUserID].active != nil {
		t.Error("active reservation should be nil after expiry")
	}
}

func TestCheckReservationExpiryNotYetExpired(t *testing.T) {
	fc, store := newReservationTestController(t)

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID

	fc.reservations[machineUserID] = &machineReservation{
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
	if fc.reservations[machineUserID].active == nil {
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
	machineUserID := testMachineUserID("gpu-box")
	if fc.opsRooms[machineUserID] != opsRoomID {
		t.Error("room should be classified as ops room")
	}

	// Ticket should be enqueued.
	reservation := fc.reservations[machineUserID]
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

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")
	holder := testEntity(t, "agent/builder")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID
	fc.reservations[machineUserID] = &machineReservation{
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

	if fc.reservations[machineUserID].active != nil {
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

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID

	fc.reservations[machineUserID] = &machineReservation{
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
	if len(fc.reservations[machineUserID].queue) != 0 {
		t.Error("cleared ticket should be removed from queue")
	}
}

// --- processLeave ops room tests ---

func TestProcessLeaveOpsRoom(t *testing.T) {
	fc := newTestFleetController(t)

	machineUserID := testMachineUserID("gpu-box")
	opsRoomID := testOpsRoomID("gpu-box")

	fc.opsRooms[machineUserID] = opsRoomID
	fc.opsRoomMachines[opsRoomID] = machineUserID
	fc.reservations[machineUserID] = &machineReservation{
		active: &activeReservation{relayTicketID: "tkt-1"},
		queue:  []queuedReservation{{relayTicketID: "tkt-2"}},
	}
	fc.relayLinks[opsTicketKey{roomID: opsRoomID, ticketID: "tkt-1"}] = schema.RelayLink{}
	fc.relayLinks[opsTicketKey{roomID: opsRoomID, ticketID: "tkt-2"}] = schema.RelayLink{}
	// Also add a relay link for a different room to verify it's preserved.
	otherRoomID := mustRoomID("!other-ops:local")
	fc.relayLinks[opsTicketKey{roomID: otherRoomID, ticketID: "tkt-3"}] = schema.RelayLink{}

	fc.processLeave(opsRoomID)

	if _, exists := fc.opsRooms[machineUserID]; exists {
		t.Error("opsRooms should be cleaned up after leave")
	}
	if _, exists := fc.opsRoomMachines[opsRoomID]; exists {
		t.Error("opsRoomMachines should be cleaned up after leave")
	}
	if _, exists := fc.reservations[machineUserID]; exists {
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

	resourceRef := fc.machineResourceRef(testMachineUserID("gpu-box"))
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

	machineUserID := testMachineUserID("gpu-box")
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

	if _, exists := fc.opsRooms[machineUserID]; !exists {
		t.Error("ops room should be classified after sync")
	}

	reservation := fc.reservations[machineUserID]
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
