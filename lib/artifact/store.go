// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Directory names within the artifact store root.
const (
	containerDir      = "containers"
	reconstructionDir = "reconstruction"
	tmpDir            = "tmp"
)

// MaxContainerChunks is the target maximum number of chunks per
// container. A container is flushed when it reaches this count or
// when the accumulated data size approaches MaxContainerSize.
const MaxContainerChunks = 1024

// MaxContainerSize is the target maximum uncompressed data size per
// container in bytes (~64 MiB). This is a soft limit: the last chunk
// that causes the container to exceed this size is included, then the
// container is flushed.
const MaxContainerSize = 64 * 1024 * 1024

// Store manages the local artifact storage directory. It provides
// the write and read pipelines that tie together chunking, hashing,
// compression, and container management with filesystem operations.
//
// The store is safe for concurrent reads but not concurrent writes
// to the same artifact. The caller (artifact service) is responsible
// for serializing writes.
type Store struct {
	root string
}

// NewStore creates a Store rooted at the given directory. The
// directory structure is created if it does not exist.
func NewStore(root string) (*Store, error) {
	for _, dir := range []string{
		root,
		filepath.Join(root, containerDir),
		filepath.Join(root, reconstructionDir),
		filepath.Join(root, tmpDir),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating store directory %s: %w", dir, err)
		}
	}
	return &Store{root: root}, nil
}

// StoreResult is returned by [Store.Write] with metadata about the
// stored artifact.
type StoreResult struct {
	// FileHash is the file-domain BLAKE3 hash (the artifact identity).
	FileHash Hash

	// Ref is the short artifact reference (art-<12 hex chars>).
	Ref string

	// Size is the total uncompressed content size in bytes.
	Size int64

	// ChunkCount is the number of chunks the content was split into.
	ChunkCount int

	// ContainerCount is the number of containers written.
	ContainerCount int

	// CompressedSize is the total compressed data size on disk
	// (container headers + compressed chunk data).
	CompressedSize int64

	// Compression is the compression algorithm selected for this
	// artifact. Individual chunks may fall back to CompressionNone if
	// the selected algorithm produces output larger than the input.
	// Per-chunk compression tags are recorded in the container format.
	Compression CompressionTag
}

// Write ingests content from r, chunks it, compresses it, packs it
// into containers, and writes everything to disk. Returns metadata
// about the stored artifact.
//
// The contentType parameter is used for compression auto-selection.
// Pass an empty string to always probe the first chunk.
//
// If compressionOverride is non-nil, it overrides the auto-selected
// compression algorithm for all chunks.
func (s *Store) Write(r io.Reader, contentType string, compressionOverride *CompressionTag) (*StoreResult, error) {
	// Read all content into memory. The streaming version (chunking
	// as bytes arrive) will come when we need to handle multi-GB
	// artifacts; for now, the in-memory path is simpler and correct.
	content, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading content: %w", err)
	}

	if len(content) == 0 {
		return nil, fmt.Errorf("cannot store empty content")
	}

	// Determine compression algorithm.
	var compression CompressionTag
	if compressionOverride != nil {
		compression = *compressionOverride
	} else {
		// Probe up to the first TargetChunkSize bytes for auto-selection.
		probeSize := len(content)
		if probeSize > TargetChunkSize {
			probeSize = TargetChunkSize
		}
		compression = SelectCompression(content[:probeSize], contentType)
	}

	// Small artifact fast path: no CDC, single chunk.
	if len(content) <= SmallArtifactThreshold {
		return s.writeSmall(content, compression)
	}

	return s.writeLarge(content, compression)
}

