// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

// newSubscriber creates a subscriber for a room with the standard
// buffer size. Returns the subscriber and a cancel function that
// closes the done channel (simulating disconnect).
func newSubscriber(roomID ref.RoomID) (*subscriber, func()) {
	done := make(chan struct{})
	return &subscriber{
		roomID:  roomID,
		channel: make(chan subscribeEvent, subscriberChannelSize),
		done:    done,
	}, func() { close(done) }
}

// receiveEvent reads one event from a subscriber's channel with a
// short timeout. Fails the test if no event arrives.
func receiveEvent(t *testing.T, subscriber *subscriber) subscribeEvent {
	t.Helper()
	return testutil.RequireReceive(t, subscriber.channel, time.Second, "waiting for subscriber event")
}

func TestAddAndRemoveSubscriber(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	subscriber, cancel := newSubscriber(roomID)
	defer cancel()

	ts.addSubscriber(subscriber)

	if length := len(ts.subscribers[roomID]); length != 1 {
		t.Fatalf("expected 1 subscriber, got %d", length)
	}

	ts.removeSubscriber(subscriber)

	if _, exists := ts.subscribers[roomID]; exists {
		t.Fatal("room entry should be deleted after removing last subscriber")
	}
}

func TestRemoveSubscriberPreservesOthers(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	first, cancelFirst := newSubscriber(roomID)
	defer cancelFirst()
	second, cancelSecond := newSubscriber(roomID)
	defer cancelSecond()

	ts.addSubscriber(first)
	ts.addSubscriber(second)

	if length := len(ts.subscribers[roomID]); length != 2 {
		t.Fatalf("expected 2 subscribers, got %d", length)
	}

	ts.removeSubscriber(first)

	if length := len(ts.subscribers[roomID]); length != 1 {
		t.Fatalf("expected 1 subscriber after removal, got %d", length)
	}
	if ts.subscribers[roomID][0] != second {
		t.Fatal("remaining subscriber should be 'second'")
	}
}

func TestNotifySubscribersDeliversEvent(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")
	content := ticket.TicketContent{Version: 1, Title: "test", Status: "open"}

	subscriber, cancel := newSubscriber(roomID)
	defer cancel()

	ts.addSubscriber(subscriber)
	ts.notifySubscribers(roomID, "put", "tkt-1", content)

	event := receiveEvent(t, subscriber)

	if event.Kind != "put" {
		t.Fatalf("event kind should be 'put', got %q", event.Kind)
	}
	if event.TicketID != "tkt-1" {
		t.Fatalf("event ticket ID should be 'tkt-1', got %q", event.TicketID)
	}
	if event.Content.Title != "test" {
		t.Fatalf("event content title should be 'test', got %q", event.Content.Title)
	}
}

func TestNotifySubscribersIsolatesRooms(t *testing.T) {
	ts := newTestService()
	roomA := testRoomID("!room-a:local")
	roomB := testRoomID("!room-b:local")
	content := ticket.TicketContent{Version: 1, Title: "test", Status: "open"}

	subscriberA, cancelA := newSubscriber(roomA)
	defer cancelA()
	subscriberB, cancelB := newSubscriber(roomB)
	defer cancelB()

	ts.addSubscriber(subscriberA)
	ts.addSubscriber(subscriberB)

	// Notify only room A.
	ts.notifySubscribers(roomA, "put", "tkt-1", content)

	// Room A subscriber should have the event.
	event := receiveEvent(t, subscriberA)
	if event.TicketID != "tkt-1" {
		t.Fatalf("room A subscriber should get tkt-1, got %q", event.TicketID)
	}

	// Room B subscriber should have nothing.
	select {
	case event := <-subscriberB.channel:
		t.Fatalf("room B subscriber should not receive events from room A, got %v", event)
	default:
	}
}

