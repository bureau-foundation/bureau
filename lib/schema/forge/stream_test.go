// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"testing"
)

// --- Event.Repo() ---

func TestEvent_Repo(t *testing.T) {
	tests := []struct {
		name  string
		event Event
		want  string
	}{
		{
			name: "push",
			event: Event{
				Type: EventCategoryPush,
				Push: &PushEvent{Provider: "github", Repo: "owner/repo"},
			},
			want: "owner/repo",
		},
		{
			name: "pull_request",
			event: Event{
				Type:        EventCategoryPullRequest,
				PullRequest: &PullRequestEvent{Provider: "github", Repo: "org/project"},
			},
			want: "org/project",
		},
		{
			name: "issues",
			event: Event{
				Type:  EventCategoryIssues,
				Issue: &IssueEvent{Provider: "github", Repo: "a/b"},
			},
			want: "a/b",
		},
		{
			name: "review",
			event: Event{
				Type:   EventCategoryReview,
				Review: &ReviewEvent{Provider: "github", Repo: "x/y"},
			},
			want: "x/y",
		},
		{
			name: "comment",
			event: Event{
				Type:    EventCategoryComment,
				Comment: &CommentEvent{Provider: "github", Repo: "c/d"},
			},
			want: "c/d",
		},
		{
			name: "ci_status",
			event: Event{
				Type:     EventCategoryCIStatus,
				CIStatus: &CIStatusEvent{Provider: "github", Repo: "e/f"},
			},
			want: "e/f",
		},
		{
			name:  "nil variant",
			event: Event{Type: EventCategoryPush},
			want:  "",
		},
		{
			name:  "unknown type",
			event: Event{Type: "unknown"},
			want:  "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.event.Repo()
			if got != test.want {
				t.Errorf("Repo() = %q, want %q", got, test.want)
			}
		})
	}
}

// --- Event.Provider() ---

func TestEvent_Provider(t *testing.T) {
	tests := []struct {
		name  string
		event Event
		want  string
	}{
		{
			name: "push returns provider",
			event: Event{
				Type: EventCategoryPush,
				Push: &PushEvent{Provider: "github"},
			},
			want: "github",
		},
		{
			name: "issue returns provider",
			event: Event{
				Type:  EventCategoryIssues,
				Issue: &IssueEvent{Provider: "forgejo"},
			},
			want: "forgejo",
		},
		{
			name:  "nil variant returns empty",
			event: Event{Type: EventCategoryReview},
			want:  "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.event.Provider()
			if got != test.want {
				t.Errorf("Provider() = %q, want %q", got, test.want)
			}
		})
	}
}

// --- Event.EntityRefFromEvent() ---

func TestEvent_EntityRefFromEvent(t *testing.T) {
	tests := []struct {
		name      string
		event     Event
		wantRef   EntityRef
		wantFound bool
	}{
		{
			name: "push has no entity",
			event: Event{
				Type: EventCategoryPush,
				Push: &PushEvent{Provider: "github", Repo: "o/r"},
			},
			wantRef:   EntityRef{},
			wantFound: false,
		},
		{
			name: "pull_request",
			event: Event{
				Type: EventCategoryPullRequest,
				PullRequest: &PullRequestEvent{
					Provider: "github", Repo: "o/r", Number: 42,
				},
			},
			wantRef: EntityRef{
				Provider: "github", Repo: "o/r",
				EntityType: EntityTypePullRequest, Number: 42,
			},
			wantFound: true,
		},
		{
			name: "issue",
			event: Event{
				Type: EventCategoryIssues,
				Issue: &IssueEvent{
					Provider: "github", Repo: "o/r", Number: 7,
				},
			},
			wantRef: EntityRef{
				Provider: "github", Repo: "o/r",
				EntityType: EntityTypeIssue, Number: 7,
			},
			wantFound: true,
		},
		{
			name: "review targets parent PR",
			event: Event{
				Type: EventCategoryReview,
				Review: &ReviewEvent{
					Provider: "github", Repo: "o/r", PRNumber: 15,
				},
			},
			wantRef: EntityRef{
				Provider: "github", Repo: "o/r",
				EntityType: EntityTypePullRequest, Number: 15,
			},
			wantFound: true,
		},
		{
			name: "comment on issue",
			event: Event{
				Type: EventCategoryComment,
				Comment: &CommentEvent{
					Provider: "github", Repo: "o/r",
					EntityType: "issue", EntityNumber: 3,
				},
			},
			wantRef: EntityRef{
				Provider: "github", Repo: "o/r",
				EntityType: EntityTypeIssue, Number: 3,
			},
			wantFound: true,
		},
		{
			name: "comment on pull_request",
			event: Event{
				Type: EventCategoryComment,
				Comment: &CommentEvent{
					Provider: "github", Repo: "o/r",
					EntityType: "pull_request", EntityNumber: 9,
				},
			},
			wantRef: EntityRef{
				Provider: "github", Repo: "o/r",
				EntityType: EntityTypePullRequest, Number: 9,
			},
			wantFound: true,
		},
		{
			name: "ci_status workflow run",
			event: Event{
				Type: EventCategoryCIStatus,
				CIStatus: &CIStatusEvent{
					Provider: "github", Repo: "o/r", RunID: "12345",
				},
			},
			wantRef: EntityRef{
				Provider: "github", Repo: "o/r",
				EntityType: EntityTypeWorkflowRun, RunID: "12345",
			},
			wantFound: true,
		},
		{
			name:      "nil variant",
			event:     Event{Type: EventCategoryIssues},
			wantRef:   EntityRef{},
			wantFound: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotRef, gotFound := test.event.EntityRefFromEvent()
			if gotFound != test.wantFound {
				t.Fatalf("EntityRefFromEvent() found = %v, want %v", gotFound, test.wantFound)
			}
			if gotRef != test.wantRef {
				t.Errorf("EntityRefFromEvent() ref = %+v, want %+v", gotRef, test.wantRef)
			}
		})
	}
}

