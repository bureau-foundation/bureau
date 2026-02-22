// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"testing"
)

func TestBeadsToTicketContent(t *testing.T) {
	entry := BeadsEntry{
		ID:          "bd-001",
		Title:       "Fix the auth bug",
		Description: "The auth module has a race condition",
		Status:      "open",
		Priority:    1,
		IssueType:   "bug",
		Labels:      []string{"infra", "security"},
		CreatedAt:   "2026-01-01T00:00:00Z",
		CreatedBy:   "ben",
		UpdatedAt:   "2026-01-02T00:00:00Z",
		Dependencies: []BeadsDependency{
			{IssueID: "bd-001", DependsOnID: "bd-002", Type: "blocks"},
			{IssueID: "bd-001", DependsOnID: "bd-003", Type: "parent-child"},
			// Dependency belonging to a different entry — should be ignored.
			{IssueID: "bd-999", DependsOnID: "bd-001", Type: "blocks"},
		},
	}

	content := BeadsToTicketContent(entry)

	if content.Version != 1 {
		t.Errorf("Version = %d, want 1", content.Version)
	}
	if content.Title != "Fix the auth bug" {
		t.Errorf("Title = %q, want %q", content.Title, "Fix the auth bug")
	}
	if content.Body != "The auth module has a race condition" {
		t.Errorf("Body = %q, want %q", content.Body, "The auth module has a race condition")
	}
	if content.Status != "open" {
		t.Errorf("Status = %q, want %q", content.Status, "open")
	}
	if content.Priority != 1 {
		t.Errorf("Priority = %d, want 1", content.Priority)
	}
	if content.Type != "bug" {
		t.Errorf("Type = %q, want %q", content.Type, "bug")
	}
	if len(content.Labels) != 2 || content.Labels[0] != "infra" || content.Labels[1] != "security" {
		t.Errorf("Labels = %v, want [infra security]", content.Labels)
	}
	if len(content.BlockedBy) != 1 || content.BlockedBy[0] != "bd-002" {
		t.Errorf("BlockedBy = %v, want [bd-002]", content.BlockedBy)
	}
	if content.Parent != "bd-003" {
		t.Errorf("Parent = %q, want %q", content.Parent, "bd-003")
	}
}

func TestBeadsToTicketContent_ClosedTicket(t *testing.T) {
	entry := BeadsEntry{
		ID:          "bd-042",
		Title:       "Closed ticket",
		Status:      "closed",
		Priority:    2,
		IssueType:   "task",
		CreatedAt:   "2026-01-01T00:00:00Z",
		CreatedBy:   "ben",
		UpdatedAt:   "2026-01-15T00:00:00Z",
		ClosedAt:    "2026-01-15T00:00:00Z",
		CloseReason: "Done",
	}

	content := BeadsToTicketContent(entry)
	if content.ClosedAt != "2026-01-15T00:00:00Z" {
		t.Errorf("ClosedAt = %q, want %q", content.ClosedAt, "2026-01-15T00:00:00Z")
	}
	if content.CloseReason != "Done" {
		t.Errorf("CloseReason = %q, want %q", content.CloseReason, "Done")
	}
}

func TestRenameBeadsIDs(t *testing.T) {
	content := TicketContent{
		Version:   1,
		Title:     "Fix bd-001 regression",
		Body:      "This is blocked by bd-002 and related to bd-003.\nSee bd-004 for context.",
		Status:    "open",
		Priority:  1,
		Type:      "bug",
		BlockedBy: []string{"bd-002", "bd-005"},
		Parent:    "bd-010",
		Notes: []TicketNote{
			{ID: "n-1", Body: "Investigated bd-002 and found the root cause."},
		},
	}

	newID, newContent := RenameBeadsIDs("bd-001", content, "bd", "tkt")

	if newID != "tkt-001" {
		t.Errorf("ID = %q, want %q", newID, "tkt-001")
	}
	if newContent.Title != "Fix tkt-001 regression" {
		t.Errorf("Title = %q, want %q", newContent.Title, "Fix tkt-001 regression")
	}
	if newContent.Body != "This is blocked by tkt-002 and related to tkt-003.\nSee tkt-004 for context." {
		t.Errorf("Body = %q", newContent.Body)
	}
	if len(newContent.BlockedBy) != 2 || newContent.BlockedBy[0] != "tkt-002" || newContent.BlockedBy[1] != "tkt-005" {
		t.Errorf("BlockedBy = %v, want [tkt-002 tkt-005]", newContent.BlockedBy)
	}
	if newContent.Parent != "tkt-010" {
		t.Errorf("Parent = %q, want %q", newContent.Parent, "tkt-010")
	}
	if newContent.Notes[0].Body != "Investigated tkt-002 and found the root cause." {
		t.Errorf("Note body = %q", newContent.Notes[0].Body)
	}
}

