// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// validTicketContent returns a TicketContent with all required fields
// set to valid values. Tests modify individual fields to test validation.
func validTicketContent() TicketContent {
	return TicketContent{
		Version:   1,
		Title:     "Fix authentication bug in login flow",
		Status:    "open",
		Priority:  2,
		Type:      "bug",
		CreatedBy: "@iree/amdgpu/pm:bureau.local",
		CreatedAt: "2026-02-12T10:00:00Z",
		UpdatedAt: "2026-02-12T10:00:00Z",
	}
}

func TestTicketContentRoundTrip(t *testing.T) {
	original := TicketContent{
		Version:   1,
		Title:     "Implement AMDGPU inference pipeline",
		Body:      "Set up the full inference pipeline for AMDGPU targets.",
		Status:    "in_progress",
		Priority:  1,
		Type:      "feature",
		Labels:    []string{"amdgpu", "inference", "p1"},
		Assignee:  "@iree/amdgpu/pm:bureau.local",
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
				Author:    "@bureau/admin:bureau.local",
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
		CreatedBy:   "@bureau/admin:bureau.local",
		CreatedAt:   "2026-02-12T09:00:00Z",
		UpdatedAt:   "2026-02-12T11:00:00Z",
		ClosedAt:    "",
		CloseReason: "",
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
	assertField(t, raw, "version", float64(1))
	assertField(t, raw, "title", original.Title)
	assertField(t, raw, "body", original.Body)
	assertField(t, raw, "status", "in_progress")
	assertField(t, raw, "priority", float64(1))
	assertField(t, raw, "type", "feature")
	assertField(t, raw, "assignee", original.Assignee)
	assertField(t, raw, "parent", "tkt-epic1")
	assertField(t, raw, "created_by", "@bureau/admin:bureau.local")
	assertField(t, raw, "created_at", "2026-02-12T09:00:00Z")
	assertField(t, raw, "updated_at", "2026-02-12T11:00:00Z")

	// Labels: verify as JSON array.
	labels, ok := raw["labels"].([]any)
	if !ok {
		t.Fatalf("labels is not an array: %T", raw["labels"])
	}
	if len(labels) != 3 || labels[0] != "amdgpu" || labels[1] != "inference" || labels[2] != "p1" {
		t.Errorf("labels = %v, want [amdgpu inference p1]", labels)
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

	// Round-trip: marshal → unmarshal → compare.
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
		"body", "labels", "assignee", "parent", "blocked_by",
		"gates", "notes", "attachments", "closed_at", "close_reason",
		"origin", "extra",
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
	// Simulate a v2 event with an unknown top-level field. This
	// documents the behavior that CanModify guards against: unknown
	// fields are silently dropped on unmarshal, so a read-modify-write
	// cycle through v1 code would lose the "new_v2_field".
	v2JSON := `{
		"version": 2,
		"title": "Fix something",
		"status": "open",
		"priority": 2,
		"type": "bug",
		"created_by": "@bureau/admin:bureau.local",
		"created_at": "2026-02-12T10:00:00Z",
		"updated_at": "2026-02-12T10:00:00Z",
		"new_v2_field": "this field does not exist in v1 TicketContent"
	}`

	// v1 code can unmarshal v2 events without error.
	var content TicketContent
	if err := json.Unmarshal([]byte(v2JSON), &content); err != nil {
		t.Fatalf("Unmarshal v2 event: %v", err)
	}

	// Known fields are correctly populated.
	if content.Version != 2 {
		t.Errorf("Version = %d, want 2", content.Version)
	}
	if content.Title != "Fix something" {
		t.Errorf("Title = %q, want %q", content.Title, "Fix something")
	}

	// CanModify rejects modification of v2 events from v1 code.
	if err := content.CanModify(); err == nil {
		t.Error("CanModify() should reject v2 events from v1 code")
	}

	// Re-marshaling drops the unknown field.
	remarshaled, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if strings.Contains(string(remarshaled), "new_v2_field") {
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
			modify:  func(tc *TicketContent) { tc.Status = "open" },
			wantErr: "",
		},
		{
			name:    "status_in_progress",
			modify:  func(tc *TicketContent) { tc.Status = "in_progress" },
			wantErr: "",
		},
		{
			name:    "status_blocked",
			modify:  func(tc *TicketContent) { tc.Status = "blocked" },
			wantErr: "",
		},
		{
			name:    "status_closed",
			modify:  func(tc *TicketContent) { tc.Status = "closed" },
			wantErr: "",
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
			modify:  func(tc *TicketContent) { tc.Type = "task" },
			wantErr: "",
		},
		{
			name:    "type_bug",
			modify:  func(tc *TicketContent) { tc.Type = "bug" },
			wantErr: "",
		},
		{
			name:    "type_feature",
			modify:  func(tc *TicketContent) { tc.Type = "feature" },
			wantErr: "",
		},
		{
			name:    "type_epic",
			modify:  func(tc *TicketContent) { tc.Type = "epic" },
			wantErr: "",
		},
		{
			name:    "type_chore",
			modify:  func(tc *TicketContent) { tc.Type = "chore" },
			wantErr: "",
		},
		{
			name:    "type_docs",
			modify:  func(tc *TicketContent) { tc.Type = "docs" },
			wantErr: "",
		},
		{
			name:    "type_question",
			modify:  func(tc *TicketContent) { tc.Type = "question" },
			wantErr: "",
		},
		{
			name:    "created_by_empty",
			modify:  func(tc *TicketContent) { tc.CreatedBy = "" },
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
				tc.Notes = []TicketNote{{ID: "n-1", Author: "", CreatedAt: "2026-02-12T10:00:00Z", Body: "test"}}
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
			name: "valid_with_all_optional",
			modify: func(tc *TicketContent) {
				tc.Body = "Full description"
				tc.Labels = []string{"important"}
				tc.Assignee = "@test:bureau.local"
				tc.Parent = "tkt-parent"
				tc.BlockedBy = []string{"tkt-dep"}
				tc.Gates = []TicketGate{{ID: "g1", Type: "human", Status: "pending"}}
				tc.Notes = []TicketNote{{ID: "n-1", Author: "@a:b.c", CreatedAt: "2026-01-01T00:00:00Z", Body: "note"}}
				tc.Attachments = []TicketAttachment{{Ref: "art-abc123"}}
				tc.Origin = &TicketOrigin{Source: "github", ExternalRef: "GH-1234"}
			},
			wantErr: "",
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
				RoomAlias: "#iree/amdgpu/inference:bureau.local",
				ContentMatch: ContentMatch{
					"status": Eq("active"),
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
			name: "timer_gate",
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
		"state_key", "room_alias", "content_match", "ticket_id",
		"duration", "created_at", "satisfied_at", "satisfied_by",
	}
	for _, field := range optionalFields {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty, but is present", field)
		}
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
			name:    "timer_missing_duration",
			gate:    TicketGate{ID: "g1", Type: "timer", Status: "pending"},
			wantErr: "duration is required for timer gates",
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
		Author:    "@bureau/admin:bureau.local",
		CreatedAt: "2026-02-12T10:30:00Z",
		Body:      "Security scanner found CVE-2026-1234 in this dependency.",
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

	var decoded TicketNote
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
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
			note:    TicketNote{ID: "n-1", Author: "@a:b.c", CreatedAt: "2026-01-01T00:00:00Z", Body: "note"},
			wantErr: "",
		},
		{
			name:    "id_empty",
			note:    TicketNote{ID: "", Author: "@a:b.c", CreatedAt: "2026-01-01T00:00:00Z", Body: "note"},
			wantErr: "id is required",
		},
		{
			name:    "author_empty",
			note:    TicketNote{ID: "n-1", Author: "", CreatedAt: "2026-01-01T00:00:00Z", Body: "note"},
			wantErr: "author is required",
		},
		{
			name:    "created_at_empty",
			note:    TicketNote{ID: "n-1", Author: "@a:b.c", CreatedAt: "", Body: "note"},
			wantErr: "created_at is required",
		},
		{
			name:    "body_empty",
			note:    TicketNote{ID: "n-1", Author: "@a:b.c", CreatedAt: "2026-01-01T00:00:00Z", Body: ""},
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

	for _, field := range []string{"prefix", "default_labels", "extra"} {
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
