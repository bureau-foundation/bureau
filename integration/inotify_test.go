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
// Uses Linux inotify for event-driven detection instead of polling.
//
// When ancestor directories do not yet exist (e.g., the launcher hasn't
// created <runDir>/principal/<name>/ yet), this function iteratively
// watches the deepest existing ancestor. After any creation event in the
// watched directory, it re-checks the full target path — this handles
// os.MkdirAll creating multiple directory levels at once. Each iteration
// advances the watch point closer to the target until the file appears.
//
// Each iteration creates a fresh inotify fd to avoid stale-event issues
// that arise when reusing a single fd across multiple watch/unwatch
// cycles (IN_IGNORED events from InotifyRmWatch can cause false matches
// when the kernel reuses watch descriptors for the same directory).
func inotifyWaitCreate(ctx context.Context, targetPath string) error {
	for {
		if _, err := os.Stat(targetPath); err == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		// Find the deepest ancestor of targetPath that already exists.
		// Watch that directory for any creation event, which will either
		// be the next intermediate directory or the target itself.
		watchDirectory := deepestExistingAncestor(targetPath)
		if watchDirectory == "" {
			return fmt.Errorf("no ancestor of %s exists", targetPath)
		}

		fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
		if err != nil {
			return fmt.Errorf("inotify_init1: %w (check fs.inotify.max_user_instances)", err)
		}

		// IN_CREATE covers bind() for Unix sockets and regular file/directory
		// creation. IN_MOVED_TO covers atomic-rename patterns.
		_, err = unix.InotifyAddWatch(fd, watchDirectory, unix.IN_CREATE|unix.IN_MOVED_TO)
		if err != nil {
			unix.Close(fd)
			return fmt.Errorf("inotify_add_watch %s: %w", watchDirectory, err)
		}

		// Re-check after establishing the watch: the target (or an
		// intermediate directory) may have appeared between our Stat
		// above and the InotifyAddWatch.
		if _, err := os.Stat(targetPath); err == nil {
			unix.Close(fd)
			return nil
		}

		// Wait for any creation event in the watched directory. With a
		// fresh per-iteration fd, the single watch is the only source of
		// events — no watch descriptor filtering needed.
		err = inotifyWaitForAnyEvent(ctx, fd)
		unix.Close(fd)
		if err != nil {
			return err
		}

		// Loop back: re-check the full target path. If MkdirAll created
		// all intermediate directories at once, we'll find the target
		// immediately. Otherwise, the next iteration watches one level
		// deeper.
	}
}

// inotifyWaitDelete blocks until the file at targetPath no longer exists
// on disk. Uses Linux inotify for event-driven detection instead of
// polling. The parent directory must exist (the file exists, so its
// parent does too).
func inotifyWaitDelete(ctx context.Context, targetPath string) error {
	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		return nil
	}

	directory := filepath.Dir(targetPath)
	name := filepath.Base(targetPath)

	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("inotify_init1: %w (check fs.inotify.max_user_instances)", err)
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

// deepestExistingAncestor walks up from targetPath's parent directory
// until it finds a directory that exists. Returns "" if no ancestor
// exists (only possible if the filesystem root is inaccessible).
func deepestExistingAncestor(targetPath string) string {
	directory := filepath.Dir(targetPath)
	for {
		if _, err := os.Stat(directory); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return ""
		}
		directory = parent
	}
}

// inotifyWaitForAnyEvent reads inotify events from a non-blocking fd
// until at least one event arrives. Uses poll(2) with a 100ms timeout
// between context cancellation checks.
func inotifyWaitForAnyEvent(ctx context.Context, fd int) error {
	buffer := make([]byte, 4096)
	pollFds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

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

		_, err = unix.Read(fd, buffer)
		if err != nil {
			if err == unix.EAGAIN {
				continue
			}
			return fmt.Errorf("read: %w", err)
		}

		// Any event on a fresh fd with a single watch means something
		// was created in the watched directory. The caller re-checks
		// the full target path.
		return nil
	}
}

// inotifyReadUntilName reads inotify events from a non-blocking fd until
// an event with the given filename appears. Uses poll(2) with a 100ms
// timeout between context cancellation checks.
func inotifyReadUntilName(ctx context.Context, fd int, targetName string) error {
	buffer := make([]byte, 4096)
	pollFds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

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
