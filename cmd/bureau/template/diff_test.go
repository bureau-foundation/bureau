// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"strings"
	"testing"
)

func TestLineDiffIdentical(t *testing.T) {
	t.Parallel()

	lines := []string{`{`, `  "description": "test"`, `}`}
	result := lineDiff(lines, lines)

	if result != nil {
		t.Errorf("identical inputs should return nil, got %v", result)
	}
}

func TestLineDiffEmpty(t *testing.T) {
	t.Parallel()

	result := lineDiff(nil, nil)
	if result != nil {
		t.Errorf("two empty inputs should return nil, got %v", result)
	}
}

func TestLineDiffAddition(t *testing.T) {
	t.Parallel()

	old := []string{`{`, `  "a": 1`, `}`}
	new := []string{`{`, `  "a": 1,`, `  "b": 2`, `}`}

	result := lineDiff(old, new)
	if result == nil {
		t.Fatal("expected differences for addition")
	}

	// Should contain at least one "+" line for the new content.
	hasAddition := false
	for _, line := range result {
		if strings.HasPrefix(line, "+ ") {
			hasAddition = true
			break
		}
	}
	if !hasAddition {
		t.Errorf("expected + line in diff, got:\n%s", strings.Join(result, "\n"))
	}
}

func TestLineDiffDeletion(t *testing.T) {
	t.Parallel()

	old := []string{`{`, `  "a": 1,`, `  "b": 2`, `}`}
	new := []string{`{`, `  "a": 1`, `}`}

	result := lineDiff(old, new)
	if result == nil {
		t.Fatal("expected differences for deletion")
	}

	hasDeletion := false
	for _, line := range result {
		if strings.HasPrefix(line, "- ") {
			hasDeletion = true
			break
		}
	}
	if !hasDeletion {
		t.Errorf("expected - line in diff, got:\n%s", strings.Join(result, "\n"))
	}
}

func TestLineDiffModification(t *testing.T) {
	t.Parallel()

	old := []string{`{`, `  "description": "old value"`, `}`}
	new := []string{`{`, `  "description": "new value"`, `}`}

	result := lineDiff(old, new)
	if result == nil {
		t.Fatal("expected differences for modification")
	}

	// Should have both a deletion and an addition for the changed line.
	hasDeletion := false
	hasAddition := false
	for _, line := range result {
		if strings.HasPrefix(line, "- ") && strings.Contains(line, "old value") {
			hasDeletion = true
		}
		if strings.HasPrefix(line, "+ ") && strings.Contains(line, "new value") {
			hasAddition = true
		}
	}
	if !hasDeletion {
		t.Error("expected - line with old value")
	}
	if !hasAddition {
		t.Error("expected + line with new value")
	}
}

func TestLineDiffContextLines(t *testing.T) {
	t.Parallel()

	old := []string{`{`, `  "a": 1,`, `  "b": 2`, `}`}
	new := []string{`{`, `  "a": 1,`, `  "b": 3`, `}`}

	result := lineDiff(old, new)
	if result == nil {
		t.Fatal("expected differences")
	}

	// Unchanged lines should appear as context (prefixed with "  ").
	hasContext := false
	for _, line := range result {
		if strings.HasPrefix(line, "  ") {
			hasContext = true
			break
		}
	}
	if !hasContext {
		t.Errorf("expected context lines (prefixed with '  '), got:\n%s", strings.Join(result, "\n"))
	}
}

func TestLineDiffOldEmpty(t *testing.T) {
	t.Parallel()

	new := []string{`{`, `  "a": 1`, `}`}
	result := lineDiff(nil, new)
	if result == nil {
		t.Fatal("expected differences when old is empty")
	}

	// All lines should be additions.
	for _, line := range result {
		if !strings.HasPrefix(line, "+ ") {
			t.Errorf("expected all + lines when old is empty, got: %q", line)
		}
	}
}

func TestLineDiffNewEmpty(t *testing.T) {
	t.Parallel()

	old := []string{`{`, `  "a": 1`, `}`}
	result := lineDiff(old, nil)
	if result == nil {
		t.Fatal("expected differences when new is empty")
	}

	// All lines should be deletions.
	for _, line := range result {
		if !strings.HasPrefix(line, "- ") {
			t.Errorf("expected all - lines when new is empty, got: %q", line)
		}
	}
}
