// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"fmt"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// --- Test helpers ---

// makeTicket returns a valid TicketContent with the given ID-like title
// and sensible defaults. Override fields after construction as needed.
func makeTicket(title string) schema.TicketContent {
	return schema.TicketContent{
		Version:   1,
		Title:     title,
		Status:    "open",
		Priority:  2,
		Type:      "task",
		CreatedBy: "@test:bureau.local",
		CreatedAt: "2026-02-12T10:00:00Z",
		UpdatedAt: "2026-02-12T10:00:00Z",
	}
}

// entryIDs extracts ticket IDs from a slice of entries, preserving
// order.
func entryIDs(entries []Entry) []string {
	ids := make([]string, len(entries))
	for i, entry := range entries {
		ids[i] = entry.ID
	}
	return ids
}

// containsID returns true if the entries include a ticket with the
// given ID.
func containsID(entries []Entry, id string) bool {
	for _, entry := range entries {
		if entry.ID == id {
			return true
		}
	}
	return false
}

// intPtr returns a pointer to the given int.
func intPtr(v int) *int {
	return &v
}

// --- NewIndex ---

func TestNewIndex(t *testing.T) {
	idx := NewIndex()
	if idx.Len() != 0 {
		t.Fatalf("new index Len() = %d, want 0", idx.Len())
	}
}

// --- Put / Get / Len ---

func TestPutAndGet(t *testing.T) {
	idx := NewIndex()
	tc := makeTicket("Fix login bug")
	idx.Put("tkt-abc1", tc)

	if idx.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", idx.Len())
	}

	got, exists := idx.Get("tkt-abc1")
	if !exists {
		t.Fatal("Get returned exists=false for a ticket that was Put")
	}
	if got.Title != "Fix login bug" {
		t.Errorf("Title = %q, want %q", got.Title, "Fix login bug")
	}
}

func TestGetNonexistent(t *testing.T) {
	idx := NewIndex()
	_, exists := idx.Get("tkt-nope")
	if exists {
		t.Fatal("Get returned exists=true for nonexistent ticket")
	}
}

func TestPutOverwritesExisting(t *testing.T) {
	idx := NewIndex()
	tc := makeTicket("Original title")
	idx.Put("tkt-abc1", tc)

	tc.Title = "Updated title"
	tc.Status = "closed"
	idx.Put("tkt-abc1", tc)

	if idx.Len() != 1 {
		t.Fatalf("Len() = %d after overwrite, want 1", idx.Len())
	}
	got, _ := idx.Get("tkt-abc1")
	if got.Title != "Updated title" {
		t.Errorf("Title = %q, want %q", got.Title, "Updated title")
	}
	if got.Status != "closed" {
		t.Errorf("Status = %q, want %q", got.Status, "closed")
	}
}

// --- Remove ---

func TestRemove(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-abc1", makeTicket("Ticket A"))
	idx.Put("tkt-abc2", makeTicket("Ticket B"))

	idx.Remove("tkt-abc1")

	if idx.Len() != 1 {
		t.Fatalf("Len() = %d after Remove, want 1", idx.Len())
	}
	_, exists := idx.Get("tkt-abc1")
	if exists {
		t.Fatal("removed ticket still exists in index")
	}
	_, exists = idx.Get("tkt-abc2")
	if !exists {
		t.Fatal("other ticket disappeared after Remove")
	}
}

func TestRemoveNonexistent(t *testing.T) {
	idx := NewIndex()
	// Should not panic.
	idx.Remove("tkt-nope")
}

func TestRemoveCleansSecondaryIndexes(t *testing.T) {
	idx := NewIndex()
	tc := makeTicket("Labeled ticket")
	tc.Labels = []string{"security", "urgent"}
	tc.Assignee = "@agent:bureau.local"
	tc.Parent = "tkt-epic"
	idx.Put("tkt-abc1", tc)

	idx.Remove("tkt-abc1")

	// Secondary indexes should be empty.
	if entries := idx.List(Filter{Label: "security"}); len(entries) != 0 {
		t.Errorf("label index still has %d entries after Remove", len(entries))
	}
	if entries := idx.List(Filter{Assignee: "@agent:bureau.local"}); len(entries) != 0 {
		t.Errorf("assignee index still has %d entries after Remove", len(entries))
	}
	if children := idx.Children("tkt-epic"); len(children) != 0 {
		t.Errorf("children index still has %d entries after Remove", len(children))
	}
}

func TestRemoveCleansDependencyGraph(t *testing.T) {
	idx := NewIndex()

	blocker := makeTicket("Blocker")
	idx.Put("tkt-a", blocker)

	dependent := makeTicket("Dependent")
	dependent.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", dependent)

	// Verify reverse edge exists.
	if blocks := idx.Blocks("tkt-a"); len(blocks) != 1 {
		t.Fatalf("Blocks before Remove = %v, want [tkt-b]", blocks)
	}

	idx.Remove("tkt-b")

	// Reverse edge should be cleaned up.
	if blocks := idx.Blocks("tkt-a"); len(blocks) != 0 {
		t.Errorf("Blocks after Remove = %v, want empty", blocks)
	}
}

// --- Ready ---

func TestReadyBasic(t *testing.T) {
	idx := NewIndex()

	// Open, no blockers, no gates → ready.
	idx.Put("tkt-a", makeTicket("Ready ticket"))

	// Closed → not ready.
	closed := makeTicket("Closed ticket")
	closed.Status = "closed"
	idx.Put("tkt-b", closed)

	// In progress → not ready.
	inProgress := makeTicket("In progress")
	inProgress.Status = "in_progress"
	idx.Put("tkt-c", inProgress)

	ready := idx.Ready()
	if len(ready) != 1 {
		t.Fatalf("Ready() returned %d entries, want 1", len(ready))
	}
	if ready[0].ID != "tkt-a" {
		t.Errorf("Ready()[0].ID = %q, want %q", ready[0].ID, "tkt-a")
	}
}

func TestReadyWithClosedBlockers(t *testing.T) {
	idx := NewIndex()

	blocker := makeTicket("Blocker")
	blocker.Status = "closed"
	idx.Put("tkt-blocker", blocker)

	dependent := makeTicket("Dependent")
	dependent.BlockedBy = []string{"tkt-blocker"}
	idx.Put("tkt-dep", dependent)

	ready := idx.Ready()
	if !containsID(ready, "tkt-dep") {
		t.Error("ticket with closed blocker should be ready")
	}
}

func TestReadyWithOpenBlockers(t *testing.T) {
	idx := NewIndex()

	blocker := makeTicket("Blocker")
	idx.Put("tkt-blocker", blocker)

	dependent := makeTicket("Dependent")
	dependent.BlockedBy = []string{"tkt-blocker"}
	idx.Put("tkt-dep", dependent)

	ready := idx.Ready()
	if containsID(ready, "tkt-dep") {
		t.Error("ticket with open blocker should not be ready")
	}
}

func TestReadyWithMissingBlocker(t *testing.T) {
	idx := NewIndex()

	dependent := makeTicket("Dependent")
	dependent.BlockedBy = []string{"tkt-missing"}
	idx.Put("tkt-dep", dependent)

	ready := idx.Ready()
	if containsID(ready, "tkt-dep") {
		t.Error("ticket with missing blocker should not be ready")
	}
}

func TestReadyWithSatisfiedGates(t *testing.T) {
	idx := NewIndex()

	gated := makeTicket("Gated ticket")
	gated.Gates = []schema.TicketGate{
		{ID: "g1", Type: "human", Status: "satisfied"},
		{ID: "g2", Type: "pipeline", Status: "satisfied", PipelineRef: "ci/test"},
	}
	idx.Put("tkt-gated", gated)

	ready := idx.Ready()
	if !containsID(ready, "tkt-gated") {
		t.Error("ticket with all gates satisfied should be ready")
	}
}

