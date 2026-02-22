// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

// testNow is a fixed time used across dependency graph tests. The
// exact value doesn't matter — it just needs to be consistent.
var testNow = time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)

// testDependencySource creates a ticket index with dependency
// relationships for testing the dependency graph renderer. The center
// ticket (center) is blocked by left-1 and left-2, and blocks right-1.
func testDependencySource() *IndexSource {
	index := ticketindex.NewIndex()
	index.Put("center", ticket.TicketContent{
		Version:   1,
		Title:     "Center ticket",
		Status:    "in_progress",
		Priority:  1,
		Type:      "task",
		BlockedBy: []string{"left-1", "left-2"},
		CreatedBy: ref.MustParseUserID("@test:bureau.local"),
	})
	index.Put("left-1", ticket.TicketContent{
		Version:  1,
		Title:    "Left one",
		Status:   "closed",
		Priority: 0,
		Type:     "task",
	})
	index.Put("left-2", ticket.TicketContent{
		Version:  1,
		Title:    "Left two",
		Status:   "open",
		Priority: 1,
		Type:     "bug",
	})
	index.Put("right-1", ticket.TicketContent{
		Version:   1,
		Title:     "Right one",
		Status:    "blocked",
		Priority:  2,
		Type:      "task",
		BlockedBy: []string{"center"},
	})
	return NewIndexSource(index)
}

func TestBoxDrawingSingleDirections(t *testing.T) {
	// Each cardinal direction alone produces a line segment.
	if got := boxDrawing(true, false, false, false); got != '─' {
		t.Errorf("left only: expected '─', got %c", got)
	}
	if got := boxDrawing(false, true, false, false); got != '─' {
		t.Errorf("right only: expected '─', got %c", got)
	}
	if got := boxDrawing(false, false, true, false); got != '│' {
		t.Errorf("up only: expected '│', got %c", got)
	}
	if got := boxDrawing(false, false, false, true); got != '│' {
		t.Errorf("down only: expected '│', got %c", got)
	}
}

func TestBoxDrawingCorners(t *testing.T) {
	cases := []struct {
		left, right, up, down bool
		expected              rune
	}{
		{true, false, false, true, '┐'},
		{true, false, true, false, '┘'},
		{false, true, false, true, '┌'},
		{false, true, true, false, '└'},
	}
	for _, testCase := range cases {
		got := boxDrawing(testCase.left, testCase.right, testCase.up, testCase.down)
		if got != testCase.expected {
			t.Errorf("boxDrawing(%v,%v,%v,%v) = %c, want %c",
				testCase.left, testCase.right, testCase.up, testCase.down,
				got, testCase.expected)
		}
	}
}

func TestBoxDrawingTees(t *testing.T) {
	cases := []struct {
		left, right, up, down bool
		expected              rune
	}{
		{true, true, true, false, '┴'},
		{true, true, false, true, '┬'},
		{true, false, true, true, '┤'},
		{false, true, true, true, '├'},
	}
	for _, testCase := range cases {
		got := boxDrawing(testCase.left, testCase.right, testCase.up, testCase.down)
		if got != testCase.expected {
			t.Errorf("boxDrawing(%v,%v,%v,%v) = %c, want %c",
				testCase.left, testCase.right, testCase.up, testCase.down,
				got, testCase.expected)
		}
	}
}

func TestBoxDrawingCross(t *testing.T) {
	got := boxDrawing(true, true, true, true)
	if got != '┼' {
		t.Errorf("all directions: expected '┼', got %c", got)
	}
}

func TestBoxDrawingNone(t *testing.T) {
	got := boxDrawing(false, false, false, false)
	if got != ' ' {
		t.Errorf("no directions: expected ' ', got %c", got)
	}
}

func TestBoxDrawingStraightLines(t *testing.T) {
	// Horizontal.
	got := boxDrawing(true, true, false, false)
	if got != '─' {
		t.Errorf("horizontal: expected '─', got %c", got)
	}
	// Vertical.
	got = boxDrawing(false, false, true, true)
	if got != '│' {
		t.Errorf("vertical: expected '│', got %c", got)
	}
}