func TestNotifySubscribersFullChannelSetsResync(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")
	content := ticket.TicketContent{Version: 1, Title: "test", Status: "open"}

	// Create a subscriber with a tiny buffer so it overflows easily.
	done := make(chan struct{})
	subscriber := &subscriber{
		roomID:  roomID,
		channel: make(chan subscribeEvent, 1),
		done:    done,
	}
	defer close(done)

	ts.addSubscriber(subscriber)

	// Fill the buffer.
	ts.notifySubscribers(roomID, "put", "tkt-1", content)

	// This should overflow and set the resync flag.
	ts.notifySubscribers(roomID, "put", "tkt-2", content)

	if !subscriber.resync.Load() {
		t.Fatal("resync flag should be set after channel overflow")
	}

	// The first event should still be readable.
	event := <-subscriber.channel
	if event.TicketID != "tkt-1" {
		t.Fatalf("buffered event should be tkt-1, got %q", event.TicketID)
	}
}

func TestNotifySubscribersRemovesDisconnected(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")
	content := ticket.TicketContent{Version: 1, Title: "test", Status: "open"}

	// Create a subscriber that's already disconnected.
	done := make(chan struct{})
	close(done)
	disconnected := &subscriber{
		roomID:  roomID,
		channel: make(chan subscribeEvent, subscriberChannelSize),
		done:    done,
	}

	// Create a live subscriber.
	live, cancelLive := newSubscriber(roomID)
	defer cancelLive()

	ts.addSubscriber(disconnected)
	ts.addSubscriber(live)

	// Notify should clean up the disconnected subscriber and deliver
	// to the live one.
	ts.notifySubscribers(roomID, "put", "tkt-1", content)

	if length := len(ts.subscribers[roomID]); length != 1 {
		t.Fatalf("expected 1 subscriber after cleanup, got %d", length)
	}

	event := receiveEvent(t, live)
	if event.TicketID != "tkt-1" {
		t.Fatalf("live subscriber should receive event, got %q", event.TicketID)
	}
}

func TestNotifySubscribersRemovesAllDisconnected(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")
	content := ticket.TicketContent{Version: 1, Title: "test", Status: "open"}

	// Create two disconnected subscribers — no live ones.
	done := make(chan struct{})
	close(done)
	first := &subscriber{
		roomID:  roomID,
		channel: make(chan subscribeEvent, subscriberChannelSize),
		done:    done,
	}
	second := &subscriber{
		roomID:  roomID,
		channel: make(chan subscribeEvent, subscriberChannelSize),
		done:    done,
	}

	ts.addSubscriber(first)
	ts.addSubscriber(second)

	ts.notifySubscribers(roomID, "put", "tkt-1", content)

	// Room entry should be cleaned up entirely.
	if _, exists := ts.subscribers[roomID]; exists {
		t.Fatal("room entry should be deleted after all subscribers disconnect")
	}
}

