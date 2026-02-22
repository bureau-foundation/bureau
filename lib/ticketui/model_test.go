// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/ticket"
)

// testSource creates a minimal ticket index with 4 tickets for testing.
// Two are ready (open, no blockers), one closed, one in-progress.
func testSource() *IndexSource {
	index := ticket.NewIndex()
	index.Put("tkt-001", schema.TicketContent{
		Version:   1,
		Title:     "Fix connection pooling leak",
		Body:      "The connection pool is leaking under high load.",
		Status:    "open",
		Priority:  0,
		Type:      "bug",
		Labels:    []string{"infra"},
		Assignee:  ref.MustParseUserID("@iree/pm:bureau.local"),
		CreatedBy: ref.MustParseUserID("@ben:bureau.local"),
		CreatedAt: "2026-02-01T00:00:00Z",
		UpdatedAt: "2026-02-01T00:00:00Z",
	})
	index.Put("tkt-002", schema.TicketContent{
		Version:   1,
		Title:     "Implement retry backoff",
		Status:    "open",
		Priority:  1,
		Type:      "task",
		Labels:    []string{"transport"},
		CreatedBy: ref.MustParseUserID("@ben:bureau.local"),
		CreatedAt: "2026-02-02T00:00:00Z",
		UpdatedAt: "2026-02-02T00:00:00Z",
	})
	index.Put("tkt-003", schema.TicketContent{
		Version:   1,
		Title:     "Update CI pipeline config",
		Status:    "closed",
		Priority:  2,
		Type:      "chore",
		CreatedBy: ref.MustParseUserID("@ben:bureau.local"),
		CreatedAt: "2026-02-03T00:00:00Z",
		UpdatedAt: "2026-02-03T00:00:00Z",
		ClosedAt:  "2026-02-04T00:00:00Z",
	})
	index.Put("tkt-004", schema.TicketContent{
		Version:   1,
		Title:     "Layout crash on resize",
		Status:    "in_progress",
		Priority:  1,
		Type:      "bug",
		Labels:    []string{"observation"},
		Assignee:  ref.MustParseUserID("@ben:bureau.local"),
		CreatedBy: ref.MustParseUserID("@ben:bureau.local"),
		CreatedAt: "2026-02-01T12:00:00Z",
		UpdatedAt: "2026-02-02T00:00:00Z",
	})
	return NewIndexSource(index)
}

// testGroupedSource creates a ticket index with epic/child relationships
// for testing the grouped ready view.
func testGroupedSource() *IndexSource {
	index := ticket.NewIndex()

	// Epic 1: priority 1
	index.Put("epic-1", schema.TicketContent{
		Version:   1,
		Title:     "Migrate to gRPC",
		Status:    "open",
		Priority:  1,
		Type:      "epic",
		CreatedBy: ref.MustParseUserID("@ben:bureau.local"),
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	})
	// Children of epic-1.
	index.Put("child-1a", schema.TicketContent{
		Version:   1,
		Title:     "Proto definitions",
		Status:    "open",
		Priority:  0,
		Type:      "task",
		Parent:    "epic-1",
		CreatedBy: ref.MustParseUserID("@ben:bureau.local"),
		CreatedAt: "2026-01-02T00:00:00Z",
		UpdatedAt: "2026-01-02T00:00:00Z",
	})
	index.Put("child-1b", schema.TicketContent{
		Version:   1,
		Title:     "Server stubs",
		Status:    "open",
		Priority:  1,
		Type:      "task",
		Parent:    "epic-1",
		CreatedBy: ref.MustParseUserID("@ben:bureau.local"),
		CreatedAt: "2026-01-03T00:00:00Z",
		UpdatedAt: "2026-01-03T00:00:00Z",
	})
	index.Put("child-1c", schema.TicketContent{
		Version:   1,
		Title:     "Client migration",
		Status:    "closed",
		Priority:  2,
		Type:      "task",
		Parent:    "epic-1",
		CreatedBy: ref.MustParseUserID("@ben:bureau.local"),
		CreatedAt: "2026-01-04T00:00:00Z",
		UpdatedAt: "2026-01-05T00:00:00Z",
		ClosedAt:  "2026-01-05T00:00:00Z",
	})

	// Epic 2: priority 0 (should sort first)
	index.Put("epic-2", schema.TicketContent{
		Version:   1,
		Title:     "Security hardening",
		Status:    "open",
		Priority:  0,
		Type:      "epic",
		CreatedBy: ref.MustParseUserID("@ben:bureau.local"),
		CreatedAt: "2026-01-10T00:00:00Z",
		UpdatedAt: "2026-01-10T00:00:00Z",
	})
	index.Put("child-2a", schema.TicketContent{
		Version:   1,
		Title:     "Auth middleware",
		Status:    "open",
		Priority:  0,
		Type:      "task",
		Parent:    "epic-2",
		CreatedBy: ref.MustParseUserID("@ben:bureau.local"),
		CreatedAt: "2026-01-11T00:00:00Z",
		UpdatedAt: "2026-01-11T00:00:00Z",
	})

	// Ungrouped ticket (no parent, not an epic).
	index.Put("standalone", schema.TicketContent{
		Version:   1,
		Title:     "Fix typo in docs",
		Status:    "open",
		Priority:  3,
		Type:      "chore",
		CreatedBy: ref.MustParseUserID("@ben:bureau.local"),
		CreatedAt: "2026-01-20T00:00:00Z",
		UpdatedAt: "2026-01-20T00:00:00Z",
	})

	return NewIndexSource(index)
}

