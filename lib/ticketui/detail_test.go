// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

func TestRenderAffects(t *testing.T) {
	renderer := NewDetailRenderer(DefaultTheme, 60)
	result := renderer.renderAffects([]string{
		"fleet/gpu/a100",
		"workspace/lib/schema/ticket.go",
	})

	if !strings.Contains(result, "Affects") {
		t.Error("missing Affects header")
	}
	if !strings.Contains(result, "fleet/gpu/a100") {
		t.Error("missing first resource")
	}
	if !strings.Contains(result, "workspace/lib/schema/ticket.go") {
		t.Error("missing second resource")
	}
}

func TestRenderAffectsTruncatesLongResource(t *testing.T) {
	renderer := NewDetailRenderer(DefaultTheme, 30)
	longResource := "very/long/resource/path/that/exceeds/the/available/width"
	result := renderer.renderAffects([]string{longResource})

	// Should contain truncation indicator.
	if !strings.Contains(result, "…") {
		t.Error("long resource should be truncated with ellipsis")
	}
}

func TestRenderReviewerLine(t *testing.T) {
	renderer := NewDetailRenderer(DefaultTheme, 80)

	line := renderer.renderReviewerLine(ticket.ReviewerEntry{
		UserID:      ref.MustParseUserID("@alice:bureau.local"),
		Disposition: "approved",
		UpdatedAt:   "2026-02-24T12:00:00Z",
	})

	if !strings.Contains(line, "@alice:bureau.local") {
		t.Error("missing user ID")
	}
	if !strings.Contains(line, "approved") {
		t.Error("missing disposition")
	}
	if !strings.Contains(line, "2026-02-24") {
		t.Error("missing shortened timestamp")
	}
}

func TestRenderReviewerLinePendingNoTimestamp(t *testing.T) {
	renderer := NewDetailRenderer(DefaultTheme, 80)

	line := renderer.renderReviewerLine(ticket.ReviewerEntry{
		UserID:      ref.MustParseUserID("@bob:bureau.local"),
		Disposition: "pending",
	})

	if !strings.Contains(line, "@bob:bureau.local") {
		t.Error("missing user ID")
	}
	if !strings.Contains(line, "pending") {
		t.Error("missing disposition")
	}
}

func TestRenderTieredReviewers(t *testing.T) {
	renderer := NewDetailRenderer(DefaultTheme, 80)

	threshold := 2
	review := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved", Tier: 0},
			{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "pending", Tier: 0},
			{UserID: ref.MustParseUserID("@charlie:bureau.local"), Disposition: "pending", Tier: 1},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Threshold: &threshold},
			{Tier: 1, Threshold: nil},
		},
	}

	lines := renderer.renderTieredReviewers(review)

	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "Tier 0") {
		t.Error("missing Tier 0 header")
	}
	if !strings.Contains(joined, "1/2 approved") {
		t.Error("missing Tier 0 progress (1/2 approved)")
	}
	if !strings.Contains(joined, "need 2") {
		t.Error("missing Tier 0 threshold")
	}
	if !strings.Contains(joined, "Tier 1") {
		t.Error("missing Tier 1 header")
	}
	if !strings.Contains(joined, "0/1 approved") {
		t.Error("missing Tier 1 progress (0/1 approved)")
	}
	if !strings.Contains(joined, "need all") {
		t.Error("missing Tier 1 'need all' for nil threshold")
	}
	if !strings.Contains(joined, "@alice:bureau.local") {
		t.Error("missing alice in tier 0")
	}
	if !strings.Contains(joined, "@charlie:bureau.local") {
		t.Error("missing charlie in tier 1")
	}
}

func TestRenderTieredReviewersOrdering(t *testing.T) {
	renderer := NewDetailRenderer(DefaultTheme, 80)

	threshold := 1
	review := &ticket.TicketReview{
		Reviewers: []ticket.ReviewerEntry{
			{UserID: ref.MustParseUserID("@tier1:bureau.local"), Disposition: "pending", Tier: 1},
			{UserID: ref.MustParseUserID("@tier0:bureau.local"), Disposition: "pending", Tier: 0},
		},
		TierThresholds: []ticket.TierThreshold{
			{Tier: 0, Threshold: &threshold},
			{Tier: 1, Threshold: &threshold},
		},
	}

	lines := renderer.renderTieredReviewers(review)
	joined := strings.Join(lines, "\n")

	// Tier 0 should appear before Tier 1.
	tier0Position := strings.Index(joined, "Tier 0")
	tier1Position := strings.Index(joined, "Tier 1")
	if tier0Position < 0 || tier1Position < 0 {
		t.Fatal("missing tier headers")
	}
	if tier0Position > tier1Position {
		t.Error("Tier 0 should appear before Tier 1")
	}
}