func TestReadyWithPendingGate(t *testing.T) {
	idx := NewIndex()

	gated := makeTicket("Gated ticket")
	gated.Gates = []schema.TicketGate{
		{ID: "g1", Type: "human", Status: "satisfied"},
		{ID: "g2", Type: "pipeline", Status: "pending", PipelineRef: "ci/test"},
	}
	idx.Put("tkt-gated", gated)

	ready := idx.Ready()
	if containsID(ready, "tkt-gated") {
		t.Error("ticket with pending gate should not be ready")
	}
}

func TestReadyWithNoGates(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-a", makeTicket("No gates"))

	ready := idx.Ready()
	if !containsID(ready, "tkt-a") {
		t.Error("ticket with no gates should be ready (vacuously satisfied)")
	}
}

func TestReadySortOrder(t *testing.T) {
	idx := NewIndex()

	low := makeTicket("Low priority")
	low.Priority = 3
	low.CreatedAt = "2026-02-12T09:00:00Z"
	idx.Put("tkt-low", low)

	critical := makeTicket("Critical")
	critical.Priority = 0
	critical.CreatedAt = "2026-02-12T11:00:00Z"
	idx.Put("tkt-crit", critical)

	high := makeTicket("High priority")
	high.Priority = 1
	high.CreatedAt = "2026-02-12T10:00:00Z"
	idx.Put("tkt-high", high)

	ready := idx.Ready()
	ids := entryIDs(ready)
	expected := []string{"tkt-crit", "tkt-high", "tkt-low"}
	for i, id := range expected {
		if ids[i] != id {
			t.Errorf("Ready()[%d].ID = %q, want %q (full order: %v)", i, ids[i], id, ids)
			break
		}
	}
}

func TestReadySortBreaksTiesByCreatedAt(t *testing.T) {
	idx := NewIndex()

	older := makeTicket("Older")
	older.CreatedAt = "2026-02-12T08:00:00Z"
	idx.Put("tkt-old", older)

	newer := makeTicket("Newer")
	newer.CreatedAt = "2026-02-12T12:00:00Z"
	idx.Put("tkt-new", newer)

	ready := idx.Ready()
	ids := entryIDs(ready)
	if ids[0] != "tkt-old" || ids[1] != "tkt-new" {
		t.Errorf("Ready() order = %v, want [tkt-old, tkt-new]", ids)
	}
}

// --- Blocked ---

func TestBlockedWithOpenBlocker(t *testing.T) {
	idx := NewIndex()

	blocker := makeTicket("Blocker")
	idx.Put("tkt-blocker", blocker)

	dependent := makeTicket("Dependent")
	dependent.BlockedBy = []string{"tkt-blocker"}
	idx.Put("tkt-dep", dependent)

	blocked := idx.Blocked()
	if !containsID(blocked, "tkt-dep") {
		t.Error("ticket with open blocker should appear in Blocked()")
	}
	// The blocker itself is open with no blockers → should be in Ready, not Blocked.
	if containsID(blocked, "tkt-blocker") {
		t.Error("unblocked open ticket should not appear in Blocked()")
	}
}

func TestBlockedWithPendingGate(t *testing.T) {
	idx := NewIndex()

	gated := makeTicket("Gated")
	gated.Gates = []schema.TicketGate{
		{ID: "g1", Type: "timer", Status: "pending", Duration: "24h"},
	}
	idx.Put("tkt-gated", gated)

	blocked := idx.Blocked()
	if !containsID(blocked, "tkt-gated") {
		t.Error("ticket with pending gate should appear in Blocked()")
	}
}

func TestBlockedExcludesNonOpen(t *testing.T) {
	idx := NewIndex()

	inProgress := makeTicket("Working")
	inProgress.Status = "in_progress"
	inProgress.BlockedBy = []string{"tkt-missing"}
	idx.Put("tkt-ip", inProgress)

	closed := makeTicket("Done")
	closed.Status = "closed"
	idx.Put("tkt-closed", closed)

	blocked := idx.Blocked()
	for _, entry := range blocked {
		if entry.ID == "tkt-ip" || entry.ID == "tkt-closed" {
			t.Errorf("Blocked() should only include status=open tickets, got %q with status %q",
				entry.ID, entry.Content.Status)
		}
	}
}

func TestReadyAndBlockedPartitionOpen(t *testing.T) {
	idx := NewIndex()

	// Open, no blockers → ready.
	idx.Put("tkt-a", makeTicket("A"))

	// Open, blocked → blocked.
	blocker := makeTicket("Blocker")
	idx.Put("tkt-blocker", blocker)
	blocked := makeTicket("Blocked")
	blocked.BlockedBy = []string{"tkt-blocker"}
	idx.Put("tkt-b", blocked)

	// Open, pending gate → blocked.
	gated := makeTicket("Gated")
	gated.Gates = []schema.TicketGate{{ID: "g1", Type: "human", Status: "pending"}}
	idx.Put("tkt-c", gated)

	// Closed → neither.
	done := makeTicket("Done")
	done.Status = "closed"
	idx.Put("tkt-d", done)

	ready := idx.Ready()
	blockedEntries := idx.Blocked()

	// tkt-a and tkt-blocker are open with no open blockers and no pending gates.
	readyIDs := entryIDs(ready)
	if !containsID(ready, "tkt-a") {
		t.Errorf("tkt-a should be ready, ready=%v", readyIDs)
	}
	if !containsID(ready, "tkt-blocker") {
		t.Errorf("tkt-blocker should be ready, ready=%v", readyIDs)
	}

	blockedIDs := entryIDs(blockedEntries)
	if !containsID(blockedEntries, "tkt-b") {
		t.Errorf("tkt-b should be blocked, blocked=%v", blockedIDs)
	}
	if !containsID(blockedEntries, "tkt-c") {
		t.Errorf("tkt-c should be blocked, blocked=%v", blockedIDs)
	}

	// No ticket should appear in both.
	for _, r := range ready {
		if containsID(blockedEntries, r.ID) {
			t.Errorf("ticket %q appears in both Ready and Blocked", r.ID)
		}
	}

	// Closed ticket should appear in neither.
	if containsID(ready, "tkt-d") || containsID(blockedEntries, "tkt-d") {
		t.Error("closed ticket should not appear in Ready or Blocked")
	}
}

// --- List ---

func TestListAll(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-a", makeTicket("A"))
	idx.Put("tkt-b", makeTicket("B"))

	entries := idx.List(Filter{})
	if len(entries) != 2 {
		t.Fatalf("List(empty filter) returned %d entries, want 2", len(entries))
	}
}

func TestListByStatus(t *testing.T) {
	idx := NewIndex()
	open := makeTicket("Open")
	idx.Put("tkt-open", open)

	closed := makeTicket("Closed")
	closed.Status = "closed"
	idx.Put("tkt-closed", closed)

	entries := idx.List(Filter{Status: "closed"})
	if len(entries) != 1 || entries[0].ID != "tkt-closed" {
		t.Errorf("List(status=closed) = %v, want [tkt-closed]", entryIDs(entries))
	}
}

func TestListByPriority(t *testing.T) {
	idx := NewIndex()

	critical := makeTicket("Critical")
	critical.Priority = 0
	idx.Put("tkt-crit", critical)

	low := makeTicket("Low")
	low.Priority = 3
	idx.Put("tkt-low", low)

	entries := idx.List(Filter{Priority: intPtr(0)})
	if len(entries) != 1 || entries[0].ID != "tkt-crit" {
		t.Errorf("List(priority=0) = %v, want [tkt-crit]", entryIDs(entries))
	}
}