// TestPutWithEchoNotifiesSubscribers verifies that the putWithEcho
// mutation path dispatches events to registered subscribers.
func TestPutWithEchoNotifiesSubscribers(t *testing.T) {
	writer := &fakeWriterForEchoTest{
		nextEventID: ref.MustParseEventID("$event-1"),
	}
	ts := newTestService()
	ts.writer = writer

	roomID := testRoomID("!room:local")
	state := newTrackedRoom(nil)
	ts.rooms[roomID] = state

	subscriber, cancel := newSubscriber(roomID)
	defer cancel()
	ts.addSubscriber(subscriber)

	content := ticket.TicketContent{
		Version: 1, Title: "test", Status: "open",
		Type: "task", CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	err := ts.putWithEcho(context.Background(), roomID, state, "tkt-1", content)
	if err != nil {
		t.Fatalf("putWithEcho: %v", err)
	}

	event := receiveEvent(t, subscriber)

	if event.Kind != "put" {
		t.Fatalf("event kind should be 'put', got %q", event.Kind)
	}
	if event.TicketID != "tkt-1" {
		t.Fatalf("event ticket ID should be 'tkt-1', got %q", event.TicketID)
	}
	if event.Content.Title != "test" {
		t.Fatalf("event content title should be 'test', got %q", event.Content.Title)
	}
}

// TestIndexTicketEventNotifiesOnPut verifies that indexing a ticket
// event dispatches a put event to subscribers.
func TestIndexTicketEventNotifiesOnPut(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")
	state := newTrackedRoom(nil)
	ts.rooms[roomID] = state

	subscriber, cancel := newSubscriber(roomID)
	defer cancel()
	ts.addSubscriber(subscriber)

	contentMap := toContentMap(t, ticket.TicketContent{
		Version: 1, Title: "synced ticket", Status: "open",
		Type: "task", CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	})

	indexed := ts.indexTicketEvent(roomID, state, messaging.Event{
		EventID:  ref.MustParseEventID("$event-1"),
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  contentMap,
	})
	if !indexed {
		t.Fatal("event should have been indexed")
	}

	event := receiveEvent(t, subscriber)

	if event.Kind != "put" {
		t.Fatalf("event kind should be 'put', got %q", event.Kind)
	}
	if event.TicketID != "tkt-1" {
		t.Fatalf("event ticket ID should be 'tkt-1', got %q", event.TicketID)
	}
	if event.Content.Title != "synced ticket" {
		t.Fatalf("event content title should be 'synced ticket', got %q", event.Content.Title)
	}
}

// TestIndexTicketEventNotifiesOnRemove verifies that indexing a
// redacted (empty content) ticket event dispatches a remove event.
func TestIndexTicketEventNotifiesOnRemove(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")
	state := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {Version: 1, Title: "existing", Status: "open"},
	})
	ts.rooms[roomID] = state

	subscriber, cancel := newSubscriber(roomID)
	defer cancel()
	ts.addSubscriber(subscriber)

	// Empty content = redaction.
	ts.indexTicketEvent(roomID, state, messaging.Event{
		EventID:  ref.MustParseEventID("$redact-1"),
		Type:     schema.EventTypeTicket,
		StateKey: stringPtr("tkt-1"),
		Content:  map[string]any{},
	})

	event := receiveEvent(t, subscriber)

	if event.Kind != "remove" {
		t.Fatalf("event kind should be 'remove', got %q", event.Kind)
	}
	if event.TicketID != "tkt-1" {
		t.Fatalf("event ticket ID should be 'tkt-1', got %q", event.TicketID)
	}

	// Verify the ticket was actually removed from the index.
	if _, exists := state.index.Get("tkt-1"); exists {
		t.Fatal("ticket should have been removed from index")
	}
}

// --- Phase 3: Subscribe stream handler tests ---

// subscribeToken creates a servicetoken.Token with the ticket/subscribe
// grant for use in subscribe handler tests.
func subscribeToken() *servicetoken.Token {
	return &servicetoken.Token{
		Subject: ref.MustParseUserID("@operator:bureau.local"),
		Grants: []servicetoken.Grant{
			{Actions: []string{"ticket/*"}},
		},
	}
}

// subscribeRaw encodes a subscribe request with the given room ID.
func subscribeRaw(t *testing.T, roomID string) []byte {
	t.Helper()
	raw, err := codec.Marshal(map[string]string{
		"action": "subscribe",
		"room":   roomID,
	})
	if err != nil {
		t.Fatalf("marshal subscribe request: %v", err)
	}
	return raw
}

// readFrame reads one CBOR subscribeFrame from a decoder. Fails the
// test on timeout or decode error.
func readFrame(t *testing.T, decoder *codec.Decoder) subscribeFrame {
	t.Helper()
	type result struct {
		frame subscribeFrame
		err   error
	}
	channel := make(chan result, 1)
	go func() {
		var frame subscribeFrame
		err := decoder.Decode(&frame)
		channel <- result{frame, err}
	}()
	select {
	case r := <-channel:
		if r.err != nil {
			t.Fatalf("decode frame: %v", r.err)
		}
		return r.frame
	case <-time.After(5 * time.Second): //nolint:realclock safety valve for blocking CBOR decode on net.Pipe
		t.Fatal("timed out reading frame")
		return subscribeFrame{} // unreachable
	}
}