func TestRenameBeadsIDs_DoesNotMutateOriginal(t *testing.T) {
	original := TicketContent{
		Version:   1,
		Title:     "Original",
		Body:      "See bd-001",
		Status:    "open",
		Type:      "task",
		BlockedBy: []string{"bd-002"},
		Parent:    "bd-003",
	}

	_, renamed := RenameBeadsIDs("bd-999", original, "bd", "tkt")

	// Verify the original is not modified.
	if original.Body != "See bd-001" {
		t.Error("original Body was mutated")
	}
	if original.BlockedBy[0] != "bd-002" {
		t.Error("original BlockedBy was mutated")
	}
	if original.Parent != "bd-003" {
		t.Error("original Parent was mutated")
	}

	// Verify the renamed version has new values.
	if renamed.Body != "See tkt-001" {
		t.Errorf("renamed Body = %q, want %q", renamed.Body, "See tkt-001")
	}
	if renamed.BlockedBy[0] != "tkt-002" {
		t.Errorf("renamed BlockedBy[0] = %q, want %q", renamed.BlockedBy[0], "tkt-002")
	}
}

func TestRenameBeadsIDs_CustomPrefix(t *testing.T) {
	content := TicketContent{
		Version:   1,
		Title:     "Custom prefix proj-abc",
		Body:      "See proj-def",
		Status:    "open",
		Type:      "task",
		BlockedBy: []string{"proj-ghi"},
	}

	newID, newContent := RenameBeadsIDs("proj-abc", content, "proj", "tkt")

	if newID != "tkt-abc" {
		t.Errorf("ID = %q, want %q", newID, "tkt-abc")
	}
	if newContent.Title != "Custom prefix tkt-abc" {
		t.Errorf("Title = %q, want %q", newContent.Title, "Custom prefix tkt-abc")
	}
	if newContent.Body != "See tkt-def" {
		t.Errorf("Body = %q, want %q", newContent.Body, "See tkt-def")
	}
	if newContent.BlockedBy[0] != "tkt-ghi" {
		t.Errorf("BlockedBy[0] = %q, want %q", newContent.BlockedBy[0], "tkt-ghi")
	}
}

func TestRenameBeadsIDs_NoMatchingPrefix(t *testing.T) {
	content := TicketContent{
		Version:   1,
		Title:     "No matching refs",
		Body:      "This has tkt-001 and no beads refs",
		Status:    "open",
		Type:      "task",
		BlockedBy: []string{"tkt-002"},
		Parent:    "tkt-003",
	}

	newID, newContent := RenameBeadsIDs("tkt-999", content, "bd", "tkt")

	// ID doesn't match source prefix — returned unchanged.
	if newID != "tkt-999" {
		t.Errorf("ID = %q, want %q", newID, "tkt-999")
	}
	// Body has no bd- references — unchanged.
	if newContent.Body != "This has tkt-001 and no beads refs" {
		t.Errorf("Body = %q", newContent.Body)
	}
	// BlockedBy doesn't match source prefix — unchanged.
	if newContent.BlockedBy[0] != "tkt-002" {
		t.Errorf("BlockedBy[0] = %q, want %q", newContent.BlockedBy[0], "tkt-002")
	}
}

func TestRenameBeadsRefsInText_WordBoundary(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "standalone ref",
			input: "See bd-abc for details",
			want:  "See tkt-abc for details",
		},
		{
			name:  "ref at start of line",
			input: "bd-abc is important",
			want:  "tkt-abc is important",
		},
		{
			name:  "ref at end of line",
			input: "Related to bd-abc",
			want:  "Related to tkt-abc",
		},
		{
			name:  "ref in parentheses",
			input: "(see bd-abc)",
			want:  "(see tkt-abc)",
		},
		{
			name:  "multiple refs",
			input: "bd-001 blocks bd-002 which blocks bd-003",
			want:  "tkt-001 blocks tkt-002 which blocks tkt-003",
		},
		{
			name:  "hyphenated context still matches",
			input: "see-bd-abc is a hyphenated compound",
			want:  "see-tkt-abc is a hyphenated compound",
		},
		{
			name:  "ref with longer hash",
			input: "See bd-10g2abcdef",
			want:  "See tkt-10g2abcdef",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := renameBeadsRefsInText(test.input, "bd", "tkt")
			if got != test.want {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}