func TestListByLabel(t *testing.T) {
	idx := NewIndex()

	labeled := makeTicket("Labeled")
	labeled.Labels = []string{"security", "amdgpu"}
	idx.Put("tkt-labeled", labeled)

	unlabeled := makeTicket("Unlabeled")
	idx.Put("tkt-unlabeled", unlabeled)

	entries := idx.List(Filter{Label: "security"})
	if len(entries) != 1 || entries[0].ID != "tkt-labeled" {
		t.Errorf("List(label=security) = %v, want [tkt-labeled]", entryIDs(entries))
	}
}

func TestListByAssignee(t *testing.T) {
	idx := NewIndex()

	assigned := makeTicket("Assigned")
	assigned.Assignee = "@pm:bureau.local"
	idx.Put("tkt-assigned", assigned)

	unassigned := makeTicket("Unassigned")
	idx.Put("tkt-unassigned", unassigned)

	entries := idx.List(Filter{Assignee: "@pm:bureau.local"})
	if len(entries) != 1 || entries[0].ID != "tkt-assigned" {
		t.Errorf("List(assignee=@pm:bureau.local) = %v, want [tkt-assigned]", entryIDs(entries))
	}
}

func TestListByType(t *testing.T) {
	idx := NewIndex()

	bug := makeTicket("Bug")
	bug.Type = "bug"
	idx.Put("tkt-bug", bug)

	task := makeTicket("Task")
	task.Type = "task"
	idx.Put("tkt-task", task)

	entries := idx.List(Filter{Type: "bug"})
	if len(entries) != 1 || entries[0].ID != "tkt-bug" {
		t.Errorf("List(type=bug) = %v, want [tkt-bug]", entryIDs(entries))
	}
}

func TestListByParent(t *testing.T) {
	idx := NewIndex()

	child := makeTicket("Child")
	child.Parent = "tkt-epic"
	idx.Put("tkt-child", child)

	orphan := makeTicket("Orphan")
	idx.Put("tkt-orphan", orphan)

	entries := idx.List(Filter{Parent: "tkt-epic"})
	if len(entries) != 1 || entries[0].ID != "tkt-child" {
		t.Errorf("List(parent=tkt-epic) = %v, want [tkt-child]", entryIDs(entries))
	}
}

func TestListMultipleFilters(t *testing.T) {
	idx := NewIndex()

	// Matches both status and type.
	match := makeTicket("Match")
	match.Status = "open"
	match.Type = "bug"
	idx.Put("tkt-match", match)

	// Matches status but not type.
	noTypeMatch := makeTicket("Wrong type")
	noTypeMatch.Status = "open"
	noTypeMatch.Type = "task"
	idx.Put("tkt-nomatch1", noTypeMatch)

	// Matches type but not status.
	noStatusMatch := makeTicket("Wrong status")
	noStatusMatch.Status = "closed"
	noStatusMatch.Type = "bug"
	idx.Put("tkt-nomatch2", noStatusMatch)

	entries := idx.List(Filter{Status: "open", Type: "bug"})
	if len(entries) != 1 || entries[0].ID != "tkt-match" {
		t.Errorf("List(status=open,type=bug) = %v, want [tkt-match]", entryIDs(entries))
	}
}

func TestListNoResults(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-a", makeTicket("A"))

	entries := idx.List(Filter{Status: "closed"})
	if len(entries) != 0 {
		t.Errorf("List with no matching tickets returned %d entries, want 0", len(entries))
	}
}

// --- Grep ---

func TestGrepMatchesTitle(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-a", makeTicket("Fix authentication bug"))
	idx.Put("tkt-b", makeTicket("Add logging"))

	entries, err := idx.Grep("auth")
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "tkt-a" {
		t.Errorf("Grep(auth) = %v, want [tkt-a]", entryIDs(entries))
	}
}

func TestGrepMatchesBody(t *testing.T) {
	idx := NewIndex()

	tc := makeTicket("Some ticket")
	tc.Body = "The authentication flow is broken in production."
	idx.Put("tkt-a", tc)

	entries, err := idx.Grep("production")
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "tkt-a" {
		t.Errorf("Grep(production) = %v, want [tkt-a]", entryIDs(entries))
	}
}

func TestGrepMatchesNoteBody(t *testing.T) {
	idx := NewIndex()

	tc := makeTicket("Some ticket")
	tc.Notes = []schema.TicketNote{
		{ID: "n-1", Author: "@a:b.c", CreatedAt: "2026-02-12T10:00:00Z", Body: "Found a race condition in the handler"},
	}
	idx.Put("tkt-a", tc)

	entries, err := idx.Grep("race condition")
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("Grep should match note body, got %d results", len(entries))
	}
}

func TestGrepRegex(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-a", makeTicket("Fix bug #123"))
	idx.Put("tkt-b", makeTicket("Fix bug #456"))
	idx.Put("tkt-c", makeTicket("Add feature"))

	entries, err := idx.Grep(`bug #\d+`)
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("Grep(regex) returned %d entries, want 2", len(entries))
	}
}

func TestGrepInvalidRegex(t *testing.T) {
	idx := NewIndex()
	_, err := idx.Grep("[invalid")
	if err == nil {
		t.Fatal("Grep should return error for invalid regex")
	}
}

func TestGrepNoMatches(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-a", makeTicket("Something"))

	entries, err := idx.Grep("nonexistent")
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Grep(nonexistent) returned %d entries, want 0", len(entries))
	}
}

// --- Children ---

func TestChildren(t *testing.T) {
	idx := NewIndex()

	epic := makeTicket("Epic")
	epic.Type = "epic"
	idx.Put("tkt-epic", epic)

	child1 := makeTicket("Child 1")
	child1.Parent = "tkt-epic"
	child1.Priority = 1
	idx.Put("tkt-c1", child1)

	child2 := makeTicket("Child 2")
	child2.Parent = "tkt-epic"
	child2.Priority = 0
	idx.Put("tkt-c2", child2)

	unrelated := makeTicket("Unrelated")
	idx.Put("tkt-other", unrelated)

	children := idx.Children("tkt-epic")
	if len(children) != 2 {
		t.Fatalf("Children returned %d entries, want 2", len(children))
	}
	// Should be sorted by priority: c2 (P0) before c1 (P1).
	if children[0].ID != "tkt-c2" {
		t.Errorf("Children[0].ID = %q, want %q (sorted by priority)", children[0].ID, "tkt-c2")
	}
}

func TestChildrenEmpty(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-a", makeTicket("No children"))

	children := idx.Children("tkt-a")
	if children != nil {
		t.Errorf("Children of ticket with no children should be nil, got %v", children)
	}
}

func TestChildrenUpdatedOnParentChange(t *testing.T) {
	idx := NewIndex()

	child := makeTicket("Child")
	child.Parent = "tkt-epic1"
	idx.Put("tkt-child", child)

	if len(idx.Children("tkt-epic1")) != 1 {
		t.Fatal("child should appear under original parent")
	}

	// Reparent the child.
	child.Parent = "tkt-epic2"
	idx.Put("tkt-child", child)

	if len(idx.Children("tkt-epic1")) != 0 {
		t.Error("child should be removed from old parent's children")
	}
	if len(idx.Children("tkt-epic2")) != 1 {
		t.Error("child should appear under new parent")
	}
}

// --- ChildProgress ---

func TestChildProgress(t *testing.T) {
	idx := NewIndex()

	epic := makeTicket("Epic")
	epic.Type = "epic"
	idx.Put("tkt-epic", epic)

	child1 := makeTicket("Child 1")
	child1.Parent = "tkt-epic"
	child1.Status = "closed"
	idx.Put("tkt-c1", child1)

	child2 := makeTicket("Child 2")
	child2.Parent = "tkt-epic"
	child2.Status = "open"
	idx.Put("tkt-c2", child2)

	child3 := makeTicket("Child 3")
	child3.Parent = "tkt-epic"
	child3.Status = "closed"
	idx.Put("tkt-c3", child3)

	total, closed := idx.ChildProgress("tkt-epic")
	if total != 3 {
		t.Errorf("ChildProgress total = %d, want 3", total)
	}
	if closed != 2 {
		t.Errorf("ChildProgress closed = %d, want 2", closed)
	}
}