func TestDependencyGraphNoDeps(t *testing.T) {
	source := NewIndexSource(ticketindex.NewIndex())
	graph := NewDependencyGraph(DefaultTheme, 80)

	rendered, targets := graph.Render("center", nil, nil, source, testNow)
	if rendered != "" {
		t.Errorf("no deps: expected empty string, got %q", rendered)
	}
	if targets != nil {
		t.Errorf("no deps: expected nil targets, got %v", targets)
	}
}

func TestDependencyGraphLeftOnly(t *testing.T) {
	source := testDependencySource()
	graph := NewDependencyGraph(DefaultTheme, 80)

	rendered, targets := graph.Render("center", []string{"left-1", "left-2"}, nil, source, testNow)
	if rendered == "" {
		t.Fatal("expected non-empty graph with left nodes")
	}

	lines := strings.Split(rendered, "\n")
	// 2 left nodes, 0 right nodes → max(2, 0) = 2 rows.
	if len(lines) != 2 {
		t.Errorf("expected 2 rows, got %d", len(lines))
	}

	// Center label should appear on the center row (row 1 for 2 rows).
	if !strings.Contains(rendered, "center") {
		t.Error("graph should contain the center ticket ID")
	}

	// Both left node IDs should appear.
	if !strings.Contains(rendered, "left-1") {
		t.Error("graph should contain left-1")
	}
	if !strings.Contains(rendered, "left-2") {
		t.Error("graph should contain left-2")
	}

	// Should have click targets for both left nodes.
	if len(targets) != 2 {
		t.Fatalf("expected 2 click targets, got %d", len(targets))
	}
	for _, target := range targets {
		if target.EndX <= target.StartX {
			t.Errorf("target %s: EndX (%d) should exceed StartX (%d)",
				target.TicketID, target.EndX, target.StartX)
		}
	}
}

func TestDependencyGraphRightOnly(t *testing.T) {
	source := testDependencySource()
	graph := NewDependencyGraph(DefaultTheme, 80)

	rendered, targets := graph.Render("center", nil, []string{"right-1"}, source, testNow)
	if rendered == "" {
		t.Fatal("expected non-empty graph with right nodes")
	}

	// 0 left, 1 right → 1 row.
	lines := strings.Split(rendered, "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 row, got %d", len(lines))
	}

	if !strings.Contains(rendered, "center") {
		t.Error("graph should contain center")
	}
	if !strings.Contains(rendered, "right-1") {
		t.Error("graph should contain right-1")
	}

	if len(targets) != 1 {
		t.Fatalf("expected 1 click target, got %d", len(targets))
	}
	if targets[0].TicketID != "right-1" {
		t.Errorf("target ticket ID = %q, want right-1", targets[0].TicketID)
	}
}

func TestDependencyGraphBothSides(t *testing.T) {
	source := testDependencySource()
	graph := NewDependencyGraph(DefaultTheme, 80)

	rendered, targets := graph.Render("center", []string{"left-1", "left-2"}, []string{"right-1"}, source, testNow)
	if rendered == "" {
		t.Fatal("expected non-empty graph")
	}

	lines := strings.Split(rendered, "\n")
	// max(2 left, 1 right) = 2 rows.
	if len(lines) != 2 {
		t.Errorf("expected 2 rows, got %d", len(lines))
	}

	// All IDs should appear.
	for _, expectedID := range []string{"left-1", "left-2", "center", "right-1"} {
		if !strings.Contains(rendered, expectedID) {
			t.Errorf("graph should contain %q", expectedID)
		}
	}

	// 3 click targets: 2 left + 1 right. Center is not clickable.
	if len(targets) != 3 {
		t.Fatalf("expected 3 click targets, got %d", len(targets))
	}

	// Verify left and right targets have non-overlapping X ranges.
	leftTargets := make([]BodyClickTarget, 0)
	rightTargets := make([]BodyClickTarget, 0)
	for _, target := range targets {
		if target.TicketID == "right-1" {
			rightTargets = append(rightTargets, target)
		} else {
			leftTargets = append(leftTargets, target)
		}
	}
	if len(leftTargets) != 2 {
		t.Fatalf("expected 2 left targets, got %d", len(leftTargets))
	}
	if len(rightTargets) != 1 {
		t.Fatalf("expected 1 right target, got %d", len(rightTargets))
	}

	// Right target X range must not overlap with left target X ranges.
	// Left targets should all end before right targets start.
	for _, leftTarget := range leftTargets {
		if leftTarget.EndX > rightTargets[0].StartX {
			t.Errorf("left target %s EndX (%d) overlaps with right target StartX (%d)",
				leftTarget.TicketID, leftTarget.EndX, rightTargets[0].StartX)
		}
	}
}