// writeSmall stores a small artifact as a single chunk in a single
// container. No CDC overhead.
func (s *Store) writeSmall(content []byte, compression CompressionTag) (*StoreResult, error) {
	chunkHash := HashChunk(content)
	fileHash := HashFile(chunkHash)

	// Compress the single chunk.
	compressed, actualTag, err := compressWithFallback(content, compression)
	if err != nil {
		return nil, fmt.Errorf("compressing small artifact: %w", err)
	}

	// Build and write a single-chunk container.
	builder := NewContainerBuilder()
	builder.AddChunk(chunkHash, compressed, actualTag, uint32(len(content)))

	containerHash, containerSize, err := s.flushContainer(builder)
	if err != nil {
		return nil, fmt.Errorf("writing container: %w", err)
	}

	// Write reconstruction record.
	record := &ReconstructionRecord{
		Version:    ReconstructionRecordVersion,
		FileHash:   fileHash,
		Size:       int64(len(content)),
		ChunkCount: 1,
		Segments: []Segment{{
			Container:  containerHash,
			StartIndex: 0,
			ChunkCount: 1,
		}},
	}

	if err := s.writeReconstruction(fileHash, record); err != nil {
		return nil, err
	}

	return &StoreResult{
		FileHash:       fileHash,
		Ref:            FormatRef(fileHash),
		Size:           int64(len(content)),
		ChunkCount:     1,
		ContainerCount: 1,
		CompressedSize: containerSize,
		Compression:    actualTag,
	}, nil
}

// writeLarge stores a large artifact using CDC chunking, potentially
// spanning multiple containers.
func (s *Store) writeLarge(content []byte, compression CompressionTag) (*StoreResult, error) {
	chunker := NewChunker(content)

	var (
		allChunkHashes  []Hash
		segments        []Segment
		builder         = NewContainerBuilder()
		containerStart  = 0 // chunk index where current container started
		totalCompressed int64
	)

	flushCurrentContainer := func() error {
		if builder.ChunkCount() == 0 {
			return nil
		}

		containerHash, containerSize, err := s.flushContainer(builder)
		if err != nil {
			return err
		}

		totalCompressed += containerSize

		segments = append(segments, Segment{
			Container:  containerHash,
			StartIndex: 0,
			ChunkCount: len(allChunkHashes) - containerStart,
		})

		containerStart = len(allChunkHashes)
		return nil
	}

	for {
		chunk := chunker.Next()
		if chunk == nil {
			break
		}

		allChunkHashes = append(allChunkHashes, chunk.Hash)

		compressed, actualTag, err := compressWithFallback(chunk.Data, compression)
		if err != nil {
			return nil, fmt.Errorf("compressing chunk %d: %w", len(allChunkHashes)-1, err)
		}

		builder.AddChunk(chunk.Hash, compressed, actualTag, uint32(len(chunk.Data)))

		// Flush container when it reaches the chunk or size limit.
		if builder.ChunkCount() >= MaxContainerChunks || builder.DataSize() >= MaxContainerSize {
			if err := flushCurrentContainer(); err != nil {
				return nil, err
			}
		}
	}

	// Flush the final container.
	if err := flushCurrentContainer(); err != nil {
		return nil, err
	}

	// Compute file hash.
	merkleRoot := MerkleRoot(chunkDomainKey, allChunkHashes)
	fileHash := HashFile(merkleRoot)

	// Write reconstruction record.
	record := &ReconstructionRecord{
		Version:    ReconstructionRecordVersion,
		FileHash:   fileHash,
		Size:       int64(len(content)),
		ChunkCount: len(allChunkHashes),
		Segments:   segments,
	}

	if err := s.writeReconstruction(fileHash, record); err != nil {
		return nil, err
	}

	return &StoreResult{
		FileHash:       fileHash,
		Ref:            FormatRef(fileHash),
		Size:           int64(len(content)),
		ChunkCount:     len(allChunkHashes),
		ContainerCount: len(segments),
		CompressedSize: totalCompressed,
		Compression:    compression,
	}, nil
}

