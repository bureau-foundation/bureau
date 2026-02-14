// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadBeadsFile(t *testing.T) {
	// Find the real beads file relative to the test working directory.
	// When running under bazel test, we need to find it via runfiles
	// or a known path. For now, try several common locations.
	paths := []string{
		"../../.beads/issues.jsonl", // relative from lib/ticketui/
		filepath.Join(os.Getenv("BUILD_WORKSPACE_DIRECTORY"), ".beads/issues.jsonl"),
	}

	var beadsPath string
	for _, candidate := range paths {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			beadsPath = candidate
			break
		}
	}
	if beadsPath == "" {
		t.Skip("beads file not found (expected in .beads/issues.jsonl)")
	}

	source, err := LoadBeadsFile(beadsPath)
	if err != nil {
		t.Fatalf("LoadBeadsFile(%s): %v", beadsPath, err)
	}

	// Verify we loaded a non-trivial number of tickets.
	allSnapshot := source.All()
	if allSnapshot.Stats.Total == 0 {
		t.Fatal("loaded zero tickets from beads file")
	}
	t.Logf("loaded %d tickets", allSnapshot.Stats.Total)

	// Verify stats are internally consistent.
	statusSum := 0
	for _, count := range allSnapshot.Stats.ByStatus {
		statusSum += count
	}
	if statusSum != allSnapshot.Stats.Total {
		t.Errorf("status counts sum to %d, expected %d", statusSum, allSnapshot.Stats.Total)
	}

	// Verify Ready view returns a subset of All.
	readySnapshot := source.Ready()
	if len(readySnapshot.Entries) > allSnapshot.Stats.Total {
		t.Errorf("ready (%d) exceeds total (%d)", len(readySnapshot.Entries), allSnapshot.Stats.Total)
	}
	t.Logf("ready: %d, blocked: %d, total: %d",
		len(readySnapshot.Entries),
		len(source.Blocked().Entries),
		allSnapshot.Stats.Total)

	// Verify every ready entry is open.
	for _, entry := range readySnapshot.Entries {
		if entry.Content.Status != "open" {
			t.Errorf("ready ticket %s has status %q, expected open", entry.ID, entry.Content.Status)
		}
	}

	// Verify entries have required fields.
	for _, entry := range allSnapshot.Entries {
		if entry.ID == "" {
			t.Error("found entry with empty ID")
		}
		if entry.Content.Title == "" {
			t.Errorf("ticket %s has empty title", entry.ID)
		}
		if entry.Content.Status == "" {
			t.Errorf("ticket %s has empty status", entry.ID)
		}
		if entry.Content.Type == "" {
			t.Errorf("ticket %s has empty type", entry.ID)
		}
	}

	// Verify parent/child relationships are consistent.
	for _, entry := range allSnapshot.Entries {
		if entry.Content.Parent != "" {
			_, exists := source.Get(entry.Content.Parent)
			if !exists {
				// Dangling parent reference — the parent ticket might
				// not be in the beads file. This is not an error for
				// the loader; it's valid data.
				t.Logf("ticket %s references parent %s which is not in the index",
					entry.ID, entry.Content.Parent)
			}
		}
	}
}