func TestChildProgressNoChildren(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-a", makeTicket("No children"))

	total, closed := idx.ChildProgress("tkt-a")
	if total != 0 || closed != 0 {
		t.Errorf("ChildProgress of childless ticket = (%d, %d), want (0, 0)", total, closed)
	}
}

// --- PendingGates ---

func TestPendingGates(t *testing.T) {
	idx := NewIndex()

	// No gates.
	idx.Put("tkt-a", makeTicket("No gates"))

	// All gates satisfied.
	allSatisfied := makeTicket("Satisfied")
	allSatisfied.Gates = []schema.TicketGate{
		{ID: "g1", Type: "human", Status: "satisfied"},
	}
	idx.Put("tkt-b", allSatisfied)

	// Has pending gate.
	pending := makeTicket("Pending")
	pending.Gates = []schema.TicketGate{
		{ID: "g1", Type: "human", Status: "satisfied"},
		{ID: "g2", Type: "pipeline", Status: "pending", PipelineRef: "ci/test"},
	}
	idx.Put("tkt-c", pending)

	result := idx.PendingGates()
	if len(result) != 1 {
		t.Fatalf("PendingGates() returned %d entries, want 1", len(result))
	}
	if result[0].ID != "tkt-c" {
		t.Errorf("PendingGates()[0].ID = %q, want tkt-c", result[0].ID)
	}
}

func TestPendingGatesEmpty(t *testing.T) {
	idx := NewIndex()
	result := idx.PendingGates()
	if len(result) != 0 {
		t.Errorf("PendingGates on empty index = %d, want 0", len(result))
	}
}

// --- Stats ---

func TestStats(t *testing.T) {
	idx := NewIndex()

	tc1 := makeTicket("Task 1")
	tc1.Status = "open"
	tc1.Priority = 2
	tc1.Type = "task"
	idx.Put("tkt-a", tc1)

	tc2 := makeTicket("Bug 1")
	tc2.Status = "open"
	tc2.Priority = 0
	tc2.Type = "bug"
	idx.Put("tkt-b", tc2)

	tc3 := makeTicket("Task 2")
	tc3.Status = "closed"
	tc3.Priority = 2
	tc3.Type = "task"
	idx.Put("tkt-c", tc3)

	stats := idx.Stats()

	if stats.Total != 3 {
		t.Errorf("Total = %d, want 3", stats.Total)
	}
	if stats.ByStatus["open"] != 2 {
		t.Errorf("ByStatus[open] = %d, want 2", stats.ByStatus["open"])
	}
	if stats.ByStatus["closed"] != 1 {
		t.Errorf("ByStatus[closed] = %d, want 1", stats.ByStatus["closed"])
	}
	if stats.ByPriority[0] != 1 {
		t.Errorf("ByPriority[0] = %d, want 1", stats.ByPriority[0])
	}
	if stats.ByPriority[2] != 2 {
		t.Errorf("ByPriority[2] = %d, want 2", stats.ByPriority[2])
	}
	if stats.ByType["task"] != 2 {
		t.Errorf("ByType[task] = %d, want 2", stats.ByType["task"])
	}
	if stats.ByType["bug"] != 1 {
		t.Errorf("ByType[bug] = %d, want 1", stats.ByType["bug"])
	}
}

func TestStatsEmpty(t *testing.T) {
	idx := NewIndex()
	stats := idx.Stats()
	if stats.Total != 0 {
		t.Errorf("empty index Total = %d, want 0", stats.Total)
	}
}

// --- Deps ---

func TestDepsLinearChain(t *testing.T) {
	idx := NewIndex()

	// A ← B ← C (C depends on B depends on A)
	idx.Put("tkt-a", makeTicket("A"))

	depB := makeTicket("B")
	depB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", depB)

	depC := makeTicket("C")
	depC.BlockedBy = []string{"tkt-b"}
	idx.Put("tkt-c", depC)

	deps := idx.Deps("tkt-c")
	if len(deps) != 2 {
		t.Fatalf("Deps(tkt-c) = %v, want [tkt-a, tkt-b]", deps)
	}
	// Sorted alphabetically.
	if deps[0] != "tkt-a" || deps[1] != "tkt-b" {
		t.Errorf("Deps(tkt-c) = %v, want [tkt-a, tkt-b]", deps)
	}
}

func TestDepsDiamond(t *testing.T) {
	idx := NewIndex()

	// Diamond: D depends on B and C, both depend on A.
	idx.Put("tkt-a", makeTicket("A"))

	depB := makeTicket("B")
	depB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", depB)

	depC := makeTicket("C")
	depC.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-c", depC)

	depD := makeTicket("D")
	depD.BlockedBy = []string{"tkt-b", "tkt-c"}
	idx.Put("tkt-d", depD)

	deps := idx.Deps("tkt-d")
	if len(deps) != 3 {
		t.Fatalf("Deps(tkt-d) = %v, want 3 entries", deps)
	}
	// A appears only once despite being reachable via two paths.
	for _, expected := range []string{"tkt-a", "tkt-b", "tkt-c"} {
		found := false
		for _, d := range deps {
			if d == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Deps(tkt-d) missing %q, got %v", expected, deps)
		}
	}
}

func TestDepsNoDependencies(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-a", makeTicket("Standalone"))

	deps := idx.Deps("tkt-a")
	if deps != nil {
		t.Errorf("Deps of standalone ticket = %v, want nil", deps)
	}
}

func TestDepsNonexistentTicket(t *testing.T) {
	idx := NewIndex()
	deps := idx.Deps("tkt-nope")
	if deps != nil {
		t.Errorf("Deps of nonexistent ticket = %v, want nil", deps)
	}
}

func TestDepsMissingIntermediateTicket(t *testing.T) {
	idx := NewIndex()

	// B depends on A, but A doesn't exist in the index.
	depB := makeTicket("B")
	depB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", depB)

	// C depends on B.
	depC := makeTicket("C")
	depC.BlockedBy = []string{"tkt-b"}
	idx.Put("tkt-c", depC)

	deps := idx.Deps("tkt-c")
	// Should find tkt-b and tkt-a (tkt-a is referenced but missing;
	// it's still listed as a dependency).
	if len(deps) != 2 {
		t.Fatalf("Deps(tkt-c) = %v, want [tkt-a, tkt-b]", deps)
	}
}

// --- Blocks (reverse deps) ---

func TestBlocks(t *testing.T) {
	idx := NewIndex()

	idx.Put("tkt-a", makeTicket("A"))

	depB := makeTicket("B")
	depB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", depB)

	depC := makeTicket("C")
	depC.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-c", depC)

	blocks := idx.Blocks("tkt-a")
	if len(blocks) != 2 {
		t.Fatalf("Blocks(tkt-a) = %v, want 2 entries", blocks)
	}
}

func TestBlocksNoDependents(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-a", makeTicket("Leaf"))

	blocks := idx.Blocks("tkt-a")
	if blocks != nil {
		t.Errorf("Blocks of leaf = %v, want nil", blocks)
	}
}

// --- WouldCycle ---

func TestWouldCycleSelfReference(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-a", makeTicket("A"))

	if !idx.WouldCycle("tkt-a", []string{"tkt-a"}) {
		t.Error("self-reference should be detected as a cycle")
	}
}