func TestDependencyGraphClickTargetDisambiguation(t *testing.T) {
	// This is the specific bug we fixed: left and right nodes on the
	// same row must have different X ranges so clicking on the right
	// side picks the right node, not the left one.
	source := testDependencySource()
	graph := NewDependencyGraph(DefaultTheme, 80)

	// 1 left, 1 right → both on the same row (row 0).
	rendered, targets := graph.Render("center", []string{"left-1"}, []string{"right-1"}, source, testNow)
	if rendered == "" {
		t.Fatal("expected non-empty graph")
	}

	lines := strings.Split(rendered, "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 row, got %d", len(lines))
	}

	// Both targets are on line 0 but must have distinct X ranges.
	if len(targets) != 2 {
		t.Fatalf("expected 2 click targets, got %d", len(targets))
	}

	var leftTarget, rightTarget BodyClickTarget
	for _, target := range targets {
		if target.TicketID == "left-1" {
			leftTarget = target
		} else {
			rightTarget = target
		}
	}

	if leftTarget.Line != rightTarget.Line {
		t.Fatalf("expected both targets on same line, got %d and %d",
			leftTarget.Line, rightTarget.Line)
	}

	// Ranges must not overlap.
	if leftTarget.EndX > rightTarget.StartX {
		t.Errorf("left EndX (%d) should be <= right StartX (%d)",
			leftTarget.EndX, rightTarget.StartX)
	}
	// Each range must be non-empty.
	if leftTarget.EndX <= leftTarget.StartX {
		t.Errorf("left target has empty range: [%d, %d)", leftTarget.StartX, leftTarget.EndX)
	}
	if rightTarget.EndX <= rightTarget.StartX {
		t.Errorf("right target has empty range: [%d, %d)", rightTarget.StartX, rightTarget.EndX)
	}
}

func TestDependencyGraphMissingNode(t *testing.T) {
	// When a blocked-by ticket is not in the index, the graph should
	// still render but the missing node should not be clickable.
	source := testDependencySource()
	graph := NewDependencyGraph(DefaultTheme, 80)

	rendered, targets := graph.Render("center", []string{"left-1", "nonexistent"}, nil, source, testNow)
	if rendered == "" {
		t.Fatal("expected non-empty graph even with missing nodes")
	}

	if !strings.Contains(rendered, "nonexistent") {
		t.Error("graph should show the missing node's ID")
	}

	// Only left-1 should be clickable (exists in index); nonexistent should not.
	if len(targets) != 1 {
		t.Fatalf("expected 1 click target (only existing nodes), got %d", len(targets))
	}
	if targets[0].TicketID != "left-1" {
		t.Errorf("click target should be left-1, got %s", targets[0].TicketID)
	}
}

func TestDependencyGraphNarrowWidth(t *testing.T) {
	// With a very narrow width, labels should be truncated but the
	// graph should still render without panicking.
	source := testDependencySource()
	graph := NewDependencyGraph(DefaultTheme, 30)

	rendered, targets := graph.Render("center", []string{"left-1"}, []string{"right-1"}, source, testNow)
	if rendered == "" {
		t.Fatal("expected non-empty graph even at narrow width")
	}

	// Verify it produced output without crashing.
	lines := strings.Split(rendered, "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 row, got %d", len(lines))
	}

	// Targets should still be present (may have truncated labels).
	if len(targets) < 1 {
		t.Error("expected at least 1 click target even at narrow width")
	}
}