// readFrames reads frames from a decoder until one matches the given
// type, collecting all intermediate frames. Returns the collected
// frames and the matching frame. Fails if the deadline is exceeded.
func readFramesUntil(t *testing.T, decoder *codec.Decoder, frameType string) (collected []subscribeFrame, target subscribeFrame) {
	t.Helper()
	deadline := time.After(5 * time.Second) //nolint:realclock safety valve for blocking CBOR decode on net.Pipe
	for {
		type result struct {
			frame subscribeFrame
			err   error
		}
		channel := make(chan result, 1)
		go func() {
			var frame subscribeFrame
			err := decoder.Decode(&frame)
			channel <- result{frame, err}
		}()
		select {
		case r := <-channel:
			if r.err != nil {
				t.Fatalf("decode frame while waiting for %q: %v", frameType, r.err)
			}
			if r.frame.Type == frameType {
				return collected, r.frame
			}
			collected = append(collected, r.frame)
		case <-deadline:
			t.Fatalf("timed out waiting for %q frame", frameType)
			return nil, subscribeFrame{} // unreachable
		}
	}
}

// TestSubscribeHandlerPhasedSnapshot verifies the phased snapshot
// ordering: closed deps → open tickets → open_complete → remaining
// closed → caught_up.
func TestSubscribeHandlerPhasedSnapshot(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	// Set up a room with:
	//   tkt-open-1: open, blocked by tkt-closed-dep
	//   tkt-open-2: open, no blockers
	//   tkt-closed-dep: closed (dependency of tkt-open-1)
	//   tkt-closed-old: closed (no open tickets depend on it)
	state := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-open-1": {
			Version: 1, Title: "open blocked", Status: "open",
			Type: "task", BlockedBy: []string{"tkt-closed-dep"},
			CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
		},
		"tkt-open-2": {
			Version: 1, Title: "open unblocked", Status: "open",
			Type:      "task",
			CreatedAt: "2026-01-02T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z",
		},
		"tkt-closed-dep": {
			Version: 1, Title: "closed dep", Status: "closed",
			Type:      "task",
			CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-03T00:00:00Z",
			ClosedAt: "2026-01-03T00:00:00Z",
		},
		"tkt-closed-old": {
			Version: 1, Title: "closed old", Status: "closed",
			Type:      "task",
			CreatedAt: "2025-12-01T00:00:00Z", UpdatedAt: "2025-12-15T00:00:00Z",
			ClosedAt: "2025-12-15T00:00:00Z",
		},
	})
	ts.rooms[roomID] = state

	// Set up the connection.
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		ts.handleSubscribe(ctx, subscribeToken(), subscribeRaw(t, roomID.String()), serverConn)
		serverConn.Close()
	}()

	decoder := codec.NewDecoder(clientConn)

	// Read frames until open_complete.
	priorFrames, openComplete := readFramesUntil(t, decoder, "open_complete")

	// Verify open_complete has stats.
	if openComplete.Stats == nil {
		t.Fatal("open_complete frame should have stats")
	}

	// Separate prior frames into closed deps and open tickets.
	// Closed deps must come before open tickets.
	var closedDepIDs []string
	var openTicketIDs []string
	seenOpenComplete := false
	for _, frame := range priorFrames {
		if frame.Type != "put" {
			t.Fatalf("unexpected frame type before open_complete: %q", frame.Type)
		}
		if frame.Content.Status == "closed" {
			if len(openTicketIDs) > 0 {
				t.Fatal("closed dep arrived after open ticket — closed deps must come first")
			}
			closedDepIDs = append(closedDepIDs, frame.TicketID)
		} else {
			openTicketIDs = append(openTicketIDs, frame.TicketID)
		}
	}
	_ = seenOpenComplete

	// Verify closed deps: only tkt-closed-dep should be here (it's a
	// direct dep of tkt-open-1). tkt-closed-old is not a dep.
	if len(closedDepIDs) != 1 || closedDepIDs[0] != "tkt-closed-dep" {
		t.Fatalf("expected closed deps = [tkt-closed-dep], got %v", closedDepIDs)
	}

	// Verify open tickets: should have both open tickets.
	if len(openTicketIDs) != 2 {
		t.Fatalf("expected 2 open tickets, got %d: %v", len(openTicketIDs), openTicketIDs)
	}

	// Read frames until caught_up.
	remainingFrames, caughtUp := readFramesUntil(t, decoder, "caught_up")

	// Verify caught_up has stats.
	if caughtUp.Stats == nil {
		t.Fatal("caught_up frame should have stats")
	}

	// Remaining closed tickets (after open_complete, before caught_up).
	var remainingClosedIDs []string
	for _, frame := range remainingFrames {
		if frame.Type != "put" {
			t.Fatalf("unexpected frame type between open_complete and caught_up: %q", frame.Type)
		}
		if frame.Content.Status != "closed" {
			t.Fatalf("expected only closed tickets after open_complete, got status %q", frame.Content.Status)
		}
		remainingClosedIDs = append(remainingClosedIDs, frame.TicketID)
	}

	// Only tkt-closed-old should be in the remaining closed section.
	if len(remainingClosedIDs) != 1 || remainingClosedIDs[0] != "tkt-closed-old" {
		t.Fatalf("expected remaining closed = [tkt-closed-old], got %v", remainingClosedIDs)
	}

	// Clean up.
	cancel()
	<-handlerDone
}

