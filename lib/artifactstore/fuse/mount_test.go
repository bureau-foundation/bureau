// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/clock"
)

// testTimestamp is a fixed timestamp for tag creation in tests.
// Using a constant avoids the check-real-clock lint rule.
var testTimestamp = time.Unix(1735689600, 0) // 2025-01-01T00:00:00Z

// fuseAvailable checks whether /dev/fuse is accessible. Tests that
// need a real FUSE mount call this and skip if the device is absent.
func fuseAvailable(t *testing.T) {
	t.Helper()
	_, err := os.Stat("/dev/fuse")
	if err != nil {
		t.Skip("skipping: /dev/fuse not available")
	}
}

// testMount creates a Store, TagStore, mounts the FUSE filesystem
// with a deterministic clock, and returns the mountpoint, store, and
// tag store. The mount is automatically unmounted when the test ends.
func testMount(t *testing.T) (mountpoint string, store *artifactstore.Store, tagStore *artifactstore.TagStore) {
	t.Helper()
	fuseAvailable(t)

	root := t.TempDir()

	var err error
	store, err = artifactstore.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	tagStore, err = artifactstore.NewTagStore(filepath.Join(root, "tags"))
	if err != nil {
		t.Fatalf("NewTagStore: %v", err)
	}

	mountpoint = filepath.Join(root, "mount")

	server, err := Mount(Options{
		Mountpoint: mountpoint,
		Store:      store,
		TagStore:   tagStore,
		Clock:      clock.Fake(testTimestamp),
	})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}

	t.Cleanup(func() {
		if err := server.Unmount(); err != nil {
			t.Errorf("Unmount: %v", err)
		}
	})

	return mountpoint, store, tagStore
}

// ---- Read path tests ----

func TestMountRootHasTagAndCas(t *testing.T) {
	mountpoint, _, _ := testMount(t)

	entries, err := os.ReadDir(mountpoint)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	names := make(map[string]bool)
	for _, entry := range entries {
		names[entry.Name()] = true
	}

	if !names["tag"] {
		t.Error("missing 'tag' directory")
	}
	if !names["cas"] {
		t.Error("missing 'cas' directory")
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(entries))
	}
}

