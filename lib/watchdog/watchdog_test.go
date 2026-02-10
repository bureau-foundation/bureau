// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package watchdog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchdog.json")
	state := State{
		Component:      "daemon",
		PreviousBinary: "/nix/store/old-bureau-daemon/bin/bureau-daemon",
		NewBinary:      "/nix/store/new-bureau-daemon/bin/bureau-daemon",
		Timestamp:      time.Date(2026, 2, 10, 15, 30, 0, 0, time.UTC),
	}

	if err := Write(path, state); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if got.Component != state.Component {
		t.Errorf("Component = %q, want %q", got.Component, state.Component)
	}
	if got.PreviousBinary != state.PreviousBinary {
		t.Errorf("PreviousBinary = %q, want %q", got.PreviousBinary, state.PreviousBinary)
	}
	if got.NewBinary != state.NewBinary {
		t.Errorf("NewBinary = %q, want %q", got.NewBinary, state.NewBinary)
	}
	if !got.Timestamp.Equal(state.Timestamp) {
		t.Errorf("Timestamp = %v, want %v", got.Timestamp, state.Timestamp)
	}
}

func TestWriteOverwritesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchdog.json")

	first := State{
		Component:      "daemon",
		PreviousBinary: "/old/v1",
		NewBinary:      "/new/v2",
		Timestamp:      time.Now(),
	}
	if err := Write(path, first); err != nil {
		t.Fatalf("Write first: %v", err)
	}

	second := State{
		Component:      "launcher",
		PreviousBinary: "/old/v2",
		NewBinary:      "/new/v3",
		Timestamp:      time.Now().Add(time.Minute),
	}
	if err := Write(path, second); err != nil {
		t.Fatalf("Write second: %v", err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Component != "launcher" {
		t.Errorf("Component = %q, want %q (second write should overwrite)", got.Component, "launcher")
	}
	if got.PreviousBinary != "/old/v2" {
		t.Errorf("PreviousBinary = %q, want %q", got.PreviousBinary, "/old/v2")
	}
}

func TestWriteFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchdog.json")
	state := State{
		Component: "daemon",
		Timestamp: time.Now(),
	}

	if err := Write(path, state); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	// Mask out file type bits, check only permission bits.
	permissions := info.Mode().Perm()
	if permissions != 0600 {
		t.Errorf("permissions = %04o, want 0600", permissions)
	}
}

func TestWriteNoTemporaryFileLeftBehind(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "watchdog.json")
	state := State{
		Component: "daemon",
		Timestamp: time.Now(),
	}

	if err := Write(path, state); err != nil {
		t.Fatalf("Write: %v", err)
	}

	temporaryPath := path + ".tmp"
	if _, err := os.Stat(temporaryPath); !os.IsNotExist(err) {
		t.Errorf("temporary file %s still exists after successful Write", temporaryPath)
	}
}

func TestWriteParentDirectoryMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent", "subdir", "watchdog.json")
	state := State{
		Component: "daemon",
		Timestamp: time.Now(),
	}

	err := Write(path, state)
	if err == nil {
		t.Fatal("Write to nonexistent parent directory should fail")
	}
}

func TestReadNonexistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")

	_, err := Read(path)
	if err == nil {
		t.Fatal("Read nonexistent file should return an error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error should wrap os.ErrNotExist, got: %v", err)
	}
}

func TestReadCorruptJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchdog.json")
	if err := os.WriteFile(path, []byte("not valid json{{{"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Read(path)
	if err == nil {
		t.Fatal("Read corrupt JSON should return an error")
	}
	// The error should mention the file path for diagnostics.
	if got := err.Error(); !contains(got, path) {
		t.Errorf("error %q should mention file path %q", got, path)
	}
}

func TestCheckRecent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchdog.json")
	state := State{
		Component:      "daemon",
		PreviousBinary: "/old",
		NewBinary:      "/new",
		Timestamp:      time.Now(),
	}

	if err := Write(path, state); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, found, err := Check(path, time.Minute)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !found {
		t.Fatal("Check should return found=true for a recent watchdog file")
	}
	if got.Component != "daemon" {
		t.Errorf("Component = %q, want %q", got.Component, "daemon")
	}
}

func TestCheckStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchdog.json")
	state := State{
		Component: "daemon",
		Timestamp: time.Now().Add(-10 * time.Minute),
	}

	if err := Write(path, state); err != nil {
		t.Fatalf("Write: %v", err)
	}

	_, found, err := Check(path, time.Minute)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if found {
		t.Error("Check should return found=false for a stale watchdog file")
	}
}

func TestCheckNonexistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")

	_, found, err := Check(path, time.Minute)
	if err != nil {
		t.Fatalf("Check should not return an error for nonexistent file, got: %v", err)
	}
	if found {
		t.Error("Check should return found=false for nonexistent file")
	}
}

func TestCheckCorruptJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchdog.json")
	if err := os.WriteFile(path, []byte("{invalid"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, _, err := Check(path, time.Minute)
	if err == nil {
		t.Fatal("Check should return an error for corrupt JSON (not silently ignore it)")
	}
}

func TestClearExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchdog.json")
	state := State{
		Component: "daemon",
		Timestamp: time.Now(),
	}

	if err := Write(path, state); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := Clear(path); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file should not exist after Clear")
	}
}

func TestClearNonexistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")

	if err := Clear(path); err != nil {
		t.Errorf("Clear nonexistent file should be idempotent, got: %v", err)
	}
}

func TestClearIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchdog.json")
	state := State{
		Component: "daemon",
		Timestamp: time.Now(),
	}

	if err := Write(path, state); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Clear twice — second call should succeed silently.
	if err := Clear(path); err != nil {
		t.Fatalf("Clear first: %v", err)
	}
	if err := Clear(path); err != nil {
		t.Errorf("Clear second (idempotent): %v", err)
	}
}

// contains reports whether s contains substr. Avoids importing strings
// for a single test helper.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
