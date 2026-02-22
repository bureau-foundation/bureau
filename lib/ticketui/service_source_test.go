// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

// newTestServiceSource creates a ServiceSource without starting the
// background stream loop. Use this with processFrames for unit tests
// that feed frames directly.
func newTestServiceSource() *ServiceSource {
	return &ServiceSource{
		IndexSource: IndexSource{
			index: ticketindex.NewIndex(),
		},
		roomID: "!test:local",
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// writeFrame encodes a serviceSubscribeFrame to the given writer.
func writeFrame(t *testing.T, conn net.Conn, frame serviceSubscribeFrame) {
	t.Helper()
	if err := codec.NewEncoder(conn).Encode(frame); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// awaitSentinel sends a sentinel put frame and waits for its event on
// the subscriber channel. Since processFrames handles frames
// sequentially, receiving the sentinel event guarantees all frames
// sent before it have been fully processed.
func awaitSentinel(t *testing.T, conn net.Conn, events <-chan Event, ticketID string) {
	t.Helper()
	writeFrame(t, conn, serviceSubscribeFrame{
		Type:     "put",
		TicketID: ticketID,
		Content:  ticket.TicketContent{Version: 1, Title: "sentinel", Status: "open"},
	})
	event := testutil.RequireReceive(t, events, time.Second, "waiting for sentinel %s", ticketID)
	if event.TicketID != ticketID {
		t.Fatalf("expected sentinel event %s, got %s", ticketID, event.TicketID)
	}
}

// TestProcessFramesPut verifies that a "put" frame adds a ticket to
// the local index and dispatches an event to subscribers.
func TestProcessFramesPut(t *testing.T) {
	source := newTestServiceSource()
	serverConn, clientConn := net.Pipe()

	// Subscribe before starting processFrames.
	events := source.Subscribe()

	processDone := make(chan error, 1)
	go func() {
		processDone <- source.processFrames(codec.NewDecoder(clientConn))
	}()

	content := ticket.TicketContent{
		Version: 1, Title: "test ticket", Status: "open",
		Type: "task", CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	writeFrame(t, serverConn, serviceSubscribeFrame{
		Type:     "put",
		TicketID: "tkt-1",
		Content:  content,
	})

	// Verify the ticket was indexed.
	event := testutil.RequireReceive(t, events, time.Second, "waiting for put event")
	if event.Kind != "put" || event.TicketID != "tkt-1" {
		t.Fatalf("expected put for tkt-1, got %s for %s", event.Kind, event.TicketID)
	}

	stored, exists := source.Get("tkt-1")
	if !exists {
		t.Fatal("ticket should exist in index after put")
	}
	if stored.Title != "test ticket" {
		t.Fatalf("expected title 'test ticket', got %q", stored.Title)
	}

	serverConn.Close()
	<-processDone
}

// TestProcessFramesRemove verifies that a "remove" frame deletes a
// ticket from the local index.
func TestProcessFramesRemove(t *testing.T) {
	source := newTestServiceSource()
	// Pre-populate the index.
	source.index.Put("tkt-1", ticket.TicketContent{
		Version: 1, Title: "existing", Status: "open",
	})

	serverConn, clientConn := net.Pipe()

	events := source.Subscribe()

	processDone := make(chan error, 1)
	go func() {
		processDone <- source.processFrames(codec.NewDecoder(clientConn))
	}()

	writeFrame(t, serverConn, serviceSubscribeFrame{
		Type:     "remove",
		TicketID: "tkt-1",
	})

	event := testutil.RequireReceive(t, events, time.Second, "waiting for remove event")
	if event.Kind != "remove" || event.TicketID != "tkt-1" {
		t.Fatalf("expected remove for tkt-1, got %s for %s", event.Kind, event.TicketID)
	}

	if _, exists := source.Get("tkt-1"); exists {
		t.Fatal("ticket should not exist after remove")
	}

	serverConn.Close()
	<-processDone
}

// TestProcessFramesLoadingStates verifies the loading state
// transitions through the snapshot phases.
func TestProcessFramesLoadingStates(t *testing.T) {
	source := newTestServiceSource()
	source.loadingState.Store("loading")

	serverConn, clientConn := net.Pipe()

	events := source.Subscribe()

	processDone := make(chan error, 1)
	go func() {
		processDone <- source.processFrames(codec.NewDecoder(clientConn))
	}()

	// Send open_complete followed by a sentinel put. Receiving the
	// sentinel event guarantees open_complete was processed.
	writeFrame(t, serverConn, serviceSubscribeFrame{
		Type:  "open_complete",
		Stats: &ticketindex.Stats{Total: 5},
	})
	awaitSentinel(t, serverConn, events, "sentinel-1")
	if state := source.LoadingState(); state != "open_complete" {
		t.Fatalf("expected loading state 'open_complete', got %q", state)
	}

	// Send caught_up followed by a sentinel put.
	writeFrame(t, serverConn, serviceSubscribeFrame{
		Type:  "caught_up",
		Stats: &ticketindex.Stats{Total: 10},
	})
	awaitSentinel(t, serverConn, events, "sentinel-2")
	if state := source.LoadingState(); state != "caught_up" {
		t.Fatalf("expected loading state 'caught_up', got %q", state)
	}

	serverConn.Close()
	<-processDone
}

// TestProcessFramesResyncClearsIndex verifies that a "resync" frame
// clears the local index and resets loading state.
func TestProcessFramesResyncClearsIndex(t *testing.T) {
	source := newTestServiceSource()
	source.loadingState.Store("caught_up")

	// Pre-populate index.
	source.index.Put("tkt-1", ticket.TicketContent{
		Version: 1, Title: "existing", Status: "open",
	})

	serverConn, clientConn := net.Pipe()

	events := source.Subscribe()

	processDone := make(chan error, 1)
	go func() {
		processDone <- source.processFrames(codec.NewDecoder(clientConn))
	}()

	// Send resync followed by a sentinel put to confirm processing.
	writeFrame(t, serverConn, serviceSubscribeFrame{Type: "resync"})
	awaitSentinel(t, serverConn, events, "sentinel-1")

	if state := source.LoadingState(); state != "loading" {
		t.Fatalf("expected loading state 'loading' after resync, got %q", state)
	}
	if _, exists := source.Get("tkt-1"); exists {
		t.Fatal("index should be cleared after resync")
	}

	// New snapshot can follow. The put dispatches its own event.
	writeFrame(t, serverConn, serviceSubscribeFrame{
		Type:     "put",
		TicketID: "tkt-2",
		Content: ticket.TicketContent{
			Version: 1, Title: "after resync", Status: "open",
		},
	})
	event := testutil.RequireReceive(t, events, time.Second, "waiting for tkt-2 event")
	if event.TicketID != "tkt-2" {
		t.Fatalf("expected event for tkt-2, got %s", event.TicketID)
	}

	if _, exists := source.Get("tkt-2"); !exists {
		t.Fatal("ticket from new snapshot should be indexed after resync")
	}

	serverConn.Close()
	<-processDone
}

// TestProcessFramesErrorReturns verifies that an "error" frame causes
// processFrames to return with the server's error message.
func TestProcessFramesErrorReturns(t *testing.T) {
	source := newTestServiceSource()

	serverConn, clientConn := net.Pipe()

	processDone := make(chan error, 1)
	go func() {
		processDone <- source.processFrames(codec.NewDecoder(clientConn))
	}()

	writeFrame(t, serverConn, serviceSubscribeFrame{
		Type:    "error",
		Message: "room no longer tracked",
	})

	err := testutil.RequireReceive(t, processDone, time.Second, "processFrames should return after error frame")
	if err == nil {
		t.Fatal("processFrames should return an error for error frames")
	}
	if err.Error() != "server error: room no longer tracked" {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// TestProcessFramesHeartbeatNoOp verifies that heartbeat frames are
// ignored (no state change, no events).
func TestProcessFramesHeartbeatNoOp(t *testing.T) {
	source := newTestServiceSource()
	source.loadingState.Store("caught_up")

	serverConn, clientConn := net.Pipe()

	events := source.Subscribe()

	processDone := make(chan error, 1)
	go func() {
		processDone <- source.processFrames(codec.NewDecoder(clientConn))
	}()

	// Send heartbeat followed by a sentinel put. If heartbeat had
	// dispatched an event, it would arrive before the sentinel.
	writeFrame(t, serverConn, serviceSubscribeFrame{Type: "heartbeat"})
	writeFrame(t, serverConn, serviceSubscribeFrame{
		Type:     "put",
		TicketID: "sentinel",
		Content:  ticket.TicketContent{Version: 1, Title: "sentinel", Status: "open"},
	})

	event := testutil.RequireReceive(t, events, time.Second, "waiting for sentinel event")
	if event.TicketID != "sentinel" {
		t.Fatalf("heartbeat should not dispatch events; got event for %s before sentinel", event.TicketID)
	}

	// Loading state should not change.
	if state := source.LoadingState(); state != "caught_up" {
		t.Fatalf("loading state should remain 'caught_up', got %q", state)
	}

	serverConn.Close()
	<-processDone
}

// TestProcessFramesFullSnapshot verifies a complete snapshot sequence:
// put frames → open_complete → more puts → caught_up → live events.
func TestProcessFramesFullSnapshot(t *testing.T) {
	source := newTestServiceSource()
	source.loadingState.Store("loading")

	serverConn, clientConn := net.Pipe()

	events := source.Subscribe()

	processDone := make(chan error, 1)
	go func() {
		processDone <- source.processFrames(codec.NewDecoder(clientConn))
	}()

	// Phase 1: closed dep.
	writeFrame(t, serverConn, serviceSubscribeFrame{
		Type:     "put",
		TicketID: "dep-1",
		Content: ticket.TicketContent{
			Version: 1, Title: "closed dep", Status: "closed",
			ClosedAt: "2026-01-02T00:00:00Z",
		},
	})

	// Phase 2: open ticket.
	writeFrame(t, serverConn, serviceSubscribeFrame{
		Type:     "put",
		TicketID: "tkt-1",
		Content: ticket.TicketContent{
			Version: 1, Title: "open ticket", Status: "open",
			BlockedBy: []string{"dep-1"},
		},
	})

	// Phase 3: open_complete.
	writeFrame(t, serverConn, serviceSubscribeFrame{
		Type:  "open_complete",
		Stats: &ticketindex.Stats{Total: 2},
	})

	// Phase 4: remaining closed.
	writeFrame(t, serverConn, serviceSubscribeFrame{
		Type:     "put",
		TicketID: "old-1",
		Content: ticket.TicketContent{
			Version: 1, Title: "old closed", Status: "closed",
			ClosedAt: "2025-12-01T00:00:00Z",
		},
	})

	// Phase 5: caught_up.
	writeFrame(t, serverConn, serviceSubscribeFrame{
		Type:  "caught_up",
		Stats: &ticketindex.Stats{Total: 3},
	})

	// Live event after caught_up. This also serves as a sentinel:
	// receiving its event guarantees caught_up was processed, since
	// processFrames handles frames sequentially.
	writeFrame(t, serverConn, serviceSubscribeFrame{
		Type:     "put",
		TicketID: "tkt-2",
		Content: ticket.TicketContent{
			Version: 1, Title: "live ticket", Status: "open",
		},
	})

	// Drain the 3 snapshot put events (dep-1, tkt-1, old-1).
	for i := 0; i < 3; i++ {
		testutil.RequireReceive(t, events, time.Second, "draining snapshot event %d of 3", i+1)
	}

	// Wait for the live event. Receiving it confirms that the
	// intermediate open_complete and caught_up frames were fully
	// processed by processFrames.
	event := testutil.RequireReceive(t, events, time.Second, "waiting for live event")
	if event.TicketID != "tkt-2" {
		t.Fatalf("expected live event for tkt-2, got %s", event.TicketID)
	}

	if state := source.LoadingState(); state != "caught_up" {
		t.Fatalf("expected loading state 'caught_up', got %q", state)
	}

	snapshot := source.All()
	if snapshot.Stats.Total != 4 {
		t.Fatalf("expected 4 tickets in index, got %d", snapshot.Stats.Total)
	}

	// Verify AllBlockersClosed works (the dep was loaded before the
	// open ticket, so it should be found).
	tkt1, exists := source.Get("tkt-1")
	if !exists {
		t.Fatal("tkt-1 should exist")
	}
	if !source.index.AllBlockersClosed(&tkt1) {
		t.Fatal("tkt-1 blockers should all be closed (dep-1 is closed)")
	}

	serverConn.Close()
	<-processDone
}

// TestClearIndex verifies that clearIndex replaces the index with a
// fresh empty one.
func TestClearIndex(t *testing.T) {
	source := newTestServiceSource()
	source.index.Put("tkt-1", ticket.TicketContent{
		Version: 1, Title: "existing", Status: "open",
	})

	if source.index.Len() != 1 {
		t.Fatalf("expected 1 ticket before clear, got %d", source.index.Len())
	}

	source.clearIndex()

	if source.index.Len() != 0 {
		t.Fatalf("expected 0 tickets after clear, got %d", source.index.Len())
	}

	// Verify the index is still usable (new index was allocated).
	source.index.Put("tkt-2", ticket.TicketContent{
		Version: 1, Title: "new", Status: "open",
	})
	if source.index.Len() != 1 {
		t.Fatalf("expected 1 ticket after put on cleared index, got %d", source.index.Len())
	}
}

// TestSetToken verifies that SetToken stores the new token.
func TestSetToken(t *testing.T) {
	source := newTestServiceSource()
	source.token.Store([]byte("old-token"))

	source.SetToken([]byte("new-token"))

	stored, ok := source.token.Load().([]byte)
	if !ok {
		t.Fatal("token should be stored as []byte")
	}
	if string(stored) != "new-token" {
		t.Fatalf("expected 'new-token', got %q", string(stored))
	}
}
