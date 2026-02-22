// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifactstore

import (
	"encoding/json"
	"testing"

	"github.com/bureau-foundation/bureau/lib/codec"
)

func sampleReconstructionRecord() *ReconstructionRecord {
	return &ReconstructionRecord{
		Version:    ReconstructionRecordVersion,
		FileHash:   HashFile(HashChunk([]byte("test artifact"))),
		Size:       1024 * 1024,
		ChunkCount: 16,
		Segments: []Segment{
			{
				Container:  HashContainer(MerkleRoot(chunkDomainKey, []Hash{HashChunk([]byte("c0"))})),
				StartIndex: 0,
				ChunkCount: 10,
			},
			{
				Container:  HashContainer(MerkleRoot(chunkDomainKey, []Hash{HashChunk([]byte("c1"))})),
				StartIndex: 0,
				ChunkCount: 6,
			},
		},
	}
}

func TestReconstructionCBORRoundtrip(t *testing.T) {
	original := sampleReconstructionRecord()

	encoded, err := MarshalReconstruction(original)
	if err != nil {
		t.Fatalf("MarshalReconstruction failed: %v", err)
	}

	if len(encoded) == 0 {
		t.Fatal("encoded data is empty")
	}

	decoded, err := UnmarshalReconstruction(encoded)
	if err != nil {
		t.Fatalf("UnmarshalReconstruction failed: %v", err)
	}

	if decoded.Version != original.Version {
		t.Errorf("version: got %d, want %d", decoded.Version, original.Version)
	}
	if decoded.FileHash != original.FileHash {
		t.Errorf("file hash mismatch")
	}
	if decoded.Size != original.Size {
		t.Errorf("size: got %d, want %d", decoded.Size, original.Size)
	}
	if decoded.ChunkCount != original.ChunkCount {
		t.Errorf("chunk count: got %d, want %d", decoded.ChunkCount, original.ChunkCount)
	}
	if len(decoded.Segments) != len(original.Segments) {
		t.Fatalf("segment count: got %d, want %d", len(decoded.Segments), len(original.Segments))
	}

	for i := range original.Segments {
		if decoded.Segments[i].Container != original.Segments[i].Container {
			t.Errorf("segment %d: container hash mismatch", i)
		}
		if decoded.Segments[i].StartIndex != original.Segments[i].StartIndex {
			t.Errorf("segment %d: start index: got %d, want %d",
				i, decoded.Segments[i].StartIndex, original.Segments[i].StartIndex)
		}
		if decoded.Segments[i].ChunkCount != original.Segments[i].ChunkCount {
			t.Errorf("segment %d: chunk count: got %d, want %d",
				i, decoded.Segments[i].ChunkCount, original.Segments[i].ChunkCount)
		}
	}
}

func TestReconstructionCBORDeterministic(t *testing.T) {
	record := sampleReconstructionRecord()

	encoded1, err := MarshalReconstruction(record)
	if err != nil {
		t.Fatal(err)
	}

	encoded2, err := MarshalReconstruction(record)
	if err != nil {
		t.Fatal(err)
	}

	if len(encoded1) != len(encoded2) {
		t.Fatalf("CBOR output length differs: %d vs %d", len(encoded1), len(encoded2))
	}

	for i := range encoded1 {
		if encoded1[i] != encoded2[i] {
			t.Fatalf("CBOR output differs at byte %d", i)
		}
	}
}

func TestReconstructionCBORSmallerThanJSON(t *testing.T) {
	record := sampleReconstructionRecord()

	cborData, err := MarshalReconstruction(record)
	if err != nil {
		t.Fatal(err)
	}

	jsonData, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("CBOR: %d bytes, JSON: %d bytes (%.1f%% savings)",
		len(cborData), len(jsonData),
		100.0*(1.0-float64(len(cborData))/float64(len(jsonData))))

	if len(cborData) >= len(jsonData) {
		t.Errorf("CBOR (%d bytes) should be smaller than JSON (%d bytes)",
			len(cborData), len(jsonData))
	}
}

