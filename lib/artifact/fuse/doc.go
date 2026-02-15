// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package fuse implements a read-only FUSE filesystem for transparent
// access to Bureau artifacts as regular files.
//
// The mount exposes two top-level directories:
//
//   - tag/ — hierarchical view of tagged artifacts. Tag names containing
//     forward slashes are presented as directory trees. Leaf entries
//     resolve to artifact files whose content is streamed on read.
//
//   - cas/ — flat lookup by content hash. No directory listing (the
//     keyspace is too large). Lookup by full hex hash returns the
//     artifact file directly.
//
// # Read Path
//
// When a file is opened, the filesystem loads the artifact's
// reconstruction metadata and all referenced container indexes to
// build a chunk table — a sorted array mapping byte offsets to
// container/chunk pairs. Reads at arbitrary offsets use binary search
// over this table to identify the relevant chunk, then decompress it
// from the container (checking the local cache first). Prefetching
// loads the next N containers in reconstruction order on first access.
//
// # Caching
//
// The filesystem uses the artifact Cache (a bounded ring buffer) for
// container-level caching. Cache misses fall through to the on-disk
// Store. Each container holds up to 1024 chunks (~64 MiB), so a single
// cache entry serves many sequential reads.
//
// # Write Path
//
// Not implemented. All mutation operations (Create, Write, Mkdir,
// Unlink, etc.) return EROFS. The write path is a separate concern
// handled through the artifact service socket API.
package fuse
