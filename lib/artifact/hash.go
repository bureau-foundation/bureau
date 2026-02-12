// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"encoding/hex"
	"fmt"

	"github.com/zeebo/blake3"
)

// Hash is a 32-byte BLAKE3 digest. All artifact hashes (chunk,
// container, file) are this size.
type Hash [32]byte

// domainKey is a 32-byte key for BLAKE3 keyed hashing. Domain
// separation ensures that the same input bytes produce different
// hashes in different contexts, preventing cross-domain collisions.
type domainKey [32]byte

// Domain separation keys. These are fixed constants — changing them
// invalidates all existing hashes in that domain. The byte values
// are the ASCII encoding of the domain name, zero-padded to 32 bytes.
// Using readable ASCII makes the keys inspectable in hex dumps and
// debuggers without sacrificing any cryptographic property (BLAKE3
// keyed mode treats the key as an opaque 32-byte value).
var (
	chunkDomainKey = domainKey{
		'b', 'u', 'r', 'e', 'a', 'u', '.', 'a', 'r', 't', 'i', 'f', 'a', 'c', 't', '.',
		'c', 'h', 'u', 'n', 'k', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	}

	containerDomainKey = domainKey{
		'b', 'u', 'r', 'e', 'a', 'u', '.', 'a', 'r', 't', 'i', 'f', 'a', 'c', 't', '.',
		'c', 'o', 'n', 't', 'a', 'i', 'n', 'e', 'r', 0, 0, 0, 0, 0, 0, 0,
	}

	fileDomainKey = domainKey{
		'b', 'u', 'r', 'e', 'a', 'u', '.', 'a', 'r', 't', 'i', 'f', 'a', 'c', 't', '.',
		'f', 'i', 'l', 'e', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	}
)

// HashChunk computes the chunk-domain BLAKE3 keyed hash of the given
// data. This is the hash stored in container chunk indexes and used
// for deduplication. Chunk hashes are always computed on uncompressed
// bytes so dedup works across compression algorithm changes.
func HashChunk(data []byte) Hash {
	return keyedHash(chunkDomainKey, data)
}

// HashFile computes the file-domain BLAKE3 keyed hash from a Merkle
// root. For single-chunk artifacts, pass the single chunk hash. For
// multi-chunk artifacts, pass the Merkle root computed by
// [MerkleRoot]. All artifact references are derived from file-domain
// hashes.
func HashFile(merkleRoot Hash) Hash {
	return keyedHash(fileDomainKey, merkleRoot[:])
}

// HashContainer computes the container-domain BLAKE3 keyed hash from
// the Merkle root of the container's chunk hashes. Used to address
// containers on disk and in transfer protocols.
func HashContainer(merkleRoot Hash) Hash {
	return keyedHash(containerDomainKey, merkleRoot[:])
}

// MerkleRoot computes a binary Merkle tree over the given hashes and
// returns the root. The tree is constructed bottom-up: adjacent pairs
// are concatenated and hashed with the domain key. If a level has an
// odd number of nodes, the last node is promoted to the next level
// without hashing (it is NOT duplicated — duplicating would mean two
// different inputs produce the same root when one is a prefix of the
// other).
//
// The domain key determines the hash domain of the resulting root.
// Use chunkDomainKey for trees over chunk hashes (when computing
// container or file hashes), or any domain key appropriate for the
// context.
//
// Panics if hashes is empty.
func MerkleRoot(key domainKey, hashes []Hash) Hash {
	if len(hashes) == 0 {
		panic("artifact.MerkleRoot: empty hash list")
	}
	if len(hashes) == 1 {
		return hashes[0]
	}

	// Pre-create a single keyed hasher and reuse it via Reset() for
	// each pair. This avoids allocating a new Hasher per pair — the
	// dominant allocation source for large trees. Reset() preserves
	// the key; it returns the hasher to its initial keyed state.
	hasher, err := blake3.NewKeyed(key[:])
	if err != nil {
		panic("artifact: BLAKE3 keyed hash initialization failed: " + err.Error())
	}

	// Scratch buffer for concatenating two hashes.
	var combined [64]byte

	// hashPairWith computes keyed hash of left||right using the
	// pre-allocated hasher.
	hashPairWith := func(left, right Hash) Hash {
		copy(combined[:32], left[:])
		copy(combined[32:], right[:])
		hasher.Reset()
		hasher.Write(combined[:])
		var result Hash
		copy(result[:], hasher.Sum(nil))
		return result
	}

	// Work on a copy to avoid mutating the caller's slice.
	level := make([]Hash, len(hashes))
	copy(level, hashes)

	for len(level) > 1 {
		nextLength := (len(level) + 1) / 2
		next := make([]Hash, nextLength)

		for i := 0; i < len(level)-1; i += 2 {
			next[i/2] = hashPairWith(level[i], level[i+1])
		}

		// Odd node: promote without hashing.
		if len(level)%2 == 1 {
			next[nextLength-1] = level[len(level)-1]
		}

		level = next
	}

	return level[0]
}

// FormatHash returns the hex-encoded string representation of a hash.
// This is the canonical format used in metadata, logs, and CLI output.
func FormatHash(hash Hash) string {
	return hex.EncodeToString(hash[:])
}

// ParseHash parses a 64-character hex string into a Hash.
func ParseHash(hexString string) (Hash, error) {
	var hash Hash
	decoded, err := hex.DecodeString(hexString)
	if err != nil {
		return hash, fmt.Errorf("parsing artifact hash: %w", err)
	}
	if len(decoded) != 32 {
		return hash, fmt.Errorf("artifact hash is %d bytes, want 32", len(decoded))
	}
	copy(hash[:], decoded)
	return hash, nil
}

// FormatRef returns the short artifact reference for a file-domain
// hash: the "art-" prefix followed by the first 12 hex characters.
func FormatRef(fileHash Hash) string {
	return "art-" + hex.EncodeToString(fileHash[:6])
}

// keyedHash computes BLAKE3 keyed hash with the given domain key.
func keyedHash(key domainKey, data []byte) Hash {
	// NewKeyed requires exactly 32 bytes, which domainKey guarantees.
	// The error is only returned for wrong key length, so this cannot
	// fail with our fixed-size type.
	hasher, err := blake3.NewKeyed(key[:])
	if err != nil {
		panic("artifact: BLAKE3 keyed hash initialization failed: " + err.Error())
	}
	hasher.Write(data)
	var hash Hash
	copy(hash[:], hasher.Sum(nil))
	return hash
}

// hashPair concatenates two hashes and computes a keyed hash of the
// result. Used internally by MerkleRoot for building the tree.
func hashPair(key domainKey, left, right Hash) Hash {
	var combined [64]byte
	copy(combined[:32], left[:])
	copy(combined[32:], right[:])
	return keyedHash(key, combined[:])
}
