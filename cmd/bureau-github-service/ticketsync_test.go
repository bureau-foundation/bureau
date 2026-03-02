// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/forgesub"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/forge"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
)

// --- Deterministic ticket ID ---

func TestIssueTicketID(t *testing.T) {
	tests := []struct {
		provider string
		repo     string
		number   int
		want     string
	}{
		{"github", "octocat/hello", 42, "gh-octocat-hello-42"},
		{"github", "bureau-foundation/bureau", 1, "gh-bureau-foundation-bureau-1"},
		{"github", "org/repo.name", 99, "gh-org-repo-name-99"},
	}

	for _, tt := range tests {
		got := issueTicketID(tt.provider, tt.repo, tt.number)
		if got != tt.want {
			t.Errorf("issueTicketID(%q, %q, %d) = %q, want %q",
				tt.provider, tt.repo, tt.number, got, tt.want)
		}
	}
}

// --- Issue → TicketContent translation ---

func TestIssueToTicketContent_Opened(t *testing.T) {
	issue := &forge.IssueEvent{
		Provider: "github",
		Repo:     "octocat/hello",
		Number:   42,
		Action:   "opened",
		Title:    "Fix the thing",
		Body:     "The thing is broken.\n\nSteps to reproduce:\n1. Do X\n2. See Y",
		Author:   "octocat",
		Labels:   []string{"bug", "P1"},
		Summary:  "[octocat/hello] opened #42: Fix the thing",
		URL:      "https://github.com/octocat/hello/issues/42",
	}

	content := issueToTicketContent(issue)

	if content.Title != "Fix the thing" {
		t.Errorf("Title = %q, want %q", content.Title, "Fix the thing")
	}
	// Body should be URL + issue body (not summary, since body is present).
	wantBody := "https://github.com/octocat/hello/issues/42\n\nThe thing is broken.\n\nSteps to reproduce:\n1. Do X\n2. See Y"
	if content.Body != wantBody {
		t.Errorf("Body = %q, want %q", content.Body, wantBody)
	}
	if content.Status != ticket.StatusOpen {
		t.Errorf("Status = %q, want %q", content.Status, ticket.StatusOpen)
	}
	if content.Type != ticket.TypeBug {
		t.Errorf("Type = %q, want %q", content.Type, ticket.TypeBug)
	}
	if content.Priority != 1 {
		t.Errorf("Priority = %d, want 1 (from P1 label)", content.Priority)
	}
	if content.Origin == nil {
		t.Fatal("Origin is nil")
	}
	if content.Origin.Source != "github" {
		t.Errorf("Origin.Source = %q, want %q", content.Origin.Source, "github")
	}
	if content.Origin.ExternalRef != "octocat/hello#42" {
		t.Errorf("Origin.ExternalRef = %q, want %q", content.Origin.ExternalRef, "octocat/hello#42")
	}
	if content.Version != ticket.TicketContentVersion {
		t.Errorf("Version = %d, want %d", content.Version, ticket.TicketContentVersion)
	}
	if content.ClosedAt != "" {
		t.Errorf("ClosedAt = %q, want empty for open issue", content.ClosedAt)
	}
	if len(content.Labels) != 2 {
		t.Errorf("Labels count = %d, want 2", len(content.Labels))
	}
}

func TestIssueToTicketContent_EmptyBodyFallsBackToSummary(t *testing.T) {
	issue := &forge.IssueEvent{
		Provider: "github",
		Repo:     "octocat/hello",
		Number:   7,
		Action:   "opened",
		Title:    "Quick fix",
		Author:   "octocat",
		Summary:  "[octocat/hello] opened #7: Quick fix",
		URL:      "https://github.com/octocat/hello/issues/7",
	}

	content := issueToTicketContent(issue)

	wantBody := "https://github.com/octocat/hello/issues/7\n\n[octocat/hello] opened #7: Quick fix"
	if content.Body != wantBody {
		t.Errorf("Body = %q, want %q (should fall back to summary when body is empty)", content.Body, wantBody)
	}
}

