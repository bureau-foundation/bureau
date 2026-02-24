// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"
)

// watchForFile watches a directory for the creation of a named file via
// inotify. Returns a channel that closes when the file appears (via
// IN_CREATE or IN_MOVED_TO), and a cleanup function that stops the
// watcher and releases the inotify file descriptor.
//
// The cleanup function must be called regardless of whether the channel
// has fired. It is safe to call multiple times.
//
// The caller should check whether the target file already exists AFTER
// calling watchForFile, not before. This ordering avoids the race where
// the file appears between an existence check and the watch setup: if
// the file already exists when checked after the watch is installed, the
// caller uses it immediately; if it appears later, the inotify event
// fires. Either way, nothing is missed.
func watchForFile(directory, filename string) (<-chan struct{}, func(), error) {
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		return nil, nil, fmt.Errorf("inotify_init1: %w", err)
	}

	_, err = unix.InotifyAddWatch(fd, directory, unix.IN_CREATE|unix.IN_MOVED_TO)
	if err != nil {
		unix.Close(fd)
		return nil, nil, fmt.Errorf("inotify_add_watch on %s: %w", directory, err)
	}

	ready := make(chan struct{})
	stopChannel := make(chan struct{})

	go inotifyReadLoop(fd, filename, ready, stopChannel)

	cleanedUp := false
	cleanup := func() {
		if cleanedUp {
			return
		}
		cleanedUp = true
		close(stopChannel)
	}

	return ready, cleanup, nil
}

// inotifyReadLoop polls the inotify fd for events matching the target
// filename. Closes the ready channel when the file appears, and closes
// the inotify fd when the loop exits (on match, stop signal, or error).
//
// Uses poll(2) with a 100ms timeout so the goroutine remains responsive
// to the stop signal without burning CPU on a tight loop.
func inotifyReadLoop(fd int, targetFilename string, ready chan struct{}, stopChannel <-chan struct{}) {
	defer unix.Close(fd)

	buffer := make([]byte, 4096)
	for {
		select {
		case <-stopChannel:
			return
		default:
		}

		// poll(2) with 100ms timeout: blocks until data is available
		// or the timeout expires, allowing periodic stop checks.
		pollDescriptors := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		count, err := unix.Poll(pollDescriptors, 100)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}
		if count == 0 {
			continue // timeout, check stopChannel
		}

		bytesRead, err := unix.Read(fd, buffer)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			return
		}

		if inotifyEventsContainFilename(buffer[:bytesRead], targetFilename) {
			close(ready)
			return
		}
	}
}

// inotifyEventsContainFilename scans a buffer of raw inotify events for
// one whose name matches the target filename.
//
// Inotify event layout (from inotify(7)):
//
//	struct inotify_event {
//	    int32_t  wd;     // offset 0
//	    uint32_t mask;   // offset 4
//	    uint32_t cookie; // offset 8
//	    uint32_t len;    // offset 12
//	    char     name[]; // offset 16, padded to alignment
//	};
func inotifyEventsContainFilename(buffer []byte, targetFilename string) bool {
	offset := 0
	for offset+unix.SizeofInotifyEvent <= len(buffer) {
		nameLength := int(binary.NativeEndian.Uint32(buffer[offset+12 : offset+16]))
		eventSize := unix.SizeofInotifyEvent + nameLength
		if offset+eventSize > len(buffer) {
			break
		}

		if nameLength > 0 {
			// The name is null-padded to an alignment boundary.
			// Find the actual string by stopping at the first null.
			nameBytes := buffer[offset+unix.SizeofInotifyEvent : offset+eventSize]
			name := nullTerminatedString(nameBytes)
			if name == targetFilename {
				return true
			}
		}

		offset += eventSize
	}
	return false
}

// nullTerminatedString extracts a string from a null-padded byte slice,
// stopping at the first null byte.
func nullTerminatedString(data []byte) string {
	for i, b := range data {
		if b == 0 {
			return string(data[:i])
		}
	}
	return string(data)
}