func TestMountTagReadSmallArtifact(t *testing.T) {
	mountpoint, store, tagStore := testMount(t)

	content := []byte("hello from the FUSE mount")
	result, err := store.WriteContent(content, "text/plain")
	if err != nil {
		t.Fatalf("WriteContent: %v", err)
	}

	err = tagStore.Set("test/greeting", result.FileHash, nil, true, testTimestamp)
	if err != nil {
		t.Fatalf("Set tag: %v", err)
	}

	// Read through the FUSE mount.
	got, err := os.ReadFile(filepath.Join(mountpoint, "tag", "test", "greeting"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if !bytes.Equal(got, content) {
		t.Errorf("got %q, want %q", string(got), string(content))
	}
}

func TestMountTagReadLargeArtifact(t *testing.T) {
	mountpoint, store, tagStore := testMount(t)

	// 512 KiB to ensure multi-chunk.
	content := make([]byte, 512*1024)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	result, err := store.WriteContent(content, "application/octet-stream")
	if err != nil {
		t.Fatalf("WriteContent: %v", err)
	}

	err = tagStore.Set("data/random", result.FileHash, nil, true, testTimestamp)
	if err != nil {
		t.Fatalf("Set tag: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(mountpoint, "tag", "data", "random"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if !bytes.Equal(got, content) {
		t.Error("large artifact content mismatch through FUSE")
	}
}

func TestMountCasLookup(t *testing.T) {
	mountpoint, store, _ := testMount(t)

	content := []byte("CAS direct access")
	result, err := store.WriteContent(content, "text/plain")
	if err != nil {
		t.Fatalf("WriteContent: %v", err)
	}

	hashHex := artifactstore.FormatHash(result.FileHash)
	got, err := os.ReadFile(filepath.Join(mountpoint, "cas", hashHex))
	if err != nil {
		t.Fatalf("ReadFile via CAS: %v", err)
	}

	if !bytes.Equal(got, content) {
		t.Errorf("CAS read: got %q, want %q", string(got), string(content))
	}
}

func TestMountCasNotFound(t *testing.T) {
	mountpoint, _, _ := testMount(t)

	// Full-length hex hash that doesn't exist.
	fakeHash := "0000000000000000000000000000000000000000000000000000000000000000"
	_, err := os.ReadFile(filepath.Join(mountpoint, "cas", fakeHash))
	if err == nil {
		t.Fatal("expected error reading nonexistent CAS entry")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected ENOENT, got: %v", err)
	}
}

func TestMountTagNotFound(t *testing.T) {
	mountpoint, _, _ := testMount(t)

	_, err := os.ReadFile(filepath.Join(mountpoint, "tag", "nonexistent"))
	if err == nil {
		t.Fatal("expected error reading nonexistent tag")
	}
}

func TestMountTagDirListing(t *testing.T) {
	mountpoint, store, tagStore := testMount(t)

	content := []byte("test")
	result, err := store.WriteContent(content, "text/plain")
	if err != nil {
		t.Fatalf("WriteContent: %v", err)
	}

	for _, tag := range []string{
		"project/alpha/v1",
		"project/alpha/v2",
		"project/beta/latest",
	} {
		if err := tagStore.Set(tag, result.FileHash, nil, true, testTimestamp); err != nil {
			t.Fatalf("Set tag %s: %v", tag, err)
		}
	}

	// List /tag/project/ — should show "alpha" and "beta".
	entries, err := os.ReadDir(filepath.Join(mountpoint, "tag", "project"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	names := make(map[string]bool)
	for _, entry := range entries {
		names[entry.Name()] = true
	}

	if !names["alpha"] {
		t.Error("missing 'alpha' directory")
	}
	if !names["beta"] {
		t.Error("missing 'beta' directory")
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d: %v", len(entries), names)
	}
}

func TestMountTagPartialRead(t *testing.T) {
	mountpoint, store, tagStore := testMount(t)

	content := []byte("0123456789abcdef")
	result, err := store.WriteContent(content, "text/plain")
	if err != nil {
		t.Fatalf("WriteContent: %v", err)
	}

	err = tagStore.Set("partial", result.FileHash, nil, true, testTimestamp)
	if err != nil {
		t.Fatalf("Set tag: %v", err)
	}

	path := filepath.Join(mountpoint, "tag", "partial")

	// Stat to check size.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != int64(len(content)) {
		t.Errorf("size = %d, want %d", info.Size(), len(content))
	}

	// Partial read using file seek.
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer file.Close()

	buffer := make([]byte, 4)
	if _, err := file.ReadAt(buffer, 5); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buffer) != "5678" {
		t.Errorf("partial read: got %q, want %q", string(buffer), "5678")
	}
}

func TestMountRootWriteRejected(t *testing.T) {
	mountpoint, _, _ := testMount(t)

	// Root directory has no Create — writing here should fail.
	err := os.WriteFile(filepath.Join(mountpoint, "should-fail"), []byte("x"), 0o644)
	if err == nil {
		t.Fatal("expected error writing to root directory")
	}
}

func TestMountMultipleArtifactsDistinctContent(t *testing.T) {
	mountpoint, store, tagStore := testMount(t)

	artifacts := map[string][]byte{
		"file/alpha": []byte("alpha content"),
		"file/beta":  []byte("beta content — longer"),
		"file/gamma": []byte("g"),
	}

	for tag, content := range artifacts {
		result, err := store.WriteContent(content, "text/plain")
		if err != nil {
			t.Fatalf("WriteContent for %s: %v", tag, err)
		}
		if err := tagStore.Set(tag, result.FileHash, nil, true, testTimestamp); err != nil {
			t.Fatalf("Set tag %s: %v", tag, err)
		}
	}

	for tag, expected := range artifacts {
		got, err := os.ReadFile(filepath.Join(mountpoint, "tag", tag))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", tag, err)
		}
		if !bytes.Equal(got, expected) {
			t.Errorf("%s: got %q, want %q", tag, string(got), string(expected))
		}
	}
}

// ---- Write path tests ----

func TestMountTagWriteNewTag(t *testing.T) {
	mountpoint, store, tagStore := testMount(t)

	// Write a file through the FUSE mount, creating a new tag.
	content := []byte("written through FUSE")
	tagPath := filepath.Join(mountpoint, "tag", "fuse-created")

	if err := os.WriteFile(tagPath, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Verify the tag was created in the TagStore.
	record, exists := tagStore.Get("fuse-created")
	if !exists {
		t.Fatal("tag 'fuse-created' not found in TagStore after write")
	}

	// Verify the content is readable through the Store.
	readBack, err := store.ReadContent(record.Target)
	if err != nil {
		t.Fatalf("Store.Read: %v", err)
	}
	if !bytes.Equal(readBack, content) {
		t.Errorf("store content: got %q, want %q", string(readBack), string(content))
	}

	// Verify the content is readable back through FUSE (via CAS
	// since the tag inode might be cached from the write).
	hashHex := artifactstore.FormatHash(record.Target)
	got, err := os.ReadFile(filepath.Join(mountpoint, "cas", hashHex))
	if err != nil {
		t.Fatalf("ReadFile via CAS: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("CAS read-back: got %q, want %q", string(got), string(content))
	}
}

func TestMountTagWriteHierarchical(t *testing.T) {
	mountpoint, store, tagStore := testMount(t)

	// For the kernel to traverse "project/build/" before calling
	// Create("latest"), the intermediate directories must already
	// exist as virtual directories (which requires existing tags
	// with those prefixes). Seed a sibling tag so the parent path
	// is resolvable.
	seedContent := []byte("seed content for sibling")
	seedResult, err := store.WriteContent(seedContent, "text/plain")
	if err != nil {
		t.Fatalf("WriteContent (seed): %v", err)
	}
	if err := tagStore.Set("project/build/sibling", seedResult.FileHash, nil, true, testTimestamp); err != nil {
		t.Fatalf("Set seed tag: %v", err)
	}

	// Now write to a new leaf under the existing directory.
	content := []byte("deep tag content written through FUSE")
	tagPath := filepath.Join(mountpoint, "tag", "project", "build", "latest")

	if err := os.WriteFile(tagPath, content, 0o644); err != nil {
		t.Fatalf("WriteFile (hierarchical): %v", err)
	}

	record, exists := tagStore.Get("project/build/latest")
	if !exists {
		t.Fatal("hierarchical tag not found after write")
	}

	readBack, err := store.ReadContent(record.Target)
	if err != nil {
		t.Fatalf("ReadContent: %v", err)
	}
	if !bytes.Equal(readBack, content) {
		t.Errorf("hierarchical write content: got %q, want %q", string(readBack), string(content))
	}
}

func TestMountTagWriteOverwrite(t *testing.T) {
	mountpoint, store, tagStore := testMount(t)

	// Store initial content and create a tag.
	initialContent := []byte("initial version")
	initialResult, err := store.WriteContent(initialContent, "text/plain")
	if err != nil {
		t.Fatalf("WriteContent: %v", err)
	}
	if err := tagStore.Set("versioned", initialResult.FileHash, nil, true, testTimestamp); err != nil {
		t.Fatalf("Set initial tag: %v", err)
	}

	// Overwrite the tag through FUSE.
	updatedContent := []byte("updated version with more content")
	tagPath := filepath.Join(mountpoint, "tag", "versioned")

	if err := os.WriteFile(tagPath, updatedContent, 0o644); err != nil {
		t.Fatalf("WriteFile (overwrite): %v", err)
	}

	// Verify the tag now points to the new content.
	record, exists := tagStore.Get("versioned")
	if !exists {
		t.Fatal("tag disappeared after overwrite")
	}
	if record.Target == initialResult.FileHash {
		t.Error("tag still points to initial content after overwrite")
	}

	// Verify the new content through the Store.
	readBack, err := store.ReadContent(record.Target)
	if err != nil {
		t.Fatalf("Store.Read (new): %v", err)
	}
	if !bytes.Equal(readBack, updatedContent) {
		t.Errorf("overwritten content: got %q, want %q", string(readBack), string(updatedContent))
	}

	// Verify the old content is still in the Store (CAS never
	// deletes — data is never lost).
	oldContent, err := store.ReadContent(initialResult.FileHash)
	if err != nil {
		t.Fatalf("Store.Read (old): %v", err)
	}
	if !bytes.Equal(oldContent, initialContent) {
		t.Error("old content was corrupted")
	}
}

func TestMountTagWriteConflict(t *testing.T) {
	mountpoint, store, tagStore := testMount(t)

	// Store initial content and create a tag.
	contentA := []byte("version A")
	resultA, err := store.WriteContent(contentA, "text/plain")
	if err != nil {
		t.Fatalf("WriteContent A: %v", err)
	}
	if err := tagStore.Set("conflicted", resultA.FileHash, nil, true, testTimestamp); err != nil {
		t.Fatalf("Set tag to A: %v", err)
	}

	// Open the tag file for writing. The Create call captures A's
	// hash as the expected previous target for CAS.
	tagPath := filepath.Join(mountpoint, "tag", "conflicted")
	file, err := os.OpenFile(tagPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// While the file is open, update the tag to point to B via the
	// TagStore directly (simulating a concurrent writer).
	contentB := []byte("version B from concurrent writer")
	resultB, err := store.WriteContent(contentB, "text/plain")
	if err != nil {
		t.Fatalf("WriteContent B: %v", err)
	}
	if err := tagStore.Set("conflicted", resultB.FileHash, &resultA.FileHash, false, testTimestamp); err != nil {
		t.Fatalf("Set tag to B: %v", err)
	}

	// Write content C and close. The Flush should detect that the
	// tag no longer points to A (it now points to B) and fail the
	// CAS check.
	contentC := []byte("version C from FUSE writer")
	if _, err := file.Write(contentC); err != nil {
		t.Fatalf("Write: %v", err)
	}

	err = file.Close()
	if err == nil {
		t.Fatal("expected error on close due to CAS conflict")
	}

	// Verify the tag still points to B (the FUSE write did not
	// overwrite the concurrent update).
	record, exists := tagStore.Get("conflicted")
	if !exists {
		t.Fatal("tag disappeared after conflict")
	}
	if record.Target != resultB.FileHash {
		t.Errorf("tag target changed despite CAS conflict: got %s, want %s",
			artifactstore.FormatHash(record.Target), artifactstore.FormatHash(resultB.FileHash))
	}
}

func TestMountCasWriteNewArtifact(t *testing.T) {
	mountpoint, store, _ := testMount(t)

	// Compute the hash without storing the content. For small
	// artifacts (≤256KB), the file hash is HashFile(HashChunk(data)).
	content := []byte("CAS write test content — not pre-stored")
	chunkHash := artifactstore.HashChunk(content)
	fileHash := artifactstore.HashFile(chunkHash)
	hashHex := artifactstore.FormatHash(fileHash)

	// Write through the CAS FUSE path. Since the content doesn't
	// exist in the store, Lookup returns ENOENT and the kernel calls
	// Create on casNode with the hash as the filename.
	casPath := filepath.Join(mountpoint, "cas", hashHex)

	if err := os.WriteFile(casPath, content, 0o644); err != nil {
		t.Fatalf("WriteFile to CAS: %v", err)
	}

	// Verify the content was stored correctly.
	readBack, err := store.ReadContent(fileHash)
	if err != nil {
		t.Fatalf("Store.ReadContent: %v", err)
	}
	if !bytes.Equal(readBack, content) {
		t.Errorf("CAS store content: got %q, want %q", string(readBack), string(content))
	}
}

func TestMountCasWriteHashMismatch(t *testing.T) {
	mountpoint, _, _ := testMount(t)

	// Use a fabricated hash that won't match the content we write.
	fakeHash := "0000000000000000000000000000000000000000000000000000000000000001"
	casPath := filepath.Join(mountpoint, "cas", fakeHash)

	content := []byte("this content does not hash to the fake hash")
	err := os.WriteFile(casPath, content, 0o644)
	if err == nil {
		t.Fatal("expected error writing to CAS with wrong hash")
	}
}

func TestMountCasWriteInvalidHashName(t *testing.T) {
	mountpoint, _, _ := testMount(t)

	// A filename that's not a valid hex hash should be rejected.
	casPath := filepath.Join(mountpoint, "cas", "not-a-hash")
	err := os.WriteFile(casPath, []byte("x"), 0o644)
	if err == nil {
		t.Fatal("expected error writing to CAS with invalid hash name")
	}
}

func TestMountTagWriteLargeArtifact(t *testing.T) {
	mountpoint, store, tagStore := testMount(t)

	// 512 KiB to exercise the CDC chunking path during write.
	content := make([]byte, 512*1024)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	tagPath := filepath.Join(mountpoint, "tag", "large-write")
	if err := os.WriteFile(tagPath, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Verify tag exists.
	record, exists := tagStore.Get("large-write")
	if !exists {
		t.Fatal("tag not found after large write")
	}

	// Verify content through the Store.
	readBack, err := store.ReadContent(record.Target)
	if err != nil {
		t.Fatalf("Store.Read: %v", err)
	}
	if !bytes.Equal(readBack, content) {
		t.Error("large write content mismatch")
	}
}

func TestMountTagWriteReadRoundtrip(t *testing.T) {
	mountpoint, _, tagStore := testMount(t)

	// Write through FUSE, then verify readable through FUSE CAS.
	// Reading back via the tag path may hit the cached
	// writeInProgressNode (which doesn't implement Read), so we
	// verify via the CAS path which always creates a fresh
	// artifactFileNode.
	content := []byte("roundtrip content through FUSE mount")
	tagPath := filepath.Join(mountpoint, "tag", "roundtrip")

	if err := os.WriteFile(tagPath, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	record, exists := tagStore.Get("roundtrip")
	if !exists {
		t.Fatal("tag not found after write")
	}

	hashHex := artifactstore.FormatHash(record.Target)
	got, err := os.ReadFile(filepath.Join(mountpoint, "cas", hashHex))
	if err != nil {
		t.Fatalf("ReadFile via CAS: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("roundtrip: got %q, want %q", string(got), string(content))
	}
}
