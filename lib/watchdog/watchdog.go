// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package watchdog

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
)

// State records the context of a process transition. Written before the
// transition and read after startup to determine the outcome.
type State struct {
	// Component identifies what is being updated (e.g., "daemon",
	// "launcher"). Used for logging and diagnostics.
	Component string `cbor:"component"`

	// PreviousBinary is the absolute path of the binary before the
	// transition. When the current process's executable matches this
	// path, the transition failed (the new binary crashed and the
	// system restarted the old one).
	PreviousBinary string `cbor:"previous_binary"`

	// NewBinary is the absolute path of the binary being transitioned
	// to. When the current process's executable matches this path, the
	// transition succeeded.
	NewBinary string `cbor:"new_binary"`

	// Timestamp is when the transition was initiated. Used by Check to
	// discard stale watchdog files from previous unrelated restarts.
	Timestamp time.Time `cbor:"timestamp"`
}

// Write atomically writes a watchdog state file. The file is written to a
// temporary location in the same directory, fsynced for durability, and
// renamed into place. Readers never see a partial write.
//
// The file is created with mode 0600 (owner read/write only). The parent
// directory must already exist.
func Write(path string, state State) error {
	data, err := codec.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshaling watchdog state: %w", err)
	}

	temporaryPath := path + ".tmp"

	file, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("creating temporary watchdog file: %w", err)
	}

	// Write, sync, close — in that order. If any step fails, remove the
	// temporary file and report the first error.
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(temporaryPath)
		return fmt.Errorf("writing temporary watchdog file: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(temporaryPath)
		return fmt.Errorf("syncing temporary watchdog file: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(temporaryPath)
		return fmt.Errorf("closing temporary watchdog file: %w", err)
	}

	if err := os.Rename(temporaryPath, path); err != nil {
		os.Remove(temporaryPath)
		return fmt.Errorf("renaming watchdog file into place: %w", err)
	}

	// Sync the parent directory to ensure the rename is durable. This
	// matters when the machine loses power between rename and the OS
	// flushing directory metadata.
	parentDirectory, err := os.Open(filepath.Dir(path))
	if err == nil {
		parentDirectory.Sync()
		parentDirectory.Close()
	}

	return nil
}

// Read reads and parses a watchdog state file. Returns the state or an error.
// When the file does not exist, the returned error wraps os.ErrNotExist
// (testable with errors.Is).
func Read(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}

	var state State
	if err := codec.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("parsing watchdog file %s: %w", path, err)
	}
	return state, nil
}

// Check reads a watchdog state file and verifies it was written recently
// enough to be relevant. Returns the state and true when the file exists
// and its Timestamp is within maxAge of now. Returns a zero State and false
// when the file does not exist or is older than maxAge.
//
// Any other error (permission denied, corrupt data) is returned as-is so
// the caller can distinguish "no watchdog" from "watchdog exists but
// unreadable."
func Check(path string, maxAge time.Duration) (State, bool, error) {
	state, err := Read(path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, false, nil
		}
		return State{}, false, err
	}

	if time.Since(state.Timestamp) > maxAge {
		return State{}, false, nil
	}

	return state, true, nil
}

// Clear removes a watchdog state file. Idempotent: returns nil when the
// file does not exist.
func Clear(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing watchdog file: %w", err)
	}
	return nil
}