// TestSubscribeHandlerLiveEvents verifies that events arrive after
// the caught_up frame.
func TestSubscribeHandlerLiveEvents(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	state := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {Version: 1, Title: "existing", Status: "open", Type: "task",
			CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
	})
	ts.rooms[roomID] = state

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		ts.handleSubscribe(ctx, subscribeToken(), subscribeRaw(t, roomID.String()), serverConn)
		serverConn.Close()
	}()

	decoder := codec.NewDecoder(clientConn)

	// Skip to caught_up.
	readFramesUntil(t, decoder, "caught_up")

	// Now simulate a live mutation. The handler is in the event loop.
	// We need to dispatch an event to the subscriber. The subscriber
	// was registered by handleSubscribe, so notifySubscribers will
	// reach it.
	newContent := ticket.TicketContent{
		Version: 1, Title: "updated", Status: "in_progress", Type: "task",
		Assignee:  ref.MustParseUserID("@alice:bureau.local"),
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z",
	}
	ts.mu.Lock()
	ts.notifySubscribers(roomID, "put", "tkt-1", newContent)
	ts.mu.Unlock()

	// Read the live event.
	frame := readFrame(t, decoder)

	if frame.Type != "put" {
		t.Fatalf("expected put frame, got %q", frame.Type)
	}
	if frame.TicketID != "tkt-1" {
		t.Fatalf("expected ticket ID tkt-1, got %q", frame.TicketID)
	}
	if frame.Content.Status != "in_progress" {
		t.Fatalf("expected status in_progress, got %q", frame.Content.Status)
	}

	cancel()
	<-handlerDone
}

// TestSubscribeHandlerErrorOnMissingGrant verifies that a token
// without the ticket/subscribe grant receives an error frame.
func TestSubscribeHandlerErrorOnMissingGrant(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")
	ts.rooms[roomID] = newTrackedRoom(nil)

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	// Token with no grants.
	token := &servicetoken.Token{
		Subject: ref.MustParseUserID("@operator:bureau.local"),
	}

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		ts.handleSubscribe(context.Background(), token, subscribeRaw(t, roomID.String()), serverConn)
		serverConn.Close()
	}()

	decoder := codec.NewDecoder(clientConn)
	frame := readFrame(t, decoder)

	if frame.Type != "error" {
		t.Fatalf("expected error frame, got %q", frame.Type)
	}
	if frame.Message == "" {
		t.Fatal("error frame should have a message")
	}

	<-handlerDone
}

// TestSubscribeHandlerErrorOnInvalidRoom verifies that subscribing
// to an untracked room receives an error frame.
func TestSubscribeHandlerErrorOnInvalidRoom(t *testing.T) {
	ts := newTestService()

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		ts.handleSubscribe(context.Background(), subscribeToken(), subscribeRaw(t, "!nonexistent:local"), serverConn)
		serverConn.Close()
	}()

	decoder := codec.NewDecoder(clientConn)
	frame := readFrame(t, decoder)

	if frame.Type != "error" {
		t.Fatalf("expected error frame, got %q", frame.Type)
	}

	<-handlerDone
}

