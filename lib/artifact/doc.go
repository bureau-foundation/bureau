// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package artifact implements the core content-addressable storage (CAS)
// engine for Bureau's artifact system. It provides chunking, hashing,
// compression, and container management — the pure data pipeline that
// the artifact service, FUSE mount, and CLI build on.
//
// The package is organized in layers, each usable independently:
//
//   - Hashing: BLAKE3 with domain-separated keyed mode. Three domains
//     (chunk, container, file) prevent cross-domain collisions. Merkle
//     trees over chunk hashes enable verified streaming of sub-ranges.
//
//   - Chunking: GearHash content-defined chunking (CDC) with 64KB
//     target, 8KB minimum, 128KB maximum. Deterministic boundary
//     placement based on content means insertions and deletions only
//     affect nearby chunks, enabling effective deduplication across
//     similar files.
//
//   - Compression: Per-chunk transparent compression with four codecs
//     (none, LZ4, zstd, BG4+LZ4). Chunk hashes are computed on
//     uncompressed bytes so deduplication works across compression
//     algorithm changes. Content-type heuristics select the codec
//     automatically, with caller override.
//
//   - Containers: Binary format aggregating up to ~1024 compressed
//     chunks into ~64MB units. Fixed-layout header with chunk index
//     preceding data for single-I/O index reads and arithmetic offset
//     computation. Containers are the unit of disk storage, network
//     transfer, and cache eviction.
//
//   - Reconstruction: CBOR-encoded metadata mapping an artifact
//     reference to the ordered list of containers and chunk ranges
//     needed to reassemble the original content. Range-based segments
//     for compactness when chunks are contiguous (the common case).
//
//   - Store: Local filesystem operations. Write path streams content
//     through chunker → compressor → container builder → disk. Read
//     path resolves reconstruction metadata → containers → chunks →
//     decompressed output. Small artifacts (< 256KB) take a fast path
//     with no CDC overhead.
//
// All artifact references are file-domain BLAKE3 hashes, regardless of
// chunk count. The short form (art- prefix + 12 hex chars of the file
// hash) is used in user-facing contexts. The full 32-byte hash is
// stored in metadata.
//
// On-disk metadata uses CBOR (RFC 8949) with Core Deterministic
// Encoding via lib/codec for compactness and reproducibility.
// Struct types use json struct tags — fxamacker/cbor falls back to
// json tags, so the same types work with both encoders.
package artifact