func TestNewModel(t *testing.T) {
	source := testSource()
	model := NewModel(source)

	// NewModel loads Ready view: open+unblocked tickets plus in_progress.
	// tkt-001 (open, P0), tkt-002 (open, P1), tkt-004 (in_progress, P1).
	// tkt-003 (closed) is excluded.
	if len(model.entries) != 3 {
		t.Fatalf("expected 3 ready entries, got %d", len(model.entries))
	}

	// Sorted by priority then creation time: P0 first, then P1s
	// in creation order.
	if model.entries[0].ID != "tkt-001" {
		t.Errorf("first entry should be tkt-001 (P0), got %s", model.entries[0].ID)
	}
	if model.entries[1].ID != "tkt-004" {
		t.Errorf("second entry should be tkt-004 (P1, earlier creation), got %s", model.entries[1].ID)
	}
	if model.entries[2].ID != "tkt-002" {
		t.Errorf("third entry should be tkt-002 (P1, later creation), got %s", model.entries[2].ID)
	}

	// Stats should reflect all tickets, not just ready.
	if model.stats.Total != 4 {
		t.Errorf("total stats should be 4, got %d", model.stats.Total)
	}

	// Items list includes an "ungrouped" header (these test tickets
	// have no parent epic).
	if len(model.items) != 4 {
		t.Fatalf("expected 4 items (1 header + 3 tickets), got %d", len(model.items))
	}
	if !model.items[0].IsHeader {
		t.Error("first item should be a group header")
	}
	if model.items[0].EpicTitle != "ungrouped" {
		t.Errorf("header should be 'ungrouped', got %q", model.items[0].EpicTitle)
	}
}

func TestModelNavigation(t *testing.T) {
	source := testSource()
	model := NewModel(source)

	// Simulate terminal dimensions.
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	model = updated.(Model)

	// Initial cursor is on the first item (the header).
	// Items: [0]=header("ungrouped"), [1]=tkt-001, [2]=tkt-004, [3]=tkt-002
	if model.cursor != 0 {
		t.Errorf("initial cursor should be 0 (header), got %d", model.cursor)
	}

	// Move down to first ticket.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	model = updated.(Model)
	if model.cursor != 1 {
		t.Errorf("cursor after first j should be 1, got %d", model.cursor)
	}

	// Move down to second ticket.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	model = updated.(Model)
	if model.cursor != 2 {
		t.Errorf("cursor after second j should be 2, got %d", model.cursor)
	}

	// Move down to third ticket.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	model = updated.(Model)
	if model.cursor != 3 {
		t.Errorf("cursor after third j should be 3, got %d", model.cursor)
	}

	// Move down again (should stay at 3 — last item).
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	model = updated.(Model)
	if model.cursor != 3 {
		t.Errorf("cursor after fourth j should stay at 3, got %d", model.cursor)
	}

	// Move up.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	model = updated.(Model)
	if model.cursor != 2 {
		t.Errorf("cursor after k should be 2, got %d", model.cursor)
	}

	// Move up again.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	model = updated.(Model)
	if model.cursor != 1 {
		t.Errorf("cursor after second k should be 1, got %d", model.cursor)
	}

	// Move up to header.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	model = updated.(Model)
	if model.cursor != 0 {
		t.Errorf("cursor after third k should be 0 (header), got %d", model.cursor)
	}

	// Move up again (should stay at 0 — first item).
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	model = updated.(Model)
	if model.cursor != 0 {
		t.Errorf("cursor after fourth k should stay at 0, got %d", model.cursor)
	}
}