// TestSubscribeHandlerResync verifies that when the subscriber channel
// overflows, the handler sends a resync frame followed by a fresh
// snapshot.
func TestSubscribeHandlerResync(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")

	state := newTrackedRoom(map[string]ticket.TicketContent{
		"tkt-1": {Version: 1, Title: "ticket one", Status: "open", Type: "task",
			CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
	})
	ts.rooms[roomID] = state

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		ts.handleSubscribe(ctx, subscribeToken(), subscribeRaw(t, roomID.String()), serverConn)
		serverConn.Close()
	}()

	decoder := codec.NewDecoder(clientConn)

	// Skip initial snapshot to caught_up.
	readFramesUntil(t, decoder, "caught_up")

	// Force a resync by finding the subscriber and setting its flag +
	// sending it an event so the event loop wakes up.
	ts.mu.Lock()
	subscribers := ts.subscribers[roomID]
	if len(subscribers) != 1 {
		ts.mu.Unlock()
		t.Fatalf("expected 1 subscriber, got %d", len(subscribers))
	}
	sub := subscribers[0]
	sub.resync.Store(true)

	// Add a new ticket to the index so the resync snapshot differs.
	state.index.Put("tkt-2", ticket.TicketContent{
		Version: 1, Title: "ticket two", Status: "open", Type: "task",
		CreatedAt: "2026-01-02T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z",
	})

	// Send an event to wake the event loop. The handler will check
	// the resync flag and initiate a re-snapshot.
	sub.channel <- subscribeEvent{Kind: "put", TicketID: "tkt-2",
		Content: ticket.TicketContent{Version: 1, Title: "ticket two", Status: "open"}}
	ts.mu.Unlock()

	// Read frames: expect resync, then a fresh snapshot.
	frame := readFrame(t, decoder)
	if frame.Type != "resync" {
		t.Fatalf("expected resync frame, got %q", frame.Type)
	}

	// Read the fresh snapshot until caught_up.
	putFrames, caughtUp := readFramesUntil(t, decoder, "caught_up")

	// The fresh snapshot should include both tkt-1 and tkt-2.
	ticketIDs := make(map[string]bool)
	for _, frame := range putFrames {
		if frame.Type == "put" {
			ticketIDs[frame.TicketID] = true
		}
	}
	if !ticketIDs["tkt-1"] || !ticketIDs["tkt-2"] {
		t.Fatalf("resync snapshot should contain both tickets, got %v", ticketIDs)
	}
	if caughtUp.Stats == nil {
		t.Fatal("caught_up after resync should have stats")
	}

	cancel()
	<-handlerDone
}

// TestSubscribeHandlerCleanup verifies that the subscriber is removed
// from the registry when the handler exits.
func TestSubscribeHandlerCleanup(t *testing.T) {
	ts := newTestService()
	roomID := testRoomID("!room:local")
	ts.rooms[roomID] = newTrackedRoom(nil)

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	ctx, cancel := context.WithCancel(context.Background())

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		ts.handleSubscribe(ctx, subscribeToken(), subscribeRaw(t, roomID.String()), serverConn)
		serverConn.Close()
	}()

	decoder := codec.NewDecoder(clientConn)
	readFramesUntil(t, decoder, "caught_up")

	// Verify subscriber is registered.
	ts.mu.RLock()
	subscriberCount := len(ts.subscribers[roomID])
	ts.mu.RUnlock()
	if subscriberCount != 1 {
		t.Fatalf("expected 1 subscriber while connected, got %d", subscriberCount)
	}

	// Cancel context to trigger shutdown.
	cancel()
	<-handlerDone

	// Verify subscriber was cleaned up.
	ts.mu.RLock()
	_, exists := ts.subscribers[roomID]
	ts.mu.RUnlock()
	if exists {
		t.Fatal("subscriber should be removed after handler exits")
	}
}

