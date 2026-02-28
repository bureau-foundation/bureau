// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/agent"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// --- Test helpers ---

// newTestAgentService creates a minimal AgentService with initialized
// maps, a mock artifact client, and timelinesLoaded set to true (so
// tests that pre-populate the index via indexCommit don't trigger a
// CAS scan on resolve-context calls).
func newTestAgentService() *AgentService {
	return &AgentService{
		artifactClient:     newMockArtifactClient(),
		commitIndex:        make(map[string]agent.ContextCommitContent),
		principalTimelines: make(map[string][]timelineEntry),
		timelinesLoaded:    true,
		logger:             slog.Default(),
	}
}

// newTestAgentServiceWithArtifacts creates an AgentService backed by a
// specific mock artifact client for tests that exercise CAS fallback
// paths (e.g., fetching commits not in the in-memory index).
func newTestAgentServiceWithArtifacts(mock *mockArtifactClient) *AgentService {
	return &AgentService{
		artifactClient:     mock,
		commitIndex:        make(map[string]agent.ContextCommitContent),
		principalTimelines: make(map[string][]timelineEntry),
		timelinesLoaded:    true,
		logger:             slog.Default(),
	}
}

// testContextCommit creates a ContextCommitContent with the given
// principal (as "@localpart:server") and timestamp, using sensible
// defaults for other fields. Panics if the principal is non-empty
// and not a valid Matrix user ID.
func testContextCommit(principal string, createdAt string) agent.ContextCommitContent {
	var userID ref.UserID
	if principal != "" {
		userID = ref.MustParseUserID(principal)
	}
	return agent.ContextCommitContent{
		Version:     agent.ContextCommitVersion,
		CommitType:  agent.CommitTypeDelta,
		ArtifactRef: "blake3:deadbeef",
		Format:      "claude-code-v1",
		Principal:   userID,
		Checkpoint:  agent.CheckpointTurnBoundary,
		CreatedAt:   createdAt,
	}
}

// testToken creates a servicetoken.Token for the given principal
// localpart (e.g., "agent/test").
func testToken(principal string) *servicetoken.Token {
	return &servicetoken.Token{
		Subject: ref.MustParseUserID("@" + principal + ":bureau.local"),
	}
}

// --- indexCommit tests ---

