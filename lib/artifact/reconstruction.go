// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

// ReconstructionRecord maps an artifact reference to the ordered list
// of container segments needed to reassemble the original content.
// Stored on disk as CBOR using Core Deterministic Encoding.
type ReconstructionRecord struct {
	// Version is the record format version. Currently 1.
	Version int `json:"version"`

	// FileHash is the full 32-byte file-domain BLAKE3 hash.
	FileHash Hash `json:"file_hash"`

	// Size is the total uncompressed content size in bytes.
	Size int64 `json:"size"`

	// ChunkCount is the total number of chunks in the artifact.
	ChunkCount int `json:"chunk_count"`

	// Segments is the ordered list of container references needed
	// to reconstruct the artifact. Chunks are contiguous within a
	// segment — a segment represents a run of consecutive chunks
	// stored in a single container.
	Segments []Segment `json:"segments"`
}

// Segment references a contiguous range of chunks within a single
// container. Most artifacts consist of a small number of segments
// (one per container), making the range-based representation much
// more compact than listing every chunk individually.
type Segment struct {
	// Container is the 32-byte container-domain hash identifying
	// which container holds these chunks.
	Container Hash `json:"container"`

	// StartIndex is the index of the first chunk within the container.
	StartIndex int `json:"start_index"`

	// ChunkCount is the number of contiguous chunks in this segment.
	ChunkCount int `json:"chunk_count"`
}

// ReconstructionRecordVersion is the current record format version.
const ReconstructionRecordVersion = 1

// cborEncMode is the CBOR encoder configured with Core Deterministic
// Encoding (RFC 8949 §4.2): sorted map keys, smallest integer
// encoding, no indefinite-length items. Same logical data always
// produces identical bytes.
var cborEncMode cbor.EncMode

// cborDecMode is the CBOR decoder configured to accept standard CBOR.
var cborDecMode cbor.DecMode

func init() {
	var err error
	cborEncMode, err = cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic("artifact: CBOR encoder initialization failed: " + err.Error())
	}

	cborDecMode, err = cbor.DecOptions{}.DecMode()
	if err != nil {
		panic("artifact: CBOR decoder initialization failed: " + err.Error())
	}
}

// MarshalReconstruction encodes a ReconstructionRecord to CBOR using
// Core Deterministic Encoding.
func MarshalReconstruction(record *ReconstructionRecord) ([]byte, error) {
	data, err := cborEncMode.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encoding reconstruction record: %w", err)
	}
	return data, nil
}

// UnmarshalReconstruction decodes a CBOR-encoded ReconstructionRecord.
// Unknown fields from future versions are silently ignored (forward
// compatibility).
func UnmarshalReconstruction(data []byte) (*ReconstructionRecord, error) {
	var record ReconstructionRecord
	if err := cborDecMode.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("decoding reconstruction record: %w", err)
	}
	if record.Version < 1 {
		return nil, fmt.Errorf("reconstruction record version %d is invalid (minimum 1)", record.Version)
	}
	return &record, nil
}

// Validate checks that a ReconstructionRecord is internally
// consistent.
func (r *ReconstructionRecord) Validate() error {
	if r.Version < 1 {
		return fmt.Errorf("version %d is invalid (minimum 1)", r.Version)
	}

	var zeroHash Hash
	if r.FileHash == zeroHash {
		return fmt.Errorf("file hash is zero")
	}

	if r.Size < 0 {
		return fmt.Errorf("size %d is negative", r.Size)
	}

	if r.ChunkCount < 1 {
		return fmt.Errorf("chunk count %d is invalid (minimum 1)", r.ChunkCount)
	}

	if len(r.Segments) == 0 {
		return fmt.Errorf("no segments")
	}

	// Verify segment chunk counts sum to the total.
	var totalChunks int
	for i, segment := range r.Segments {
		if segment.Container == zeroHash {
			return fmt.Errorf("segment %d: container hash is zero", i)
		}
		if segment.ChunkCount < 1 {
			return fmt.Errorf("segment %d: chunk count %d is invalid (minimum 1)", i, segment.ChunkCount)
		}
		if segment.StartIndex < 0 {
			return fmt.Errorf("segment %d: start index %d is negative", i, segment.StartIndex)
		}
		totalChunks += segment.ChunkCount
	}

	if totalChunks != r.ChunkCount {
		return fmt.Errorf("segment chunk counts sum to %d, but total chunk count is %d",
			totalChunks, r.ChunkCount)
	}

	return nil
}