func TestModelView(t *testing.T) {
	source := testSource()
	model := NewModel(source)

	// Before receiving WindowSizeMsg, View returns loading text.
	view := model.View()
	if view != "Loading..." {
		t.Errorf("expected 'Loading...' before WindowSizeMsg, got %q", view)
	}

	// Use a wide terminal so titles aren't truncated by the two-pane layout.
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 20})
	model = updated.(Model)

	view = model.View()

	// Verify key elements are present in the rendered output.
	if !strings.Contains(view, "1:Ready") {
		t.Error("view should contain tab labels")
	}
	if !strings.Contains(view, "Fix connection pooling leak") {
		t.Error("view should contain first ticket title")
	}
	if !strings.Contains(view, "Implement retry backoff") {
		t.Error("view should contain second ticket title")
	}
	if !strings.Contains(view, "P0") {
		t.Error("view should contain P0 priority")
	}
	if !strings.Contains(view, "3 shown") {
		t.Error("view should contain shown count")
	}
	if !strings.Contains(view, "q quit") {
		t.Error("view should contain help text")
	}
	if !strings.Contains(view, "ungrouped") {
		t.Error("view should contain 'ungrouped' header for parentless tickets")
	}
}

func TestModelEmptyState(t *testing.T) {
	index := ticket.NewIndex()
	source := NewIndexSource(index)
	model := NewModel(source)

	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = updated.(Model)

	view := model.View()
	if !strings.Contains(view, "No tickets found") {
		t.Error("empty view should contain 'No tickets found'")
	}
}

func TestModelQuit(t *testing.T) {
	source := testSource()
	model := NewModel(source)

	_, command := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if command == nil {
		t.Fatal("q key should return a command")
	}

	// Execute the command and check it produces a QuitMsg.
	message := command()
	if _, isQuit := message.(tea.QuitMsg); !isQuit {
		t.Errorf("expected QuitMsg, got %T", message)
	}
}

func TestModelTabSwitching(t *testing.T) {
	source := testSource()
	model := NewModel(source)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	model = updated.(Model)

	// Initially on Ready tab.
	if model.activeTab != TabReady {
		t.Errorf("expected TabReady, got %d", model.activeTab)
	}

	// Switch to All tab (key "3").
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	model = updated.(Model)
	if model.activeTab != TabAll {
		t.Errorf("expected TabAll after pressing 3, got %d", model.activeTab)
	}
	// All tab should show all 4 tickets (flat, no grouping).
	ticketCount := 0
	for _, item := range model.items {
		if !item.IsHeader {
			ticketCount++
		}
	}
	if ticketCount != 4 {
		t.Errorf("All tab should show 4 tickets, got %d", ticketCount)
	}

	// Switch to Blocked tab (key "2").
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	model = updated.(Model)
	if model.activeTab != TabBlocked {
		t.Errorf("expected TabBlocked after pressing 2, got %d", model.activeTab)
	}
	// No blocked tickets in our test data (none have unsatisfied blockers).
	ticketCount = 0
	for _, item := range model.items {
		if !item.IsHeader {
			ticketCount++
		}
	}
	if ticketCount != 0 {
		t.Errorf("Blocked tab should show 0 tickets, got %d", ticketCount)
	}

	// Switch back to Ready tab (key "1").
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	model = updated.(Model)
	if model.activeTab != TabReady {
		t.Errorf("expected TabReady after pressing 1, got %d", model.activeTab)
	}
}