func TestRenderReviewWithGateStatus(t *testing.T) {
	renderer := NewDetailRenderer(DefaultTheme, 80)

	content := ticket.TicketContent{
		Status: ticket.StatusReview,
		Gates: []ticket.TicketGate{
			{
				ID:          "stewardship:fleet/gpu",
				Type:        "review",
				Status:      "pending",
				Description: "Fleet GPU stewardship",
			},
		},
		Review: &ticket.TicketReview{
			Reviewers: []ticket.ReviewerEntry{
				{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "pending"},
			},
		},
	}

	result := renderer.renderReview(content)

	if !strings.Contains(result, "Review") {
		t.Error("missing Review header")
	}
	if !strings.Contains(result, "pending") {
		t.Error("missing gate pending status")
	}
	if !strings.Contains(result, "Fleet GPU stewardship") {
		t.Error("missing gate description")
	}
	if !strings.Contains(result, "@alice:bureau.local") {
		t.Error("missing reviewer")
	}
}

func TestRenderReviewSatisfiedGate(t *testing.T) {
	renderer := NewDetailRenderer(DefaultTheme, 80)

	content := ticket.TicketContent{
		Status: ticket.StatusReview,
		Gates: []ticket.TicketGate{
			{
				ID:          "stewardship:fleet/gpu",
				Type:        "review",
				Status:      "satisfied",
				Description: "Fleet GPU stewardship",
			},
		},
		Review: &ticket.TicketReview{
			Reviewers: []ticket.ReviewerEntry{
				{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved"},
			},
		},
	}

	result := renderer.renderReview(content)

	if !strings.Contains(result, "satisfied") {
		t.Error("missing satisfied status indicator")
	}
}

func TestRenderReviewSkipsNonReviewGates(t *testing.T) {
	renderer := NewDetailRenderer(DefaultTheme, 80)

	content := ticket.TicketContent{
		Status: ticket.StatusOpen,
		Gates: []ticket.TicketGate{
			{ID: "ci-pass", Type: "pipeline", Status: "pending"},
			{ID: "stewardship:fleet/gpu", Type: "review", Status: "pending", Description: "Fleet GPU"},
		},
		Review: &ticket.TicketReview{
			Reviewers: []ticket.ReviewerEntry{
				{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "pending"},
			},
		},
	}

	result := renderer.renderReview(content)

	// Pipeline gate should not appear in review section.
	if strings.Contains(result, "ci-pass") {
		t.Error("pipeline gate should not appear in review section")
	}
	// Review gate should appear.
	if !strings.Contains(result, "Fleet GPU") {
		t.Error("review gate should appear")
	}
}

func TestRenderReviewClassicFlatDisplay(t *testing.T) {
	renderer := NewDetailRenderer(DefaultTheme, 80)

	content := ticket.TicketContent{
		Status: ticket.StatusReview,
		Review: &ticket.TicketReview{
			Reviewers: []ticket.ReviewerEntry{
				{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "approved"},
				{UserID: ref.MustParseUserID("@bob:bureau.local"), Disposition: "pending"},
			},
		},
	}

	result := renderer.renderReview(content)

	// Without TierThresholds, should show flat list (no Tier headers).
	if strings.Contains(result, "Tier") {
		t.Error("flat display should not contain tier headers")
	}
	if !strings.Contains(result, "@alice:bureau.local") {
		t.Error("missing alice in flat display")
	}
	if !strings.Contains(result, "@bob:bureau.local") {
		t.Error("missing bob in flat display")
	}
}

func TestRenderReviewWithScope(t *testing.T) {
	renderer := NewDetailRenderer(DefaultTheme, 80)

	content := ticket.TicketContent{
		Status: ticket.StatusReview,
		Review: &ticket.TicketReview{
			Reviewers: []ticket.ReviewerEntry{
				{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "pending"},
			},
			Scope: &ticket.ReviewScope{
				Description: "Review GPU driver changes",
				Base:        "main",
				Head:        "feature/gpu",
			},
		},
	}

	result := renderer.renderReview(content)

	if !strings.Contains(result, "scope:") {
		t.Error("missing scope description label")
	}
	if !strings.Contains(result, "Review GPU driver changes") {
		t.Error("missing scope description content")
	}
	if !strings.Contains(result, "main..feature/gpu") {
		t.Error("missing range display")
	}
}

func TestDispositionOptions(t *testing.T) {
	options := DispositionOptions()

	if len(options) != 3 {
		t.Fatalf("expected 3 options, got %d", len(options))
	}

	values := make(map[string]bool)
	for _, option := range options {
		values[option.Value] = true
	}

	for _, expected := range []string{"approved", "changes_requested", "commented"} {
		if !values[expected] {
			t.Errorf("missing disposition option: %s", expected)
		}
	}
}

func TestRenderBodyIncludesAffects(t *testing.T) {
	source := NewIndexSource(nil)
	// Create a source with one ticket that has affects.
	index := newTestIndex()
	index.Put("tkt-review", ticket.TicketContent{
		Version: 1,
		Title:   "Review ticket",
		Status:  ticket.StatusReview,
		Type:    "task",
		Affects: []string{"fleet/gpu/a100", "workspace/deploy"},
	})
	source = NewIndexSource(index)

	renderer := NewDetailRenderer(DefaultTheme, 80)
	entry := ticketindex.Entry{
		ID: "tkt-review",
		Content: ticket.TicketContent{
			Version: 1,
			Title:   "Review ticket",
			Status:  ticket.StatusReview,
			Type:    "task",
			Affects: []string{"fleet/gpu/a100", "workspace/deploy"},
		},
	}

	body, _ := renderer.RenderBody(source, entry, testTime())
	if !strings.Contains(body, "Affects") {
		t.Error("body should contain Affects section")
	}
	if !strings.Contains(body, "fleet/gpu/a100") {
		t.Error("body should contain first affects entry")
	}
	if !strings.Contains(body, "workspace/deploy") {
		t.Error("body should contain second affects entry")
	}
}

func TestRenderBodyNoAffectsWhenEmpty(t *testing.T) {
	index := newTestIndex()
	index.Put("tkt-plain", ticket.TicketContent{
		Version: 1,
		Title:   "Plain ticket",
		Status:  ticket.StatusOpen,
		Type:    "task",
	})
	source := NewIndexSource(index)

	renderer := NewDetailRenderer(DefaultTheme, 80)
	entry := ticketindex.Entry{
		ID: "tkt-plain",
		Content: ticket.TicketContent{
			Version: 1,
			Title:   "Plain ticket",
			Status:  ticket.StatusOpen,
			Type:    "task",
		},
	}

	body, _ := renderer.RenderBody(source, entry, testTime())
	if strings.Contains(body, "Affects") {
		t.Error("body should not contain Affects section when affects is empty")
	}
}

// newTestIndex creates a fresh empty ticket index for use in tests.
func newTestIndex() *ticketindex.Index {
	return ticketindex.NewIndex()
}

// testTime returns a fixed time for deterministic test output.
func testTime() time.Time {
	return time.Date(2026, 2, 24, 12, 0, 0, 0, time.UTC)
}

func TestCanReviewFileMode(t *testing.T) {
	t.Parallel()

	// IndexSource does not implement Mutator, so canReview should
	// always return false regardless of ticket state.
	index := newTestIndex()
	index.Put("tkt-review", ticket.TicketContent{
		Version: 1,
		Title:   "Review ticket",
		Status:  ticket.StatusReview,
		Type:    "task",
		Review: &ticket.TicketReview{
			Reviewers: []ticket.ReviewerEntry{
				{UserID: ref.MustParseUserID("@alice:bureau.local"), Disposition: "pending"},
			},
		},
	})
	source := NewIndexSource(index)

	model := NewModel(source)
	model.width = 120
	model.height = 40
	model.ready = true
	model.operatorUserID = "@alice:bureau.local"
	model.detailPane.SetSize(60, 40)

	// Select the review ticket.
	model.switchTab(TabAll)
	for index, item := range model.items {
		if !item.IsHeader && item.Entry.ID == "tkt-review" {
			model.cursor = index
			model.selectedID = item.Entry.ID
			break
		}
	}

	if model.canReview() {
		t.Error("canReview should return false for IndexSource (no Mutator)")
	}
}

func TestOpenReviewDropdownFileMode(t *testing.T) {
	t.Parallel()

	// openReviewDropdown should be a no-op when source doesn't
	// implement Mutator.
	source := testSource()
	model := NewModel(source)
	model.width = 120
	model.height = 40
	model.ready = true
	model.operatorUserID = "@alice:bureau.local"

	model.openReviewDropdown()

	if model.activeDropdown != nil {
		t.Error("expected no dropdown in file-backed mode")
	}
}

func TestOpenReviewDropdownWrongStatus(t *testing.T) {
	t.Parallel()

	// Even with a selected ticket, if status is not "review",
	// dropdown should not open.
	source := testSource()
	model := NewModel(source)
	model.width = 120
	model.height = 40
	model.ready = true
	model.operatorUserID = "@alice:bureau.local"

	// Select first ticket (which is open, not review).
	if len(model.items) > 0 && !model.items[0].IsHeader {
		model.selectedID = model.items[0].Entry.ID
	}

	model.openReviewDropdown()

	if model.activeDropdown != nil {
		t.Error("expected no dropdown when ticket is not in review status")
	}
}

func TestStatusTransitionsReview(t *testing.T) {
	t.Parallel()

	options := StatusTransitions(string(ticket.StatusReview))
	if len(options) != 3 {
		t.Fatalf("expected 3 transitions from review, got %d", len(options))
	}

	values := make(map[string]bool)
	for _, option := range options {
		values[option.Value] = true
	}

	for _, expected := range []string{string(ticket.StatusOpen), string(ticket.StatusInProgress), string(ticket.StatusClosed)} {
		if !values[expected] {
			t.Errorf("missing transition: %s", expected)
		}
	}
}