func TestDependencyGraphSymmetricFan(t *testing.T) {
	// 3 left, 3 right should produce 3 rows with symmetric layout.
	index := ticketindex.NewIndex()
	index.Put("center", ticket.TicketContent{
		Version:   1,
		Title:     "Center",
		Status:    "open",
		Priority:  1,
		Type:      "task",
		BlockedBy: []string{"a", "b", "c"},
	})
	for _, ticketID := range []string{"a", "b", "c", "x", "y", "z"} {
		index.Put(ticketID, ticket.TicketContent{
			Version:  1,
			Title:    "Node " + ticketID,
			Status:   "open",
			Priority: 2,
			Type:     "task",
		})
	}
	// Make x, y, z blocked by center.
	index.Put("x", ticket.TicketContent{
		Version:   1,
		Title:     "Node x",
		Status:    "open",
		Priority:  2,
		Type:      "task",
		BlockedBy: []string{"center"},
	})
	index.Put("y", ticket.TicketContent{
		Version:   1,
		Title:     "Node y",
		Status:    "open",
		Priority:  2,
		Type:      "task",
		BlockedBy: []string{"center"},
	})
	index.Put("z", ticket.TicketContent{
		Version:   1,
		Title:     "Node z",
		Status:    "open",
		Priority:  2,
		Type:      "task",
		BlockedBy: []string{"center"},
	})
	source := NewIndexSource(index)
	graph := NewDependencyGraph(DefaultTheme, 100)

	rendered, targets := graph.Render("center", []string{"a", "b", "c"}, []string{"x", "y", "z"}, source, testNow)
	if rendered == "" {
		t.Fatal("expected non-empty graph")
	}

	lines := strings.Split(rendered, "\n")
	if len(lines) != 3 {
		t.Errorf("3 left + 3 right → 3 rows, got %d", len(lines))
	}

	// 6 click targets (3 left + 3 right).
	if len(targets) != 6 {
		t.Errorf("expected 6 click targets, got %d", len(targets))
	}

	// Center should appear on the middle row.
	if !strings.Contains(lines[1], "center") {
		t.Error("center ticket should appear on the middle row (row 1)")
	}
}

func TestMergeCharSingleNode(t *testing.T) {
	graph := DependencyGraph{}
	// Single node at row 0, fan [0,0], center at 0 → both left and
	// right (node + center on same row) with no vertical → horizontal.
	got := graph.mergeChar(0, 0, 0, 0)
	if got != '─' {
		t.Errorf("single node aligned with center: expected '─', got %c", got)
	}
}

func TestMergeCharSingleNodeOffsetBelow(t *testing.T) {
	graph := DependencyGraph{}
	// Single left node at row 0, center at row 1. The merge column
	// must connect the node downward to the center row.
	// Row 0: left + down → ┐
	got := graph.mergeChar(0, 0, 0, 1)
	if got != '┐' {
		t.Errorf("row 0 (node above center): expected '┐', got %c", got)
	}
	// Row 1: right + up → └
	got = graph.mergeChar(1, 0, 0, 1)
	if got != '└' {
		t.Errorf("row 1 (center below node): expected '└', got %c", got)
	}
}

func TestSplitCharSingleNodeOffsetBelow(t *testing.T) {
	graph := DependencyGraph{}
	// Single right node at row 0, center at row 1. The split column
	// must connect the center upward to the node row.
	// Row 0: right + down → ┌
	got := graph.splitChar(0, 0, 0, 1)
	if got != '┌' {
		t.Errorf("row 0 (node above center): expected '┌', got %c", got)
	}
	// Row 1: left + up → ┘
	got = graph.splitChar(1, 0, 0, 1)
	if got != '┘' {
		t.Errorf("row 1 (center below node): expected '┘', got %c", got)
	}
}