// --- Event.IsEntityClose() ---

func TestEvent_IsEntityClose(t *testing.T) {
	tests := []struct {
		name  string
		event Event
		want  bool
	}{
		{
			name: "issue closed",
			event: Event{
				Type:  EventCategoryIssues,
				Issue: &IssueEvent{Action: string(IssueClosed)},
			},
			want: true,
		},
		{
			name: "issue opened is not close",
			event: Event{
				Type:  EventCategoryIssues,
				Issue: &IssueEvent{Action: string(IssueOpened)},
			},
			want: false,
		},
		{
			name: "issue reopened is not close",
			event: Event{
				Type:  EventCategoryIssues,
				Issue: &IssueEvent{Action: string(IssueReopened)},
			},
			want: false,
		},
		{
			name: "PR closed",
			event: Event{
				Type:        EventCategoryPullRequest,
				PullRequest: &PullRequestEvent{Action: string(PullRequestClosed)},
			},
			want: true,
		},
		{
			name: "PR merged",
			event: Event{
				Type:        EventCategoryPullRequest,
				PullRequest: &PullRequestEvent{Action: string(PullRequestMerged)},
			},
			want: true,
		},
		{
			name: "PR opened is not close",
			event: Event{
				Type:        EventCategoryPullRequest,
				PullRequest: &PullRequestEvent{Action: string(PullRequestOpened)},
			},
			want: false,
		},
		{
			name: "CI completed",
			event: Event{
				Type:     EventCategoryCIStatus,
				CIStatus: &CIStatusEvent{Status: string(CIStatusCompleted)},
			},
			want: true,
		},
		{
			name: "CI in_progress is not close",
			event: Event{
				Type:     EventCategoryCIStatus,
				CIStatus: &CIStatusEvent{Status: string(CIStatusInProgress)},
			},
			want: false,
		},
		{
			name: "push is never close",
			event: Event{
				Type: EventCategoryPush,
				Push: &PushEvent{},
			},
			want: false,
		},
		{
			name: "review is never close",
			event: Event{
				Type:   EventCategoryReview,
				Review: &ReviewEvent{State: string(ReviewApproved)},
			},
			want: false,
		},
		{
			name: "comment is never close",
			event: Event{
				Type:    EventCategoryComment,
				Comment: &CommentEvent{},
			},
			want: false,
		},
		{
			name:  "nil variant",
			event: Event{Type: EventCategoryIssues},
			want:  false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.event.IsEntityClose()
			if got != test.want {
				t.Errorf("IsEntityClose() = %v, want %v", got, test.want)
			}
		})
	}
}

// --- EntityRef.IsZero() ---

func TestEntityRef_IsZero(t *testing.T) {
	if !(EntityRef{}).IsZero() {
		t.Error("zero EntityRef should be zero")
	}

	nonZero := EntityRef{Provider: "github", Repo: "o/r", EntityType: EntityTypeIssue, Number: 1}
	if nonZero.IsZero() {
		t.Error("non-zero EntityRef should not be zero")
	}

	partial := EntityRef{Provider: "github"}
	if partial.IsZero() {
		t.Error("partially-populated EntityRef should not be zero")
	}
}
