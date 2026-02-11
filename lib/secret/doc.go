// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package secret provides a memory-safe buffer for sensitive data such
// as passwords, access tokens, and encryption keys.
//
// [Buffer] allocates memory outside the Go heap via mmap(MAP_ANONYMOUS),
// locks it into physical RAM via mlock (preventing swap), and marks it
// excluded from core dumps via madvise(MADV_DONTDUMP). On Close, the
// memory is zeroed, unlocked, and unmapped. Because the memory lives
// outside the Go heap, the garbage collector cannot copy or relocate
// it, guaranteeing secret material does not persist after release.
//
// Constructors:
//
//   - [New] -- allocates a zero-filled buffer of a given size
//   - [NewFromBytes] -- copies into protected memory, zeros the source
//   - [NewFromReader] -- reads from an io.Reader with a size limit
//
// Access via [Buffer.Bytes] (slice into mmap region) or
// [Buffer.String] (heap copy for API boundaries). [Buffer.Equal] uses
// constant-time comparison. [Buffer.WriteTo] implements io.WriterTo
// for efficient transfer without heap intermediaries. After Close, any
// access panics. Close is idempotent.
//
// Depends on golang.org/x/sys/unix. No Bureau-internal dependencies.
// Imported by lib/sealed for age keypair and credential protection.
package secret