func TestMergeCharThreeNodes(t *testing.T) {
	graph := DependencyGraph{}
	// 3 left nodes: fan [0,2], center at row 1.

	// Row 0: top of fan, no up, has down, has left, no right → ┐
	got := graph.mergeChar(0, 0, 2, 1)
	if got != '┐' {
		t.Errorf("row 0: expected '┐', got %c", got)
	}

	// Row 1: center row → has all four directions → ┼
	got = graph.mergeChar(1, 0, 2, 1)
	if got != '┼' {
		t.Errorf("row 1: expected '┼', got %c", got)
	}

	// Row 2: bottom of fan, has up, no down, has left, no right → ┘
	got = graph.mergeChar(2, 0, 2, 1)
	if got != '┘' {
		t.Errorf("row 2: expected '┘', got %c", got)
	}
}

func TestSplitCharThreeNodes(t *testing.T) {
	graph := DependencyGraph{}
	// 3 right nodes: fan [0,2], center at row 1.

	// Row 0: top of fan, no up, has down, no left, has right → ┌
	got := graph.splitChar(0, 0, 2, 1)
	if got != '┌' {
		t.Errorf("row 0: expected '┌', got %c", got)
	}

	// Row 1: center row → has all four → ┼
	got = graph.splitChar(1, 0, 2, 1)
	if got != '┼' {
		t.Errorf("row 1: expected '┼', got %c", got)
	}

	// Row 2: bottom, has up, no down, no left, has right → └
	got = graph.splitChar(2, 0, 2, 1)
	if got != '└' {
		t.Errorf("row 2: expected '└', got %c", got)
	}
}

func TestMergeCharOutsideFan(t *testing.T) {
	graph := DependencyGraph{}
	// Row outside the fan range → space.
	got := graph.mergeChar(5, 0, 2, 1)
	if got != ' ' {
		t.Errorf("outside fan: expected ' ', got %c", got)
	}
}

func TestSplitCharOutsideFan(t *testing.T) {
	graph := DependencyGraph{}
	got := graph.splitChar(5, 0, 2, 1)
	if got != ' ' {
		t.Errorf("outside fan: expected ' ', got %c", got)
	}
}

func TestDependencyGraphLabelTruncation(t *testing.T) {
	// With a narrow width and both sides populated, existing node
	// labels should be truncated to fit. Previously only non-existent
	// nodes were truncated. Priority indicators are included in the
	// label width, so truncation must account for them.
	source := testDependencySource()

	graph := NewDependencyGraph(DefaultTheme, 40)
	rendered, _ := graph.Render("center", []string{"left-1", "left-2"}, []string{"right-1"}, source, testNow)
	if rendered == "" {
		t.Fatal("expected non-empty graph")
	}

	lines := strings.Split(rendered, "\n")
	for lineNumber, line := range lines {
		visualWidth := lipgloss.Width(line)
		if visualWidth > 40 {
			t.Errorf("line %d visual width %d exceeds graph width 40: %q",
				lineNumber, visualWidth, line)
		}
	}
}

func TestDependencyGraphAsymmetricOffset(t *testing.T) {
	// Regression: 1 left node + 2 right nodes. The left node sits on
	// row 0 while center is on row 1. The merge column must connect
	// the left node downward to the center — previously it produced a
	// straight '─' with no vertical continuation.
	source := testDependencySource()
	graph := NewDependencyGraph(DefaultTheme, 80)

	rendered, _ := graph.Render("center", []string{"left-1"}, []string{"right-1", "left-2"}, source, testNow)
	if rendered == "" {
		t.Fatal("expected non-empty graph")
	}

	lines := strings.Split(rendered, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(lines))
	}

	// Row 0: left node, no center text. Row 1: center with connectors.
	if strings.Contains(lines[0], "center") {
		t.Error("center should be on row 1 (center row), not row 0")
	}
	if !strings.Contains(lines[1], "center") {
		t.Error("center should appear on row 1")
	}

	// The merge column on row 0 must contain a downward connector (┐),
	// and row 1 must contain an upward-right connector (└). Verify by
	// checking that the box-drawing chars appear in the rendered output.
	if !strings.ContainsRune(rendered, '┐') {
		t.Error("expected ┐ in merge column on row 0 (left node connecting down)")
	}
	if !strings.ContainsRune(rendered, '└') {
		t.Error("expected └ in merge column on row 1 (center connecting up)")
	}
}

