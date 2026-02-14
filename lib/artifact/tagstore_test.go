// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"sort"
	"testing"
	"time"
)

func TestTagStoreSetAndGet(t *testing.T) {
	store, err := NewTagStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	target := HashChunk([]byte("artifact content"))
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	if err := store.Set("model/latest", target, nil, false, now); err != nil {
		t.Fatal(err)
	}

	record, exists := store.Get("model/latest")
	if !exists {
		t.Fatal("tag not found after set")
	}
	if record.Target != target {
		t.Errorf("target = %s, want %s", FormatHash(record.Target), FormatHash(target))
	}
	if record.Name != "model/latest" {
		t.Errorf("name = %q, want %q", record.Name, "model/latest")
	}
	if !record.CreatedAt.Equal(now) {
		t.Errorf("created_at = %v, want %v", record.CreatedAt, now)
	}
	if !record.UpdatedAt.Equal(now) {
		t.Errorf("updated_at = %v, want %v", record.UpdatedAt, now)
	}
}

func TestTagStoreSetOptimistic(t *testing.T) {
	store, err := NewTagStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	first := HashChunk([]byte("first"))
	second := HashChunk([]byte("second"))
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)

	if err := store.Set("my/tag", first, nil, true, now); err != nil {
		t.Fatal(err)
	}

	// Optimistic write overwrites regardless of expected previous.
	if err := store.Set("my/tag", second, nil, true, later); err != nil {
		t.Fatal(err)
	}

	record, exists := store.Get("my/tag")
	if !exists {
		t.Fatal("tag not found")
	}
	if record.Target != second {
		t.Error("optimistic write did not update target")
	}
	if !record.CreatedAt.Equal(now) {
		t.Error("optimistic write changed created_at")
	}
	if !record.UpdatedAt.Equal(later) {
		t.Error("optimistic write did not update updated_at")
	}
}

func TestTagStoreSetCompareAndSwap(t *testing.T) {
	store, err := NewTagStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	first := HashChunk([]byte("first"))
	second := HashChunk([]byte("second"))
	wrong := HashChunk([]byte("wrong"))
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)

	if err := store.Set("cas/tag", first, nil, false, now); err != nil {
		t.Fatal(err)
	}

	// CAS with wrong expected previous should fail.
	err = store.Set("cas/tag", second, &wrong, false, later)
	if err == nil {
		t.Fatal("expected CAS conflict error")
	}

	// Verify the tag was not updated.
	record, _ := store.Get("cas/tag")
	if record.Target != first {
		t.Error("CAS conflict should not have modified the tag")
	}

	// CAS with correct expected previous should succeed.
	if err := store.Set("cas/tag", second, &first, false, later); err != nil {
		t.Fatalf("CAS with correct previous failed: %v", err)
	}

	record, _ = store.Get("cas/tag")
	if record.Target != second {
		t.Error("CAS with correct previous did not update target")
	}
}

func TestTagStoreDelete(t *testing.T) {
	store, err := NewTagStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	target := HashChunk([]byte("content"))
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	if err := store.Set("ephemeral", target, nil, true, now); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete("ephemeral"); err != nil {
		t.Fatal(err)
	}

	if _, exists := store.Get("ephemeral"); exists {
		t.Error("tag still exists after delete")
	}

	if store.Len() != 0 {
		t.Errorf("Len() = %d, want 0", store.Len())
	}
}

func TestTagStoreDeleteNotFound(t *testing.T) {
	store, err := NewTagStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	err = store.Delete("nonexistent")
	if err == nil {
		t.Fatal("expected error when deleting nonexistent tag")
	}
}

func TestTagStoreList(t *testing.T) {
	store, err := NewTagStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{
		"model/production/latest",
		"model/production/v2",
		"model/staging/latest",
		"data/dataset-a",
		"data/dataset-b",
	} {
		target := HashChunk([]byte(name))
		if err := store.Set(name, target, nil, true, now); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		prefix string
		want   int
	}{
		{"", 5},                  // all tags
		{"model/", 3},            // all model tags
		{"model/production/", 2}, // production model tags
		{"model/staging/", 1},    // staging model tags
		{"data/", 2},             // all data tags
		{"nonexistent/", 0},      // no matches
	}

	for _, test := range tests {
		results := store.List(test.prefix)
		if len(results) != test.want {
			t.Errorf("List(%q) returned %d results, want %d", test.prefix, len(results), test.want)
		}
	}
}