func TestIndexCommit(t *testing.T) {
	t.Parallel()

	t.Run("adds new commit to index and timeline", func(t *testing.T) {
		t.Parallel()
		service := newTestAgentService()

		content := testContextCommit("@agent/test:bureau.local", "2026-01-15T10:00:00Z")
		service.indexCommit("ctx-aaaa0000", content)

		if _, exists := service.commitIndex["ctx-aaaa0000"]; !exists {
			t.Fatal("commit not found in index")
		}

		timeline := service.principalTimelines["agent/test"]
		if len(timeline) != 1 {
			t.Fatalf("timeline length = %d, want 1", len(timeline))
		}
		if timeline[0].CommitID != "ctx-aaaa0000" {
			t.Errorf("timeline[0].CommitID = %q, want %q", timeline[0].CommitID, "ctx-aaaa0000")
		}
		if timeline[0].CreatedAt != "2026-01-15T10:00:00Z" {
			t.Errorf("timeline[0].CreatedAt = %q, want %q", timeline[0].CreatedAt, "2026-01-15T10:00:00Z")
		}
	})

	t.Run("deduplicates on re-index", func(t *testing.T) {
		t.Parallel()
		service := newTestAgentService()

		content := testContextCommit("@agent/test:bureau.local", "2026-01-15T10:00:00Z")
		service.indexCommit("ctx-aaaa0000", content)

		// Re-index with updated summary.
		content.Summary = "updated summary"
		service.indexCommit("ctx-aaaa0000", content)

		if service.commitIndex["ctx-aaaa0000"].Summary != "updated summary" {
			t.Error("content not updated on re-index")
		}

		timeline := service.principalTimelines["agent/test"]
		if len(timeline) != 1 {
			t.Fatalf("timeline length = %d, want 1 (should not duplicate)", len(timeline))
		}
	})

	t.Run("maintains sorted timeline order", func(t *testing.T) {
		t.Parallel()
		service := newTestAgentService()

		// Insert out of chronological order.
		service.indexCommit("ctx-cccc0000", testContextCommit("@agent/test:bureau.local", "2026-01-15T12:00:00Z"))
		service.indexCommit("ctx-aaaa0000", testContextCommit("@agent/test:bureau.local", "2026-01-15T10:00:00Z"))
		service.indexCommit("ctx-bbbb0000", testContextCommit("@agent/test:bureau.local", "2026-01-15T11:00:00Z"))

		timeline := service.principalTimelines["agent/test"]
		if len(timeline) != 3 {
			t.Fatalf("timeline length = %d, want 3", len(timeline))
		}
		for i := 1; i < len(timeline); i++ {
			if timeline[i].CreatedAt < timeline[i-1].CreatedAt {
				t.Errorf("timeline not sorted: [%d]=%s >= [%d]=%s",
					i-1, timeline[i-1].CreatedAt, i, timeline[i].CreatedAt)
			}
		}

		// Verify specific order.
		wantOrder := []string{"ctx-aaaa0000", "ctx-bbbb0000", "ctx-cccc0000"}
		for i, want := range wantOrder {
			if timeline[i].CommitID != want {
				t.Errorf("timeline[%d].CommitID = %q, want %q", i, timeline[i].CommitID, want)
			}
		}
	})

	t.Run("separate timelines per principal", func(t *testing.T) {
		t.Parallel()
		service := newTestAgentService()

		service.indexCommit("ctx-aaaa0000", testContextCommit("@agent/alice:bureau.local", "2026-01-15T10:00:00Z"))
		service.indexCommit("ctx-bbbb0000", testContextCommit("@agent/bob:bureau.local", "2026-01-15T10:00:00Z"))

		if len(service.principalTimelines["agent/alice"]) != 1 {
			t.Errorf("alice timeline length = %d, want 1", len(service.principalTimelines["agent/alice"]))
		}
		if len(service.principalTimelines["agent/bob"]) != 1 {
			t.Errorf("bob timeline length = %d, want 1", len(service.principalTimelines["agent/bob"]))
		}
	})

	t.Run("skips timeline for empty principal", func(t *testing.T) {
		t.Parallel()
		service := newTestAgentService()

		content := testContextCommit("", "2026-01-15T10:00:00Z")
		service.indexCommit("ctx-aaaa0000", content)

		// Commit should be in the index but not in any timeline.
		if _, exists := service.commitIndex["ctx-aaaa0000"]; !exists {
			t.Fatal("commit not found in index")
		}
		if len(service.principalTimelines) != 0 {
			t.Errorf("principal timelines should be empty, got %d entries", len(service.principalTimelines))
		}
	})
}

// --- Resolve context tests ---