func TestDependencyGraphAsymmetricOffsetInverse(t *testing.T) {
	// Mirror of the above: 2 left nodes + 1 right node. The right
	// node sits on row 0 while center is on row 1. The split column
	// must connect the right node downward to the center.
	source := testDependencySource()
	graph := NewDependencyGraph(DefaultTheme, 80)

	rendered, _ := graph.Render("center", []string{"left-1", "left-2"}, []string{"right-1"}, source, testNow)
	if rendered == "" {
		t.Fatal("expected non-empty graph")
	}

	lines := strings.Split(rendered, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(lines))
	}

	// The split column on row 0 must contain a downward connector (┌),
	// and row 1 must contain an upward-left connector (┘).
	if !strings.ContainsRune(rendered, '┌') {
		t.Error("expected ┌ in split column on row 0 (right node connecting down)")
	}
	if !strings.ContainsRune(rendered, '┘') {
		t.Error("expected ┘ in split column on row 1 (center connecting up)")
	}
}

func TestDependencyGraphPriorityIndicators(t *testing.T) {
	// Verify that priority indicators appear in node labels.
	source := testDependencySource()
	graph := NewDependencyGraph(DefaultTheme, 100)

	rendered, _ := graph.Render("center", []string{"left-1", "left-2"}, []string{"right-1"}, source, testNow)
	if rendered == "" {
		t.Fatal("expected non-empty graph")
	}

	// left-1 is P0, left-2 is P1, right-1 is P2. All should show
	// their priority in the rendered output.
	if !strings.Contains(rendered, "P0") {
		t.Error("expected P0 indicator for left-1 (priority 0)")
	}
	if !strings.Contains(rendered, "P1") {
		t.Error("expected P1 indicator for left-2 (priority 1)")
	}
	if !strings.Contains(rendered, "P2") {
		t.Error("expected P2 indicator for right-1 (priority 2)")
	}
}

func TestDependencyGraphBorrowedPriority(t *testing.T) {
	// When a ticket has a borrowed priority more urgent than its own,
	// the label should include the escalation indicator →P{n}.
	index := ticketindex.NewIndex()
	index.Put("center", ticket.TicketContent{
		Version:   1,
		Title:     "Center",
		Status:    "open",
		Priority:  2,
		Type:      "task",
		BlockedBy: []string{"blocker"},
	})
	// blocker is P3 but blocks center (P2) which blocks downstream-p0.
	// So blocker's borrowed priority should be P0 (from downstream-p0
	// depending transitively through center).
	index.Put("blocker", ticket.TicketContent{
		Version:  1,
		Title:    "Blocker",
		Status:   "open",
		Priority: 3,
		Type:     "task",
	})
	index.Put("downstream-p0", ticket.TicketContent{
		Version:   1,
		Title:     "Urgent downstream",
		Status:    "open",
		Priority:  0,
		Type:      "task",
		BlockedBy: []string{"center"},
	})
	source := NewIndexSource(index)
	graph := NewDependencyGraph(DefaultTheme, 100)

	rendered, _ := graph.Render("center", []string{"blocker"}, []string{"downstream-p0"}, source, testNow)
	if rendered == "" {
		t.Fatal("expected non-empty graph")
	}

	// blocker is P3 with borrowed priority P0 → should show →P0.
	if !strings.Contains(rendered, "→P0") {
		t.Errorf("expected borrowed priority indicator →P0 for blocker, got:\n%s", rendered)
	}
	// blocker should also show its own priority P3.
	if !strings.Contains(rendered, "P3") {
		t.Errorf("expected own priority P3 for blocker, got:\n%s", rendered)
	}
}
