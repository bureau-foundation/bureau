// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package fuse implements a FUSE filesystem for transparent read and
// write access to Bureau artifacts as regular files.
//
// The mount exposes two top-level directories:
//
//   - tag/ — hierarchical view of tagged artifacts. Tag names containing
//     forward slashes are presented as directory trees. Leaf entries
//     resolve to artifact files whose content is streamed on read.
//     Writing to a tag path stores the content as a new artifact and
//     atomically updates the tag to point at it.
//
//   - cas/ — flat lookup by content hash. No directory listing (the
//     keyspace is too large). Lookup by full hex hash returns the
//     artifact file directly. Writing to a CAS path stores the content
//     and verifies that it hashes to the filename.
//
// # Read Path
//
// When a file is opened for reading, the filesystem loads the
// artifact's reconstruction metadata and all referenced container
// indexes to build a chunk table — a sorted array mapping byte offsets
// to container/chunk pairs. Reads at arbitrary offsets use binary
// search over this table to identify the relevant chunk, then
// decompress it from the container (checking the local cache first).
// Prefetching loads the next N containers in reconstruction order on
// first access.
//
// # Write Path
//
// When a file is opened for writing (via cp, shell redirection, curl,
// etc.), a writeHandle buffers all data in memory. On close:
//
//  1. The content is passed to Store.Write, which chunks, compresses,
//     and writes containers to disk.
//  2. For tag writes: the tag is atomically updated via compare-and-swap.
//     If another writer updated the tag between open and close, the
//     artifact is still stored (data is never lost) but the tag update
//     fails with EIO. The caller can retry.
//  3. For CAS writes: the content hash is verified against the filename.
//     A mismatch returns EIO.
//
// # Caching
//
// The filesystem uses the artifact Cache (a bounded ring buffer) for
// container-level caching. Cache misses fall through to the on-disk
// Store. Each container holds up to 1024 chunks (~64 MiB), so a single
// cache entry serves many sequential reads.
package fuse