func TestHandleResolveContext(t *testing.T) {
	t.Parallel()

	service := newTestAgentService()
	service.indexCommit("ctx-aaaa0000", testContextCommit("@agent/test:bureau.local", "2026-01-15T10:00:00Z"))
	service.indexCommit("ctx-bbbb0000", testContextCommit("@agent/test:bureau.local", "2026-01-15T11:00:00Z"))
	service.indexCommit("ctx-cccc0000", testContextCommit("@agent/test:bureau.local", "2026-01-15T12:00:00Z"))

	token := testToken("agent/test")

	tests := []struct {
		name      string
		timestamp string
		wantID    string
	}{
		{"exact match first", "2026-01-15T10:00:00Z", "ctx-aaaa0000"},
		{"exact match middle", "2026-01-15T11:00:00Z", "ctx-bbbb0000"},
		{"exact match last", "2026-01-15T12:00:00Z", "ctx-cccc0000"},
		{"between first and second", "2026-01-15T10:30:00Z", "ctx-aaaa0000"},
		{"between second and third", "2026-01-15T11:30:00Z", "ctx-bbbb0000"},
		{"after all", "2026-01-15T13:00:00Z", "ctx-cccc0000"},
		{"before all", "2026-01-15T09:00:00Z", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			request := resolveContextRequest{
				Action:    "resolve-context",
				Timestamp: tt.timestamp,
			}
			raw, err := codec.Marshal(request)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}

			result, err := service.handleResolveContext(context.Background(), token, raw)
			if err != nil {
				t.Fatalf("handleResolveContext: %v", err)
			}

			response, ok := result.(resolveContextResponse)
			if !ok {
				t.Fatalf("unexpected response type: %T", result)
			}

			if response.CommitID != tt.wantID {
				t.Errorf("CommitID = %q, want %q", response.CommitID, tt.wantID)
			}
		})
	}

	t.Run("empty timeline", func(t *testing.T) {
		t.Parallel()

		emptyService := newTestAgentService()
		request := resolveContextRequest{
			Action:    "resolve-context",
			Timestamp: "2026-01-15T12:00:00Z",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		result, err := emptyService.handleResolveContext(context.Background(), token, raw)
		if err != nil {
			t.Fatalf("handleResolveContext: %v", err)
		}

		response := result.(resolveContextResponse)
		if response.CommitID != "" {
			t.Errorf("CommitID = %q, want empty", response.CommitID)
		}
	})

	t.Run("cold start loads timelines from CAS", func(t *testing.T) {
		t.Parallel()

		// Simulate a service restart: the in-memory index is empty
		// and timelinesLoaded is false. Commits exist only in CAS.
		mock := newMockArtifactClient()
		mock.storeCommit("ctx-aaaa0000", testContextCommit("@agent/test:bureau.local", "2026-01-15T10:00:00Z"))
		mock.storeCommit("ctx-bbbb0000", testContextCommit("@agent/test:bureau.local", "2026-01-15T11:00:00Z"))
		mock.storeCommit("ctx-cccc0000", testContextCommit("@agent/test:bureau.local", "2026-01-15T12:00:00Z"))

		coldService := newTestAgentServiceWithArtifacts(mock)
		coldService.timelinesLoaded = false // simulate cold start

		request := resolveContextRequest{
			Action:    "resolve-context",
			Timestamp: "2026-01-15T11:30:00Z",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		result, err := coldService.handleResolveContext(context.Background(), token, raw)
		if err != nil {
			t.Fatalf("handleResolveContext: %v", err)
		}

		response := result.(resolveContextResponse)
		if response.CommitID != "ctx-bbbb0000" {
			t.Errorf("CommitID = %q, want %q", response.CommitID, "ctx-bbbb0000")
		}

		// Verify that timelinesLoaded is now true.
		if !coldService.timelinesLoaded {
			t.Error("timelinesLoaded should be true after lazy load")
		}

		// Verify all commits were indexed.
		if len(coldService.commitIndex) != 3 {
			t.Errorf("commitIndex size = %d, want 3", len(coldService.commitIndex))
		}
	})

	t.Run("requires timestamp", func(t *testing.T) {
		t.Parallel()

		request := resolveContextRequest{Action: "resolve-context"}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		_, err = service.handleResolveContext(context.Background(), token, raw)
		if err == nil {
			t.Fatal("expected error for missing timestamp")
		}
	})

	t.Run("cross-principal requires grant", func(t *testing.T) {
		t.Parallel()

		otherToken := testToken("agent/other")
		request := resolveContextRequest{
			Action:         "resolve-context",
			PrincipalLocal: "agent/test",
			Timestamp:      "2026-01-15T12:00:00Z",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		_, err = service.handleResolveContext(context.Background(), otherToken, raw)
		if err == nil {
			t.Fatal("expected access denied error")
		}
	})
}

// --- Show context commit tests ---

func TestHandleShowContextCommit(t *testing.T) {
	t.Parallel()

	service := newTestAgentService()
	content := testContextCommit("@agent/test:bureau.local", "2026-01-15T10:00:00Z")
	content.Summary = "test summary"
	service.indexCommit("ctx-aaaa0000", content)

	t.Run("returns commit from index", func(t *testing.T) {
		t.Parallel()

		token := testToken("agent/test")
		request := showContextCommitRequest{
			Action:   "show-context-commit",
			CommitID: "ctx-aaaa0000",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		result, err := service.handleShowContextCommit(context.Background(), token, raw)
		if err != nil {
			t.Fatalf("handleShowContextCommit: %v", err)
		}

		response := result.(showContextCommitResponse)
		if response.ID != "ctx-aaaa0000" {
			t.Errorf("ID = %q, want %q", response.ID, "ctx-aaaa0000")
		}
		if response.Commit.Summary != "test summary" {
			t.Errorf("Summary = %q, want %q", response.Commit.Summary, "test summary")
		}
		if response.Commit.Format != "claude-code-v1" {
			t.Errorf("Format = %q, want %q", response.Commit.Format, "claude-code-v1")
		}
	})

	t.Run("requires commit_id", func(t *testing.T) {
		t.Parallel()

		token := testToken("agent/test")
		request := showContextCommitRequest{Action: "show-context-commit"}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		_, err = service.handleShowContextCommit(context.Background(), token, raw)
		if err == nil {
			t.Fatal("expected error for missing commit_id")
		}
	})

	t.Run("cross-principal requires agent/read grant", func(t *testing.T) {
		t.Parallel()

		otherToken := testToken("agent/other")
		request := showContextCommitRequest{
			Action:   "show-context-commit",
			CommitID: "ctx-aaaa0000",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		_, err = service.handleShowContextCommit(context.Background(), otherToken, raw)
		if err == nil {
			t.Fatal("expected access denied error")
		}
	})

	t.Run("falls back to CAS for unindexed commit", func(t *testing.T) {
		t.Parallel()

		commitContent := testContextCommit("@agent/test:bureau.local", "2026-01-15T15:00:00Z")
		mock := newMockArtifactClient()
		mock.storeCommit("ctx-ffff0000", commitContent)
		serviceWithArtifacts := newTestAgentServiceWithArtifacts(mock)

		token := testToken("agent/test")
		request := showContextCommitRequest{
			Action:   "show-context-commit",
			CommitID: "ctx-ffff0000",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		result, err := serviceWithArtifacts.handleShowContextCommit(context.Background(), token, raw)
		if err != nil {
			t.Fatalf("handleShowContextCommit: %v", err)
		}

		response := result.(showContextCommitResponse)
		if response.ID != "ctx-ffff0000" {
			t.Errorf("ID = %q, want %q", response.ID, "ctx-ffff0000")
		}
	})
}

// --- History context tests ---

func TestHandleHistoryContext(t *testing.T) {
	t.Parallel()

	// Build a 3-commit chain: root → middle → tip.
	service := newTestAgentService()

	root := testContextCommit("@agent/test:bureau.local", "2026-01-15T10:00:00Z")
	root.Summary = "root"
	service.indexCommit("ctx-root0000", root)

	middle := testContextCommit("@agent/test:bureau.local", "2026-01-15T11:00:00Z")
	middle.Parent = "ctx-root0000"
	middle.Summary = "middle"
	service.indexCommit("ctx-mid00000", middle)

	tip := testContextCommit("@agent/test:bureau.local", "2026-01-15T12:00:00Z")
	tip.Parent = "ctx-mid00000"
	tip.Summary = "tip"
	service.indexCommit("ctx-tip00000", tip)

	token := testToken("agent/test")

	t.Run("full chain from tip", func(t *testing.T) {
		t.Parallel()

		request := historyContextRequest{
			Action:   "history-context",
			CommitID: "ctx-tip00000",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		result, err := service.handleHistoryContext(context.Background(), token, raw)
		if err != nil {
			t.Fatalf("handleHistoryContext: %v", err)
		}

		response := result.(historyContextResponse)
		if len(response.Commits) != 3 {
			t.Fatalf("commit count = %d, want 3", len(response.Commits))
		}

		// Order should be tip → middle → root.
		wantOrder := []string{"ctx-tip00000", "ctx-mid00000", "ctx-root0000"}
		for i, want := range wantOrder {
			if response.Commits[i].ID != want {
				t.Errorf("commits[%d].ID = %q, want %q", i, response.Commits[i].ID, want)
			}
		}
	})

	t.Run("depth limited to 2", func(t *testing.T) {
		t.Parallel()

		request := historyContextRequest{
			Action:   "history-context",
			CommitID: "ctx-tip00000",
			Depth:    2,
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		result, err := service.handleHistoryContext(context.Background(), token, raw)
		if err != nil {
			t.Fatalf("handleHistoryContext: %v", err)
		}

		response := result.(historyContextResponse)
		if len(response.Commits) != 2 {
			t.Fatalf("commit count = %d, want 2", len(response.Commits))
		}

		// Tip and middle only.
		if response.Commits[0].ID != "ctx-tip00000" {
			t.Errorf("commits[0].ID = %q, want %q", response.Commits[0].ID, "ctx-tip00000")
		}
		if response.Commits[1].ID != "ctx-mid00000" {
			t.Errorf("commits[1].ID = %q, want %q", response.Commits[1].ID, "ctx-mid00000")
		}
	})

	t.Run("single commit with no parent", func(t *testing.T) {
		t.Parallel()

		request := historyContextRequest{
			Action:   "history-context",
			CommitID: "ctx-root0000",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		result, err := service.handleHistoryContext(context.Background(), token, raw)
		if err != nil {
			t.Fatalf("handleHistoryContext: %v", err)
		}

		response := result.(historyContextResponse)
		if len(response.Commits) != 1 {
			t.Fatalf("commit count = %d, want 1", len(response.Commits))
		}
		if response.Commits[0].ID != "ctx-root0000" {
			t.Errorf("commits[0].ID = %q, want %q", response.Commits[0].ID, "ctx-root0000")
		}
	})
}

// --- Update context metadata tests ---

func TestHandleUpdateContextMetadata(t *testing.T) {
	t.Parallel()

	t.Run("updates summary on own commit", func(t *testing.T) {
		t.Parallel()

		content := testContextCommit("@agent/test:bureau.local", "2026-01-15T10:00:00Z")
		mock := newMockArtifactClient()
		service := newTestAgentServiceWithArtifacts(mock)
		service.indexCommit("ctx-aaaa0000", content)

		token := testToken("agent/test")
		request := updateContextMetadataRequest{
			Action:   "update-context-metadata",
			CommitID: "ctx-aaaa0000",
			Summary:  "new summary",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		_, err = service.handleUpdateContextMetadata(context.Background(), token, raw)
		if err != nil {
			t.Fatalf("handleUpdateContextMetadata: %v", err)
		}

		// Verify the in-memory index was updated.
		indexed := service.commitIndex["ctx-aaaa0000"]
		if indexed.Summary != "new summary" {
			t.Errorf("indexed summary = %q, want %q", indexed.Summary, "new summary")
		}

		// Verify the artifact was stored with the correct tag.
		if mock.lastStoredTag != "ctx/ctx-aaaa0000" {
			t.Errorf("stored tag = %q, want %q", mock.lastStoredTag, "ctx/ctx-aaaa0000")
		}
	})

	t.Run("cross-principal requires agent/write grant", func(t *testing.T) {
		t.Parallel()

		content := testContextCommit("@agent/test:bureau.local", "2026-01-15T10:00:00Z")
		service := newTestAgentService()
		service.indexCommit("ctx-aaaa0000", content)

		otherToken := testToken("agent/other")
		request := updateContextMetadataRequest{
			Action:   "update-context-metadata",
			CommitID: "ctx-aaaa0000",
			Summary:  "unauthorized update",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		_, err = service.handleUpdateContextMetadata(context.Background(), otherToken, raw)
		if err == nil {
			t.Fatal("expected access denied error")
		}
	})
}

// --- Archive artifact tests ---

func TestHandleArchiveArtifact(t *testing.T) {
	t.Parallel()

	t.Run("stores artifact and returns ref", func(t *testing.T) {
		t.Parallel()

		mock := newMockArtifactClient()
		service := newTestAgentServiceWithArtifacts(mock)
		token := testToken("agent/test")

		request := archiveArtifactRequest{
			Action:      "archive-artifact",
			Data:        []byte("# Design Document\n\nThis is a plan."),
			ContentType: "text/markdown",
			Label:       "plan",
			Description: "Sprint planning document for auth refactor",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		result, err := service.handleArchiveArtifact(context.Background(), token, raw)
		if err != nil {
			t.Fatalf("handleArchiveArtifact: %v", err)
		}

		response, ok := result.(archiveArtifactResponse)
		if !ok {
			t.Fatalf("unexpected response type: %T", result)
		}

		if response.Ref == "" {
			t.Fatal("expected non-empty artifact ref")
		}
		if response.Size != int64(len(request.Data)) {
			t.Errorf("response size = %d, want %d", response.Size, len(request.Data))
		}

		// Verify the artifact was stored with correct content.
		stored, exists := mock.artifacts[response.Ref]
		if !exists {
			t.Fatalf("artifact %q not found in mock store", response.Ref)
		}
		if !bytes.Equal(stored, request.Data) {
			t.Errorf("stored data = %q, want %q", stored, request.Data)
		}

		// Verify the store header fields were passed correctly.
		if mock.lastStoredContentType != "text/markdown" {
			t.Errorf("content type = %q, want %q", mock.lastStoredContentType, "text/markdown")
		}
		if len(mock.lastStoredLabels) != 1 || mock.lastStoredLabels[0] != "plan" {
			t.Errorf("labels = %v, want [plan]", mock.lastStoredLabels)
		}
		if mock.lastStoredDescription != "Sprint planning document for auth refactor" {
			t.Errorf("description = %q, want %q",
				mock.lastStoredDescription, "Sprint planning document for auth refactor")
		}
	})

	t.Run("stores binary data", func(t *testing.T) {
		t.Parallel()

		mock := newMockArtifactClient()
		service := newTestAgentServiceWithArtifacts(mock)
		token := testToken("agent/test")

		// Binary content (CBOR, protobuf, compressed data, etc.)
		binaryData := []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0xfd}

		request := archiveArtifactRequest{
			Action:      "archive-artifact",
			Data:        binaryData,
			ContentType: "application/octet-stream",
			Label:       "checkpoint",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		result, err := service.handleArchiveArtifact(context.Background(), token, raw)
		if err != nil {
			t.Fatalf("handleArchiveArtifact: %v", err)
		}

		response := result.(archiveArtifactResponse)
		stored := mock.artifacts[response.Ref]
		if !bytes.Equal(stored, binaryData) {
			t.Errorf("stored binary data mismatch: got %x, want %x", stored, binaryData)
		}
	})

	t.Run("description is optional", func(t *testing.T) {
		t.Parallel()

		mock := newMockArtifactClient()
		service := newTestAgentServiceWithArtifacts(mock)
		token := testToken("agent/test")

		request := archiveArtifactRequest{
			Action:      "archive-artifact",
			Data:        []byte("content"),
			ContentType: "text/plain",
			Label:       "note",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		result, err := service.handleArchiveArtifact(context.Background(), token, raw)
		if err != nil {
			t.Fatalf("handleArchiveArtifact: %v", err)
		}

		response := result.(archiveArtifactResponse)
		if response.Ref == "" {
			t.Fatal("expected non-empty artifact ref")
		}
		if mock.lastStoredDescription != "" {
			t.Errorf("description should be empty, got %q", mock.lastStoredDescription)
		}
	})

	t.Run("rejects empty data", func(t *testing.T) {
		t.Parallel()

		service := newTestAgentService()
		token := testToken("agent/test")

		request := archiveArtifactRequest{
			Action:      "archive-artifact",
			Data:        nil,
			ContentType: "text/plain",
			Label:       "empty",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		_, err = service.handleArchiveArtifact(context.Background(), token, raw)
		if err == nil {
			t.Fatal("expected error for empty data")
		}
	})

	t.Run("rejects empty content_type", func(t *testing.T) {
		t.Parallel()

		service := newTestAgentService()
		token := testToken("agent/test")

		request := archiveArtifactRequest{
			Action:      "archive-artifact",
			Data:        []byte("content"),
			ContentType: "",
			Label:       "note",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		_, err = service.handleArchiveArtifact(context.Background(), token, raw)
		if err == nil {
			t.Fatal("expected error for empty content_type")
		}
	})

	t.Run("rejects empty label", func(t *testing.T) {
		t.Parallel()

		service := newTestAgentService()
		token := testToken("agent/test")

		request := archiveArtifactRequest{
			Action:      "archive-artifact",
			Data:        []byte("content"),
			ContentType: "text/plain",
			Label:       "",
		}
		raw, err := codec.Marshal(request)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}

		_, err = service.handleArchiveArtifact(context.Background(), token, raw)
		if err == nil {
			t.Fatal("expected error for empty label")
		}
	})
}

// --- Mock artifact client ---

// mockArtifactClient is an in-memory implementation of artifactAccess
// for testing. Artifacts are stored as raw bytes keyed by ref. Tags
// map names to artifact refs. Store generates deterministic refs from
// an atomic counter.
type mockArtifactClient struct {
	// artifacts maps artifact ref → content bytes.
	artifacts map[string][]byte

	// tags maps tag name → artifact ref.
	tags map[string]string

	// storeCount generates unique refs for stored artifacts.
	storeCount atomic.Int64

	// lastStoredTag records the tag from the most recent Store call
	// (via StoreHeader.Tag). Empty if the last Store had no tag.
	lastStoredTag string

	// lastStoredLabels records the labels from the most recent Store call.
	lastStoredLabels []string

	// lastStoredContentType records the content type from the most
	// recent Store call.
	lastStoredContentType string

	// lastStoredDescription records the description from the most
	// recent Store call.
	lastStoredDescription string
}

func newMockArtifactClient() *mockArtifactClient {
	return &mockArtifactClient{
		artifacts: make(map[string][]byte),
		tags:      make(map[string]string),
	}
}

// storeCommit is a test helper that serializes a ContextCommitContent
// to CBOR and stores it with a "ctx/<commitID>" tag, mimicking what
// storeCommitMetadata does in production.
func (m *mockArtifactClient) storeCommit(commitID string, content agent.ContextCommitContent) {
	data, err := codec.Marshal(content)
	if err != nil {
		panic("mockArtifactClient.storeCommit: marshal failed: " + err.Error())
	}
	artifactRef := fmt.Sprintf("art-%04d", m.storeCount.Add(1))
	m.artifacts[artifactRef] = data
	m.tags["ctx/"+commitID] = artifactRef
}

func (m *mockArtifactClient) Store(_ context.Context, header *artifactstore.StoreHeader, _ io.Reader) (*artifactstore.StoreResponse, error) {
	artifactRef := fmt.Sprintf("art-%04d", m.storeCount.Add(1))
	m.artifacts[artifactRef] = header.Data
	if header.Tag != "" {
		m.tags[header.Tag] = artifactRef
		m.lastStoredTag = header.Tag
	}
	m.lastStoredLabels = header.Labels
	m.lastStoredContentType = header.ContentType
	m.lastStoredDescription = header.Description
	return &artifactstore.StoreResponse{
		Ref:  artifactRef,
		Size: int64(len(header.Data)),
	}, nil
}

func (m *mockArtifactClient) Fetch(_ context.Context, artifactRef string) (*artifactstore.FetchResult, error) {
	data, exists := m.artifacts[artifactRef]
	if !exists {
		return nil, fmt.Errorf("artifact %q not found", artifactRef)
	}
	return &artifactstore.FetchResult{
		Response: artifactstore.FetchResponse{
			Size: int64(len(data)),
			Data: data,
		},
		Content: io.NopCloser(bytes.NewReader(data)),
	}, nil
}

func (m *mockArtifactClient) Resolve(_ context.Context, nameOrRef string) (*artifactstore.ResolveResponse, error) {
	// Check tags first.
	if artifactRef, exists := m.tags[nameOrRef]; exists {
		return &artifactstore.ResolveResponse{
			Ref: artifactRef,
			Tag: nameOrRef,
		}, nil
	}
	// Check direct artifact refs.
	if _, exists := m.artifacts[nameOrRef]; exists {
		return &artifactstore.ResolveResponse{Ref: nameOrRef}, nil
	}
	return nil, fmt.Errorf("ref %q not found", nameOrRef)
}

func (m *mockArtifactClient) Tags(_ context.Context, prefix string) (*artifactstore.TagsResponse, error) {
	var entries []artifactstore.TagEntry
	for name, artifactRef := range m.tags {
		if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			entries = append(entries, artifactstore.TagEntry{
				Name: name,
				Ref:  artifactRef,
			})
		}
	}
	return &artifactstore.TagsResponse{Tags: entries}, nil
}
