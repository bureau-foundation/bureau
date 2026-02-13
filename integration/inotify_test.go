// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// inotifyWaitCreate blocks until the file at targetPath exists on disk.
// Uses Linux inotify for event-driven detection instead of polling. If
// ancestor directories do not yet exist, watches them recursively until
// the full path appears.
//
// The re-check after InotifyAddWatch closes the race between the initial
// Stat and watch setup: if the file appeared in between, we return
// immediately without waiting for an event.
func inotifyWaitCreate(ctx context.Context, targetPath string) error {
	if _, err := os.Stat(targetPath); err == nil {
		return nil
	}

	directory := filepath.Dir(targetPath)
	name := filepath.Base(targetPath)

	// If the parent directory doesn't exist, wait for it first. This
	// handles the common case of principal sockets where the launcher
	// creates <runDir>/principal/ as part of sandbox setup.
	if _, err := os.Stat(directory); os.IsNotExist(err) {
		if err := inotifyWaitCreate(ctx, directory); err != nil {
			return fmt.Errorf("parent directory %s: %w", directory, err)
		}
	}

	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("inotify_init1: %w", err)
	}
	defer unix.Close(fd)

	// IN_CREATE covers bind() for Unix sockets and regular file creation.
	// IN_MOVED_TO covers atomic-rename patterns (create temp, rename into place).
	_, err = unix.InotifyAddWatch(fd, directory, unix.IN_CREATE|unix.IN_MOVED_TO)
	if err != nil {
		return fmt.Errorf("inotify_add_watch %s: %w", directory, err)
	}

	// Re-check after establishing the watch: the file may have appeared
	// between our initial Stat and the InotifyAddWatch.
	if _, err := os.Stat(targetPath); err == nil {
		return nil
	}

	return inotifyReadUntilName(ctx, fd, name)
}

// inotifyWaitDelete blocks until the file at targetPath no longer exists
// on disk. Uses Linux inotify for event-driven detection instead of polling.
func inotifyWaitDelete(ctx context.Context, targetPath string) error {
	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		return nil
	}

	directory := filepath.Dir(targetPath)
	name := filepath.Base(targetPath)

	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("inotify_init1: %w", err)
	}
	defer unix.Close(fd)

	// IN_DELETE covers unlink(). IN_MOVED_FROM covers rename-away.
	_, err = unix.InotifyAddWatch(fd, directory, unix.IN_DELETE|unix.IN_MOVED_FROM)
	if err != nil {
		return fmt.Errorf("inotify_add_watch %s: %w", directory, err)
	}

	// Re-check after establishing the watch.
	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		return nil
	}

	return inotifyReadUntilName(ctx, fd, name)
}

// inotifyReadUntilName reads inotify events from a non-blocking fd until
// an event with the given filename appears. Uses poll(2) with a 100ms
// timeout between context checks for cancellation responsiveness.
func inotifyReadUntilName(ctx context.Context, fd int, targetName string) error {
	// Each event is SizeofInotifyEvent (16 bytes) plus a null-padded
	// filename. 4096 bytes comfortably holds several events.
	buffer := make([]byte, 4096)
	pollFds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Wait up to 100ms for readability, then re-check the context.
		readyCount, err := unix.Poll(pollFds, 100)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("poll: %w", err)
		}
		if readyCount == 0 {
			continue
		}

		bytesRead, err := unix.Read(fd, buffer)
		if err != nil {
			if err == unix.EAGAIN {
				continue
			}
			return fmt.Errorf("read: %w", err)
		}

		if inotifyBufferContainsName(buffer[:bytesRead], targetName) {
			return nil
		}
	}
}

// inotifyBufferContainsName parses raw inotify events from a read buffer
// and returns true if any event's filename matches targetName.
//
// The inotify_event struct layout (all little-endian):
//
//	offset  0: int32  wd      (watch descriptor)
//	offset  4: uint32 mask    (event type bitmask)
//	offset  8: uint32 cookie  (for rename pairing)
//	offset 12: uint32 len     (name length including null padding)
//	offset 16: []byte name    (null-terminated, padded to alignment)
func inotifyBufferContainsName(buffer []byte, targetName string) bool {
	offset := 0
	for offset+unix.SizeofInotifyEvent <= len(buffer) {
		nameLength := binary.LittleEndian.Uint32(buffer[offset+12 : offset+16])
		eventEnd := offset + unix.SizeofInotifyEvent + int(nameLength)
		if eventEnd > len(buffer) {
			break
		}
		if nameLength > 0 {
			nameBytes := buffer[offset+unix.SizeofInotifyEvent : eventEnd]
			if nullTerminatedString(nameBytes) == targetName {
				return true
			}
		}
		offset = eventEnd
	}
	return false
}

// nullTerminatedString extracts a Go string from a null-padded byte slice,
// stopping at the first null byte.
func nullTerminatedString(data []byte) string {
	for i, b := range data {
		if b == 0 {
			return string(data[:i])
		}
	}
	return string(data)
}
