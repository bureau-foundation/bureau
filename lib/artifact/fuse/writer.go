// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"bytes"
	"context"
	"sync"
	"syscall"

	"github.com/bureau-foundation/bureau/lib/artifact"
	gofuse "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// writeHandle buffers data for an in-progress write and finalizes the
// artifact on Flush. Returned by Create on tag/CAS directories and by
// Open-for-write on existing tagged artifacts.
//
// The buffer grows to hold all written data in memory. On Flush, the
// complete content is passed to Store.Write (which chunks, compresses,
// and writes containers), then the tag is updated atomically.
type writeHandle struct {
	mu      sync.Mutex
	buffer  []byte
	options *Options

	// tagName is the tag to update on close. Empty for CAS writes.
	tagName string

	// previousTarget is the tag's target hash at open time. Used for
	// compare-and-swap on close to detect concurrent updates. Nil for
	// new tags (no CAS check needed).
	previousTarget *artifact.Hash

	// expectedHash is the hash the caller expects the content to have.
	// Set for CAS-path writes where the filename is the expected hash.
	// After Store.Write, the actual hash is compared against this and
	// the write fails if they differ.
	expectedHash *artifact.Hash

	// result is populated by Flush with the StoreResult, enabling
	// the writeInProgressNode to report the correct file size after
	// the write completes.
	result *artifact.StoreResult

	// flushed prevents double-finalization when Flush is called
	// multiple times (e.g., dup'd file descriptors).
	flushed bool
}

var _ gofuse.FileWriter = (*writeHandle)(nil)
var _ gofuse.FileFlusher = (*writeHandle)(nil)
var _ gofuse.FileReleaser = (*writeHandle)(nil)

// Write appends data at the given offset, growing the buffer as
// needed. Supports both sequential writes (the common case for cp,
// shell redirection, curl) and random-offset writes.
func (h *writeHandle) Write(_ context.Context, data []byte, offset int64) (uint32, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()

	endOffset := offset + int64(len(data))
	if endOffset > int64(len(h.buffer)) {
		grown := make([]byte, endOffset)
		copy(grown, h.buffer)
		h.buffer = grown
	}
	copy(h.buffer[offset:], data)
	return uint32(len(data)), 0
}

// Flush finalizes the write: stores the artifact content and updates
// the tag (or verifies the CAS hash). Called when the file descriptor
// is closed. Idempotent — subsequent calls are no-ops.
func (h *writeHandle) Flush(_ context.Context) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.flushed {
		return 0
	}
	h.flushed = true

	if len(h.buffer) == 0 {
		h.options.Logger.Error("flush called with empty buffer")
		return syscall.EIO
	}

	// Serialize with other writers (both FUSE and the service socket
	// API share the same Store, which is not safe for concurrent writes).
	h.options.WriteMu.Lock()
	result, err := h.options.Store.Write(bytes.NewReader(h.buffer), "", nil)
	h.options.WriteMu.Unlock()

	if err != nil {
		h.options.Logger.Error("artifact write failed", "error", err)
		return syscall.EIO
	}

	h.result = result

	// CAS verification: the filename was the expected hash, so the
	// content must hash to that value.
	if h.expectedHash != nil && result.FileHash != *h.expectedHash {
		h.options.Logger.Error("CAS hash mismatch",
			"expected", artifact.FormatHash(*h.expectedHash),
			"actual", artifact.FormatHash(result.FileHash),
		)
		return syscall.EIO
	}

	// Tag update: compare-and-swap by default. New tags (previousTarget
	// is nil) skip the CAS check automatically.
	if h.tagName != "" {
		now := h.options.Clock.Now()
		if err := h.options.TagStore.Set(h.tagName, result.FileHash, h.previousTarget, false, now); err != nil {
			h.options.Logger.Error("tag update failed",
				"tag", h.tagName,
				"hash", artifact.FormatHash(result.FileHash),
				"error", err,
			)
			return syscall.EIO
		}
	}

	h.buffer = nil // Release memory.
	return 0
}

// Release is called when the last reference to the file handle is
// dropped. Cleanup is already handled in Flush.
func (h *writeHandle) Release(_ context.Context) syscall.Errno {
	return 0
}

// writeInProgressNode is a minimal inode for files being written. It
// reports the current buffered size (or the final artifact size after
// Flush) via Getattr, and delegates all I/O to the writeHandle.
type writeInProgressNode struct {
	gofuse.Inode
	handle *writeHandle
}

var _ gofuse.InodeEmbedder = (*writeInProgressNode)(nil)
var _ gofuse.NodeGetattrer = (*writeInProgressNode)(nil)

func (w *writeInProgressNode) Getattr(_ context.Context, _ gofuse.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFREG | 0o644
	w.handle.mu.Lock()
	if w.handle.result != nil {
		out.Size = uint64(w.handle.result.Size)
	} else {
		out.Size = uint64(len(w.handle.buffer))
	}
	w.handle.mu.Unlock()
	return 0
}
