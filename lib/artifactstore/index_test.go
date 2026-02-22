// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifactstore

import (
	"testing"
	"time"
)

func testMeta(content string, contentType string, labels []string, size int64, storedAt time.Time) ArtifactMetadata {
	chunkHash := HashChunk([]byte(content))
	fileHash := HashFile(chunkHash)
	return ArtifactMetadata{
		FileHash:    fileHash,
		Ref:         FormatRef(fileHash),
		ContentType: contentType,
		Labels:      labels,
		Size:        size,
		StoredAt:    storedAt,
	}
}

func TestArtifactIndexBuildAndLen(t *testing.T) {
	idx := NewArtifactIndex()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	records := []ArtifactMetadata{
		testMeta("a", "text/plain", nil, 100, now),
		testMeta("b", "text/plain", nil, 200, now),
		testMeta("c", "image/png", nil, 300, now),
	}

	idx.Build(records)

	if idx.Len() != 3 {
		t.Errorf("Len() = %d, want 3", idx.Len())
	}
}

func TestArtifactIndexPutAndGet(t *testing.T) {
	idx := NewArtifactIndex()

	meta := testMeta("content", "text/plain", []string{"docs"}, 500, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	idx.Put(meta)

	got, exists := idx.Get(meta.FileHash)
	if !exists {
		t.Fatal("artifact not found after Put")
	}
	if got.ContentType != "text/plain" {
		t.Errorf("content_type = %q, want 'text/plain'", got.ContentType)
	}
}

func TestArtifactIndexRemove(t *testing.T) {
	idx := NewArtifactIndex()

	meta := testMeta("removable", "text/plain", []string{"temp"}, 100, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	idx.Put(meta)
	idx.Remove(meta.FileHash)

	if idx.Len() != 0 {
		t.Errorf("Len() = %d after remove, want 0", idx.Len())
	}

	// Verify secondary indexes are cleaned up.
	results, _ := idx.List(ArtifactFilter{ContentType: "text/plain"})
	if len(results) != 0 {
		t.Error("content type index not cleaned up after remove")
	}
	results, _ = idx.List(ArtifactFilter{Label: "temp"})
	if len(results) != 0 {
		t.Error("label index not cleaned up after remove")
	}
}

func TestArtifactIndexListByContentType(t *testing.T) {
	idx := NewArtifactIndex()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	idx.Put(testMeta("a", "text/plain", nil, 100, now))
	idx.Put(testMeta("b", "text/plain", nil, 200, now))
	idx.Put(testMeta("c", "image/png", nil, 300, now))

	results, total := idx.List(ArtifactFilter{ContentType: "text/plain"})
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2", len(results))
	}
}

func TestArtifactIndexListByLabel(t *testing.T) {
	idx := NewArtifactIndex()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	idx.Put(testMeta("a", "text/plain", []string{"docs", "important"}, 100, now))
	idx.Put(testMeta("b", "text/plain", []string{"docs"}, 200, now))
	idx.Put(testMeta("c", "text/plain", []string{"code"}, 300, now))

	results, total := idx.List(ArtifactFilter{Label: "docs"})
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2", len(results))
	}

	results, total = idx.List(ArtifactFilter{Label: "important"})
	if total != 1 {
		t.Errorf("total for 'important' = %d, want 1", total)
	}
}

func TestArtifactIndexListMultipleFilters(t *testing.T) {
	idx := NewArtifactIndex()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	idx.Put(testMeta("a", "text/plain", []string{"docs"}, 100, now))
	idx.Put(testMeta("b", "image/png", []string{"docs"}, 200, now))
	idx.Put(testMeta("c", "text/plain", []string{"code"}, 300, now))

	// content_type=text/plain AND label=docs → only "a"
	results, total := idx.List(ArtifactFilter{
		ContentType: "text/plain",
		Label:       "docs",
	})
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Metadata.ContentType != "text/plain" {
		t.Error("wrong content type in result")
	}
}

func TestArtifactIndexListSizeRange(t *testing.T) {
	idx := NewArtifactIndex()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	idx.Put(testMeta("small", "text/plain", nil, 100, now))
	idx.Put(testMeta("medium", "text/plain", nil, 500, now))
	idx.Put(testMeta("large", "text/plain", nil, 1000, now))

	// Min size 200.
	results, total := idx.List(ArtifactFilter{MinSize: 200})
	if total != 2 {
		t.Errorf("min 200: total = %d, want 2", total)
	}

	// Max size 500.
	results, total = idx.List(ArtifactFilter{MaxSize: 500})
	if total != 2 {
		t.Errorf("max 500: total = %d, want 2", total)
	}
	_ = results

	// Min and max.
	results, total = idx.List(ArtifactFilter{MinSize: 200, MaxSize: 800})
	if total != 1 {
		t.Errorf("200-800: total = %d, want 1", total)
	}
}

