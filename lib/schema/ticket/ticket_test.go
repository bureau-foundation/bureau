// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// assertField checks that a JSON object has a field with the expected value.
func assertField(t *testing.T, object map[string]any, key string, want any) {
	t.Helper()
	got, ok := object[key]
	if !ok {
		t.Errorf("field %q is missing", key)
		return
	}
	if got != want {
		t.Errorf("field %q = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}

// validTicketContent returns a TicketContent with all required fields
// set to valid values. Tests modify individual fields to test validation.
func validTicketContent() TicketContent {
	return TicketContent{
		Version:   TicketContentVersion,
		Title:     "Fix authentication bug in login flow",
		Status:    StatusOpen,
		Priority:  2,
		Type:      TypeBug,
		CreatedBy: ref.MustParseUserID("@iree/amdgpu/pm:bureau.local"),
		CreatedAt: "2026-02-12T10:00:00Z",
		UpdatedAt: "2026-02-12T10:00:00Z",
	}
}

func TestTicketContentRoundTrip(t *testing.T) {
	original := TicketContent{
		Version:   TicketContentVersion,
		Title:     "Implement AMDGPU inference pipeline",
		Body:      "Set up the full inference pipeline for AMDGPU targets.",
		Status:    StatusInProgress,
		Priority:  1,
		Type:      TypeFeature,
		Labels:    []string{"amdgpu", "inference", "p1"},
		Affects:   []string{"fleet/gpu/a100", "workspace/lib/schema/ticket.go"},
		Assignee:  ref.MustParseUserID("@iree/amdgpu/pm:bureau.local"),
		Parent:    "tkt-epic1",
		BlockedBy: []string{"tkt-a3f9", "tkt-b2c4"},
		Gates: []TicketGate{
			{
				ID:          "ci-pass",
				Type:        "pipeline",
				Status:      "pending",
				Description: "CI pipeline must pass",
				PipelineRef: "ci/amdgpu-tests",
				Conclusion:  "success",
				CreatedAt:   "2026-02-12T10:00:00Z",
			},
			{
				ID:          "lead-approval",
				Type:        "human",
				Status:      "satisfied",
				Description: "Team lead approval",
				CreatedAt:   "2026-02-12T10:00:00Z",
				SatisfiedAt: "2026-02-12T11:00:00Z",
				SatisfiedBy: "@bureau/admin:bureau.local",
			},
		},
		Notes: []TicketNote{
			{
				ID:        "n-1",
				Author:    ref.MustParseUserID("@bureau/admin:bureau.local"),
				CreatedAt: "2026-02-12T10:30:00Z",
				Body:      "Be careful about the memory alignment on MI300X.",
			},
		},
		Attachments: []TicketAttachment{
			{
				Ref:         "art-a3f9b2c1e7d4",
				Label:       "stack trace from crash",
				ContentType: "text/plain",
			},
			{
				Ref:   "art-b7e3d9f0a1c5",
				Label: "screenshot of rendering bug",
			},
		},
		CreatedBy:   ref.MustParseUserID("@bureau/admin:bureau.local"),
		CreatedAt:   "2026-02-12T09:00:00Z",
		UpdatedAt:   "2026-02-12T11:00:00Z",
		ClosedAt:    "",
		CloseReason: "",
		ContextID:   "ctx-a1b2c3d4",
		Origin: &TicketOrigin{
			Source:      "github",
			ExternalRef: "GH-4201",
			SourceRoom:  "!old_room:bureau.local",
		},
		Extra: map[string]json.RawMessage{
			"experimental": json.RawMessage(`{"nested":true}`),
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify JSON field names match the wire format.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "version", float64(TicketContentVersion))
	assertField(t, raw, "title", original.Title)
	assertField(t, raw, "body", original.Body)
	assertField(t, raw, "status", "in_progress")
	assertField(t, raw, "priority", float64(1))
	assertField(t, raw, "type", "feature")
	assertField(t, raw, "assignee", original.Assignee.String())
	assertField(t, raw, "parent", "tkt-epic1")
	assertField(t, raw, "created_by", "@bureau/admin:bureau.local")
	assertField(t, raw, "created_at", "2026-02-12T09:00:00Z")
	assertField(t, raw, "updated_at", "2026-02-12T11:00:00Z")
	assertField(t, raw, "context_id", "ctx-a1b2c3d4")

	// Labels: verify as JSON array.
	labels, ok := raw["labels"].([]any)
	if !ok {
		t.Fatalf("labels is not an array: %T", raw["labels"])
	}
	if len(labels) != 3 || labels[0] != "amdgpu" || labels[1] != "inference" || labels[2] != "p1" {
		t.Errorf("labels = %v, want [amdgpu inference p1]", labels)
	}

	// Affects: verify as JSON array.
	affects, ok := raw["affects"].([]any)
	if !ok {
		t.Fatalf("affects is not an array: %T", raw["affects"])
	}
	if len(affects) != 2 || affects[0] != "fleet/gpu/a100" || affects[1] != "workspace/lib/schema/ticket.go" {
		t.Errorf("affects = %v, want [fleet/gpu/a100 workspace/lib/schema/ticket.go]", affects)
	}

	// BlockedBy: verify as JSON array.
	blockedBy, ok := raw["blocked_by"].([]any)
	if !ok {
		t.Fatalf("blocked_by is not an array: %T", raw["blocked_by"])
	}
	if len(blockedBy) != 2 || blockedBy[0] != "tkt-a3f9" || blockedBy[1] != "tkt-b2c4" {
		t.Errorf("blocked_by = %v, want [tkt-a3f9 tkt-b2c4]", blockedBy)
	}

	// Gates: verify first gate wire format.
	gates, ok := raw["gates"].([]any)
	if !ok {
		t.Fatalf("gates is not an array: %T", raw["gates"])
	}
	if len(gates) != 2 {
		t.Fatalf("gates count = %d, want 2", len(gates))
	}
	firstGate := gates[0].(map[string]any)
	assertField(t, firstGate, "id", "ci-pass")
	assertField(t, firstGate, "type", "pipeline")
	assertField(t, firstGate, "status", "pending")
	assertField(t, firstGate, "description", "CI pipeline must pass")
	assertField(t, firstGate, "pipeline_ref", "ci/amdgpu-tests")
	assertField(t, firstGate, "conclusion", "success")

	// Satisfied gate: verify lifecycle metadata.
	secondGate := gates[1].(map[string]any)
	assertField(t, secondGate, "id", "lead-approval")
	assertField(t, secondGate, "type", "human")
	assertField(t, secondGate, "status", "satisfied")
	assertField(t, secondGate, "satisfied_at", "2026-02-12T11:00:00Z")
	assertField(t, secondGate, "satisfied_by", "@bureau/admin:bureau.local")

	// Notes: verify wire format.
	notes, ok := raw["notes"].([]any)
	if !ok {
		t.Fatalf("notes is not an array: %T", raw["notes"])
	}
	if len(notes) != 1 {
		t.Fatalf("notes count = %d, want 1", len(notes))
	}
	firstNote := notes[0].(map[string]any)
	assertField(t, firstNote, "id", "n-1")
	assertField(t, firstNote, "author", "@bureau/admin:bureau.local")
	assertField(t, firstNote, "body", "Be careful about the memory alignment on MI300X.")

	// Attachments: verify wire format.
	attachments, ok := raw["attachments"].([]any)
	if !ok {
		t.Fatalf("attachments is not an array: %T", raw["attachments"])
	}
	if len(attachments) != 2 {
		t.Fatalf("attachments count = %d, want 2", len(attachments))
	}
	firstAttachment := attachments[0].(map[string]any)
	assertField(t, firstAttachment, "ref", "art-a3f9b2c1e7d4")
	assertField(t, firstAttachment, "label", "stack trace from crash")
	assertField(t, firstAttachment, "content_type", "text/plain")

	// Origin: verify wire format.
	origin, ok := raw["origin"].(map[string]any)
	if !ok {
		t.Fatalf("origin is not an object: %T", raw["origin"])
	}
	assertField(t, origin, "source", "github")
	assertField(t, origin, "external_ref", "GH-4201")
	assertField(t, origin, "source_room", "!old_room:bureau.local")

	// Extra: verify nested structure.
	extra, ok := raw["extra"].(map[string]any)
	if !ok {
		t.Fatalf("extra is not an object: %T", raw["extra"])
	}
	experimental, ok := extra["experimental"].(map[string]any)
	if !ok {
		t.Fatalf("extra.experimental is not an object: %T", extra["experimental"])
	}
	if experimental["nested"] != true {
		t.Errorf("extra.experimental.nested = %v, want true", experimental["nested"])
	}

	// Round-trip: marshal -> unmarshal -> compare.
	var decoded TicketContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

func TestTicketContentOmitsEmptyOptionals(t *testing.T) {
	content := validTicketContent()

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	optionalFields := []string{
		"body", "labels", "affects", "parent", "blocked_by",
		"gates", "notes", "attachments", "closed_at", "close_reason",
		"context_id", "origin", "review", "pipeline", "extra",
	}
	// Assignee is ref.UserID which implements TextMarshaler — Go's
	// encoding/json emits "" for the zero value even with omitempty.
	// Verify it marshals to empty string rather than being absent.
	if got, ok := raw["assignee"]; !ok {
		t.Error("assignee should be present as empty string for zero UserID")
	} else if got != "" {
		t.Errorf("assignee = %v, want empty string for zero UserID", got)
	}
	for _, field := range optionalFields {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty, but is present", field)
		}
	}

	// Required fields must be present.
	requiredFields := []string{
		"version", "title", "status", "priority", "type",
		"created_by", "created_at", "updated_at",
	}
	for _, field := range requiredFields {
		if _, exists := raw[field]; !exists {
			t.Errorf("required field %s is missing from JSON", field)
		}
	}
}

func TestTicketContentExtraRoundTrip(t *testing.T) {
	original := validTicketContent()
	original.Extra = map[string]json.RawMessage{
		"string_field":  json.RawMessage(`"hello"`),
		"number_field":  json.RawMessage(`42`),
		"object_field":  json.RawMessage(`{"key":"value","count":3}`),
		"array_field":   json.RawMessage(`[1,2,3]`),
		"boolean_field": json.RawMessage(`false`),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded TicketContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Extra) != 5 {
		t.Fatalf("Extra has %d entries, want 5", len(decoded.Extra))
	}

	for key, want := range original.Extra {
		got, ok := decoded.Extra[key]
		if !ok {
			t.Errorf("Extra[%q] missing after round-trip", key)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("Extra[%q] = %s, want %s", key, got, want)
		}
	}
}

func TestTicketContentForwardCompatibility(t *testing.T) {
	// Simulate a future-version event with an unknown top-level field.
	// This documents the behavior that CanModify guards against:
	// unknown fields are silently dropped on unmarshal, so a
	// read-modify-write cycle through current code would lose them.
	futureVersion := TicketContentVersion + 1
	futureJSON := fmt.Sprintf(`{
		"version": %d,
		"title": "Fix something",
		"status": "open",
		"priority": 2,
		"type": "bug",
		"created_by": "@bureau/admin:bureau.local",
		"created_at": "2026-02-12T10:00:00Z",
		"updated_at": "2026-02-12T10:00:00Z",
		"new_future_field": "this field does not exist in the current TicketContent"
	}`, futureVersion)

	// Current code can unmarshal future-version events without error.
	var content TicketContent
	if err := json.Unmarshal([]byte(futureJSON), &content); err != nil {
		t.Fatalf("Unmarshal future event: %v", err)
	}

	// Known fields are correctly populated.
	if content.Version != futureVersion {
		t.Errorf("Version = %d, want %d", content.Version, futureVersion)
	}
	if content.Title != "Fix something" {
		t.Errorf("Title = %q, want %q", content.Title, "Fix something")
	}

	// CanModify rejects modification of future events from current code.
	if err := content.CanModify(); err == nil {
		t.Error("CanModify() should reject future-version events from current code")
	}

	// Re-marshaling drops the unknown field.
	remarshaled, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if strings.Contains(string(remarshaled), "new_future_field") {
		t.Error("unknown field survived re-marshal; expected it to be dropped")
	}
}

func TestTicketContentValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*TicketContent)
		wantErr string
	}{
		{
			name:    "valid",
			modify:  func(tc *TicketContent) {},
			wantErr: "",
		},
		{
			name:    "version_zero",
			modify:  func(tc *TicketContent) { tc.Version = 0 },
			wantErr: "version must be >= 1",
		},
		{
			name:    "version_negative",
			modify:  func(tc *TicketContent) { tc.Version = -1 },
			wantErr: "version must be >= 1",
		},
		{
			name:    "title_empty",
			modify:  func(tc *TicketContent) { tc.Title = "" },
			wantErr: "title is required",
		},
		{
			name:    "status_empty",
			modify:  func(tc *TicketContent) { tc.Status = "" },
			wantErr: "status is required",
		},
		{
			name:    "status_invalid",
			modify:  func(tc *TicketContent) { tc.Status = "wontfix" },
			wantErr: `unknown status "wontfix"`,
		},
		{
			name:    "status_open",
			modify:  func(tc *TicketContent) { tc.Status = StatusOpen },
			wantErr: "",
		},
		{
			name:    "status_in_progress",
			modify:  func(tc *TicketContent) { tc.Status = StatusInProgress },
			wantErr: "",
		},
		{
			name:    "status_blocked",
			modify:  func(tc *TicketContent) { tc.Status = StatusBlocked },
			wantErr: "",
		},
		{
			name:    "status_closed",
			modify:  func(tc *TicketContent) { tc.Status = StatusClosed },
			wantErr: "",
		},
		{
			name: "status_review_with_valid_review",
			modify: func(tc *TicketContent) {
				tc.Status = StatusReview
				tc.Review = &TicketReview{
					Reviewers: []ReviewerEntry{
						{UserID: ref.MustParseUserID("@reviewer:bureau.local"), Disposition: "pending"},
					},
				}
			},
			wantErr: "",
		},
		{
			name:    "review_status_without_review",
			modify:  func(tc *TicketContent) { tc.Status = StatusReview },
			wantErr: "review with at least one reviewer is required when status is \"review\"",
		},
		{
			name: "review_status_with_empty_reviewers",
			modify: func(tc *TicketContent) {
				tc.Status = StatusReview
				tc.Review = &TicketReview{Reviewers: []ReviewerEntry{}}
			},
			wantErr: "review with at least one reviewer is required when status is \"review\"",
		},
		{
			name: "review_on_non_review_status",
			modify: func(tc *TicketContent) {
				tc.Status = StatusOpen
				tc.Review = &TicketReview{
					Reviewers: []ReviewerEntry{
						{UserID: ref.MustParseUserID("@reviewer:bureau.local"), Disposition: "pending"},
					},
				}
			},
			wantErr: "",
		},
		{
			name: "review_with_invalid_reviewer",
			modify: func(tc *TicketContent) {
				tc.Status = StatusReview
				tc.Review = &TicketReview{
					Reviewers: []ReviewerEntry{
						{UserID: ref.UserID{}, Disposition: "pending"},
					},
				}
			},
			wantErr: "review: reviewers[0]: user_id is required",
		},
		{
			name: "review_with_invalid_disposition",
			modify: func(tc *TicketContent) {
				tc.Status = StatusReview
				tc.Review = &TicketReview{
					Reviewers: []ReviewerEntry{
						{UserID: ref.MustParseUserID("@reviewer:bureau.local"), Disposition: "rejected"},
					},
				}
			},
			wantErr: `review: reviewers[0]: unknown disposition "rejected"`,
		},
		{
			name:    "priority_negative",
			modify:  func(tc *TicketContent) { tc.Priority = -1 },
			wantErr: "priority must be 0-4",
		},
		{
			name:    "priority_too_high",
			modify:  func(tc *TicketContent) { tc.Priority = 5 },
			wantErr: "priority must be 0-4",
		},
		{
			name:    "priority_zero_critical",
			modify:  func(tc *TicketContent) { tc.Priority = 0 },
			wantErr: "",
		},
		{
			name:    "priority_four_backlog",
			modify:  func(tc *TicketContent) { tc.Priority = 4 },
			wantErr: "",
		},
		{
			name:    "type_empty",
			modify:  func(tc *TicketContent) { tc.Type = "" },
			wantErr: "type is required",
		},
		{
			name:    "type_invalid",
			modify:  func(tc *TicketContent) { tc.Type = "improvement" },
			wantErr: `unknown type "improvement"`,
		},
		{
			name:    "type_task",
			modify:  func(tc *TicketContent) { tc.Type = TypeTask },
			wantErr: "",
		},
		{
			name:    "type_bug",
			modify:  func(tc *TicketContent) { tc.Type = TypeBug },
			wantErr: "",
		},
		{
			name:    "type_feature",
			modify:  func(tc *TicketContent) { tc.Type = TypeFeature },
			wantErr: "",
		},
		{
			name:    "type_epic",
			modify:  func(tc *TicketContent) { tc.Type = TypeEpic },
			wantErr: "",
		},
		{
			name:    "type_chore",
			modify:  func(tc *TicketContent) { tc.Type = TypeChore },
			wantErr: "",
		},
		{
			name:    "type_docs",
			modify:  func(tc *TicketContent) { tc.Type = TypeDocs },
			wantErr: "",
		},
		{
			name:    "type_question",
			modify:  func(tc *TicketContent) { tc.Type = TypeQuestion },
			wantErr: "",
		},
		{
			name: "type_pipeline",
			modify: func(tc *TicketContent) {
				tc.Type = TypePipeline
				tc.Pipeline = &PipelineExecutionContent{
					PipelineRef: "dev-workspace-init",
					TotalSteps:  3,
				}
			},
			wantErr: "",
		},
		{
			name:    "type_pipeline_missing_content",
			modify:  func(tc *TicketContent) { tc.Type = TypePipeline },
			wantErr: "pipeline content is required when type is \"pipeline\"",
		},
		{
			name: "type_pipeline_invalid_content",
			modify: func(tc *TicketContent) {
				tc.Type = TypePipeline
				tc.Pipeline = &PipelineExecutionContent{
					PipelineRef: "",
					TotalSteps:  3,
				}
			},
			wantErr: "pipeline: pipeline_ref is required",
		},
		{
			name: "non_pipeline_type_with_pipeline_content",
			modify: func(tc *TicketContent) {
				tc.Type = TypeTask
				tc.Pipeline = &PipelineExecutionContent{
					PipelineRef: "dev-workspace-init",
					TotalSteps:  3,
				}
			},
			wantErr: "pipeline content must be nil when type is \"task\"",
		},
		{
			name: "type_review_finding_with_parent",
			modify: func(tc *TicketContent) {
				tc.Type = TypeReviewFinding
				tc.Parent = "tkt-parent"
			},
			wantErr: "",
		},
		{
			name:    "type_review_finding_missing_parent",
			modify:  func(tc *TicketContent) { tc.Type = TypeReviewFinding },
			wantErr: "parent is required for review_finding type",
		},
		{
			name:    "created_by_empty",
			modify:  func(tc *TicketContent) { tc.CreatedBy = ref.UserID{} },
			wantErr: "created_by is required",
		},
		{
			name:    "created_at_empty",
			modify:  func(tc *TicketContent) { tc.CreatedAt = "" },
			wantErr: "created_at is required",
		},
		{
			name:    "updated_at_empty",
			modify:  func(tc *TicketContent) { tc.UpdatedAt = "" },
			wantErr: "updated_at is required",
		},
		{
			name: "invalid_gate",
			modify: func(tc *TicketContent) {
				tc.Gates = []TicketGate{{ID: "", Type: "human", Status: "pending"}}
			},
			wantErr: "gates[0]: gate: id is required",
		},
		{
			name: "invalid_note",
			modify: func(tc *TicketContent) {
				tc.Notes = []TicketNote{{ID: "n-1", Author: ref.UserID{}, CreatedAt: "2026-02-12T10:00:00Z", Body: "test"}}
			},
			wantErr: "notes[0]: note: author is required",
		},
		{
			name: "invalid_attachment",
			modify: func(tc *TicketContent) {
				tc.Attachments = []TicketAttachment{{Ref: ""}}
			},
			wantErr: "attachments[0]: attachment: ref is required",
		},
		{
			name: "invalid_origin",
			modify: func(tc *TicketContent) {
				tc.Origin = &TicketOrigin{Source: "", ExternalRef: "GH-4201"}
			},
			wantErr: "origin: origin: source is required",
		},
		{
			name: "affects_valid",
			modify: func(tc *TicketContent) {
				tc.Affects = []string{"fleet/gpu/a100", "workspace/lib/schema/ticket.go"}
			},
			wantErr: "",
		},
		{
			name: "affects_empty_string",
			modify: func(tc *TicketContent) {
				tc.Affects = []string{"fleet/gpu/a100", ""}
			},
			wantErr: "affects[1]: resource identifier cannot be empty",
		},
		{
			name:    "affects_nil",
			modify:  func(tc *TicketContent) { tc.Affects = nil },
			wantErr: "",
		},
		{
			name: "review_with_invalid_tier_threshold",
			modify: func(tc *TicketContent) {
				tc.Review = &TicketReview{
					Reviewers: []ReviewerEntry{
						{UserID: ref.MustParseUserID("@reviewer:bureau.local"), Disposition: "pending"},
					},
					TierThresholds: []TierThreshold{
						{Tier: -1},
					},
				}
			},
			wantErr: "review: tier_thresholds[0]: tier must be >= 0",
		},
		{
			name: "review_with_valid_tier_thresholds",
			modify: func(tc *TicketContent) {
				threshold := 2
				tc.Review = &TicketReview{
					Reviewers: []ReviewerEntry{
						{UserID: ref.MustParseUserID("@reviewer:bureau.local"), Disposition: "pending", Tier: 1},
					},
					TierThresholds: []TierThreshold{
						{Tier: 0},
						{Tier: 1, Threshold: &threshold},
					},
				}
			},
			wantErr: "",
		},
		{
			name: "valid_with_all_optional",
			modify: func(tc *TicketContent) {
				tc.Body = "Full description"
				tc.Labels = []string{"important"}
				tc.Affects = []string{"fleet/gpu/a100"}
				tc.Assignee = ref.MustParseUserID("@test:bureau.local")
				tc.Parent = "tkt-parent"
				tc.BlockedBy = []string{"tkt-dep"}
				tc.Gates = []TicketGate{{ID: "g1", Type: "human", Status: "pending"}}
				tc.Notes = []TicketNote{{ID: "n-1", Author: ref.MustParseUserID("@a:b.c"), CreatedAt: "2026-01-01T00:00:00Z", Body: "note"}}
				tc.Attachments = []TicketAttachment{{Ref: "art-abc123"}}
				tc.ContextID = "ctx-a1b2c3d4"
				tc.Deadline = "2026-03-01T00:00:00Z"
				tc.Origin = &TicketOrigin{Source: "github", ExternalRef: "GH-1234"}
			},
			wantErr: "",
		},
		{
			name:    "deadline_valid",
			modify:  func(tc *TicketContent) { tc.Deadline = "2026-12-31T23:59:59Z" },
			wantErr: "",
		},
		{
			name:    "deadline_invalid",
			modify:  func(tc *TicketContent) { tc.Deadline = "next friday" },
			wantErr: "deadline must be RFC 3339",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := validTicketContent()
			test.modify(&content)
			err := content.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestTicketContentCanModify(t *testing.T) {
	tests := []struct {
		name    string
		version int
		wantErr bool
	}{
		{"current_version", TicketContentVersion, false},
		{"older_version", 1, false},
		{"newer_version", TicketContentVersion + 1, true},
		{"far_future_version", TicketContentVersion + 100, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := validTicketContent()
			content.Version = test.version
			err := content.CanModify()
			if test.wantErr {
				if err == nil {
					t.Fatal("CanModify() = nil, want error")
				}
				if !strings.Contains(err.Error(), "upgrade") {
					t.Errorf("error should mention upgrade: %q", err)
				}
			} else {
				if err != nil {
					t.Errorf("CanModify() = %v, want nil", err)
				}
			}
		})
	}
}

func TestTicketGateRoundTrip(t *testing.T) {
	// Exercise each gate type to verify wire format.
	tests := []struct {
		name string
		gate TicketGate
		// Fields to check on the wire-format JSON map.
		checks map[string]any
	}{
		{
			name: "pipeline_gate",
			gate: TicketGate{
				ID:          "ci-pass",
				Type:        "pipeline",
				Status:      "pending",
				Description: "CI must pass",
				PipelineRef: "ci/build-test",
				Conclusion:  "success",
				CreatedAt:   "2026-02-12T10:00:00Z",
			},
			checks: map[string]any{
				"id":           "ci-pass",
				"type":         "pipeline",
				"status":       "pending",
				"description":  "CI must pass",
				"pipeline_ref": "ci/build-test",
				"conclusion":   "success",
				"created_at":   "2026-02-12T10:00:00Z",
			},
		},
		{
			name: "human_gate_satisfied",
			gate: TicketGate{
				ID:          "lead-approval",
				Type:        "human",
				Status:      "satisfied",
				Description: "Team lead approval",
				CreatedAt:   "2026-02-12T10:00:00Z",
				SatisfiedAt: "2026-02-12T11:00:00Z",
				SatisfiedBy: "@bureau/admin:bureau.local",
			},
			checks: map[string]any{
				"id":           "lead-approval",
				"type":         "human",
				"status":       "satisfied",
				"satisfied_at": "2026-02-12T11:00:00Z",
				"satisfied_by": "@bureau/admin:bureau.local",
			},
		},
		{
			name: "state_event_gate",
			gate: TicketGate{
				ID:        "workspace-ready",
				Type:      "state_event",
				Status:    "pending",
				EventType: "m.bureau.workspace",
				StateKey:  "",
				RoomAlias: ref.MustParseRoomAlias("#iree/amdgpu/inference:bureau.local"),
				ContentMatch: schema.ContentMatch{
					"status": schema.Eq("active"),
				},
			},
			checks: map[string]any{
				"id":         "workspace-ready",
				"type":       "state_event",
				"event_type": "m.bureau.workspace",
				"room_alias": "#iree/amdgpu/inference:bureau.local",
			},
		},
		{
			name: "ticket_gate",
			gate: TicketGate{
				ID:       "dep-closed",
				Type:     "ticket",
				Status:   "pending",
				TicketID: "tkt-a3f9",
			},
			checks: map[string]any{
				"id":        "dep-closed",
				"type":      "ticket",
				"ticket_id": "tkt-a3f9",
			},
		},
		{
			name: "timer_gate_duration",
			gate: TicketGate{
				ID:        "soak-period",
				Type:      "timer",
				Status:    "pending",
				Duration:  "24h",
				CreatedAt: "2026-02-12T10:00:00Z",
			},
			checks: map[string]any{
				"id":       "soak-period",
				"type":     "timer",
				"duration": "24h",
			},
		},
		{
			name: "timer_gate_recurring",
			gate: TicketGate{
				ID:             "daily-check",
				Type:           "timer",
				Status:         "pending",
				Target:         "2026-02-18T07:00:00Z",
				Schedule:       "0 7 * * *",
				LastFiredAt:    "2026-02-17T07:00:00Z",
				FireCount:      3,
				MaxOccurrences: 30,
				CreatedAt:      "2026-02-15T07:00:00Z",
			},
			checks: map[string]any{
				"id":              "daily-check",
				"type":            "timer",
				"target":          "2026-02-18T07:00:00Z",
				"schedule":        "0 7 * * *",
				"last_fired_at":   "2026-02-17T07:00:00Z",
				"fire_count":      float64(3),
				"max_occurrences": float64(30),
			},
		},
		{
			name: "timer_gate_interval_with_base",
			gate: TicketGate{
				ID:       "retry",
				Type:     "timer",
				Status:   "pending",
				Duration: "4h",
				Interval: "4h",
				Base:     "unblocked",
			},
			checks: map[string]any{
				"id":       "retry",
				"type":     "timer",
				"duration": "4h",
				"interval": "4h",
				"base":     "unblocked",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(test.gate)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatalf("Unmarshal to map: %v", err)
			}

			for key, want := range test.checks {
				assertField(t, raw, key, want)
			}

			// Round-trip.
			var decoded TicketGate
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(decoded, test.gate) {
				t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, test.gate)
			}
		})
	}
}

func TestTicketGateOmitsEmptyOptionals(t *testing.T) {
	// A minimal human gate has no type-specific or lifecycle fields.
	gate := TicketGate{
		ID:     "manual",
		Type:   "human",
		Status: "pending",
	}

	data, err := json.Marshal(gate)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	optionalFields := []string{
		"description", "pipeline_ref", "conclusion", "event_type",
		"state_key", "content_match", "ticket_id",
		"duration", "target", "base", "schedule", "interval",
		"last_fired_at", "fire_count", "max_occurrences",
		"created_at", "satisfied_at", "satisfied_by",
	}
	for _, field := range optionalFields {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty, but is present", field)
		}
	}
	// ref.RoomAlias implements TextMarshaler, so omitempty serializes
	// the zero value as "" rather than omitting the field entirely.
	if value, exists := raw["room_alias"]; exists && value != "" {
		t.Errorf("room_alias should be empty string for zero value, got %v", value)
	}
}

