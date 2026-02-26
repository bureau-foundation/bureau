// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"time"

	"github.com/bureau-foundation/bureau/lib/ticketindex"
	"golang.org/x/sys/unix"
)

// WatchBeadsFile loads the initial beads snapshot and starts an inotify
// watcher that feeds incremental updates into the returned IndexSource.
// The cleanup function stops the watcher and closes the inotify fd.
//
// The watcher monitors the parent directory for IN_CLOSE_WRITE and
// IN_MOVED_TO events on the target filename (handling both in-place
// writes and atomic renames). On each change, the file is re-read in
// full and diffed against the previous snapshot; only actual changes
// produce Put/Remove calls on the IndexSource.
func WatchBeadsFile(path string) (*IndexSource, func(), error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}

	snapshot, err := readBeadsSnapshot(absolutePath)
	if err != nil {
		return nil, nil, err
	}

	index := ticketindex.NewIndex()
	for id, entry := range snapshot {
		index.Put(id, entry.content)
	}
	source := NewIndexSource(index)

	// Set up inotify on the parent directory. Watching the directory
	// (not the file) catches atomic renames: tools that write a temp
	// file and rename it create a new inode, so a file-level watch
	// on the old inode misses the replacement.
	directory := filepath.Dir(absolutePath)
	filename := filepath.Base(absolutePath)

	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		return nil, nil, err
	}

	_, err = unix.InotifyAddWatch(fd, directory, unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO)
	if err != nil {
		unix.Close(fd)
		return nil, nil, err
	}

	stopChannel := make(chan struct{})
	go beadsWatchLoop(fd, absolutePath, filename, source, snapshot, stopChannel)

	cleanedUp := false
	cleanup := func() {
		if cleanedUp {
			return
		}
		cleanedUp = true
		close(stopChannel)
	}

	return source, cleanup, nil
}

// beadsWatchLoop polls the inotify fd for changes to the target file,
// re-reads the snapshot, and diffs it against the previous state.
// Changes are pushed into the IndexSource via Put/Remove, which
// dispatches events to subscribers (driving TUI heat animation).
//
// Uses poll(2) with 100ms timeout for responsive stop-channel checking.
// After detecting a change, waits 50ms and drains any queued events to
// coalesce rapid writes (e.g., closing multiple beads in one command).
func beadsWatchLoop(
	fd int,
	path string,
	filename string,
	source *IndexSource,
	previous map[string]beadsSnapshotEntry,
	stopChannel <-chan struct{},
) {
	defer unix.Close(fd)

	buffer := make([]byte, 4096)

	for {
		select {
		case <-stopChannel:
			return
		default:
		}

		pollDescriptors := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		count, err := unix.Poll(pollDescriptors, 100)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			// Fatal poll error — watcher exits. The viewer degrades
			// to static mode (same as before this feature existed).
			return
		}
		if count == 0 {
			continue
		}

		bytesRead, err := unix.Read(fd, buffer)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			return
		}

		if !beadsInotifyMatchesFile(buffer[:bytesRead], filename) {
			continue
		}

		// Debounce: wait 50ms and drain any additional events that
		// arrived during that window. Coalesces rapid writes from
		// commands like "br close a b c" that update the JSONL
		// multiple times in quick succession.
		time.Sleep(50 * time.Millisecond)
		drainInotifyEvents(fd, buffer)

		current, err := readBeadsSnapshot(path)
		if err != nil {
			// File might be mid-write or briefly absent during an
			// atomic replace. Skip this update; the next inotify
			// event (from the completed write) will succeed.
			continue
		}

		diffBeadsSnapshots(source, previous, current)
		previous = current
	}
}

// diffBeadsSnapshots compares two snapshots and pushes changes into the
// IndexSource. Only entries whose raw JSON bytes differ (or are
// new/removed) produce Put/Remove calls, avoiding false heat animation.
func diffBeadsSnapshots(source *IndexSource, previous, current map[string]beadsSnapshotEntry) {
	// New or changed entries.
	for id, entry := range current {
		old, exists := previous[id]
		if !exists || !bytes.Equal(old.rawJSON, entry.rawJSON) {
			source.Put(id, entry.content)
		}
	}

	// Removed entries.
	for id := range previous {
		if _, exists := current[id]; !exists {
			source.Remove(id)
		}
	}
}

// beadsInotifyMatchesFile checks whether any inotify event in the
// buffer matches the target filename. Layout from inotify(7):
//
//	struct inotify_event {
//	    int32_t  wd;     // offset 0
//	    uint32_t mask;   // offset 4
//	    uint32_t cookie; // offset 8
//	    uint32_t len;    // offset 12
//	    char     name[]; // offset 16, null-padded to alignment
//	};
func beadsInotifyMatchesFile(buffer []byte, targetFilename string) bool {
	offset := 0
	for offset+unix.SizeofInotifyEvent <= len(buffer) {
		nameLength := int(binary.NativeEndian.Uint32(buffer[offset+12 : offset+16]))
		eventSize := unix.SizeofInotifyEvent + nameLength
		if offset+eventSize > len(buffer) {
			break
		}

		if nameLength > 0 {
			nameBytes := buffer[offset+unix.SizeofInotifyEvent : offset+eventSize]
			name := beadsNullTerminatedString(nameBytes)
			if name == targetFilename {
				return true
			}
		}

		offset += eventSize
	}
	return false
}

// beadsNullTerminatedString extracts a string from a null-padded byte
// slice, stopping at the first null byte.
func beadsNullTerminatedString(data []byte) string {
	for i, b := range data {
		if b == 0 {
			return string(data[:i])
		}
	}
	return string(data)
}

// drainInotifyEvents reads and discards any pending inotify events.
// Called after the debounce sleep to coalesce rapid writes into a
// single re-read.
func drainInotifyEvents(fd int, buffer []byte) {
	for {
		_, err := unix.Read(fd, buffer)
		if err != nil {
			// EAGAIN means no more events; any other error, stop.
			return
		}
	}
}