func TestWouldCycleDirectCycle(t *testing.T) {
	idx := NewIndex()

	// A depends on B.
	depA := makeTicket("A")
	depA.BlockedBy = []string{"tkt-b"}
	idx.Put("tkt-a", depA)

	idx.Put("tkt-b", makeTicket("B"))

	// Adding B depends on A would create: A → B → A.
	if !idx.WouldCycle("tkt-b", []string{"tkt-a"}) {
		t.Error("direct cycle A→B→A should be detected")
	}
}

func TestWouldCycleTransitiveCycle(t *testing.T) {
	idx := NewIndex()

	// A → B → C (A depends on B, B depends on C).
	depA := makeTicket("A")
	depA.BlockedBy = []string{"tkt-b"}
	idx.Put("tkt-a", depA)

	depB := makeTicket("B")
	depB.BlockedBy = []string{"tkt-c"}
	idx.Put("tkt-b", depB)

	idx.Put("tkt-c", makeTicket("C"))

	// Adding C depends on A would create: A → B → C → A.
	if !idx.WouldCycle("tkt-c", []string{"tkt-a"}) {
		t.Error("transitive cycle A→B→C→A should be detected")
	}
}

func TestWouldCycleNoCycle(t *testing.T) {
	idx := NewIndex()

	idx.Put("tkt-a", makeTicket("A"))
	idx.Put("tkt-b", makeTicket("B"))
	idx.Put("tkt-c", makeTicket("C"))

	// A depends on B is fine (no cycle).
	if idx.WouldCycle("tkt-a", []string{"tkt-b"}) {
		t.Error("A→B with no existing edges should not be a cycle")
	}
}

func TestWouldCycleNonexistentTarget(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-a", makeTicket("A"))

	// Depending on a nonexistent ticket is not a cycle (it's a
	// dangling reference, but not cyclic).
	if idx.WouldCycle("tkt-a", []string{"tkt-missing"}) {
		t.Error("depending on nonexistent ticket should not be a cycle")
	}
}

func TestWouldCycleMultipleProposed(t *testing.T) {
	idx := NewIndex()

	// Existing: B → A.
	depB := makeTicket("B")
	depB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", depB)

	idx.Put("tkt-a", makeTicket("A"))
	idx.Put("tkt-c", makeTicket("C"))

	// Adding A depends on [B, C]: B already depends on A, so A→B
	// is a cycle. C is fine.
	if !idx.WouldCycle("tkt-a", []string{"tkt-b", "tkt-c"}) {
		t.Error("should detect cycle through any proposed blocker")
	}
}

// --- Index update correctness ---

func TestSecondaryIndexesUpdatedOnPut(t *testing.T) {
	idx := NewIndex()

	tc := makeTicket("Original")
	tc.Status = "open"
	tc.Labels = []string{"frontend"}
	tc.Assignee = "@agent1:bureau.local"
	idx.Put("tkt-a", tc)

	// Verify initial state.
	if entries := idx.List(Filter{Status: "open"}); len(entries) != 1 {
		t.Fatalf("initial List(status=open) = %d, want 1", len(entries))
	}
	if entries := idx.List(Filter{Label: "frontend"}); len(entries) != 1 {
		t.Fatalf("initial List(label=frontend) = %d, want 1", len(entries))
	}

	// Update: change status, labels, and assignee.
	tc.Status = "in_progress"
	tc.Labels = []string{"backend"}
	tc.Assignee = "@agent2:bureau.local"
	idx.Put("tkt-a", tc)

	// Old values should be gone.
	if entries := idx.List(Filter{Status: "open"}); len(entries) != 0 {
		t.Errorf("List(status=open) after update = %d, want 0", len(entries))
	}
	if entries := idx.List(Filter{Label: "frontend"}); len(entries) != 0 {
		t.Errorf("List(label=frontend) after update = %d, want 0", len(entries))
	}
	if entries := idx.List(Filter{Assignee: "@agent1:bureau.local"}); len(entries) != 0 {
		t.Errorf("List(assignee=agent1) after update = %d, want 0", len(entries))
	}

	// New values should be present.
	if entries := idx.List(Filter{Status: "in_progress"}); len(entries) != 1 {
		t.Errorf("List(status=in_progress) after update = %d, want 1", len(entries))
	}
	if entries := idx.List(Filter{Label: "backend"}); len(entries) != 1 {
		t.Errorf("List(label=backend) after update = %d, want 1", len(entries))
	}
	if entries := idx.List(Filter{Assignee: "@agent2:bureau.local"}); len(entries) != 1 {
		t.Errorf("List(assignee=agent2) after update = %d, want 1", len(entries))
	}
}

func TestDependencyGraphUpdatedOnPut(t *testing.T) {
	idx := NewIndex()

	idx.Put("tkt-a", makeTicket("A"))
	idx.Put("tkt-b", makeTicket("B"))
	idx.Put("tkt-c", makeTicket("C"))

	// C depends on A.
	depC := makeTicket("C")
	depC.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-c", depC)

	if blocks := idx.Blocks("tkt-a"); len(blocks) != 1 || blocks[0] != "tkt-c" {
		t.Errorf("Blocks(tkt-a) = %v, want [tkt-c]", blocks)
	}

	// Change: C now depends on B instead of A.
	depC.BlockedBy = []string{"tkt-b"}
	idx.Put("tkt-c", depC)

	if blocks := idx.Blocks("tkt-a"); len(blocks) != 0 {
		t.Errorf("Blocks(tkt-a) after change = %v, want []", blocks)
	}
	if blocks := idx.Blocks("tkt-b"); len(blocks) != 1 || blocks[0] != "tkt-c" {
		t.Errorf("Blocks(tkt-b) after change = %v, want [tkt-c]", blocks)
	}
}

func TestReadyRecomputesWhenBlockerCloses(t *testing.T) {
	idx := NewIndex()

	blocker := makeTicket("Blocker")
	idx.Put("tkt-blocker", blocker)

	dependent := makeTicket("Dependent")
	dependent.BlockedBy = []string{"tkt-blocker"}
	idx.Put("tkt-dep", dependent)

	// Initially: dependent is blocked.
	if containsID(idx.Ready(), "tkt-dep") {
		t.Fatal("dependent should not be ready while blocker is open")
	}

	// Close the blocker.
	blocker.Status = "closed"
	idx.Put("tkt-blocker", blocker)

	// Now: dependent should be ready.
	if !containsID(idx.Ready(), "tkt-dep") {
		t.Error("dependent should be ready after blocker closes")
	}
}

func TestReadyRecomputesWhenGateSatisfied(t *testing.T) {
	idx := NewIndex()

	gated := makeTicket("Gated")
	gated.Gates = []schema.TicketGate{
		{ID: "g1", Type: "human", Status: "pending"},
	}
	idx.Put("tkt-gated", gated)

	if containsID(idx.Ready(), "tkt-gated") {
		t.Fatal("gated ticket should not be ready with pending gate")
	}

	// Satisfy the gate.
	gated.Gates[0].Status = "satisfied"
	idx.Put("tkt-gated", gated)

	if !containsID(idx.Ready(), "tkt-gated") {
		t.Error("gated ticket should be ready after gate is satisfied")
	}
}

// --- Edge cases ---

func TestPutMultipleLabels(t *testing.T) {
	idx := NewIndex()

	tc := makeTicket("Multi-label")
	tc.Labels = []string{"security", "amdgpu", "p0"}
	idx.Put("tkt-a", tc)

	for _, label := range []string{"security", "amdgpu", "p0"} {
		entries := idx.List(Filter{Label: label})
		if len(entries) != 1 {
			t.Errorf("List(label=%s) = %d, want 1", label, len(entries))
		}
	}
}