func TestModelGroupedReadyView(t *testing.T) {
	source := testGroupedSource()
	model := NewModel(source)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	model = updated.(Model)

	// Ready view should group tickets by parent epic.
	// epic-2 (P0) should come first, then epic-1 (P1), then ungrouped.
	// Epics themselves are not "ready" entries (they're type=epic), so
	// they appear only as headers.

	if len(model.items) == 0 {
		t.Fatal("items should not be empty")
	}

	// Verify grouping order: epic-2 header first (P0), then epic-1 (P1), then ungrouped.
	var headerTitles []string
	for _, item := range model.items {
		if item.IsHeader {
			headerTitles = append(headerTitles, item.EpicTitle)
		}
	}

	if len(headerTitles) < 2 {
		t.Fatalf("expected at least 2 group headers, got %d: %v", len(headerTitles), headerTitles)
	}
	if headerTitles[0] != "Security hardening" {
		t.Errorf("first group should be 'Security hardening' (P0 epic), got %q", headerTitles[0])
	}
	if headerTitles[1] != "Migrate to gRPC" {
		t.Errorf("second group should be 'Migrate to gRPC' (P1 epic), got %q", headerTitles[1])
	}

	// Verify children appear after their epic header.
	foundEpic2 := false
	for _, item := range model.items {
		if item.IsHeader && item.EpicTitle == "Security hardening" {
			foundEpic2 = true
			continue
		}
		if foundEpic2 && !item.IsHeader {
			if item.Entry.ID != "child-2a" {
				t.Errorf("first child of epic-2 should be child-2a, got %s", item.Entry.ID)
			}
			break
		}
	}
	if !foundEpic2 {
		t.Error("epic-2 header not found in items")
	}

	// Cursor starts on the first item (a header).
	if model.cursor != 0 {
		t.Errorf("cursor should start at 0, got %d", model.cursor)
	}
}

func TestModelFilter(t *testing.T) {
	source := testSource()
	model := NewModel(source)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	model = updated.(Model)

	// Switch to All tab first so we have all 4 tickets visible.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	model = updated.(Model)

	// Activate filter (/).
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	model = updated.(Model)
	if model.focusRegion != FocusFilter {
		t.Errorf("after pressing /, focus should be FocusFilter, got %d", model.focusRegion)
	}

	// Type "pooling".
	for _, char := range "pooling" {
		updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{char}})
		model = updated.(Model)
	}

	// Should filter down to 1 ticket (tkt-001: "Fix connection pooling leak").
	ticketCount := 0
	for _, item := range model.items {
		if !item.IsHeader {
			ticketCount++
		}
	}
	if ticketCount != 1 {
		t.Errorf("filter 'pooling' should match 1 ticket, got %d", ticketCount)
	}

	// Press Esc to clear filter.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEscape})
	model = updated.(Model)

	// After clearing, all tickets should be visible again.
	ticketCount = 0
	for _, item := range model.items {
		if !item.IsHeader {
			ticketCount++
		}
	}
	if ticketCount != 4 {
		t.Errorf("after clearing filter, should see 4 tickets, got %d", ticketCount)
	}
}

func TestModelFilterByLabel(t *testing.T) {
	source := testSource()
	model := NewModel(source)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	model = updated.(Model)

	// Switch to All tab.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	model = updated.(Model)

	// Activate filter and type "transport".
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	model = updated.(Model)
	for _, char := range "transport" {
		updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{char}})
		model = updated.(Model)
	}

	// Should match tkt-002 (label: "transport").
	ticketCount := 0
	var matchedID string
	for _, item := range model.items {
		if !item.IsHeader {
			ticketCount++
			matchedID = item.Entry.ID
		}
	}
	if ticketCount != 1 {
		t.Errorf("filter 'transport' should match 1 ticket, got %d", ticketCount)
	}
	if matchedID != "tkt-002" {
		t.Errorf("filter 'transport' should match tkt-002, got %s", matchedID)
	}
}

