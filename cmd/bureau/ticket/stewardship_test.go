// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadStewardshipContentValid(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "stewardship.json")

	data := `{
		"version": 1,
		"resource_patterns": ["fleet/gpu/**"],
		"gate_types": ["task"],
		"tiers": [
			{
				"principals": ["bureau/gpu/admin:bureau.local"],
				"threshold": 1
			}
		]
	}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	content, err := loadStewardshipContent(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content.Version != 1 {
		t.Errorf("version = %d, want 1", content.Version)
	}
	if len(content.ResourcePatterns) != 1 || content.ResourcePatterns[0] != "fleet/gpu/**" {
		t.Errorf("patterns = %v, want [fleet/gpu/**]", content.ResourcePatterns)
	}
	if len(content.Tiers) != 1 {
		t.Errorf("tiers length = %d, want 1", len(content.Tiers))
	}
}

func TestLoadStewardshipContentInvalid(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "bad.json")

	// Missing required fields.
	data := `{"version": 1}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := loadStewardshipContent(path)
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestLoadStewardshipContentMissingFile(t *testing.T) {
	t.Parallel()

	_, err := loadStewardshipContent("/nonexistent/path/stewardship.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}