// Read reconstructs an artifact from its containers and writes the
// content to w. Returns the total number of bytes written.
func (s *Store) Read(fileHash Hash, w io.Writer) (int64, error) {
	record, err := s.readReconstruction(fileHash)
	if err != nil {
		return 0, err
	}

	if err := record.Validate(); err != nil {
		return 0, fmt.Errorf("invalid reconstruction record: %w", err)
	}

	if record.FileHash != fileHash {
		return 0, fmt.Errorf("reconstruction record file hash %s does not match requested %s",
			FormatHash(record.FileHash), FormatHash(fileHash))
	}

	var totalWritten int64
	var allChunkHashes []Hash

	for segmentIndex, segment := range record.Segments {
		containerPath := s.ContainerPath(segment.Container)
		file, err := os.Open(containerPath)
		if err != nil {
			return totalWritten, fmt.Errorf("opening container %s: %w",
				FormatHash(segment.Container), err)
		}

		cr, err := ReadContainerIndex(file)
		if err != nil {
			file.Close()
			return totalWritten, fmt.Errorf("reading container %s index: %w",
				FormatHash(segment.Container), err)
		}

		// Verify container hash matches what the reconstruction record expects.
		if cr.Hash != segment.Container {
			file.Close()
			return totalWritten, fmt.Errorf("container hash mismatch for segment %d: expected %s, got %s",
				segmentIndex, FormatHash(segment.Container), FormatHash(cr.Hash))
		}

		// Extract the chunk range for this segment.
		for i := 0; i < segment.ChunkCount; i++ {
			chunkIndex := segment.StartIndex + i
			if chunkIndex >= len(cr.Index) {
				file.Close()
				return totalWritten, fmt.Errorf("segment %d: chunk index %d out of range (container has %d chunks)",
					segmentIndex, chunkIndex, len(cr.Index))
			}

			decompressed, err := cr.ExtractChunk(file, chunkIndex)
			if err != nil {
				file.Close()
				return totalWritten, fmt.Errorf("extracting chunk %d from container %s: %w",
					chunkIndex, FormatHash(segment.Container), err)
			}

			written, err := w.Write(decompressed)
			if err != nil {
				file.Close()
				return totalWritten, fmt.Errorf("writing chunk %d: %w", chunkIndex, err)
			}
			totalWritten += int64(written)

			allChunkHashes = append(allChunkHashes, cr.Index[chunkIndex].Hash)
		}

		file.Close()
	}

	// Verify the file hash.
	merkleRoot := MerkleRoot(chunkDomainKey, allChunkHashes)
	computedFileHash := HashFile(merkleRoot)
	if computedFileHash != fileHash {
		return totalWritten, fmt.Errorf("file hash verification failed: expected %s, computed %s",
			FormatHash(fileHash), FormatHash(computedFileHash))
	}

	return totalWritten, nil
}

// Exists checks whether an artifact's reconstruction record exists
// on disk.
func (s *Store) Exists(fileHash Hash) bool {
	path := s.reconstructionPath(fileHash)
	_, err := os.Stat(path)
	return err == nil
}

// Stat returns metadata about a stored artifact without reading its
// content. Returns os.ErrNotExist if the artifact is not stored.
func (s *Store) Stat(fileHash Hash) (*ReconstructionRecord, error) {
	return s.readReconstruction(fileHash)
}

// flushContainer writes a container to disk via atomic rename through
// the tmp directory. Returns the container hash and total bytes
// written.
func (s *Store) flushContainer(builder *ContainerBuilder) (Hash, int64, error) {
	// Write to a temp file first.
	tmpFile, err := os.CreateTemp(filepath.Join(s.root, tmpDir), "container-*.bin")
	if err != nil {
		return Hash{}, 0, fmt.Errorf("creating temp container file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Clean up the temp file on any error path.
	success := false
	defer func() {
		if !success {
			os.Remove(tmpPath)
		}
	}()

	containerHash, err := builder.Flush(tmpFile)
	if err != nil {
		tmpFile.Close()
		return Hash{}, 0, fmt.Errorf("flushing container: %w", err)
	}

	// Get the file size before closing.
	info, err := tmpFile.Stat()
	if err != nil {
		tmpFile.Close()
		return Hash{}, 0, fmt.Errorf("stating temp container: %w", err)
	}
	containerSize := info.Size()

	if err := tmpFile.Close(); err != nil {
		return Hash{}, 0, fmt.Errorf("closing temp container: %w", err)
	}

	// Atomic rename to final location.
	finalPath := s.ContainerPath(containerHash)

	// Create the shard directory if needed.
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return Hash{}, 0, fmt.Errorf("creating container shard directory: %w", err)
	}

	// If the container already exists (dedup: same content produces
	// same hash), remove the temp file and return success. The
	// existing container is identical by construction.
	if _, err := os.Stat(finalPath); err == nil {
		os.Remove(tmpPath)
		success = true
		return containerHash, containerSize, nil
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		return Hash{}, 0, fmt.Errorf("renaming container to %s: %w", finalPath, err)
	}

	success = true
	return containerHash, containerSize, nil
}

// writeReconstruction writes a reconstruction record to disk via
// atomic rename.
func (s *Store) writeReconstruction(fileHash Hash, record *ReconstructionRecord) error {
	data, err := MarshalReconstruction(record)
	if err != nil {
		return fmt.Errorf("marshaling reconstruction record: %w", err)
	}

	// Write to temp file.
	tmpFile, err := os.CreateTemp(filepath.Join(s.root, tmpDir), "recon-*.cbor")
	if err != nil {
		return fmt.Errorf("creating temp reconstruction file: %w", err)
	}
	tmpPath := tmpFile.Name()

	success := false
	defer func() {
		if !success {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("writing reconstruction data: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing reconstruction file: %w", err)
	}

	// Atomic rename to final location.
	finalPath := s.reconstructionPath(fileHash)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return fmt.Errorf("creating reconstruction shard directory: %w", err)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("renaming reconstruction to %s: %w", finalPath, err)
	}

	success = true
	return nil
}

// readReconstruction reads a reconstruction record from disk.
func (s *Store) readReconstruction(fileHash Hash) (*ReconstructionRecord, error) {
	path := s.reconstructionPath(fileHash)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading reconstruction record for %s: %w",
			FormatHash(fileHash), err)
	}
	return UnmarshalReconstruction(data)
}