func TestModelEpicCollapse(t *testing.T) {
	source := testGroupedSource()
	model := NewModel(source)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	model = updated.(Model)

	// Count initial items.
	initialCount := len(model.items)
	if initialCount == 0 {
		t.Fatal("items should not be empty")
	}

	// Find the first header and move cursor there.
	headerIndex := -1
	for index, item := range model.items {
		if item.IsHeader && item.EpicID != "" {
			headerIndex = index
			break
		}
	}
	if headerIndex == -1 {
		t.Fatal("no epic header found in items")
	}

	// We can't navigate to headers with j/k (they're skipped), but we
	// can set the cursor directly for testing the collapse logic.
	model.cursor = headerIndex
	epicID := model.items[headerIndex].EpicID

	// Count children of this epic before collapse.
	childrenBefore := 0
	for index := headerIndex + 1; index < len(model.items); index++ {
		if model.items[index].IsHeader {
			break
		}
		childrenBefore++
	}

	// Collapse via Enter key.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)

	if !model.collapsedEpics[epicID] {
		t.Error("epic should be collapsed after Enter")
	}

	// Items should be fewer after collapse.
	if len(model.items) != initialCount-childrenBefore {
		t.Errorf("expected %d items after collapse, got %d", initialCount-childrenBefore, len(model.items))
	}

	// Expand again.
	// Re-find the header (index may have changed).
	for index, item := range model.items {
		if item.IsHeader && item.EpicID == epicID {
			model.cursor = index
			break
		}
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)

	if model.collapsedEpics[epicID] {
		t.Error("epic should be expanded after second Enter")
	}
	if len(model.items) != initialCount {
		t.Errorf("expected %d items after re-expand, got %d", initialCount, len(model.items))
	}
}

func TestModelLeftRightCollapseExpand(t *testing.T) {
	source := testGroupedSource()
	model := NewModel(source)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	model = updated.(Model)

	initialCount := len(model.items)

	// Cursor starts on the first header (epic-2 "Security hardening",
	// sorted first by P0 priority).
	if !model.items[model.cursor].IsHeader {
		t.Fatal("cursor should start on a header")
	}
	headerEpicID := model.items[model.cursor].EpicID

	// Right on an expanded header should move to the first child.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model = updated.(Model)
	if model.items[model.cursor].IsHeader {
		t.Error("right on expanded header should move cursor to first child")
	}

	// Left on a child should collapse the parent group directly.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyLeft})
	model = updated.(Model)
	if !model.collapsedEpics[headerEpicID] {
		t.Error("left on child should collapse the parent epic")
	}
	if !model.items[model.cursor].IsHeader {
		t.Error("after collapse, cursor should be on the header")
	}
	if len(model.items) >= initialCount {
		t.Error("items should be fewer after collapsing")
	}

	// Right on a collapsed header should expand it and move to first child.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model = updated.(Model)
	if model.collapsedEpics[headerEpicID] {
		t.Error("right on collapsed header should expand it")
	}
	if model.items[model.cursor].IsHeader {
		t.Error("right on collapsed header should move cursor to first child")
	}
	if len(model.items) != initialCount {
		t.Errorf("items should be restored after expand, expected %d got %d", initialCount, len(model.items))
	}
}

func TestModelMouseWheelScrollsList(t *testing.T) {
	source := testGroupedSource()
	model := NewModel(source)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	model = updated.(Model)

	initialCursor := model.cursor

	// Scroll wheel down in the list pane (X=10, well within the list pane).
	contentStart := model.contentStartY()
	updated, _ = model.Update(tea.MouseMsg{
		X:      10,
		Y:      contentStart + 2,
		Button: tea.MouseButtonWheelDown,
	})
	model = updated.(Model)

	if model.cursor <= initialCursor {
		t.Errorf("mouse wheel down in list pane should move cursor down, was %d now %d", initialCursor, model.cursor)
	}

	// Scroll wheel up in the list pane.
	movedCursor := model.cursor
	updated, _ = model.Update(tea.MouseMsg{
		X:      10,
		Y:      contentStart + 2,
		Button: tea.MouseButtonWheelUp,
	})
	model = updated.(Model)

	if model.cursor >= movedCursor {
		t.Errorf("mouse wheel up in list pane should move cursor up, was %d now %d", movedCursor, model.cursor)
	}
}