func TestArtifactIndexListEmptyFilter(t *testing.T) {
	idx := NewArtifactIndex()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		idx.Put(testMeta(string(rune('a'+i)), "text/plain", nil, 100, now))
	}

	results, total := idx.List(ArtifactFilter{})
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(results) != 5 {
		t.Errorf("results = %d, want 5", len(results))
	}
}

func TestArtifactIndexListPagination(t *testing.T) {
	idx := NewArtifactIndex()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		meta := testMeta(string(rune('a'+i)), "text/plain", nil, 100, base.Add(time.Duration(i)*time.Hour))
		idx.Put(meta)
	}

	// Page 1: first 3 (newest).
	results, total := idx.List(ArtifactFilter{Limit: 3})
	if total != 10 {
		t.Errorf("total = %d, want 10", total)
	}
	if len(results) != 3 {
		t.Errorf("page 1 results = %d, want 3", len(results))
	}

	// Verify newest-first ordering.
	for i := 1; i < len(results); i++ {
		if results[i].Metadata.StoredAt.After(results[i-1].Metadata.StoredAt) {
			t.Error("results not sorted by StoredAt descending")
		}
	}

	// Page 2: offset 3, limit 3.
	results2, _ := idx.List(ArtifactFilter{Limit: 3, Offset: 3})
	if len(results2) != 3 {
		t.Errorf("page 2 results = %d, want 3", len(results2))
	}

	// Offset beyond results.
	results3, total := idx.List(ArtifactFilter{Limit: 3, Offset: 100})
	if len(results3) != 0 {
		t.Errorf("out of range results = %d, want 0", len(results3))
	}
	if total != 10 {
		t.Errorf("total still 10 even with large offset, got %d", total)
	}
}

func TestArtifactIndexListNoMatch(t *testing.T) {
	idx := NewArtifactIndex()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	idx.Put(testMeta("a", "text/plain", nil, 100, now))

	results, total := idx.List(ArtifactFilter{ContentType: "application/pdf"})
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

func TestArtifactIndexGrep(t *testing.T) {
	idx := NewArtifactIndex()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	meta1 := testMeta("a", "text/plain", nil, 100, now)
	meta1.Description = "training log for resnet50"
	meta1.Filename = "train.log"
	idx.Put(meta1)

	meta2 := testMeta("b", "text/plain", nil, 200, now)
	meta2.Description = "validation results"
	meta2.Filename = "val.csv"
	idx.Put(meta2)

	meta3 := testMeta("c", "image/png", nil, 300, now)
	meta3.Description = "resnet50 architecture diagram"
	idx.Put(meta3)

	results, err := idx.Grep("resnet50")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Errorf("grep 'resnet50' = %d results, want 2", len(results))
	}

	results, err = idx.Grep("train")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Errorf("grep 'train' = %d results, want 1", len(results))
	}
}

func TestArtifactIndexStats(t *testing.T) {
	idx := NewArtifactIndex()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	meta1 := testMeta("a", "text/plain", nil, 100, now)
	meta1.CachePolicy = "pin"
	idx.Put(meta1)

	idx.Put(testMeta("b", "text/plain", nil, 200, now))
	idx.Put(testMeta("c", "image/png", nil, 300, now))

	stats := idx.Stats()
	if stats.Total != 3 {
		t.Errorf("total = %d, want 3", stats.Total)
	}
	if stats.TotalSize != 600 {
		t.Errorf("total_size = %d, want 600", stats.TotalSize)
	}
	if stats.ByContentType["text/plain"] != 2 {
		t.Errorf("text/plain count = %d, want 2", stats.ByContentType["text/plain"])
	}
	if stats.ByContentType["image/png"] != 1 {
		t.Errorf("image/png count = %d, want 1", stats.ByContentType["image/png"])
	}
	if stats.ByCachePolicy["pin"] != 1 {
		t.Errorf("pin count = %d, want 1", stats.ByCachePolicy["pin"])
	}
}

func TestArtifactIndexPutUpdate(t *testing.T) {
	idx := NewArtifactIndex()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	meta := testMeta("a", "text/plain", []string{"old"}, 100, now)
	idx.Put(meta)

	// Update with new label.
	meta.Labels = []string{"new"}
	meta.ContentType = "application/json"
	idx.Put(meta)

	if idx.Len() != 1 {
		t.Errorf("Len() = %d, want 1 (update should not add duplicate)", idx.Len())
	}

	// Old label index should be cleaned up.
	results, _ := idx.List(ArtifactFilter{Label: "old"})
	if len(results) != 0 {
		t.Error("old label index not cleaned up after update")
	}

	// New label should work.
	results, _ = idx.List(ArtifactFilter{Label: "new"})
	if len(results) != 1 {
		t.Error("new label not indexed after update")
	}

	// Old content type should be cleaned up.
	results, _ = idx.List(ArtifactFilter{ContentType: "text/plain"})
	if len(results) != 0 {
		t.Error("old content type index not cleaned up after update")
	}
}