func TestLoadBeadsFile_Synthetic(t *testing.T) {
	// Create a temporary beads file with known content.
	content := `{"id":"bd-001","title":"First ticket","description":"Body text","status":"open","priority":1,"issue_type":"task","created_at":"2026-01-01T00:00:00Z","created_by":"test","updated_at":"2026-01-01T00:00:00Z","labels":["infra"]}
{"id":"bd-002","title":"Second ticket","description":"Another body","status":"closed","priority":0,"issue_type":"bug","created_at":"2026-01-02T00:00:00Z","created_by":"test","updated_at":"2026-01-02T00:00:00Z","closed_at":"2026-01-02T12:00:00Z","close_reason":"Fixed","dependencies":[{"issue_id":"bd-002","depends_on_id":"bd-001","type":"blocks"}]}
{"id":"bd-003","title":"Epic","description":"Top-level","status":"open","priority":1,"issue_type":"epic","created_at":"2026-01-01T00:00:00Z","created_by":"test","updated_at":"2026-01-01T00:00:00Z"}
{"id":"bd-004","title":"Child of epic","description":"","status":"open","priority":2,"issue_type":"task","created_at":"2026-01-03T00:00:00Z","created_by":"test","updated_at":"2026-01-03T00:00:00Z","dependencies":[{"issue_id":"bd-004","depends_on_id":"bd-003","type":"parent-child"}]}
`
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "test.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	source, err := LoadBeadsFile(path)
	if err != nil {
		t.Fatalf("LoadBeadsFile: %v", err)
	}

	all := source.All()
	if all.Stats.Total != 4 {
		t.Fatalf("expected 4 tickets, got %d", all.Stats.Total)
	}

	// Verify dependency mapping: bd-002 is blocked by bd-001.
	ticket002, ok := source.Get("bd-002")
	if !ok {
		t.Fatal("bd-002 not found")
	}
	if len(ticket002.BlockedBy) != 1 || ticket002.BlockedBy[0] != "bd-001" {
		t.Errorf("bd-002 BlockedBy = %v, expected [bd-001]", ticket002.BlockedBy)
	}

	// Verify parent mapping: bd-004 is child of bd-003.
	ticket004, ok := source.Get("bd-004")
	if !ok {
		t.Fatal("bd-004 not found")
	}
	if ticket004.Parent != "bd-003" {
		t.Errorf("bd-004 Parent = %q, expected bd-003", ticket004.Parent)
	}

	// Verify children query.
	children := source.Children("bd-003")
	if len(children) != 1 || children[0].ID != "bd-004" {
		t.Errorf("children of bd-003 = %v, expected [bd-004]", children)
	}

	// Verify child progress.
	total, closed := source.ChildProgress("bd-003")
	if total != 1 || closed != 0 {
		t.Errorf("child progress of bd-003 = (%d, %d), expected (1, 0)", total, closed)
	}

	// Verify ready: bd-001 is open with no blockers (ready),
	// bd-002 is closed (not ready), bd-003 is open (ready),
	// bd-004 is open with no blockers (ready).
	ready := source.Ready()
	readyIDs := make(map[string]bool)
	for _, entry := range ready.Entries {
		readyIDs[entry.ID] = true
	}
	if !readyIDs["bd-001"] {
		t.Error("bd-001 should be ready")
	}
	if readyIDs["bd-002"] {
		t.Error("bd-002 is closed, should not be in ready")
	}
	if !readyIDs["bd-003"] {
		t.Error("bd-003 should be ready")
	}
	if !readyIDs["bd-004"] {
		t.Error("bd-004 should be ready (no blockers)")
	}
}

func TestLoadBeadsFile_EmptyFile(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "empty.jsonl")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	source, err := LoadBeadsFile(path)
	if err != nil {
		t.Fatalf("LoadBeadsFile on empty file: %v", err)
	}

	all := source.All()
	if all.Stats.Total != 0 {
		t.Errorf("expected 0 tickets from empty file, got %d", all.Stats.Total)
	}
}

func TestLoadBeadsFile_MissingFile(t *testing.T) {
	_, err := LoadBeadsFile("/nonexistent/path/issues.jsonl")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadBeadsFile_InvalidJSON(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "bad.jsonl")
	if err := os.WriteFile(path, []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadBeadsFile(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadBeadsFile_MissingID(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "noid.jsonl")
	content := `{"title":"No ID ticket","status":"open","priority":1,"issue_type":"task","created_at":"2026-01-01T00:00:00Z","created_by":"test","updated_at":"2026-01-01T00:00:00Z"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadBeadsFile(path)
	if err == nil {
		t.Fatal("expected error for entry with missing ID")
	}
}