func TestModelMouseClickSelectsRow(t *testing.T) {
	source := testGroupedSource()
	model := NewModel(source)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	model = updated.(Model)

	// Find a ticket row at a known offset.
	contentStart := model.contentStartY()
	targetIndex := -1
	for index, item := range model.items {
		if !item.IsHeader && index != model.cursor {
			targetIndex = index
			break
		}
	}
	if targetIndex == -1 {
		t.Skip("no alternative selectable item found")
	}

	// Click on that row.
	rowOffset := targetIndex - model.scrollOffset
	updated, _ = model.Update(tea.MouseMsg{
		X:      10,
		Y:      contentStart + rowOffset,
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
	})
	model = updated.(Model)

	if model.cursor != targetIndex {
		t.Errorf("click should select row at index %d, got cursor %d", targetIndex, model.cursor)
	}
}

func TestModelMouseClickFocusesDetailPane(t *testing.T) {
	source := testSource()
	model := NewModel(source)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	model = updated.(Model)

	if model.focusRegion != FocusList {
		t.Fatal("should start with list focus")
	}

	// Click in the detail pane area (X = 100, well past the list width of 80).
	contentStart := model.contentStartY()
	updated, _ = model.Update(tea.MouseMsg{
		X:      100,
		Y:      contentStart + 2,
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
	})
	model = updated.(Model)

	if model.focusRegion != FocusDetail {
		t.Errorf("clicking detail pane should set FocusDetail, got %d", model.focusRegion)
	}
}

// testDependencyModelSource creates a source with dependency chains
// for testing navigateToTicket and navigation history. Tickets A, B,
// and C are all open. B is blocked by A. C is on the All tab only
// (closed).
func testDependencyModelSource() *IndexSource {
	index := ticket.NewIndex()
	index.Put("tkt-a", schema.TicketContent{
		Version:   1,
		Title:     "Ticket A",
		Status:    "open",
		Priority:  0,
		Type:      "task",
		CreatedBy: ref.MustParseUserID("@test:bureau.local"),
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	})
	index.Put("tkt-b", schema.TicketContent{
		Version:   1,
		Title:     "Ticket B",
		Status:    "open",
		Priority:  1,
		Type:      "task",
		BlockedBy: []string{"tkt-a"},
		CreatedBy: ref.MustParseUserID("@test:bureau.local"),
		CreatedAt: "2026-01-02T00:00:00Z",
		UpdatedAt: "2026-01-02T00:00:00Z",
	})
	index.Put("tkt-c", schema.TicketContent{
		Version:   1,
		Title:     "Ticket C",
		Status:    "closed",
		Priority:  2,
		Type:      "task",
		CreatedBy: ref.MustParseUserID("@test:bureau.local"),
		CreatedAt: "2026-01-03T00:00:00Z",
		UpdatedAt: "2026-01-03T00:00:00Z",
		ClosedAt:  "2026-01-04T00:00:00Z",
	})
	return NewIndexSource(index)
}

func TestNavigateToTicketOnCurrentTab(t *testing.T) {
	source := testDependencyModelSource()
	model := NewModel(source)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	model = updated.(Model)

	// Move cursor to the first ticket so we have a known starting point.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	model = updated.(Model)
	originalID := model.selectedID

	// Navigate to tkt-a which should be on the ready tab.
	model.navigateToTicket("tkt-a")
	if model.selectedID != "tkt-a" {
		t.Errorf("after navigateToTicket, selectedID = %q, want tkt-a", model.selectedID)
	}

	// Navigation history should have one entry (the original position).
	if len(model.navHistory) != 1 {
		t.Fatalf("navHistory should have 1 entry, got %d", len(model.navHistory))
	}
	if model.navHistory[0].ticketID != originalID {
		t.Errorf("navHistory[0].ticketID = %q, want %q", model.navHistory[0].ticketID, originalID)
	}

	// Forward stack should be empty (new navigation clears it).
	if len(model.navForward) != 0 {
		t.Errorf("navForward should be empty, got %d", len(model.navForward))
	}
}

