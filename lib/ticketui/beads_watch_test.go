// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

func TestReadBeadsSnapshot(t *testing.T) {
	content := `{"id":"bd-001","title":"First","description":"","status":"open","priority":1,"issue_type":"task","created_at":"2026-01-01T00:00:00Z","created_by":"test","updated_at":"2026-01-01T00:00:00Z"}
{"id":"bd-002","title":"Second","description":"","status":"closed","priority":0,"issue_type":"bug","created_at":"2026-01-02T00:00:00Z","created_by":"test","updated_at":"2026-01-02T00:00:00Z","closed_at":"2026-01-02T12:00:00Z","close_reason":"Done"}
`
	path := writeTestJSONL(t, content)

	entries, err := readBeadsSnapshot(path)
	if err != nil {
		t.Fatalf("readBeadsSnapshot: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	entry, exists := entries["bd-001"]
	if !exists {
		t.Fatal("bd-001 not found")
	}
	if entry.content.Title != "First" {
		t.Errorf("bd-001 title = %q, expected First", entry.content.Title)
	}
	if len(entry.rawJSON) == 0 {
		t.Error("bd-001 rawJSON is empty")
	}

	entry, exists = entries["bd-002"]
	if !exists {
		t.Fatal("bd-002 not found")
	}
	if entry.content.Status != ticket.StatusClosed {
		t.Errorf("bd-002 status = %q, expected closed", entry.content.Status)
	}
}

func TestReadBeadsSnapshot_EmptyFile(t *testing.T) {
	path := writeTestJSONL(t, "")

	entries, err := readBeadsSnapshot(path)
	if err != nil {
		t.Fatalf("readBeadsSnapshot on empty file: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestReadBeadsSnapshot_MissingFile(t *testing.T) {
	_, err := readBeadsSnapshot("/nonexistent/path/issues.jsonl")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestDiffBeadsSnapshots_NewEntry(t *testing.T) {
	index := ticketindex.NewIndex()
	source := NewIndexSource(index)
	events := source.Subscribe()

	previous := map[string]beadsSnapshotEntry{}
	current := map[string]beadsSnapshotEntry{
		"bd-001": {
			rawJSON: []byte(`{"id":"bd-001","title":"New"}`),
			content: ticket.TicketContent{Title: "New", Status: ticket.StatusOpen},
		},
	}

	diffBeadsSnapshots(source, previous, current)

	select {
	case event := <-events:
		if event.TicketID != "bd-001" {
			t.Errorf("event ticket ID = %q, expected bd-001", event.TicketID)
		}
		if event.Kind != "put" {
			t.Errorf("event kind = %q, expected put", event.Kind)
		}
	default:
		t.Error("expected put event for new entry, got none")
	}
}

func TestDiffBeadsSnapshots_RemovedEntry(t *testing.T) {
	index := ticketindex.NewIndex()
	// Pre-populate so Remove has something to dispatch.
	index.Put("bd-001", ticket.TicketContent{Title: "Old", Status: ticket.StatusOpen})
	source := NewIndexSource(index)
	events := source.Subscribe()

	previous := map[string]beadsSnapshotEntry{
		"bd-001": {
			rawJSON: []byte(`{"id":"bd-001","title":"Old"}`),
			content: ticket.TicketContent{Title: "Old", Status: ticket.StatusOpen},
		},
	}
	current := map[string]beadsSnapshotEntry{}

	diffBeadsSnapshots(source, previous, current)

	select {
	case event := <-events:
		if event.TicketID != "bd-001" {
			t.Errorf("event ticket ID = %q, expected bd-001", event.TicketID)
		}
		if event.Kind != "remove" {
			t.Errorf("event kind = %q, expected remove", event.Kind)
		}
	default:
		t.Error("expected remove event for deleted entry, got none")
	}
}

func TestDiffBeadsSnapshots_ChangedEntry(t *testing.T) {
	index := ticketindex.NewIndex()
	index.Put("bd-001", ticket.TicketContent{Title: "Old title", Status: ticket.StatusOpen})
	source := NewIndexSource(index)
	events := source.Subscribe()

	previous := map[string]beadsSnapshotEntry{
		"bd-001": {
			rawJSON: []byte(`{"id":"bd-001","title":"Old title"}`),
			content: ticket.TicketContent{Title: "Old title", Status: ticket.StatusOpen},
		},
	}
	current := map[string]beadsSnapshotEntry{
		"bd-001": {
			rawJSON: []byte(`{"id":"bd-001","title":"New title"}`),
			content: ticket.TicketContent{Title: "New title", Status: ticket.StatusOpen},
		},
	}

	diffBeadsSnapshots(source, previous, current)

	select {
	case event := <-events:
		if event.TicketID != "bd-001" {
			t.Errorf("event ticket ID = %q, expected bd-001", event.TicketID)
		}
		if event.Kind != "put" {
			t.Errorf("event kind = %q, expected put", event.Kind)
		}
		if event.Content.Title != "New title" {
			t.Errorf("event content title = %q, expected New title", event.Content.Title)
		}
	default:
		t.Error("expected put event for changed entry, got none")
	}
}

func TestDiffBeadsSnapshots_NoChange(t *testing.T) {
	index := ticketindex.NewIndex()
	index.Put("bd-001", ticket.TicketContent{Title: "Same", Status: ticket.StatusOpen})
	source := NewIndexSource(index)
	events := source.Subscribe()

	sameJSON := []byte(`{"id":"bd-001","title":"Same"}`)
	entry := beadsSnapshotEntry{
		rawJSON: sameJSON,
		content: ticket.TicketContent{Title: "Same", Status: ticket.StatusOpen},
	}
	previous := map[string]beadsSnapshotEntry{"bd-001": entry}
	current := map[string]beadsSnapshotEntry{"bd-001": entry}

	diffBeadsSnapshots(source, previous, current)

	select {
	case event := <-events:
		t.Errorf("expected no events for unchanged entry, got %+v", event)
	default:
		// Good — no events dispatched.
	}
}

func TestDiffBeadsSnapshots_MixedChanges(t *testing.T) {
	index := ticketindex.NewIndex()
	index.Put("bd-001", ticket.TicketContent{Title: "Unchanged", Status: ticket.StatusOpen})
	index.Put("bd-002", ticket.TicketContent{Title: "Will change", Status: ticket.StatusOpen})
	index.Put("bd-003", ticket.TicketContent{Title: "Will remove", Status: ticket.StatusOpen})
	source := NewIndexSource(index)
	events := source.Subscribe()

	previous := map[string]beadsSnapshotEntry{
		"bd-001": {
			rawJSON: []byte(`{"id":"bd-001","title":"Unchanged"}`),
			content: ticket.TicketContent{Title: "Unchanged", Status: ticket.StatusOpen},
		},
		"bd-002": {
			rawJSON: []byte(`{"id":"bd-002","title":"Will change"}`),
			content: ticket.TicketContent{Title: "Will change", Status: ticket.StatusOpen},
		},
		"bd-003": {
			rawJSON: []byte(`{"id":"bd-003","title":"Will remove"}`),
			content: ticket.TicketContent{Title: "Will remove", Status: ticket.StatusOpen},
		},
	}
	current := map[string]beadsSnapshotEntry{
		"bd-001": {
			rawJSON: []byte(`{"id":"bd-001","title":"Unchanged"}`),
			content: ticket.TicketContent{Title: "Unchanged", Status: ticket.StatusOpen},
		},
		"bd-002": {
			rawJSON: []byte(`{"id":"bd-002","title":"Changed"}`),
			content: ticket.TicketContent{Title: "Changed", Status: ticket.StatusOpen},
		},
		"bd-004": {
			rawJSON: []byte(`{"id":"bd-004","title":"New entry"}`),
			content: ticket.TicketContent{Title: "New entry", Status: ticket.StatusOpen},
		},
	}

	diffBeadsSnapshots(source, previous, current)

	// Collect all events (should be 3: put bd-002, put bd-004, remove bd-003).
	collected := map[string]Event{}
	for range 3 {
		select {
		case event := <-events:
			collected[event.TicketID] = event
		default:
			t.Fatal("expected 3 events, ran out early")
		}
	}

	// No more events.
	select {
	case event := <-events:
		t.Errorf("unexpected extra event: %+v", event)
	default:
	}

	if event, exists := collected["bd-002"]; !exists || event.Kind != "put" {
		t.Errorf("expected put for bd-002, got %+v", collected["bd-002"])
	}
	if event, exists := collected["bd-003"]; !exists || event.Kind != "remove" {
		t.Errorf("expected remove for bd-003, got %+v", collected["bd-003"])
	}
	if event, exists := collected["bd-004"]; !exists || event.Kind != "put" {
		t.Errorf("expected put for bd-004, got %+v", collected["bd-004"])
	}
	if _, exists := collected["bd-001"]; exists {
		t.Error("bd-001 should not have produced an event (unchanged)")
	}
}

func TestWatchBeadsFile_InitialLoad(t *testing.T) {
	content := `{"id":"bd-001","title":"First","description":"","status":"open","priority":1,"issue_type":"task","created_at":"2026-01-01T00:00:00Z","created_by":"test","updated_at":"2026-01-01T00:00:00Z"}
{"id":"bd-002","title":"Second","description":"","status":"open","priority":2,"issue_type":"bug","created_at":"2026-01-02T00:00:00Z","created_by":"test","updated_at":"2026-01-02T00:00:00Z"}
`
	path := writeTestJSONL(t, content)

	source, cleanup, err := WatchBeadsFile(path)
	if err != nil {
		t.Fatalf("WatchBeadsFile: %v", err)
	}
	defer cleanup()

	all := source.All()
	if all.Stats.Total != 2 {
		t.Fatalf("expected 2 tickets, got %d", all.Stats.Total)
	}

	ticket001, exists := source.Get("bd-001")
	if !exists {
		t.Fatal("bd-001 not found")
	}
	if ticket001.Title != "First" {
		t.Errorf("bd-001 title = %q, expected First", ticket001.Title)
	}
}

func TestWatchBeadsFile_DetectsFileChange(t *testing.T) {
	initialContent := `{"id":"bd-001","title":"Original","description":"","status":"open","priority":1,"issue_type":"task","created_at":"2026-01-01T00:00:00Z","created_by":"test","updated_at":"2026-01-01T00:00:00Z"}
`
	path := writeTestJSONL(t, initialContent)

	source, cleanup, err := WatchBeadsFile(path)
	if err != nil {
		t.Fatalf("WatchBeadsFile: %v", err)
	}
	defer cleanup()

	events := source.Subscribe()

	// Verify initial state.
	ticket001, exists := source.Get("bd-001")
	if !exists {
		t.Fatal("bd-001 not found after initial load")
	}
	if ticket001.Title != "Original" {
		t.Fatalf("bd-001 title = %q, expected Original", ticket001.Title)
	}

	// Write updated content to the file.
	updatedContent := `{"id":"bd-001","title":"Updated","description":"","status":"open","priority":1,"issue_type":"task","created_at":"2026-01-01T00:00:00Z","created_by":"test","updated_at":"2026-01-01T00:00:00Z"}
{"id":"bd-002","title":"New ticket","description":"","status":"open","priority":2,"issue_type":"task","created_at":"2026-01-02T00:00:00Z","created_by":"test","updated_at":"2026-01-02T00:00:00Z"}
`
	if err := os.WriteFile(path, []byte(updatedContent), 0o644); err != nil {
		t.Fatalf("write updated content: %v", err)
	}

	// Wait for events. The watcher has a 50ms debounce plus poll
	// interval, so give it up to 500ms. This timeout is genuine OS
	// I/O: we're waiting for real inotify events from real filesystem
	// writes — no fake clock can drive the kernel's inotify subsystem.
	deadline := time.After(500 * time.Millisecond) //nolint:realclock
	collected := map[string]Event{}
	for len(collected) < 2 {
		select {
		case event := <-events:
			collected[event.TicketID] = event
		case <-deadline:
			t.Fatalf("timed out waiting for events; got %d of 2: %+v", len(collected), collected)
		}
	}

	// Verify bd-001 was updated.
	if event, exists := collected["bd-001"]; !exists || event.Kind != "put" {
		t.Errorf("expected put for bd-001, got %+v", collected["bd-001"])
	}
	updatedTicket, _ := source.Get("bd-001")
	if updatedTicket.Title != "Updated" {
		t.Errorf("bd-001 title after update = %q, expected Updated", updatedTicket.Title)
	}

	// Verify bd-002 was added.
	if event, exists := collected["bd-002"]; !exists || event.Kind != "put" {
		t.Errorf("expected put for bd-002, got %+v", collected["bd-002"])
	}
}

func writeTestJSONL(t *testing.T, content string) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "issues.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
