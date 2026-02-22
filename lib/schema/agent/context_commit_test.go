// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// --- ContextCommitContent ---

func TestContextCommitContentRoundTrip(t *testing.T) {
	original := ContextCommitContent{
		Version:      1,
		Parent:       "ctx-a1b2c3d4",
		CommitType:   CommitTypeDelta,
		ArtifactRef:  "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		Format:       "claude-code-v1",
		Template:     "bureau/template:code-reviewer",
		Principal:    ref.MustParseUserID("@iree/amdgpu/reviewer:bureau.local"),
		Machine:      ref.MustParseUserID("@iree/fleet/prod/machine/gpu-box:bureau.local"),
		SessionID:    "session-12345",
		Checkpoint:   CheckpointTurnBoundary,
		TicketID:     "tkt-a3f9",
		ThreadID:     "$thread-event-id",
		Summary:      "Reviewed the AMDGPU driver initialization code",
		MessageCount: 15,
		TokenCount:   42000,
		CreatedAt:    "2026-02-22T10:30:00Z",
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
	assertField(t, raw, "parent", "ctx-a1b2c3d4")
	assertField(t, raw, "commit_type", "delta")
	assertField(t, raw, "artifact_ref", "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2")
	assertField(t, raw, "format", "claude-code-v1")
	assertField(t, raw, "template", "bureau/template:code-reviewer")
	assertField(t, raw, "principal", "@iree/amdgpu/reviewer:bureau.local")
	assertField(t, raw, "machine", "@iree/fleet/prod/machine/gpu-box:bureau.local")
	assertField(t, raw, "session_id", "session-12345")
	assertField(t, raw, "checkpoint", "turn_boundary")
	assertField(t, raw, "ticket_id", "tkt-a3f9")
	assertField(t, raw, "thread_id", "$thread-event-id")
	assertField(t, raw, "summary", "Reviewed the AMDGPU driver initialization code")
	assertField(t, raw, "message_count", float64(15))
	assertField(t, raw, "token_count", float64(42000))
	assertField(t, raw, "created_at", "2026-02-22T10:30:00Z")

	var decoded ContextCommitContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

func TestContextCommitContentRoundTripCompaction(t *testing.T) {
	original := ContextCommitContent{
		Version:     1,
		Parent:      "ctx-b2c3d4e5",
		CommitType:  CommitTypeCompaction,
		ArtifactRef: "f1e2d3c4b5a6f1e2d3c4b5a6f1e2d3c4b5a6f1e2d3c4b5a6f1e2d3c4b5a6f1e2",
		Format:      "claude-code-v1",
		Checkpoint:  CheckpointCompaction,
		Summary:     "Compacted 30 messages: investigated AMDGPU init bug, narrowed to register write ordering",
		CreatedAt:   "2026-02-22T11:00:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded ContextCommitContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

func TestContextCommitContentRoundTripRootCommit(t *testing.T) {
	original := ContextCommitContent{
		Version:      1,
		CommitType:   CommitTypeDelta,
		ArtifactRef:  "c3d4e5f6a7b8c3d4e5f6a7b8c3d4e5f6a7b8c3d4e5f6a7b8c3d4e5f6a7b8c3d4",
		Format:       "bureau-agent-v1",
		Template:     "bureau/template:pm",
		SessionID:    "session-001",
		Checkpoint:   CheckpointSessionEnd,
		MessageCount: 5,
		TokenCount:   8000,
		CreatedAt:    "2026-02-22T09:00:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Root commit should not have "parent" in JSON.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["parent"]; exists {
		t.Error("parent should be omitted for root commit, but is present")
	}

	var decoded ContextCommitContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

func TestContextCommitContentOmitsEmptyOptionals(t *testing.T) {
	content := ContextCommitContent{
		Version:     1,
		CommitType:  CommitTypeDelta,
		ArtifactRef: "abc123",
		Format:      "bureau-agent-v1",
		Checkpoint:  CheckpointExplicit,
		CreatedAt:   "2026-02-22T10:00:00Z",
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{
		"parent",
		"template",
		"session_id",
		"ticket_id",
		"thread_id",
		"summary",
		"message_count",
		"token_count",
	} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty, but is present", field)
		}
	}

	// ref.UserID implements TextMarshaler, so zero values marshal as
	// empty strings rather than being omitted. Verify they are present
	// but empty (matching the TicketContent.Assignee behavior).
	for _, field := range []string{"principal", "machine"} {
		got, ok := raw[field]
		if !ok {
			t.Errorf("%s should be present as empty string for zero UserID", field)
		} else if got != "" {
			t.Errorf("%s = %v, want empty string for zero UserID", field, got)
		}
	}
}

func TestContextCommitContentValidate(t *testing.T) {
	valid := func() ContextCommitContent {
		return ContextCommitContent{
			Version:     1,
			CommitType:  CommitTypeDelta,
			ArtifactRef: "abc123",
			Format:      "claude-code-v1",
			Checkpoint:  CheckpointTurnBoundary,
			CreatedAt:   "2026-02-22T10:00:00Z",
		}
	}

	tests := []struct {
		name    string
		modify  func(*ContextCommitContent)
		wantErr string
	}{
		{
			name:    "valid_minimal",
			modify:  func(_ *ContextCommitContent) {},
			wantErr: "",
		},
		{
			name: "valid_full",
			modify: func(content *ContextCommitContent) {
				content.Parent = "ctx-a1b2c3d4"
				content.Template = "bureau/template:reviewer"
				content.Principal = ref.MustParseUserID("@agent:bureau.local")
				content.Machine = ref.MustParseUserID("@machine:bureau.local")
				content.SessionID = "session-42"
				content.TicketID = "tkt-x1y2"
				content.ThreadID = "$thread-id"
				content.Summary = "Reviewed code"
				content.MessageCount = 10
				content.TokenCount = 5000
			},
			wantErr: "",
		},
		{
			name: "valid_compaction",
			modify: func(content *ContextCommitContent) {
				content.CommitType = CommitTypeCompaction
				content.Checkpoint = CheckpointCompaction
				content.Parent = "ctx-prev"
				content.Summary = "Compacted 50 messages"
			},
			wantErr: "",
		},
		{
			name: "valid_snapshot",
			modify: func(content *ContextCommitContent) {
				content.CommitType = CommitTypeSnapshot
			},
			wantErr: "",
		},
		{
			name: "version_zero",
			modify: func(content *ContextCommitContent) {
				content.Version = 0
			},
			wantErr: "version must be >= 1",
		},
		{
			name: "missing_commit_type",
			modify: func(content *ContextCommitContent) {
				content.CommitType = ""
			},
			wantErr: "commit_type is required",
		},
		{
			name: "unknown_commit_type",
			modify: func(content *ContextCommitContent) {
				content.CommitType = "patch"
			},
			wantErr: "unknown commit_type",
		},
		{
			name: "missing_artifact_ref",
			modify: func(content *ContextCommitContent) {
				content.ArtifactRef = ""
			},
			wantErr: "artifact_ref is required",
		},
		{
			name: "missing_format",
			modify: func(content *ContextCommitContent) {
				content.Format = ""
			},
			wantErr: "format is required",
		},
		{
			name: "missing_checkpoint",
			modify: func(content *ContextCommitContent) {
				content.Checkpoint = ""
			},
			wantErr: "checkpoint is required",
		},
		{
			name: "unknown_checkpoint",
			modify: func(content *ContextCommitContent) {
				content.Checkpoint = "auto_save"
			},
			wantErr: "unknown checkpoint",
		},
		{
			name: "missing_created_at",
			modify: func(content *ContextCommitContent) {
				content.CreatedAt = ""
			},
			wantErr: "created_at is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := valid()
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

func TestContextCommitContentCanModify(t *testing.T) {
	tests := []struct {
		name    string
		version int
		wantErr bool
	}{
		{"current_version", ContextCommitVersion, false},
		{"older_version", ContextCommitVersion - 1, false},
		{"newer_version", ContextCommitVersion + 1, true},
		{"far_future_version", ContextCommitVersion + 100, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := ContextCommitContent{Version: test.version}
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

func TestContextCommitContentForwardCompatibility(t *testing.T) {
	v2JSON := `{
		"version": 2,
		"commit_type": "delta",
		"artifact_ref": "abc123",
		"format": "claude-code-v1",
		"checkpoint": "turn_boundary",
		"created_at": "2026-02-22T10:00:00Z",
		"new_v2_field": "unknown to v1"
	}`

	var content ContextCommitContent
	if err := json.Unmarshal([]byte(v2JSON), &content); err != nil {
		t.Fatalf("Unmarshal v2 event: %v", err)
	}

	if content.Version != 2 {
		t.Errorf("Version = %d, want 2", content.Version)
	}
	if content.CommitType != CommitTypeDelta {
		t.Errorf("CommitType = %q, want %q", content.CommitType, CommitTypeDelta)
	}

	if err := content.CanModify(); err == nil {
		t.Fatal("CanModify() = nil for v2 event, want error")
	}

	remarshaled, _ := json.Marshal(content)
	var raw map[string]any
	json.Unmarshal(remarshaled, &raw)
	if _, exists := raw["new_v2_field"]; exists {
		t.Error("new_v2_field survived round-trip through v1 struct (unexpected)")
	}
}

// --- GenerateContextCommitID ---

func TestGenerateContextCommitID(t *testing.T) {
	id := GenerateContextCommitID(
		"ctx-a1b2c3d4",
		"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		"2026-02-22T10:30:00Z",
		"bureau/template:code-reviewer",
	)

	if !strings.HasPrefix(id, "ctx-") {
		t.Errorf("ID should start with ctx-, got %q", id)
	}
	// "ctx-" (4) + 8 hex chars = 12 total.
	if len(id) != 12 {
		t.Errorf("ID length = %d, want 12 (ctx- + 8 hex chars), got %q", len(id), id)
	}
	// The hex portion should be valid hex.
	hexPart := id[4:]
	if _, err := hex.DecodeString(hexPart); err != nil {
		t.Errorf("hex portion %q is not valid hex: %v", hexPart, err)
	}
}

func TestGenerateContextCommitIDDeterministic(t *testing.T) {
	args := [4]string{
		"ctx-parent",
		"artifact-ref-hash",
		"2026-02-22T10:30:00Z",
		"bureau/template:reviewer",
	}

	first := GenerateContextCommitID(args[0], args[1], args[2], args[3])
	second := GenerateContextCommitID(args[0], args[1], args[2], args[3])

	if first != second {
		t.Errorf("same inputs produced different IDs: %q vs %q", first, second)
	}
}

func TestGenerateContextCommitIDDistinct(t *testing.T) {
	base := [4]string{
		"ctx-parent",
		"artifact-ref-hash",
		"2026-02-22T10:30:00Z",
		"bureau/template:reviewer",
	}

	baseID := GenerateContextCommitID(base[0], base[1], base[2], base[3])

	// Varying each input should produce a different ID.
	variations := [][4]string{
		{"ctx-other-parent", base[1], base[2], base[3]},
		{base[0], "different-artifact-ref", base[2], base[3]},
		{base[0], base[1], "2026-02-22T11:00:00Z", base[3]},
		{base[0], base[1], base[2], "bureau/template:other"},
	}

	for i, variant := range variations {
		variantID := GenerateContextCommitID(variant[0], variant[1], variant[2], variant[3])
		if variantID == baseID {
			t.Errorf("variation %d produced same ID as base: %q (changed input %d)", i, variantID, i)
		}
	}
}

func TestGenerateContextCommitIDRootCommit(t *testing.T) {
	// Root commits have empty parent — should still produce a valid ID.
	id := GenerateContextCommitID(
		"",
		"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		"2026-02-22T09:00:00Z",
		"bureau/template:pm",
	)

	if !strings.HasPrefix(id, "ctx-") {
		t.Errorf("root commit ID should start with ctx-, got %q", id)
	}
	if len(id) != 12 {
		t.Errorf("root commit ID length = %d, want 12, got %q", len(id), id)
	}
}

// --- Event type constant ---

func TestContextCommitEventTypeConstant(t *testing.T) {
	if !strings.HasPrefix(string(EventTypeAgentContextCommit), "m.bureau.") {
		t.Errorf("EventTypeAgentContextCommit = %q, must start with m.bureau.", EventTypeAgentContextCommit)
	}
}
