// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forgesub

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema/forge"
)

func TestMatchesForgeConfig_NilConfig(t *testing.T) {
	event := &forge.Event{Type: forge.EventCategoryPush, Push: &forge.PushEvent{}}
	if matchesForgeConfig(nil, event) {
		t.Error("nil config should reject all events")
	}
}

func TestMatchesForgeConfig_EmptyEvents(t *testing.T) {
	config := &forge.ForgeConfig{Events: []string{}}
	event := &forge.Event{Type: forge.EventCategoryPush, Push: &forge.PushEvent{}}
	if matchesForgeConfig(config, event) {
		t.Error("empty Events list should reject all events (default-deny)")
	}
}

func TestMatchesForgeConfig_CategoryMatch(t *testing.T) {
	config := &forge.ForgeConfig{
		Events: []string{forge.EventCategoryPush, forge.EventCategoryIssues},
	}

	// Matching category.
	push := &forge.Event{Type: forge.EventCategoryPush, Push: &forge.PushEvent{}}
	if !matchesForgeConfig(config, push) {
		t.Error("push should match config with push in Events")
	}

	issue := &forge.Event{
		Type:  forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{Action: "opened"},
	}
	if !matchesForgeConfig(config, issue) {
		t.Error("issue should match config with issues in Events")
	}

	// Non-matching category.
	review := &forge.Event{
		Type:   forge.EventCategoryReview,
		Review: &forge.ReviewEvent{State: "approved"},
	}
	if matchesForgeConfig(config, review) {
		t.Error("review should not match config without review in Events")
	}
}

func TestMatchesTriageFilter_Nil(t *testing.T) {
	event := &forge.Event{Type: forge.EventCategoryPush, Push: &forge.PushEvent{}}
	if !matchesTriageFilter(nil, event) {
		t.Error("nil filter should pass all events")
	}
}

func TestMatchesTriageFilter_Labels(t *testing.T) {
	filter := &forge.TriageFilter{
		Labels: []string{"bug", "security"},
	}

	// Issue with matching label.
	matching := &forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Action: "opened",
			Labels: []string{"feature", "bug"},
		},
	}
	if !matchesTriageFilter(filter, matching) {
		t.Error("issue with 'bug' label should match")
	}

	// Issue without matching label.
	noMatch := &forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Action: "opened",
			Labels: []string{"feature", "docs"},
		},
	}
	if matchesTriageFilter(filter, noMatch) {
		t.Error("issue without matching labels should not match")
	}

	// Issue with no labels at all.
	noLabels := &forge.Event{
		Type:  forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{Action: "opened"},
	}
	if matchesTriageFilter(filter, noLabels) {
		t.Error("issue with no labels should not match when labels filter is set")
	}

	// Push event (no labels) — passes vacuously.
	push := &forge.Event{Type: forge.EventCategoryPush, Push: &forge.PushEvent{}}
	if !matchesTriageFilter(filter, push) {
		t.Error("push event should pass label filter vacuously (push has no labels)")
	}

	// Review event — passes vacuously.
	review := &forge.Event{
		Type:   forge.EventCategoryReview,
		Review: &forge.ReviewEvent{State: "approved"},
	}
	if !matchesTriageFilter(filter, review) {
		t.Error("review event should pass label filter vacuously")
	}
}

func TestMatchesTriageFilter_EventTypes(t *testing.T) {
	filter := &forge.TriageFilter{
		EventTypes: []string{"issue_opened", "pr_opened"},
	}

	// Matching event type.
	issueOpened := &forge.Event{
		Type:  forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{Action: "opened"},
	}
	if !matchesTriageFilter(filter, issueOpened) {
		t.Error("issue_opened should match")
	}

	// Non-matching event type.
	issueClosed := &forge.Event{
		Type:  forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{Action: "closed"},
	}
	if matchesTriageFilter(filter, issueClosed) {
		t.Error("issue_closed should not match filter for issue_opened")
	}

	// PR opened matches.
	prOpened := &forge.Event{
		Type:        forge.EventCategoryPullRequest,
		PullRequest: &forge.PullRequestEvent{Action: "opened"},
	}
	if !matchesTriageFilter(filter, prOpened) {
		t.Error("pr_opened should match")
	}

	// Push — compound string is "push", not in filter.
	push := &forge.Event{Type: forge.EventCategoryPush, Push: &forge.PushEvent{}}
	if matchesTriageFilter(filter, push) {
		t.Error("push should not match filter for issue_opened and pr_opened")
	}
}

func TestMatchesTriageFilter_Conjunctive(t *testing.T) {
	// Both labels AND event types must match.
	filter := &forge.TriageFilter{
		Labels:     []string{"bug"},
		EventTypes: []string{"issue_opened"},
	}

	// Matches both.
	both := &forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Action: "opened",
			Labels: []string{"bug"},
		},
	}
	if !matchesTriageFilter(filter, both) {
		t.Error("event matching both label and event type should pass")
	}

	// Matches label but not event type.
	labelOnly := &forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Action: "closed",
			Labels: []string{"bug"},
		},
	}
	if matchesTriageFilter(filter, labelOnly) {
		t.Error("event matching label but not event type should fail")
	}

	// Matches event type but not label.
	typeOnly := &forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Action: "opened",
			Labels: []string{"feature"},
		},
	}
	if matchesTriageFilter(filter, typeOnly) {
		t.Error("event matching event type but not label should fail")
	}
}

func TestEventTypeString(t *testing.T) {
	tests := []struct {
		name  string
		event *forge.Event
		want  string
	}{
		{
			name:  "push",
			event: &forge.Event{Type: forge.EventCategoryPush, Push: &forge.PushEvent{}},
			want:  "push",
		},
		{
			name: "issue_opened",
			event: &forge.Event{
				Type:  forge.EventCategoryIssues,
				Issue: &forge.IssueEvent{Action: "opened"},
			},
			want: "issue_opened",
		},
		{
			name: "pr_merged",
			event: &forge.Event{
				Type:        forge.EventCategoryPullRequest,
				PullRequest: &forge.PullRequestEvent{Action: "merged"},
			},
			want: "pr_merged",
		},
		{
			name: "review_approved",
			event: &forge.Event{
				Type:   forge.EventCategoryReview,
				Review: &forge.ReviewEvent{State: "approved"},
			},
			want: "review_approved",
		},
		{
			name: "comment_created",
			event: &forge.Event{
				Type:    forge.EventCategoryComment,
				Comment: &forge.CommentEvent{},
			},
			want: "comment_created",
		},
		{
			name: "ci_completed",
			event: &forge.Event{
				Type:     forge.EventCategoryCIStatus,
				CIStatus: &forge.CIStatusEvent{Status: "completed"},
			},
			want: "ci_completed",
		},
		{
			name:  "nil variant",
			event: &forge.Event{Type: forge.EventCategoryIssues},
			want:  "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := eventTypeString(test.event)
			if got != test.want {
				t.Errorf("eventTypeString() = %q, want %q", got, test.want)
			}
		})
	}
}