// ContainerPath returns the sharded filesystem path for a container.
// Containers are sharded by the first two bytes of the hash hex:
// containers/a3/f9/a3f9b2c1e7d4...
func (s *Store) ContainerPath(hash Hash) string {
	hex := FormatHash(hash)
	return filepath.Join(s.root, containerDir, hex[:2], hex[2:4], hex)
}

// reconstructionPath returns the sharded filesystem path for a
// reconstruction record.
func (s *Store) reconstructionPath(fileHash Hash) string {
	hex := FormatHash(fileHash)
	return filepath.Join(s.root, reconstructionDir, hex[:2], hex[2:4], hex+".cbor")
}

// Delete removes an artifact's reconstruction record and any
// containers that are not in the liveContainers set. Containers
// shared by other artifacts (whose hashes appear in liveContainers)
// are preserved. Returns the list of container hashes that were
// actually deleted from disk.
//
// The caller must hold the write mutex. The caller is responsible
// for building the live set from all remaining artifacts' reconstruction
// records before calling Delete.
func (s *Store) Delete(fileHash Hash, liveContainers map[Hash]struct{}) ([]Hash, error) {
	record, err := s.readReconstruction(fileHash)
	if err != nil {
		return nil, fmt.Errorf("reading reconstruction record for deletion: %w", err)
	}

	// Remove the reconstruction record first.
	reconPath := s.reconstructionPath(fileHash)
	if err := os.Remove(reconPath); err != nil {
		return nil, fmt.Errorf("removing reconstruction record %s: %w", FormatHash(fileHash), err)
	}

	// Remove containers not in the live set.
	var deletedContainers []Hash
	for _, segment := range record.Segments {
		if _, live := liveContainers[segment.Container]; live {
			continue
		}
		containerPath := s.ContainerPath(segment.Container)
		if err := os.Remove(containerPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return deletedContainers, fmt.Errorf("removing container %s: %w",
				FormatHash(segment.Container), err)
		}
		deletedContainers = append(deletedContainers, segment.Container)
	}

	return deletedContainers, nil
}

// compressWithFallback attempts to compress data with the given
// algorithm. If the data is incompressible, falls back to
// CompressionNone and returns the original data.
func compressWithFallback(data []byte, tag CompressionTag) ([]byte, CompressionTag, error) {
	if tag == CompressionNone {
		return data, CompressionNone, nil
	}

	compressed, err := CompressChunk(data, tag)
	if err != nil {
		if IsIncompressible(err) {
			return data, CompressionNone, nil
		}
		return nil, 0, err
	}
	return compressed, tag, nil
}

// ReadContent is a convenience function that reads an artifact into
// a byte slice. For large artifacts, prefer [Store.Read] with a
// streaming writer.
func (s *Store) ReadContent(fileHash Hash) ([]byte, error) {
	var buffer bytes.Buffer
	if _, err := s.Read(fileHash, &buffer); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// WriteContent is a convenience function that stores content from a
// byte slice. For large artifacts, prefer [Store.Write] with a
// streaming reader.
func (s *Store) WriteContent(content []byte, contentType string) (*StoreResult, error) {
	return s.Write(bytes.NewReader(content), contentType, nil)
}