func TestPutMultipleBlockedBy(t *testing.T) {
	idx := NewIndex()

	idx.Put("tkt-a", makeTicket("A"))
	idx.Put("tkt-b", makeTicket("B"))

	dep := makeTicket("Dependent")
	dep.BlockedBy = []string{"tkt-a", "tkt-b"}
	idx.Put("tkt-dep", dep)

	// Both should show up as blocking.
	if blocks := idx.Blocks("tkt-a"); len(blocks) != 1 {
		t.Errorf("Blocks(tkt-a) = %v, want [tkt-dep]", blocks)
	}
	if blocks := idx.Blocks("tkt-b"); len(blocks) != 1 {
		t.Errorf("Blocks(tkt-b) = %v, want [tkt-dep]", blocks)
	}

	// Dependent should show both in Deps.
	deps := idx.Deps("tkt-dep")
	if len(deps) != 2 {
		t.Errorf("Deps(tkt-dep) = %v, want [tkt-a, tkt-b]", deps)
	}
}

func TestEmptyIndex(t *testing.T) {
	idx := NewIndex()

	if ready := idx.Ready(); len(ready) != 0 {
		t.Errorf("Ready on empty index = %v, want empty", ready)
	}
	if blocked := idx.Blocked(); len(blocked) != 0 {
		t.Errorf("Blocked on empty index = %v, want empty", blocked)
	}
	if entries := idx.List(Filter{}); len(entries) != 0 {
		t.Errorf("List on empty index = %v, want empty", entries)
	}
	entries, err := idx.Grep("anything")
	if err != nil {
		t.Fatalf("Grep on empty index: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Grep on empty index = %v, want empty", entries)
	}
	stats := idx.Stats()
	if stats.Total != 0 {
		t.Errorf("Stats.Total on empty index = %d, want 0", stats.Total)
	}
}

func TestGrepCaseInsensitiveWithFlag(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-a", makeTicket("Fix Authentication Bug"))

	// Regex with case-insensitive flag.
	entries, err := idx.Grep("(?i)authentication")
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("case-insensitive Grep returned %d, want 1", len(entries))
	}
}

// --- Scoring and ranking tests ---

// fixedNow is a deterministic timestamp used for all scoring tests.
// Three days after the default makeTicket creation time.
var fixedNow = time.Date(2026, 2, 15, 10, 0, 0, 0, time.UTC)

func TestUnblockScoreNoDependents(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-a", makeTicket("A"))

	if score := idx.UnblockScore("tkt-a"); score != 0 {
		t.Errorf("UnblockScore(no dependents) = %d, want 0", score)
	}
}

func TestUnblockScoreNonexistent(t *testing.T) {
	idx := NewIndex()
	if score := idx.UnblockScore("tkt-missing"); score != 0 {
		t.Errorf("UnblockScore(nonexistent) = %d, want 0", score)
	}
}

func TestUnblockScoreLinearChain(t *testing.T) {
	idx := NewIndex()

	// A (open) ← B (open, blocked by A) ← C (open, blocked by B).
	// Closing A makes B ready (B's only blocker). B still blocks C.
	idx.Put("tkt-a", makeTicket("A"))

	depB := makeTicket("B")
	depB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", depB)

	depC := makeTicket("C")
	depC.BlockedBy = []string{"tkt-b"}
	idx.Put("tkt-c", depC)

	if score := idx.UnblockScore("tkt-a"); score != 1 {
		t.Errorf("UnblockScore(A) = %d, want 1 (only B becomes ready)", score)
	}
	if score := idx.UnblockScore("tkt-b"); score != 1 {
		t.Errorf("UnblockScore(B) = %d, want 1 (C becomes ready if B closes)", score)
	}
}

func TestUnblockScoreDiamond(t *testing.T) {
	idx := NewIndex()

	// A (closed) blocks B and C. B and C both block D.
	// Closing B: D still blocked by C → score 0.
	// Closing C: D still blocked by B → score 0.
	closedA := makeTicket("A")
	closedA.Status = "closed"
	closedA.ClosedAt = "2026-02-13T10:00:00Z"
	idx.Put("tkt-a", closedA)

	depB := makeTicket("B")
	depB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", depB)

	depC := makeTicket("C")
	depC.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-c", depC)

	depD := makeTicket("D")
	depD.BlockedBy = []string{"tkt-b", "tkt-c"}
	idx.Put("tkt-d", depD)

	if score := idx.UnblockScore("tkt-b"); score != 0 {
		t.Errorf("UnblockScore(B) = %d, want 0 (D still blocked by C)", score)
	}
	if score := idx.UnblockScore("tkt-c"); score != 0 {
		t.Errorf("UnblockScore(C) = %d, want 0 (D still blocked by B)", score)
	}
}

func TestUnblockScoreLastBlocker(t *testing.T) {
	idx := NewIndex()

	// A (closed) and B (open) both block C. B is the last open blocker.
	closedA := makeTicket("A")
	closedA.Status = "closed"
	closedA.ClosedAt = "2026-02-13T10:00:00Z"
	idx.Put("tkt-a", closedA)

	idx.Put("tkt-b", makeTicket("B"))

	depC := makeTicket("C")
	depC.BlockedBy = []string{"tkt-a", "tkt-b"}
	idx.Put("tkt-c", depC)

	if score := idx.UnblockScore("tkt-b"); score != 1 {
		t.Errorf("UnblockScore(B) = %d, want 1 (B is last blocker of C)", score)
	}
}

func TestUnblockScoreFanOut(t *testing.T) {
	idx := NewIndex()

	// A blocks B, C, D — all have A as sole blocker.
	idx.Put("tkt-a", makeTicket("A"))
	for _, id := range []string{"tkt-b", "tkt-c", "tkt-d"} {
		dep := makeTicket(id)
		dep.BlockedBy = []string{"tkt-a"}
		idx.Put(id, dep)
	}

	if score := idx.UnblockScore("tkt-a"); score != 3 {
		t.Errorf("UnblockScore(A) = %d, want 3", score)
	}
}

func TestUnblockScoreIgnoresClosedDependents(t *testing.T) {
	idx := NewIndex()

	// A blocks B, but B is already closed.
	idx.Put("tkt-a", makeTicket("A"))

	closedB := makeTicket("B")
	closedB.Status = "closed"
	closedB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", closedB)

	if score := idx.UnblockScore("tkt-a"); score != 0 {
		t.Errorf("UnblockScore(A) = %d, want 0 (B already closed)", score)
	}
}

func TestUnblockScoreRequiresGatesSatisfied(t *testing.T) {
	idx := NewIndex()

	// A blocks B, but B also has an unsatisfied gate.
	idx.Put("tkt-a", makeTicket("A"))

	depB := makeTicket("B")
	depB.BlockedBy = []string{"tkt-a"}
	depB.Gates = []schema.TicketGate{{ID: "g1", Status: "pending"}}
	idx.Put("tkt-b", depB)

	if score := idx.UnblockScore("tkt-a"); score != 0 {
		t.Errorf("UnblockScore(A) = %d, want 0 (B has pending gate)", score)
	}
}

func TestBorrowedPriorityNoDependents(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-a", makeTicket("A"))

	if bp := idx.BorrowedPriority("tkt-a"); bp != -1 {
		t.Errorf("BorrowedPriority(no dependents) = %d, want -1", bp)
	}
}

func TestBorrowedPriorityNonexistent(t *testing.T) {
	idx := NewIndex()
	if bp := idx.BorrowedPriority("tkt-missing"); bp != -1 {
		t.Errorf("BorrowedPriority(nonexistent) = %d, want -1", bp)
	}
}

func TestBorrowedPriorityDirect(t *testing.T) {
	idx := NewIndex()

	// A (P3) blocks B (P0). A's borrowed priority is 0.
	ticketA := makeTicket("A")
	ticketA.Priority = 3
	idx.Put("tkt-a", ticketA)

	ticketB := makeTicket("B")
	ticketB.Priority = 0
	ticketB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", ticketB)

	if bp := idx.BorrowedPriority("tkt-a"); bp != 0 {
		t.Errorf("BorrowedPriority(A) = %d, want 0", bp)
	}
}