func TestTicketGateValidate(t *testing.T) {
	tests := []struct {
		name    string
		gate    TicketGate
		wantErr string
	}{
		{
			name:    "valid_human",
			gate:    TicketGate{ID: "g1", Type: "human", Status: "pending"},
			wantErr: "",
		},
		{
			name:    "valid_pipeline",
			gate:    TicketGate{ID: "g1", Type: "pipeline", Status: "pending", PipelineRef: "ci/test"},
			wantErr: "",
		},
		{
			name:    "valid_state_event",
			gate:    TicketGate{ID: "g1", Type: "state_event", Status: "satisfied", EventType: "m.bureau.workspace"},
			wantErr: "",
		},
		{
			name:    "valid_ticket",
			gate:    TicketGate{ID: "g1", Type: "ticket", Status: "pending", TicketID: "tkt-abc"},
			wantErr: "",
		},
		{
			name:    "valid_timer",
			gate:    TicketGate{ID: "g1", Type: "timer", Status: "pending", Duration: "24h"},
			wantErr: "",
		},
		{
			name:    "id_empty",
			gate:    TicketGate{ID: "", Type: "human", Status: "pending"},
			wantErr: "id is required",
		},
		{
			name:    "type_empty",
			gate:    TicketGate{ID: "g1", Type: "", Status: "pending"},
			wantErr: "type is required",
		},
		{
			name:    "type_invalid",
			gate:    TicketGate{ID: "g1", Type: "webhook", Status: "pending"},
			wantErr: `unknown type "webhook"`,
		},
		{
			name:    "status_empty",
			gate:    TicketGate{ID: "g1", Type: "human", Status: ""},
			wantErr: "status is required",
		},
		{
			name:    "status_invalid",
			gate:    TicketGate{ID: "g1", Type: "human", Status: "failed"},
			wantErr: `unknown status "failed"`,
		},
		{
			name:    "pipeline_missing_ref",
			gate:    TicketGate{ID: "g1", Type: "pipeline", Status: "pending"},
			wantErr: "pipeline_ref is required for pipeline gates",
		},
		{
			name:    "state_event_missing_event_type",
			gate:    TicketGate{ID: "g1", Type: "state_event", Status: "pending"},
			wantErr: "event_type is required for state_event gates",
		},
		{
			name:    "ticket_missing_ticket_id",
			gate:    TicketGate{ID: "g1", Type: "ticket", Status: "pending"},
			wantErr: "ticket_id is required for ticket gates",
		},
		{
			name:    "timer_missing_target_and_duration",
			gate:    TicketGate{ID: "g1", Type: "timer", Status: "pending"},
			wantErr: "target or duration is required for timer gates",
		},
		{
			name:    "timer_target_only",
			gate:    TicketGate{ID: "g1", Type: "timer", Status: "pending", Target: "2026-02-18T07:00:00Z"},
			wantErr: "",
		},
		{
			name:    "timer_duration_only",
			gate:    TicketGate{ID: "g1", Type: "timer", Status: "pending", Duration: "4h"},
			wantErr: "",
		},
		{
			name:    "timer_target_and_duration",
			gate:    TicketGate{ID: "g1", Type: "timer", Status: "pending", Target: "2026-02-18T07:00:00Z", Duration: "4h"},
			wantErr: "",
		},
		{
			name:    "timer_invalid_target",
			gate:    TicketGate{ID: "g1", Type: "timer", Status: "pending", Target: "not-a-time"},
			wantErr: "target must be RFC 3339",
		},
		{
			name:    "timer_invalid_duration",
			gate:    TicketGate{ID: "g1", Type: "timer", Status: "pending", Duration: "3 days"},
			wantErr: "invalid duration",
		},
		{
			name:    "timer_negative_duration",
			gate:    TicketGate{ID: "g1", Type: "timer", Status: "pending", Duration: "-1h"},
			wantErr: "duration must be positive",
		},
		{
			name: "timer_schedule_valid",
			gate: TicketGate{
				ID: "g1", Type: "timer", Status: "pending",
				Target: "2026-02-18T07:00:00Z", Schedule: "0 7 * * *",
			},
			wantErr: "",
		},
		{
			name: "timer_interval_valid",
			gate: TicketGate{
				ID: "g1", Type: "timer", Status: "pending",
				Duration: "4h", Interval: "4h",
			},
			wantErr: "",
		},
		{
			name: "timer_schedule_and_interval_exclusive",
			gate: TicketGate{
				ID: "g1", Type: "timer", Status: "pending",
				Duration: "1h", Schedule: "0 7 * * *", Interval: "4h",
			},
			wantErr: "schedule and interval are mutually exclusive",
		},
		{
			name: "timer_schedule_wrong_field_count",
			gate: TicketGate{
				ID: "g1", Type: "timer", Status: "pending",
				Target: "2026-02-18T07:00:00Z", Schedule: "0 7 * *",
			},
			wantErr: "expected 5 fields",
		},
		{
			name: "timer_interval_too_short",
			gate: TicketGate{
				ID: "g1", Type: "timer", Status: "pending",
				Duration: "5s", Interval: "5s",
			},
			wantErr: "interval must be >= 30s",
		},
		{
			name: "timer_interval_at_minimum",
			gate: TicketGate{
				ID: "g1", Type: "timer", Status: "pending",
				Duration: "30s", Interval: "30s",
			},
			wantErr: "",
		},
		{
			name: "timer_base_created",
			gate: TicketGate{
				ID: "g1", Type: "timer", Status: "pending",
				Duration: "1h", Base: "created",
			},
			wantErr: "",
		},
		{
			name: "timer_base_unblocked",
			gate: TicketGate{
				ID: "g1", Type: "timer", Status: "pending",
				Duration: "1h", Base: "unblocked",
			},
			wantErr: "",
		},
		{
			name: "timer_base_invalid",
			gate: TicketGate{
				ID: "g1", Type: "timer", Status: "pending",
				Duration: "1h", Base: "started",
			},
			wantErr: `unknown base "started"`,
		},
		{
			name: "timer_max_occurrences_without_recurrence",
			gate: TicketGate{
				ID: "g1", Type: "timer", Status: "pending",
				Duration: "1h", MaxOccurrences: 5,
			},
			wantErr: "max_occurrences requires schedule or interval",
		},
		{
			name: "timer_max_occurrences_with_schedule",
			gate: TicketGate{
				ID: "g1", Type: "timer", Status: "pending",
				Target: "2026-02-18T07:00:00Z", Schedule: "0 7 * * *", MaxOccurrences: 10,
			},
			wantErr: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.gate.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestTicketNoteRoundTrip(t *testing.T) {
	original := TicketNote{
		ID:        "n-1",
		Author:    ref.MustParseUserID("@bureau/admin:bureau.local"),
		CreatedAt: "2026-02-12T10:30:00Z",
		Body:      "Security scanner found CVE-2026-1234 in this dependency.",
		ContextID: "ctx-e5f6a7b8",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "id", "n-1")
	assertField(t, raw, "author", "@bureau/admin:bureau.local")
	assertField(t, raw, "created_at", "2026-02-12T10:30:00Z")
	assertField(t, raw, "body", original.Body)
	assertField(t, raw, "context_id", "ctx-e5f6a7b8")

	var decoded TicketNote
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestTicketNoteOmitsContextIDWhenEmpty(t *testing.T) {
	note := TicketNote{
		ID:        "n-1",
		Author:    ref.MustParseUserID("@a:b.c"),
		CreatedAt: "2026-02-12T10:00:00Z",
		Body:      "note without context",
	}

	data, err := json.Marshal(note)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	if _, exists := raw["context_id"]; exists {
		t.Error("context_id should be omitted when empty")
	}
}

func TestTicketNoteValidate(t *testing.T) {
	tests := []struct {
		name    string
		note    TicketNote
		wantErr string
	}{
		{
			name:    "valid",
			note:    TicketNote{ID: "n-1", Author: ref.MustParseUserID("@a:b.c"), CreatedAt: "2026-01-01T00:00:00Z", Body: "note"},
			wantErr: "",
		},
		{
			name:    "id_empty",
			note:    TicketNote{ID: "", Author: ref.MustParseUserID("@a:b.c"), CreatedAt: "2026-01-01T00:00:00Z", Body: "note"},
			wantErr: "id is required",
		},
		{
			name:    "author_empty",
			note:    TicketNote{ID: "n-1", Author: ref.UserID{}, CreatedAt: "2026-01-01T00:00:00Z", Body: "note"},
			wantErr: "author is required",
		},
		{
			name:    "created_at_empty",
			note:    TicketNote{ID: "n-1", Author: ref.MustParseUserID("@a:b.c"), CreatedAt: "", Body: "note"},
			wantErr: "created_at is required",
		},
		{
			name:    "body_empty",
			note:    TicketNote{ID: "n-1", Author: ref.MustParseUserID("@a:b.c"), CreatedAt: "2026-01-01T00:00:00Z", Body: ""},
			wantErr: "body is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.note.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestTicketAttachmentRoundTrip(t *testing.T) {
	original := TicketAttachment{
		Ref:         "art-a3f9b2c1e7d4",
		Label:       "crash stack trace",
		ContentType: "text/plain",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "ref", "art-a3f9b2c1e7d4")
	assertField(t, raw, "label", "crash stack trace")
	assertField(t, raw, "content_type", "text/plain")

	var decoded TicketAttachment
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestTicketAttachmentOmitsEmptyOptionals(t *testing.T) {
	attachment := TicketAttachment{Ref: "art-abc123"}

	data, err := json.Marshal(attachment)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"label", "content_type"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty, but is present", field)
		}
	}
}

func TestTicketAttachmentValidate(t *testing.T) {
	tests := []struct {
		name    string
		attach  TicketAttachment
		wantErr string
	}{
		{
			name:    "valid_artifact",
			attach:  TicketAttachment{Ref: "art-a3f9b2c1e7d4"},
			wantErr: "",
		},
		{
			name:    "rejected_mxc",
			attach:  TicketAttachment{Ref: "mxc://bureau.local/abc123"},
			wantErr: "mxc:// refs are not supported",
		},
		{
			name:    "ref_empty",
			attach:  TicketAttachment{Ref: ""},
			wantErr: "ref is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.attach.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestTicketOriginRoundTrip(t *testing.T) {
	original := TicketOrigin{
		Source:      "github",
		ExternalRef: "GH-4201",
		SourceRoom:  "!old_room:bureau.local",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "source", "github")
	assertField(t, raw, "external_ref", "GH-4201")
	assertField(t, raw, "source_room", "!old_room:bureau.local")

	var decoded TicketOrigin
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestTicketOriginOmitsEmptySourceRoom(t *testing.T) {
	origin := TicketOrigin{Source: "github", ExternalRef: "org/repo#42"}

	data, err := json.Marshal(origin)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	if _, exists := raw["source_room"]; exists {
		t.Error("source_room should be omitted when empty")
	}
}

func TestTicketOriginValidate(t *testing.T) {
	tests := []struct {
		name    string
		origin  TicketOrigin
		wantErr string
	}{
		{
			name:    "valid",
			origin:  TicketOrigin{Source: "github", ExternalRef: "GH-4201"},
			wantErr: "",
		},
		{
			name:    "source_empty",
			origin:  TicketOrigin{Source: "", ExternalRef: "GH-4201"},
			wantErr: "source is required",
		},
		{
			name:    "external_ref_empty",
			origin:  TicketOrigin{Source: "github", ExternalRef: ""},
			wantErr: "external_ref is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.origin.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestPipelineExecutionContentRoundTrip(t *testing.T) {
	original := PipelineExecutionContent{
		PipelineRef:     "dev-workspace-init",
		Variables:       map[string]string{"REPOSITORY": "https://github.com/example/repo.git", "BRANCH": "main"},
		CurrentStep:     2,
		TotalSteps:      5,
		CurrentStepName: "install-deps",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "pipeline_ref", "dev-workspace-init")
	assertField(t, raw, "current_step", float64(2))
	assertField(t, raw, "total_steps", float64(5))
	assertField(t, raw, "current_step_name", "install-deps")

	variables, ok := raw["variables"].(map[string]any)
	if !ok {
		t.Fatalf("variables is not an object: %T", raw["variables"])
	}
	assertField(t, variables, "REPOSITORY", "https://github.com/example/repo.git")
	assertField(t, variables, "BRANCH", "main")

	var decoded PipelineExecutionContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.PipelineRef != original.PipelineRef {
		t.Errorf("PipelineRef = %q, want %q", decoded.PipelineRef, original.PipelineRef)
	}
	if decoded.CurrentStep != original.CurrentStep {
		t.Errorf("CurrentStep = %d, want %d", decoded.CurrentStep, original.CurrentStep)
	}
	if decoded.TotalSteps != original.TotalSteps {
		t.Errorf("TotalSteps = %d, want %d", decoded.TotalSteps, original.TotalSteps)
	}
	if decoded.CurrentStepName != original.CurrentStepName {
		t.Errorf("CurrentStepName = %q, want %q", decoded.CurrentStepName, original.CurrentStepName)
	}
	if len(decoded.Variables) != 2 {
		t.Fatalf("Variables count = %d, want 2", len(decoded.Variables))
	}
}

func TestPipelineExecutionContentOmitsEmptyOptionals(t *testing.T) {
	content := PipelineExecutionContent{
		PipelineRef: "simple-pipeline",
		TotalSteps:  1,
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"variables", "current_step", "current_step_name", "conclusion"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty/zero, but is present", field)
		}
	}

	// Required fields must be present.
	for _, field := range []string{"pipeline_ref", "total_steps"} {
		if _, exists := raw[field]; !exists {
			t.Errorf("required field %s is missing from JSON", field)
		}
	}
}

func TestPipelineExecutionContentValidate(t *testing.T) {
	tests := []struct {
		name    string
		content PipelineExecutionContent
		wantErr string
	}{
		{
			name:    "valid_minimal",
			content: PipelineExecutionContent{PipelineRef: "ci-pipeline", TotalSteps: 3},
			wantErr: "",
		},
		{
			name: "valid_in_progress",
			content: PipelineExecutionContent{
				PipelineRef:     "ci-pipeline",
				TotalSteps:      5,
				CurrentStep:     3,
				CurrentStepName: "run-tests",
			},
			wantErr: "",
		},
		{
			name: "valid_completed",
			content: PipelineExecutionContent{
				PipelineRef: "ci-pipeline",
				TotalSteps:  3,
				CurrentStep: 3,
				Conclusion:  "success",
			},
			wantErr: "",
		},
		{
			name:    "pipeline_ref_empty",
			content: PipelineExecutionContent{PipelineRef: "", TotalSteps: 3},
			wantErr: "pipeline_ref is required",
		},
		{
			name:    "total_steps_zero",
			content: PipelineExecutionContent{PipelineRef: "ci-pipeline", TotalSteps: 0},
			wantErr: "",
		},
		{
			name:    "total_steps_negative",
			content: PipelineExecutionContent{PipelineRef: "ci-pipeline", TotalSteps: -1},
			wantErr: "total_steps must be >= 0",
		},
		{
			name:    "current_step_negative",
			content: PipelineExecutionContent{PipelineRef: "ci-pipeline", TotalSteps: 3, CurrentStep: -1},
			wantErr: "current_step must be >= 0",
		},
		{
			name:    "current_step_exceeds_total",
			content: PipelineExecutionContent{PipelineRef: "ci-pipeline", TotalSteps: 3, CurrentStep: 4},
			wantErr: "current_step (4) exceeds total_steps (3)",
		},
		{
			name: "conclusion_success",
			content: PipelineExecutionContent{
				PipelineRef: "ci-pipeline", TotalSteps: 3, Conclusion: "success",
			},
			wantErr: "",
		},
		{
			name: "conclusion_failure",
			content: PipelineExecutionContent{
				PipelineRef: "ci-pipeline", TotalSteps: 3, Conclusion: "failure",
			},
			wantErr: "",
		},
		{
			name: "conclusion_aborted",
			content: PipelineExecutionContent{
				PipelineRef: "ci-pipeline", TotalSteps: 3, Conclusion: "aborted",
			},
			wantErr: "",
		},
		{
			name: "conclusion_cancelled",
			content: PipelineExecutionContent{
				PipelineRef: "ci-pipeline", TotalSteps: 3, Conclusion: "cancelled",
			},
			wantErr: "",
		},
		{
			name: "conclusion_invalid",
			content: PipelineExecutionContent{
				PipelineRef: "ci-pipeline", TotalSteps: 3, Conclusion: "timeout",
			},
			wantErr: `unknown conclusion "timeout"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.content.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestPipelineTicketRoundTrip(t *testing.T) {
	// A complete pipeline ticket with type-specific content.
	original := validTicketContent()
	original.Type = TypePipeline
	original.Pipeline = &PipelineExecutionContent{
		PipelineRef:     "dev-workspace-init",
		Variables:       map[string]string{"REPOSITORY": "https://github.com/example/repo.git"},
		CurrentStep:     2,
		TotalSteps:      5,
		CurrentStepName: "install-deps",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "type", "pipeline")
	pipeline, ok := raw["pipeline"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline is not an object: %T", raw["pipeline"])
	}
	assertField(t, pipeline, "pipeline_ref", "dev-workspace-init")
	assertField(t, pipeline, "total_steps", float64(5))

	var decoded TicketContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Pipeline == nil {
		t.Fatal("Pipeline should not be nil after round-trip")
	}
	if decoded.Pipeline.PipelineRef != "dev-workspace-init" {
		t.Errorf("Pipeline.PipelineRef = %q, want %q",
			decoded.Pipeline.PipelineRef, "dev-workspace-init")
	}
	if decoded.Pipeline.CurrentStep != 2 {
		t.Errorf("Pipeline.CurrentStep = %d, want 2", decoded.Pipeline.CurrentStep)
	}

	if err := decoded.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestPrefixForType(t *testing.T) {
	tests := []struct {
		ticketType TicketType
		want       string
	}{
		{TypePipeline, "pip"},
		{TypeTask, ""},
		{TypeBug, ""},
		{TypeFeature, ""},
		{TypeEpic, ""},
		{TypeChore, ""},
		{TypeDocs, ""},
		{TypeQuestion, ""},
		{TicketType("unknown"), ""},
	}
	for _, test := range tests {
		t.Run(string(test.ticketType), func(t *testing.T) {
			if got := PrefixForType(test.ticketType); got != test.want {
				t.Errorf("PrefixForType(%q) = %q, want %q", test.ticketType, got, test.want)
			}
		})
	}
}

func TestTicketTypeIsKnown(t *testing.T) {
	knownTypes := []TicketType{
		TypeTask, TypeBug, TypeFeature, TypeEpic, TypeChore,
		TypeDocs, TypeQuestion, TypePipeline, TypeReviewFinding,
		TypeReview, TypeResourceRequest, TypeAccessRequest,
		TypeDeployment, TypeCredentialRotation,
	}
	for _, typeName := range knownTypes {
		if !typeName.IsKnown() {
			t.Errorf("TicketType(%q).IsKnown() = false, want true", typeName)
		}
	}

	unknownTypes := []TicketType{"", "improvement", "story", "incident"}
	for _, typeName := range unknownTypes {
		if typeName.IsKnown() {
			t.Errorf("TicketType(%q).IsKnown() = true, want false", typeName)
		}
	}
}

func TestTicketConfigContentRoundTrip(t *testing.T) {
	original := TicketConfigContent{
		Version:       1,
		Prefix:        "iree",
		DefaultLabels: []string{"amdgpu", "inference"},
		Extra: map[string]json.RawMessage{
			"auto_triage": json.RawMessage(`true`),
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "version", float64(1))
	assertField(t, raw, "prefix", "iree")

	labels, ok := raw["default_labels"].([]any)
	if !ok {
		t.Fatalf("default_labels is not an array: %T", raw["default_labels"])
	}
	if len(labels) != 2 || labels[0] != "amdgpu" || labels[1] != "inference" {
		t.Errorf("default_labels = %v, want [amdgpu inference]", labels)
	}

	var decoded TicketConfigContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

func TestTicketConfigContentAllowedTypesRoundTrip(t *testing.T) {
	original := TicketConfigContent{
		Version:      1,
		Prefix:       "pip",
		AllowedTypes: []TicketType{TypePipeline},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	allowedTypes, ok := raw["allowed_types"].([]any)
	if !ok {
		t.Fatalf("allowed_types is not an array: %T", raw["allowed_types"])
	}
	if len(allowedTypes) != 1 || allowedTypes[0] != "pipeline" {
		t.Errorf("allowed_types = %v, want [pipeline]", allowedTypes)
	}

	var decoded TicketConfigContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(decoded.AllowedTypes) != 1 || decoded.AllowedTypes[0] != TypePipeline {
		t.Errorf("AllowedTypes = %v, want [pipeline]", decoded.AllowedTypes)
	}
}

func TestTicketConfigContentOmitsEmptyOptionals(t *testing.T) {
	config := TicketConfigContent{Version: 1}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"prefix", "allowed_types", "default_labels", "extra"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty, but is present", field)
		}
	}
}

func TestTicketConfigContentValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  TicketConfigContent
		wantErr string
	}{
		{
			name:    "valid_minimal",
			config:  TicketConfigContent{Version: 1},
			wantErr: "",
		},
		{
			name:    "valid_with_prefix",
			config:  TicketConfigContent{Version: 1, Prefix: "iree"},
			wantErr: "",
		},
		{
			name:    "valid_with_labels",
			config:  TicketConfigContent{Version: 1, DefaultLabels: []string{"amdgpu"}},
			wantErr: "",
		},
		{
			name:    "valid_with_allowed_types",
			config:  TicketConfigContent{Version: 1, AllowedTypes: []TicketType{TypePipeline}},
			wantErr: "",
		},
		{
			name:    "valid_with_multiple_allowed_types",
			config:  TicketConfigContent{Version: 1, AllowedTypes: []TicketType{TypeTask, TypeBug, TypePipeline}},
			wantErr: "",
		},
		{
			name:    "invalid_allowed_type",
			config:  TicketConfigContent{Version: 1, AllowedTypes: []TicketType{TypeTask, TicketType("story")}},
			wantErr: `allowed_types[1]: unknown type "story"`,
		},
		{
			name:    "version_zero",
			config:  TicketConfigContent{Version: 0},
			wantErr: "version must be >= 1",
		},
		{
			name:    "version_negative",
			config:  TicketConfigContent{Version: -1},
			wantErr: "version must be >= 1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestTicketConfigContentCanModify(t *testing.T) {
	tests := []struct {
		name    string
		version int
		wantErr bool
	}{
		{"current_version", TicketConfigVersion, false},
		{"older_version", 1, false},
		{"newer_version", TicketConfigVersion + 1, true},
		{"far_future_version", TicketConfigVersion + 100, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := TicketConfigContent{Version: test.version}
			err := config.CanModify()
			if test.wantErr {
				if err == nil {
					t.Fatal("CanModify() = nil, want error")
				}
				if !strings.Contains(err.Error(), "upgrade") {
					t.Errorf("error should mention upgrade: %q", err)
				}
			} else {
				if err != nil {
					t.Errorf("CanModify() = %v, want nil", err)
				}
			}
		})
	}
}

func TestTicketConfigContentForwardCompatibility(t *testing.T) {
	v2JSON := `{
		"version": 2,
		"prefix": "tkt",
		"new_v2_field": "unknown to v1"
	}`

	var config TicketConfigContent
	if err := json.Unmarshal([]byte(v2JSON), &config); err != nil {
		t.Fatalf("Unmarshal v2 event: %v", err)
	}

	if config.Version != 2 {
		t.Errorf("Version = %d, want 2", config.Version)
	}
	if config.Prefix != "tkt" {
		t.Errorf("Prefix = %q, want %q", config.Prefix, "tkt")
	}

	if err := config.CanModify(); err == nil {
		t.Error("CanModify() should reject v2 events from v1 code")
	}

	remarshaled, err2 := json.Marshal(config)
	if err2 != nil {
		t.Fatalf("re-Marshal: %v", err2)
	}
	if strings.Contains(string(remarshaled), "new_v2_field") {
		t.Error("unknown field survived re-marshal; expected it to be dropped")
	}
}

func TestTicketGateIsRecurring(t *testing.T) {
	tests := []struct {
		name string
		gate TicketGate
		want bool
	}{
		{
			name: "schedule",
			gate: TicketGate{Type: "timer", Schedule: "0 7 * * *"},
			want: true,
		},
		{
			name: "interval",
			gate: TicketGate{Type: "timer", Interval: "4h"},
			want: true,
		},
		{
			name: "one_shot_duration",
			gate: TicketGate{Type: "timer", Duration: "1h"},
			want: false,
		},
		{
			name: "one_shot_target",
			gate: TicketGate{Type: "timer", Target: "2026-02-18T07:00:00Z"},
			want: false,
		},
		{
			name: "non_timer_type",
			gate: TicketGate{Type: "human"},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.gate.IsRecurring(); got != test.want {
				t.Errorf("IsRecurring() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestTicketContentDeadlineRoundTrip(t *testing.T) {
	content := validTicketContent()
	content.Deadline = "2026-03-15T17:00:00Z"

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "deadline", "2026-03-15T17:00:00Z")

	var decoded TicketContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Deadline != content.Deadline {
		t.Errorf("Deadline = %q, want %q", decoded.Deadline, content.Deadline)
	}
}

func TestTicketContentDeadlineOmittedWhenEmpty(t *testing.T) {
	content := validTicketContent()
	// Deadline is empty by default.

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["deadline"]; exists {
		t.Error("deadline should be omitted when empty")
	}
}

func TestValidateCronExpression(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		wantErr    string
	}{
		{name: "standard_5_field", expression: "0 7 * * *", wantErr: ""},
		{name: "all_stars", expression: "* * * * *", wantErr: ""},
		{name: "ranges_and_steps", expression: "*/15 0-6 1,15 * 1-5", wantErr: ""},
		{name: "4_fields", expression: "0 7 * *", wantErr: "expected 5 fields"},
		{name: "6_fields", expression: "0 0 7 * * *", wantErr: "expected 5 fields"},
		{name: "empty", expression: "", wantErr: "expected 5 fields"},
		{name: "whitespace_only", expression: "   ", wantErr: "expected 5 fields"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCronExpression(test.expression)
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("validateCronExpression(%q) = %v, want nil", test.expression, err)
				}
			} else {
				if err == nil {
					t.Fatalf("validateCronExpression(%q) = nil, want error containing %q", test.expression, test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("validateCronExpression(%q) = %q, want error containing %q", test.expression, err, test.wantErr)
				}
			}
		})
	}
}

func TestTicketReviewRoundTrip(t *testing.T) {
	original := TicketReview{
		Reviewers: []ReviewerEntry{
			{
				UserID:      ref.MustParseUserID("@iree/amdgpu/engineer:bureau.local"),
				Disposition: "approved",
				UpdatedAt:   "2026-02-20T14:30:00Z",
			},
			{
				UserID:      ref.MustParseUserID("@iree/amdgpu/pm:bureau.local"),
				Disposition: "pending",
			},
		},
		Scope: &ReviewScope{
			Base:     "abc123",
			Head:     "def456",
			Worktree: "feature/auth-refactor",
			Files:    []string{"lib/auth/token.go", "lib/auth/token_test.go"},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Verify reviewers wire format.
	reviewers, ok := raw["reviewers"].([]any)
	if !ok {
		t.Fatalf("reviewers is not an array: %T", raw["reviewers"])
	}
	if len(reviewers) != 2 {
		t.Fatalf("reviewers count = %d, want 2", len(reviewers))
	}
	firstReviewer := reviewers[0].(map[string]any)
	assertField(t, firstReviewer, "user_id", "@iree/amdgpu/engineer:bureau.local")
	assertField(t, firstReviewer, "disposition", "approved")
	assertField(t, firstReviewer, "updated_at", "2026-02-20T14:30:00Z")

	secondReviewer := reviewers[1].(map[string]any)
	assertField(t, secondReviewer, "user_id", "@iree/amdgpu/pm:bureau.local")
	assertField(t, secondReviewer, "disposition", "pending")
	if _, exists := secondReviewer["updated_at"]; exists {
		t.Error("updated_at should be omitted for pending reviewer")
	}

	// Verify scope wire format.
	scope, ok := raw["scope"].(map[string]any)
	if !ok {
		t.Fatalf("scope is not an object: %T", raw["scope"])
	}
	assertField(t, scope, "base", "abc123")
	assertField(t, scope, "head", "def456")
	assertField(t, scope, "worktree", "feature/auth-refactor")
	files, ok := scope["files"].([]any)
	if !ok {
		t.Fatalf("scope.files is not an array: %T", scope["files"])
	}
	if len(files) != 2 || files[0] != "lib/auth/token.go" {
		t.Errorf("scope.files = %v, want [lib/auth/token.go lib/auth/token_test.go]", files)
	}

	// Round-trip.
	var decoded TicketReview
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

func TestTicketReviewOmitsEmptyScope(t *testing.T) {
	review := TicketReview{
		Reviewers: []ReviewerEntry{
			{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "pending"},
		},
	}

	data, err := json.Marshal(review)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	if _, exists := raw["scope"]; exists {
		t.Error("scope should be omitted when nil")
	}
}

func TestTicketReviewValidate(t *testing.T) {
	tests := []struct {
		name    string
		review  TicketReview
		wantErr string
	}{
		{
			name: "valid_single_reviewer",
			review: TicketReview{
				Reviewers: []ReviewerEntry{
					{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "pending"},
				},
			},
			wantErr: "",
		},
		{
			name: "valid_multiple_reviewers",
			review: TicketReview{
				Reviewers: []ReviewerEntry{
					{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "approved"},
					{UserID: ref.MustParseUserID("@b:b.c"), Disposition: "changes_requested"},
					{UserID: ref.MustParseUserID("@c:b.c"), Disposition: "commented"},
				},
			},
			wantErr: "",
		},
		{
			name: "valid_with_scope",
			review: TicketReview{
				Reviewers: []ReviewerEntry{
					{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "pending"},
				},
				Scope: &ReviewScope{
					Base: "abc123",
					Head: "def456",
				},
			},
			wantErr: "",
		},
		{
			name:    "empty_reviewers",
			review:  TicketReview{Reviewers: []ReviewerEntry{}},
			wantErr: "at least one reviewer is required",
		},
		{
			name:    "nil_reviewers",
			review:  TicketReview{},
			wantErr: "at least one reviewer is required",
		},
		{
			name: "invalid_reviewer_user_id",
			review: TicketReview{
				Reviewers: []ReviewerEntry{
					{UserID: ref.UserID{}, Disposition: "pending"},
				},
			},
			wantErr: "reviewers[0]: user_id is required",
		},
		{
			name: "invalid_reviewer_disposition",
			review: TicketReview{
				Reviewers: []ReviewerEntry{
					{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "rejected"},
				},
			},
			wantErr: `reviewers[0]: unknown disposition "rejected"`,
		},
		{
			name: "valid_with_tier_thresholds",
			review: TicketReview{
				Reviewers: []ReviewerEntry{
					{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "pending", Tier: 0},
					{UserID: ref.MustParseUserID("@b:b.c"), Disposition: "pending", Tier: 1},
				},
				TierThresholds: []TierThreshold{
					{Tier: 0},
					{Tier: 1, Threshold: func() *int { v := 1; return &v }()},
				},
			},
			wantErr: "",
		},
		{
			name: "invalid_tier_threshold",
			review: TicketReview{
				Reviewers: []ReviewerEntry{
					{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "pending"},
				},
				TierThresholds: []TierThreshold{
					{Tier: 0, Threshold: func() *int { v := 0; return &v }()},
				},
			},
			wantErr: "tier_thresholds[0]: threshold must be >= 1",
		},
		{
			name: "invalid_reviewer_tier",
			review: TicketReview{
				Reviewers: []ReviewerEntry{
					{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "pending", Tier: -1},
				},
			},
			wantErr: "reviewers[0]: tier must be >= 0",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.review.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestReviewerEntryValidate(t *testing.T) {
	tests := []struct {
		name    string
		entry   ReviewerEntry
		wantErr string
	}{
		{
			name:    "valid_pending",
			entry:   ReviewerEntry{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "pending"},
			wantErr: "",
		},
		{
			name:    "valid_approved",
			entry:   ReviewerEntry{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "approved"},
			wantErr: "",
		},
		{
			name:    "valid_changes_requested",
			entry:   ReviewerEntry{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "changes_requested"},
			wantErr: "",
		},
		{
			name:    "valid_commented",
			entry:   ReviewerEntry{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "commented"},
			wantErr: "",
		},
		{
			name:    "missing_user_id",
			entry:   ReviewerEntry{UserID: ref.UserID{}, Disposition: "pending"},
			wantErr: "user_id is required",
		},
		{
			name:    "empty_disposition",
			entry:   ReviewerEntry{UserID: ref.MustParseUserID("@a:b.c"), Disposition: ""},
			wantErr: `unknown disposition ""`,
		},
		{
			name:    "invalid_disposition",
			entry:   ReviewerEntry{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "lgtm"},
			wantErr: `unknown disposition "lgtm"`,
		},
		{
			name:    "valid_tier_zero",
			entry:   ReviewerEntry{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "pending", Tier: 0},
			wantErr: "",
		},
		{
			name:    "valid_tier_two",
			entry:   ReviewerEntry{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "pending", Tier: 2},
			wantErr: "",
		},
		{
			name:    "negative_tier",
			entry:   ReviewerEntry{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "pending", Tier: -1},
			wantErr: "tier must be >= 0",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.entry.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestIsValidDisposition(t *testing.T) {
	valid := []string{"pending", "approved", "changes_requested", "commented"}
	for _, disposition := range valid {
		if !IsValidDisposition(disposition) {
			t.Errorf("IsValidDisposition(%q) = false, want true", disposition)
		}
	}

	invalid := []string{"", "rejected", "lgtm", "wontfix"}
	for _, disposition := range invalid {
		if IsValidDisposition(disposition) {
			t.Errorf("IsValidDisposition(%q) = true, want false", disposition)
		}
	}
}

func TestReviewScopeRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		scope  ReviewScope
		checks map[string]any
	}{
		{
			name: "code_review_scope",
			scope: ReviewScope{
				Base:     "abc123",
				Head:     "def456",
				Worktree: "feature/auth-refactor",
				Files:    []string{"lib/auth/token.go"},
			},
			checks: map[string]any{
				"base":     "abc123",
				"head":     "def456",
				"worktree": "feature/auth-refactor",
			},
		},
		{
			name: "artifact_review_scope",
			scope: ReviewScope{
				ArtifactRef: "art-a3f9b2c1e7d4",
			},
			checks: map[string]any{
				"artifact_ref": "art-a3f9b2c1e7d4",
			},
		},
		{
			name: "freeform_review_scope",
			scope: ReviewScope{
				Description: "Please review the deployment plan for production rollout.",
			},
			checks: map[string]any{
				"description": "Please review the deployment plan for production rollout.",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(test.scope)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatalf("Unmarshal to map: %v", err)
			}
			for key, want := range test.checks {
				assertField(t, raw, key, want)
			}

			var decoded ReviewScope
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(decoded, test.scope) {
				t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, test.scope)
			}
		})
	}
}

func TestReviewScopeOmitsEmptyOptionals(t *testing.T) {
	// An empty scope should omit all fields.
	scope := ReviewScope{}

	data, err := json.Marshal(scope)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	optionalFields := []string{"base", "head", "worktree", "files", "artifact_ref", "description"}
	for _, field := range optionalFields {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty, but is present", field)
		}
	}
}

func TestReviewTicketRoundTrip(t *testing.T) {
	// A complete ticket in review status with review content.
	original := validTicketContent()
	original.Status = "review"
	original.Assignee = ref.MustParseUserID("@iree/amdgpu/engineer:bureau.local")
	original.Review = &TicketReview{
		Reviewers: []ReviewerEntry{
			{
				UserID:      ref.MustParseUserID("@iree/amdgpu/pm:bureau.local"),
				Disposition: "approved",
				UpdatedAt:   "2026-02-20T15:00:00Z",
			},
			{
				UserID:      ref.MustParseUserID("@iree/amdgpu/lead:bureau.local"),
				Disposition: "changes_requested",
				UpdatedAt:   "2026-02-20T15:30:00Z",
			},
		},
		Scope: &ReviewScope{
			Base:     "main",
			Head:     "feature/auth",
			Worktree: "feature/auth-refactor",
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "status", "review")
	review, ok := raw["review"].(map[string]any)
	if !ok {
		t.Fatalf("review is not an object: %T", raw["review"])
	}
	reviewers, ok := review["reviewers"].([]any)
	if !ok {
		t.Fatalf("review.reviewers is not an array: %T", review["reviewers"])
	}
	if len(reviewers) != 2 {
		t.Fatalf("reviewers count = %d, want 2", len(reviewers))
	}

	var decoded TicketContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Review == nil {
		t.Fatal("Review should not be nil after round-trip")
	}
	if len(decoded.Review.Reviewers) != 2 {
		t.Errorf("Review.Reviewers count = %d, want 2", len(decoded.Review.Reviewers))
	}
	if decoded.Review.Scope == nil {
		t.Fatal("Review.Scope should not be nil after round-trip")
	}
	if decoded.Review.Scope.Worktree != "feature/auth-refactor" {
		t.Errorf("Review.Scope.Worktree = %q, want %q",
			decoded.Review.Scope.Worktree, "feature/auth-refactor")
	}

	if err := decoded.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestTierThresholdValidate(t *testing.T) {
	intPointer := func(value int) *int { return &value }

	tests := []struct {
		name    string
		entry   TierThreshold
		wantErr string
	}{
		{
			name:    "valid_tier_zero_all_approve",
			entry:   TierThreshold{Tier: 0},
			wantErr: "",
		},
		{
			name:    "valid_tier_one_threshold_two",
			entry:   TierThreshold{Tier: 1, Threshold: intPointer(2)},
			wantErr: "",
		},
		{
			name:    "valid_threshold_one",
			entry:   TierThreshold{Tier: 0, Threshold: intPointer(1)},
			wantErr: "",
		},
		{
			name:    "negative_tier",
			entry:   TierThreshold{Tier: -1},
			wantErr: "tier must be >= 0",
		},
		{
			name:    "threshold_zero",
			entry:   TierThreshold{Tier: 0, Threshold: intPointer(0)},
			wantErr: "threshold must be >= 1 when set",
		},
		{
			name:    "threshold_negative",
			entry:   TierThreshold{Tier: 0, Threshold: intPointer(-1)},
			wantErr: "threshold must be >= 1 when set",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.entry.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestTierThresholdRoundTrip(t *testing.T) {
	intPointer := func(value int) *int { return &value }

	// TierThreshold with nil Threshold — threshold absent from JSON.
	nilThreshold := TierThreshold{Tier: 0}
	data, err := json.Marshal(nilThreshold)
	if err != nil {
		t.Fatalf("Marshal nil threshold: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}
	assertField(t, raw, "tier", float64(0))
	if _, exists := raw["threshold"]; exists {
		t.Error("nil Threshold should be absent from JSON")
	}

	// TierThreshold with non-nil Threshold — round-trips.
	withThreshold := TierThreshold{Tier: 1, Threshold: intPointer(2)}
	data, err = json.Marshal(withThreshold)
	if err != nil {
		t.Fatalf("Marshal with threshold: %v", err)
	}
	var decoded TierThreshold
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Tier != 1 {
		t.Errorf("Tier = %d, want 1", decoded.Tier)
	}
	if decoded.Threshold == nil || *decoded.Threshold != 2 {
		t.Errorf("Threshold round-trip: got %v, want 2", decoded.Threshold)
	}
}

func TestReviewerEntryTierRoundTrip(t *testing.T) {
	// Tier 0 should be omitted from JSON (omitempty with zero value).
	tierZero := ReviewerEntry{
		UserID:      ref.MustParseUserID("@reviewer:bureau.local"),
		Disposition: "pending",
		Tier:        0,
	}
	data, err := json.Marshal(tierZero)
	if err != nil {
		t.Fatalf("Marshal tier 0: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}
	if _, exists := raw["tier"]; exists {
		t.Error("Tier 0 should be absent from JSON (omitempty)")
	}

	// Tier 2 should be present.
	tierTwo := ReviewerEntry{
		UserID:      ref.MustParseUserID("@reviewer:bureau.local"),
		Disposition: "pending",
		Tier:        2,
	}
	data, err = json.Marshal(tierTwo)
	if err != nil {
		t.Fatalf("Marshal tier 2: %v", err)
	}
	raw = nil
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}
	assertField(t, raw, "tier", float64(2))

	// Round-trip: Tier 2 survives marshal/unmarshal.
	var decoded ReviewerEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Tier != 2 {
		t.Errorf("Tier round-trip: got %d, want 2", decoded.Tier)
	}
}

func TestTicketReviewWithTierThresholdsRoundTrip(t *testing.T) {
	intPointer := func(value int) *int { return &value }

	original := TicketReview{
		Reviewers: []ReviewerEntry{
			{
				UserID:      ref.MustParseUserID("@iree/amdgpu/engineer:bureau.local"),
				Disposition: "approved",
				UpdatedAt:   "2026-02-20T14:30:00Z",
				Tier:        0,
			},
			{
				UserID:      ref.MustParseUserID("@ben:bureau.local"),
				Disposition: "pending",
				Tier:        1,
			},
		},
		TierThresholds: []TierThreshold{
			{Tier: 0},
			{Tier: 1, Threshold: intPointer(1)},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded TicketReview
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if !reflect.DeepEqual(original, decoded) {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}

	// Verify tier_thresholds appears in JSON.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	thresholds, ok := raw["tier_thresholds"].([]any)
	if !ok {
		t.Fatalf("tier_thresholds is not an array: %T", raw["tier_thresholds"])
	}
	if len(thresholds) != 2 {
		t.Fatalf("tier_thresholds count = %d, want 2", len(thresholds))
	}

	// TierThresholds absent — should be omitted.
	noThresholds := TicketReview{
		Reviewers: []ReviewerEntry{
			{UserID: ref.MustParseUserID("@a:b.c"), Disposition: "pending"},
		},
	}
	data, err = json.Marshal(noThresholds)
	if err != nil {
		t.Fatalf("Marshal no thresholds: %v", err)
	}
	raw = nil
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}
	if _, exists := raw["tier_thresholds"]; exists {
		t.Error("nil TierThresholds should be absent from JSON")
	}
}