func TestTagStoreNames(t *testing.T) {
	store, err := NewTagStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	target1 := HashChunk([]byte("one"))
	target2 := HashChunk([]byte("two"))
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	store.Set("tag/one", target1, nil, true, now)
	store.Set("tag/two", target2, nil, true, now)

	names := store.Names()
	if len(names) != 2 {
		t.Fatalf("Names() returned %d entries, want 2", len(names))
	}
	if names[target1] != "tag/one" {
		t.Errorf("Names()[target1] = %q, want %q", names[target1], "tag/one")
	}
	if names[target2] != "tag/two" {
		t.Errorf("Names()[target2] = %q, want %q", names[target2], "tag/two")
	}
}

func TestTagStorePersistence(t *testing.T) {
	dir := t.TempDir()

	// Create a store and write some tags.
	store1, err := NewTagStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	target := HashChunk([]byte("persistent"))
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	if err := store1.Set("persist/this", target, nil, true, now); err != nil {
		t.Fatal(err)
	}
	if err := store1.Set("persist/that", HashChunk([]byte("other")), nil, true, now); err != nil {
		t.Fatal(err)
	}

	// Create a new store pointing at the same directory.
	store2, err := NewTagStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	if store2.Len() != 2 {
		t.Fatalf("rebuilt store has %d tags, want 2", store2.Len())
	}

	record, exists := store2.Get("persist/this")
	if !exists {
		t.Fatal("tag not found in rebuilt store")
	}
	if record.Target != target {
		t.Error("tag target does not match in rebuilt store")
	}
}

func TestTagStoreEmptyName(t *testing.T) {
	store, err := NewTagStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	err = store.Set("", HashChunk([]byte("x")), nil, true, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected error for empty tag name")
	}
}

func TestTagStoreNameTooLong(t *testing.T) {
	store, err := NewTagStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	longName := string(make([]byte, MaxTagNameLength+1))
	err = store.Set(longName, HashChunk([]byte("x")), nil, true, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected error for name exceeding maximum length")
	}
}

func TestTagStoreHierarchicalNames(t *testing.T) {
	store, err := NewTagStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	// Names with various special characters that would be
	// problematic as direct filesystem paths.
	names := []string{
		"a/b/c/d/e/f",
		"pipeline/build/2026-02-13/artifact",
		"model/gpt-4/weights/v3.2",
		"shared/datasets/imagenet-subset",
	}

	for _, name := range names {
		target := HashChunk([]byte(name))
		if err := store.Set(name, target, nil, true, now); err != nil {
			t.Fatalf("Set(%q) failed: %v", name, err)
		}
	}

	if store.Len() != len(names) {
		t.Fatalf("Len() = %d, want %d", store.Len(), len(names))
	}

	for _, name := range names {
		record, exists := store.Get(name)
		if !exists {
			t.Errorf("tag %q not found", name)
			continue
		}
		if record.Name != name {
			t.Errorf("tag name = %q, want %q", record.Name, name)
		}
	}
}

func TestTagStoreListSorted(t *testing.T) {
	store, err := NewTagStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	names := []string{"c", "a", "b"}
	for _, name := range names {
		store.Set(name, HashChunk([]byte(name)), nil, true, now)
	}

	results := store.List("")
	resultNames := make([]string, len(results))
	for i, r := range results {
		resultNames[i] = r.Name
	}
	sort.Strings(resultNames)

	expected := []string{"a", "b", "c"}
	for i, name := range expected {
		if resultNames[i] != name {
			t.Errorf("sorted result[%d] = %q, want %q", i, resultNames[i], name)
		}
	}
}