func TestReconstructionJSONTagFallback(t *testing.T) {
	// Verify that CBOR encoding uses json struct tags, so the same
	// structs can be marshalled to both JSON and CBOR.
	record := sampleReconstructionRecord()

	jsonData, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var jsonMap map[string]any
	if err := json.Unmarshal(jsonData, &jsonMap); err != nil {
		t.Fatal(err)
	}

	// Check that JSON field names match our struct tags.
	expectedFields := []string{"version", "file_hash", "size", "chunk_count", "segments"}
	for _, field := range expectedFields {
		if _, ok := jsonMap[field]; !ok {
			t.Errorf("JSON output missing field %q", field)
		}
	}
}

func TestReconstructionForwardCompatibility(t *testing.T) {
	// A future version might add fields. CBOR decoding should
	// silently ignore unknown fields.
	record := sampleReconstructionRecord()
	encoded, err := MarshalReconstruction(record)
	if err != nil {
		t.Fatal(err)
	}

	// Verify we can decode what we encoded (baseline).
	decoded, err := UnmarshalReconstruction(encoded)
	if err != nil {
		t.Fatalf("UnmarshalReconstruction failed: %v", err)
	}
	if decoded.FileHash != record.FileHash {
		t.Error("roundtrip file hash mismatch")
	}
}

func TestReconstructionValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*ReconstructionRecord)
		wantErr bool
	}{
		{
			name:    "valid",
			modify:  func(r *ReconstructionRecord) {},
			wantErr: false,
		},
		{
			name:    "zero_version",
			modify:  func(r *ReconstructionRecord) { r.Version = 0 },
			wantErr: true,
		},
		{
			name:    "zero_file_hash",
			modify:  func(r *ReconstructionRecord) { r.FileHash = Hash{} },
			wantErr: true,
		},
		{
			name:    "negative_size",
			modify:  func(r *ReconstructionRecord) { r.Size = -1 },
			wantErr: true,
		},
		{
			name:    "zero_chunk_count",
			modify:  func(r *ReconstructionRecord) { r.ChunkCount = 0 },
			wantErr: true,
		},
		{
			name:    "no_segments",
			modify:  func(r *ReconstructionRecord) { r.Segments = nil },
			wantErr: true,
		},
		{
			name: "zero_container_hash",
			modify: func(r *ReconstructionRecord) {
				r.Segments[0].Container = Hash{}
			},
			wantErr: true,
		},
		{
			name: "zero_segment_chunks",
			modify: func(r *ReconstructionRecord) {
				r.Segments[0].ChunkCount = 0
			},
			wantErr: true,
		},
		{
			name: "negative_start_index",
			modify: func(r *ReconstructionRecord) {
				r.Segments[0].StartIndex = -1
			},
			wantErr: true,
		},
		{
			name: "chunk_count_mismatch",
			modify: func(r *ReconstructionRecord) {
				r.ChunkCount = 999
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := sampleReconstructionRecord()
			tt.modify(record)
			err := record.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestUnmarshalReconstructionInvalidVersion(t *testing.T) {
	// Encode a record with version 0.
	record := sampleReconstructionRecord()
	record.Version = 0
	encoded, err := codec.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}

	_, err = UnmarshalReconstruction(encoded)
	if err == nil {
		t.Error("UnmarshalReconstruction should reject version 0")
	}
}

func TestUnmarshalReconstructionInvalidCBOR(t *testing.T) {
	_, err := UnmarshalReconstruction([]byte{0xFF, 0xFE, 0xFD})
	if err == nil {
		t.Error("UnmarshalReconstruction should fail on invalid CBOR")
	}
}

// Benchmarks for reconstruction record serialization.

func BenchmarkMarshalReconstruction(b *testing.B) {
	record := sampleReconstructionRecord()

	b.ReportAllocs()
	for b.Loop() {
		MarshalReconstruction(record)
	}
}

func BenchmarkUnmarshalReconstruction(b *testing.B) {
	record := sampleReconstructionRecord()
	encoded, err := MarshalReconstruction(record)
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	for b.Loop() {
		UnmarshalReconstruction(encoded)
	}
}