func TestIssueToTicketContent_Closed(t *testing.T) {
	issue := &forge.IssueEvent{
		Provider: "github",
		Repo:     "octocat/hello",
		Number:   42,
		Action:   "closed",
		Title:    "Fix the thing",
		Author:   "octocat",
		URL:      "https://github.com/octocat/hello/issues/42",
	}

	content := issueToTicketContent(issue)

	if content.Status != ticket.StatusClosed {
		t.Errorf("Status = %q, want %q", content.Status, ticket.StatusClosed)
	}
	if content.ClosedAt == "" {
		t.Error("ClosedAt should be set for closed issues")
	}
	if content.CloseReason != "Closed on GitHub" {
		t.Errorf("CloseReason = %q, want %q", content.CloseReason, "Closed on GitHub")
	}
}

// --- Priority derivation ---

func TestDerivePriority(t *testing.T) {
	tests := []struct {
		labels []string
		want   int
	}{
		{nil, 2},                        // default
		{[]string{}, 2},                 // default
		{[]string{"unrelated"}, 2},      // no match → default
		{[]string{"P0"}, 0},             // exact match
		{[]string{"critical"}, 0},       // alias
		{[]string{"P1"}, 1},             // high
		{[]string{"high"}, 1},           // alias
		{[]string{"P3", "P1"}, 1},       // takes highest (lowest number)
		{[]string{"low", "backlog"}, 3}, // takes highest of the two
		{[]string{"P4"}, 4},             // backlog
		{[]string{"HIGH PRIORITY"}, 1},  // case insensitive
	}

	for _, tt := range tests {
		got := derivePriority(tt.labels)
		if got != tt.want {
			t.Errorf("derivePriority(%v) = %d, want %d", tt.labels, got, tt.want)
		}
	}
}

// --- Type derivation ---

func TestDeriveType(t *testing.T) {
	tests := []struct {
		labels []string
		want   ticket.TicketType
	}{
		{nil, ticket.TypeTask},                        // default
		{[]string{}, ticket.TypeTask},                 // default
		{[]string{"unrelated"}, ticket.TypeTask},      // no match → default
		{[]string{"bug"}, ticket.TypeBug},             // exact
		{[]string{"feature"}, ticket.TypeFeature},     // exact
		{[]string{"enhancement"}, ticket.TypeFeature}, // alias
		{[]string{"documentation"}, ticket.TypeDocs},  // exact
		{[]string{"docs"}, ticket.TypeDocs},           // alias
		{[]string{"question"}, ticket.TypeQuestion},   // exact
		{[]string{"chore"}, ticket.TypeChore},         // exact
		{[]string{"Bug"}, ticket.TypeBug},             // case insensitive
		{[]string{"P1", "bug"}, ticket.TypeBug},       // non-type label skipped
	}

	for _, tt := range tests {
		got := deriveType(tt.labels)
		if got != tt.want {
			t.Errorf("deriveType(%v) = %q, want %q", tt.labels, got, tt.want)
		}
	}
}

// --- issueSyncEnabled ---

func TestIssueSyncEnabled(t *testing.T) {
	tests := []struct {
		name   string
		config *forge.ForgeConfig
		want   bool
	}{
		{"nil config", nil, false},
		{"no issue_sync", &forge.ForgeConfig{}, false},
		{"sync none", &forge.ForgeConfig{IssueSync: forge.IssueSyncNone}, false},
		{"sync import", &forge.ForgeConfig{IssueSync: forge.IssueSyncImport}, true},
		{"sync bidirectional", &forge.ForgeConfig{IssueSync: forge.IssueSyncBidirectional}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := issueSyncEnabled(tt.config)
			if got != tt.want {
				t.Errorf("issueSyncEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Room filtering ---

func TestSyncEvent_SkipsRoomsWithoutIssueSync(t *testing.T) {
	testRoomID := func(name string) ref.RoomID {
		roomID, err := ref.ParseRoomID("!" + name + ":test.local")
		if err != nil {
			t.Fatal(err)
		}
		return roomID
	}

	rooms := []forgesub.RoomMatch{
		{
			RoomID: testRoomID("room1"),
			Config: &forge.ForgeConfig{IssueSync: forge.IssueSyncNone},
		},
		{
			RoomID: testRoomID("room2"),
			Config: nil, // no config
		},
		{
			RoomID: testRoomID("room3"),
			Config: &forge.ForgeConfig{IssueSync: forge.IssueSyncImport},
		},
	}

	// Only room3 has issue_sync enabled.
	enabledCount := 0
	for _, room := range rooms {
		if issueSyncEnabled(room.Config) {
			enabledCount++
		}
	}
	if enabledCount != 1 {
		t.Errorf("expected 1 room with issue_sync enabled, got %d", enabledCount)
	}
}