func TestBorrowedPriorityTransitive(t *testing.T) {
	idx := NewIndex()

	// A (P3) ← B (P2) ← C (P0). A's borrowed priority is 0 (from C).
	ticketA := makeTicket("A")
	ticketA.Priority = 3
	idx.Put("tkt-a", ticketA)

	ticketB := makeTicket("B")
	ticketB.Priority = 2
	ticketB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", ticketB)

	ticketC := makeTicket("C")
	ticketC.Priority = 0
	ticketC.BlockedBy = []string{"tkt-b"}
	idx.Put("tkt-c", ticketC)

	if bp := idx.BorrowedPriority("tkt-a"); bp != 0 {
		t.Errorf("BorrowedPriority(A) = %d, want 0 (transitive from C)", bp)
	}
}

func TestBorrowedPriorityTakesMinimum(t *testing.T) {
	idx := NewIndex()

	// A blocks B (P3) and C (P1). Borrowed priority is 1 (min).
	ticketA := makeTicket("A")
	ticketA.Priority = 4
	idx.Put("tkt-a", ticketA)

	ticketB := makeTicket("B")
	ticketB.Priority = 3
	ticketB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", ticketB)

	ticketC := makeTicket("C")
	ticketC.Priority = 1
	ticketC.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-c", ticketC)

	if bp := idx.BorrowedPriority("tkt-a"); bp != 1 {
		t.Errorf("BorrowedPriority(A) = %d, want 1 (min of B=3, C=1)", bp)
	}
}

func TestCriticalDepthNoChildren(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-epic", makeTicket("Epic"))

	if depth := idx.CriticalDepth("tkt-epic"); depth != 0 {
		t.Errorf("CriticalDepth(no children) = %d, want 0", depth)
	}
}

func TestCriticalDepthIndependentChildren(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-epic", makeTicket("Epic"))

	// Three independent children — no inter-child deps → depth 0.
	for _, id := range []string{"tkt-a", "tkt-b", "tkt-c"} {
		child := makeTicket(id)
		child.Parent = "tkt-epic"
		idx.Put(id, child)
	}

	if depth := idx.CriticalDepth("tkt-epic"); depth != 0 {
		t.Errorf("CriticalDepth(independent children) = %d, want 0", depth)
	}
}

func TestCriticalDepthChain(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-epic", makeTicket("Epic"))

	// A ← B ← C (all children of epic). Chain of depth 2.
	childA := makeTicket("A")
	childA.Parent = "tkt-epic"
	idx.Put("tkt-a", childA)

	childB := makeTicket("B")
	childB.Parent = "tkt-epic"
	childB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", childB)

	childC := makeTicket("C")
	childC.Parent = "tkt-epic"
	childC.BlockedBy = []string{"tkt-b"}
	idx.Put("tkt-c", childC)

	if depth := idx.CriticalDepth("tkt-epic"); depth != 2 {
		t.Errorf("CriticalDepth(chain A←B←C) = %d, want 2", depth)
	}
}

func TestCriticalDepthIgnoresClosedChildren(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-epic", makeTicket("Epic"))

	// A (closed) ← B (open). Only B is open, A is closed → depth 0.
	closedA := makeTicket("A")
	closedA.Parent = "tkt-epic"
	closedA.Status = "closed"
	idx.Put("tkt-a", closedA)

	childB := makeTicket("B")
	childB.Parent = "tkt-epic"
	childB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", childB)

	if depth := idx.CriticalDepth("tkt-epic"); depth != 0 {
		t.Errorf("CriticalDepth(closed blocker) = %d, want 0", depth)
	}
}

func TestCriticalDepthIgnoresExternalBlockers(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-epic", makeTicket("Epic"))

	// External ticket (not a child of epic) blocks child B.
	// External edges are not counted for critical depth.
	idx.Put("tkt-external", makeTicket("External"))

	childB := makeTicket("B")
	childB.Parent = "tkt-epic"
	childB.BlockedBy = []string{"tkt-external"}
	idx.Put("tkt-b", childB)

	if depth := idx.CriticalDepth("tkt-epic"); depth != 0 {
		t.Errorf("CriticalDepth(external blocker) = %d, want 0", depth)
	}
}

func TestCriticalDepthDiamond(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-epic", makeTicket("Epic"))

	// A ← B, A ← C, B ← D, C ← D. Diamond with depth 2.
	childA := makeTicket("A")
	childA.Parent = "tkt-epic"
	idx.Put("tkt-a", childA)

	childB := makeTicket("B")
	childB.Parent = "tkt-epic"
	childB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", childB)

	childC := makeTicket("C")
	childC.Parent = "tkt-epic"
	childC.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-c", childC)

	childD := makeTicket("D")
	childD.Parent = "tkt-epic"
	childD.BlockedBy = []string{"tkt-b", "tkt-c"}
	idx.Put("tkt-d", childD)

	if depth := idx.CriticalDepth("tkt-epic"); depth != 2 {
		t.Errorf("CriticalDepth(diamond) = %d, want 2", depth)
	}
}

func TestEpicHealthBasic(t *testing.T) {
	idx := NewIndex()

	epic := makeTicket("Epic")
	epic.Type = "epic"
	idx.Put("tkt-epic", epic)

	// 5 children: 2 closed, 2 ready (open, no blockers), 1 blocked.
	for _, spec := range []struct {
		id     string
		status string
		blocks []string
	}{
		{"tkt-a", "closed", nil},
		{"tkt-b", "closed", nil},
		{"tkt-c", "open", nil},               // ready
		{"tkt-d", "open", nil},               // ready
		{"tkt-e", "open", []string{"tkt-c"}}, // blocked by C
	} {
		child := makeTicket(spec.id)
		child.Parent = "tkt-epic"
		child.Status = spec.status
		child.BlockedBy = spec.blocks
		if spec.status == "closed" {
			child.ClosedAt = "2026-02-13T10:00:00Z"
		}
		idx.Put(spec.id, child)
	}

	health := idx.EpicHealth("tkt-epic")

	if health.TotalChildren != 5 {
		t.Errorf("TotalChildren = %d, want 5", health.TotalChildren)
	}
	if health.ClosedChildren != 2 {
		t.Errorf("ClosedChildren = %d, want 2", health.ClosedChildren)
	}
	if health.ReadyChildren != 2 {
		t.Errorf("ReadyChildren = %d, want 2 (C and D are ready)", health.ReadyChildren)
	}
	// ActiveFraction: (2 ready + 0 in_progress) / 3 open = 0.666...
	expectedFraction := 2.0 / 3.0
	if health.ActiveFraction < expectedFraction-0.01 || health.ActiveFraction > expectedFraction+0.01 {
		t.Errorf("ActiveFraction = %f, want ~%f", health.ActiveFraction, expectedFraction)
	}
	// CriticalDepth: C ← E is depth 1.
	if health.CriticalDepth != 1 {
		t.Errorf("CriticalDepth = %d, want 1 (C←E chain)", health.CriticalDepth)
	}
}

func TestEpicHealthEmpty(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-epic", makeTicket("Epic"))

	health := idx.EpicHealth("tkt-epic")
	if health.TotalChildren != 0 {
		t.Errorf("TotalChildren = %d, want 0", health.TotalChildren)
	}
	if health.ReadyChildren != 0 {
		t.Errorf("ReadyChildren = %d, want 0", health.ReadyChildren)
	}
}

