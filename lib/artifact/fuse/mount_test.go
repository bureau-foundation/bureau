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

	"github.com/bureau-foundation/bureau/lib/artifact"
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

// testMount creates a Store, TagStore, stores content, mounts the
// FUSE filesystem, and returns the mountpoint and a cleanup function.
func testMount(t *testing.T) (mountpoint string, store *artifact.Store, tagStore *artifact.TagStore) {
	t.Helper()
	fuseAvailable(t)

	root := t.TempDir()

	var err error
	store, err = artifact.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	tagStore, err = artifact.NewTagStore(filepath.Join(root, "tags"))
	if err != nil {
		t.Fatalf("NewTagStore: %v", err)
	}

	mountpoint = filepath.Join(root, "mount")

	server, err := Mount(Options{
		Mountpoint: mountpoint,
		Store:      store,
		TagStore:   tagStore,
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

	hashHex := artifact.FormatHash(result.FileHash)
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

	buf := make([]byte, 4)
	if _, err := file.ReadAt(buf, 5); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != "5678" {
		t.Errorf("partial read: got %q, want %q", string(buf), "5678")
	}
}

func TestMountReadOnly(t *testing.T) {
	mountpoint, _, _ := testMount(t)

	// Attempt to create a file — should fail with EROFS.
	err := os.WriteFile(filepath.Join(mountpoint, "tag", "should-fail"), []byte("x"), 0o644)
	if err == nil {
		t.Fatal("expected error writing to read-only mount")
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