// TestCollectSnapshotPartitioning verifies the snapshot partitioning
// logic independently of the stream handler.
func TestCollectSnapshotPartitioning(t *testing.T) {
	state := newTrackedRoom(map[string]ticket.TicketContent{
		"epic-1": {
			Version: 1, Title: "epic", Status: "open", Type: "epic",
			CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
			BlockedBy: []string{"dep-a", "dep-b"},
		},
		"task-1": {
			Version: 1, Title: "task", Status: "in_progress", Type: "task",
			CreatedAt: "2026-01-02T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z",
		},
		"dep-a": {
			Version: 1, Title: "dep a", Status: "closed", Type: "task",
			CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-03T00:00:00Z",
			ClosedAt: "2026-01-03T00:00:00Z",
		},
		"dep-b": {
			Version: 1, Title: "dep b", Status: "closed", Type: "task",
			CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-04T00:00:00Z",
			ClosedAt: "2026-01-04T00:00:00Z",
		},
		"old-closed-1": {
			Version: 1, Title: "old closed 1", Status: "closed", Type: "task",
			CreatedAt: "2025-12-01T00:00:00Z", UpdatedAt: "2025-12-10T00:00:00Z",
			ClosedAt: "2025-12-10T00:00:00Z",
		},
		"old-closed-2": {
			Version: 1, Title: "old closed 2", Status: "closed", Type: "task",
			CreatedAt: "2025-12-01T00:00:00Z", UpdatedAt: "2025-12-20T00:00:00Z",
			ClosedAt: "2025-12-20T00:00:00Z",
		},
	})

	snapshot := collectSnapshot(state)

	// Closed deps: dep-a and dep-b (blocked by epic-1).
	closedDepIDs := make(map[string]bool)
	for _, entry := range snapshot.closedDeps {
		closedDepIDs[entry.ID] = true
	}
	if !closedDepIDs["dep-a"] || !closedDepIDs["dep-b"] || len(closedDepIDs) != 2 {
		t.Fatalf("expected closed deps {dep-a, dep-b}, got %v", closedDepIDs)
	}

	// Open tickets: epic-1 and task-1.
	openIDs := make(map[string]bool)
	for _, entry := range snapshot.openTickets {
		openIDs[entry.ID] = true
	}
	if !openIDs["epic-1"] || !openIDs["task-1"] || len(openIDs) != 2 {
		t.Fatalf("expected open tickets {epic-1, task-1}, got %v", openIDs)
	}

	// Remaining closed: old-closed-1 and old-closed-2, sorted most
	// recently closed first.
	if len(snapshot.remainingClosed) != 2 {
		t.Fatalf("expected 2 remaining closed, got %d", len(snapshot.remainingClosed))
	}
	if snapshot.remainingClosed[0].ID != "old-closed-2" {
		t.Fatalf("expected old-closed-2 first (more recent), got %q", snapshot.remainingClosed[0].ID)
	}
	if snapshot.remainingClosed[1].ID != "old-closed-1" {
		t.Fatalf("expected old-closed-1 second (older), got %q", snapshot.remainingClosed[1].ID)
	}

	// Stats should reflect all 6 tickets.
	if snapshot.stats.Total != 6 {
		t.Fatalf("expected 6 total tickets in stats, got %d", snapshot.stats.Total)
	}
}

// TestCollectSnapshotEmptyRoom verifies snapshot collection on an
// empty room produces valid (but empty) output.
func TestCollectSnapshotEmptyRoom(t *testing.T) {
	state := newTrackedRoom(nil)
	snapshot := collectSnapshot(state)

	if len(snapshot.closedDeps) != 0 {
		t.Fatalf("expected no closed deps, got %d", len(snapshot.closedDeps))
	}
	if len(snapshot.openTickets) != 0 {
		t.Fatalf("expected no open tickets, got %d", len(snapshot.openTickets))
	}
	if len(snapshot.remainingClosed) != 0 {
		t.Fatalf("expected no remaining closed, got %d", len(snapshot.remainingClosed))
	}
	if snapshot.stats.Total != 0 {
		t.Fatalf("expected 0 total in stats, got %d", snapshot.stats.Total)
	}
}