func TestEpicHealthWithInProgress(t *testing.T) {
	idx := NewIndex()
	idx.Put("tkt-epic", makeTicket("Epic"))

	// 3 open children: 1 ready, 1 in_progress, 1 blocked.
	childA := makeTicket("A")
	childA.Parent = "tkt-epic"
	idx.Put("tkt-a", childA) // ready

	childB := makeTicket("B")
	childB.Parent = "tkt-epic"
	childB.Status = "in_progress"
	childB.Assignee = "@agent:bureau.local"
	idx.Put("tkt-b", childB) // in_progress

	childC := makeTicket("C")
	childC.Parent = "tkt-epic"
	childC.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-c", childC) // blocked

	health := idx.EpicHealth("tkt-epic")
	if health.ReadyChildren != 1 {
		t.Errorf("ReadyChildren = %d, want 1", health.ReadyChildren)
	}
	// ActiveFraction: (1 ready + 1 in_progress) / 3 open = 0.666...
	expectedFraction := 2.0 / 3.0
	if health.ActiveFraction < expectedFraction-0.01 || health.ActiveFraction > expectedFraction+0.01 {
		t.Errorf("ActiveFraction = %f, want ~%f", health.ActiveFraction, expectedFraction)
	}
}

func TestScoreBasic(t *testing.T) {
	idx := NewIndex()

	// A (P2) blocks B (P0). A is ready with no notes.
	ticketA := makeTicket("A")
	ticketA.Priority = 2
	idx.Put("tkt-a", ticketA)

	ticketB := makeTicket("B")
	ticketB.Priority = 0
	ticketB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", ticketB)

	weights := DefaultRankWeights()
	score := idx.Score("tkt-a", fixedNow, weights)

	if score.UnblockCount != 1 {
		t.Errorf("UnblockCount = %d, want 1", score.UnblockCount)
	}
	if score.BorrowedPriority != 0 {
		t.Errorf("BorrowedPriority = %d, want 0", score.BorrowedPriority)
	}
	// Created 2026-02-12, now 2026-02-15 → 3 days.
	if score.DaysSinceReady != 3 {
		t.Errorf("DaysSinceReady = %d, want 3", score.DaysSinceReady)
	}
	if score.NoteCount != 0 {
		t.Errorf("NoteCount = %d, want 0", score.NoteCount)
	}
	if score.Composite <= 0 {
		t.Errorf("Composite = %f, want > 0", score.Composite)
	}
}

func TestScoreNonexistent(t *testing.T) {
	idx := NewIndex()
	score := idx.Score("tkt-missing", fixedNow, DefaultRankWeights())
	if score.BorrowedPriority != -1 {
		t.Errorf("BorrowedPriority = %d, want -1 for nonexistent", score.BorrowedPriority)
	}
}

func TestScoreDaysSinceReadyFromBlockerClosedAt(t *testing.T) {
	idx := NewIndex()

	// Blocker closed 1 day ago. Ticket became ready then.
	closedA := makeTicket("A")
	closedA.Status = "closed"
	closedA.ClosedAt = "2026-02-14T10:00:00Z" // 1 day before fixedNow
	idx.Put("tkt-a", closedA)

	ticketB := makeTicket("B")
	ticketB.BlockedBy = []string{"tkt-a"}
	idx.Put("tkt-b", ticketB)

	score := idx.Score("tkt-b", fixedNow, DefaultRankWeights())
	if score.DaysSinceReady != 1 {
		t.Errorf("DaysSinceReady = %d, want 1 (blocker closed 1 day ago)", score.DaysSinceReady)
	}
}

func TestRankedSortOrder(t *testing.T) {
	idx := NewIndex()

	// Three ready tickets with different characteristics:
	// A: P4, blocks 3 things → high leverage
	// B: P0, blocks nothing → high urgency, no leverage
	// C: P2, blocks 1 thing → moderate

	ticketA := makeTicket("High leverage")
	ticketA.Priority = 4
	idx.Put("tkt-a", ticketA)

	ticketB := makeTicket("High urgency")
	ticketB.Priority = 0
	idx.Put("tkt-b", ticketB)

	ticketC := makeTicket("Moderate")
	ticketC.Priority = 2
	idx.Put("tkt-c", ticketC)

	// A blocks three things.
	for _, id := range []string{"tkt-d", "tkt-e", "tkt-f"} {
		dep := makeTicket(id)
		dep.BlockedBy = []string{"tkt-a"}
		idx.Put(id, dep)
	}

	// C blocks one thing.
	depG := makeTicket("G")
	depG.BlockedBy = []string{"tkt-c"}
	idx.Put("tkt-g", depG)

	ranked := idx.Ranked(fixedNow, DefaultRankWeights())

	// Extract just the IDs of the ready tickets (A, B, C).
	var readyIDs []string
	for _, entry := range ranked {
		switch entry.ID {
		case "tkt-a", "tkt-b", "tkt-c":
			readyIDs = append(readyIDs, entry.ID)
		}
	}

	if len(readyIDs) != 3 {
		t.Fatalf("expected 3 ranked ready tickets among A/B/C, got %v", readyIDs)
	}

	// A has leverage 3×3=9, urgency 2×0=0 → ~10.5
	// B has leverage 3×0=0, urgency 2×4=8 → ~9.5
	// C has leverage 3×1=3, urgency 2×2=4 → ~8.5
	// So order should be A, B, C.
	if readyIDs[0] != "tkt-a" {
		t.Errorf("first ranked = %s, want tkt-a (highest leverage)", readyIDs[0])
	}
	if readyIDs[1] != "tkt-b" {
		t.Errorf("second ranked = %s, want tkt-b (highest urgency)", readyIDs[1])
	}
	if readyIDs[2] != "tkt-c" {
		t.Errorf("third ranked = %s, want tkt-c (moderate)", readyIDs[2])
	}
}

func TestRankedEmpty(t *testing.T) {
	idx := NewIndex()
	ranked := idx.Ranked(fixedNow, DefaultRankWeights())
	if len(ranked) != 0 {
		t.Errorf("Ranked on empty index = %v, want empty", ranked)
	}
}

func TestRankedCompositeScoresDescending(t *testing.T) {
	idx := NewIndex()

	for i := range 5 {
		tc := makeTicket(fmt.Sprintf("Ticket %d", i))
		tc.Priority = i
		idx.Put(fmt.Sprintf("tkt-%d", i), tc)
	}

	ranked := idx.Ranked(fixedNow, DefaultRankWeights())
	for i := 1; i < len(ranked); i++ {
		if ranked[i].Score.Composite > ranked[i-1].Score.Composite {
			t.Errorf("ranked[%d].Composite (%f) > ranked[%d].Composite (%f): not descending",
				i, ranked[i].Score.Composite, i-1, ranked[i-1].Score.Composite)
		}
	}
}

func TestBulkInsertAndQuery(t *testing.T) {
	idx := NewIndex()

	// Simulate initial sync: insert 100 tickets with varied properties.
	for i := range 100 {
		tc := makeTicket("Ticket")
		tc.Priority = i % 5
		if i%3 == 0 {
			tc.Status = "closed"
		}
		if i%7 == 0 {
			tc.Labels = []string{"flagged"}
		}
		idx.Put(fmt.Sprintf("tkt-%04d", i), tc)
	}

	if idx.Len() != 100 {
		t.Fatalf("Len() = %d, want 100", idx.Len())
	}

	stats := idx.Stats()
	if stats.Total != 100 {
		t.Errorf("Stats.Total = %d, want 100", stats.Total)
	}

	// About 34 closed (every 3rd).
	if got := stats.ByStatus["closed"]; got != 34 {
		t.Errorf("ByStatus[closed] = %d, want 34", got)
	}

	// About 15 flagged (every 7th: 0,7,14,...,98 = 15).
	flagged := idx.List(Filter{Label: "flagged"})
	if len(flagged) != 15 {
		t.Errorf("List(label=flagged) = %d, want 15", len(flagged))
	}
}