func TestNavigateToTicketSwitchesTab(t *testing.T) {
	source := testDependencyModelSource()
	model := NewModel(source)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	model = updated.(Model)

	// Start on ready tab.
	if model.activeTab != TabReady {
		t.Fatalf("expected TabReady, got %d", model.activeTab)
	}

	// Navigate to tkt-c which is closed and not on the ready tab.
	model.navigateToTicket("tkt-c")
	if model.activeTab != TabAll {
		t.Errorf("navigating to a closed ticket should switch to TabAll, got %d", model.activeTab)
	}
	if model.selectedID != "tkt-c" {
		t.Errorf("selectedID = %q, want tkt-c", model.selectedID)
	}
}

func TestNavigateBackRestoresPosition(t *testing.T) {
	source := testDependencyModelSource()
	model := NewModel(source)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	model = updated.(Model)

	// Move to a known ticket on the ready tab.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	model = updated.(Model)
	originalTab := model.activeTab
	originalID := model.selectedID

	// Navigate to closed ticket (switches to All tab).
	model.navigateToTicket("tkt-c")
	if model.activeTab != TabAll {
		t.Fatalf("should be on TabAll after navigating to closed ticket, got %d", model.activeTab)
	}

	// Navigate back.
	model.navigateBack()
	if model.activeTab != originalTab {
		t.Errorf("navigateBack: activeTab = %d, want %d", model.activeTab, originalTab)
	}
	if model.selectedID != originalID {
		t.Errorf("navigateBack: selectedID = %q, want %q", model.selectedID, originalID)
	}

	// Forward stack should now have the position we came back from.
	if len(model.navForward) != 1 {
		t.Fatalf("navForward should have 1 entry, got %d", len(model.navForward))
	}
	if model.navForward[0].ticketID != "tkt-c" {
		t.Errorf("navForward[0].ticketID = %q, want tkt-c", model.navForward[0].ticketID)
	}
}

func TestNavigateForwardRestoresPosition(t *testing.T) {
	source := testDependencyModelSource()
	model := NewModel(source)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	model = updated.(Model)

	// Navigate away then back.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	model = updated.(Model)

	model.navigateToTicket("tkt-c")
	model.navigateBack()

	// Now navigate forward — should return to tkt-c on All tab.
	model.navigateForward()
	if model.activeTab != TabAll {
		t.Errorf("navigateForward: activeTab = %d, want TabAll", model.activeTab)
	}
	if model.selectedID != "tkt-c" {
		t.Errorf("navigateForward: selectedID = %q, want tkt-c", model.selectedID)
	}
}

func TestNavigateBackNoHistory(t *testing.T) {
	source := testSource()
	model := NewModel(source)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	model = updated.(Model)

	originalID := model.selectedID
	originalTab := model.activeTab

	// navigateBack with empty history should be a no-op.
	model.navigateBack()
	if model.selectedID != originalID {
		t.Errorf("navigateBack with no history should not change selectedID: got %q, want %q",
			model.selectedID, originalID)
	}
	if model.activeTab != originalTab {
		t.Errorf("navigateBack with no history should not change tab: got %d, want %d",
			model.activeTab, originalTab)
	}
}

func TestNewNavigationClearsForwardStack(t *testing.T) {
	source := testDependencyModelSource()
	model := NewModel(source)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	model = updated.(Model)

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	model = updated.(Model)

	// Build up a forward stack: navigate away, then back.
	model.navigateToTicket("tkt-c")
	model.navigateBack()
	if len(model.navForward) == 0 {
		t.Fatal("forward stack should not be empty after navigateBack")
	}

	// New navigation should clear the forward stack.
	model.navigateToTicket("tkt-a")
	if len(model.navForward) != 0 {
		t.Errorf("new navigation should clear forward stack, got %d entries", len(model.navForward))
	}
}
