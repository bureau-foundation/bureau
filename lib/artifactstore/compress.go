// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifactstore

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

// CompressionTag identifies the compression algorithm used for a
// chunk. Tags are stored in container chunk headers (1 byte each).
// These values are protocol constants — changing them breaks
// container format compatibility.
type CompressionTag uint8

const (
	// CompressionNone indicates uncompressed data. Used for
	// already-compressed content (PNG, video, zlib packfiles) where
	// compression adds CPU cost without reducing size.
	CompressionNone CompressionTag = 0

	// CompressionLZ4 indicates LZ4 block compression. Fast default
	// for binary data (~1.5-2x ratio, ~4 GB/s decode). Good
	// tradeoff between compression ratio and CPU cost when content
	// type is unknown or mixed.
	CompressionLZ4 CompressionTag = 1

	// CompressionZstd indicates zstd compression at level 3.
	// Better ratios for text, JSON, logs, SQL, configs (~3-5x
	// ratio, ~1.5 GB/s decode). Used when content is known to be
	// text-like.
	CompressionZstd CompressionTag = 2

	// CompressionBG4LZ4 indicates ByteGrouping4 + LZ4 compression
	// for float32 tensor data. Transposes 4-byte groups so that
	// similar-magnitude floats have their bytes grouped by position,
	// then applies LZ4. Effective for arrays of float32 values
	// where adjacent values are close in magnitude (common in
	// neural network weights).
	CompressionBG4LZ4 CompressionTag = 3
)

// String returns the human-readable name of a compression tag.
func (tag CompressionTag) String() string {
	switch tag {
	case CompressionNone:
		return "none"
	case CompressionLZ4:
		return "lz4"
	case CompressionZstd:
		return "zstd"
	case CompressionBG4LZ4:
		return "bg4_lz4"
	default:
		return fmt.Sprintf("unknown(%d)", tag)
	}
}

// ParseCompressionTag parses a compression tag from its string
// representation.
func ParseCompressionTag(name string) (CompressionTag, error) {
	switch name {
	case "none":
		return CompressionNone, nil
	case "lz4":
		return CompressionLZ4, nil
	case "zstd":
		return CompressionZstd, nil
	case "bg4_lz4":
		return CompressionBG4LZ4, nil
	default:
		return 0, fmt.Errorf("unknown compression tag: %q", name)
	}
}

// CompressChunk compresses data using the specified algorithm.
// Returns the compressed bytes. For CompressionNone, returns the
// input unchanged (no copy).
func CompressChunk(data []byte, tag CompressionTag) ([]byte, error) {
	switch tag {
	case CompressionNone:
		return data, nil

	case CompressionLZ4:
		return compressLZ4(data)

	case CompressionZstd:
		return compressZstd(data)

	case CompressionBG4LZ4:
		return compressBG4LZ4(data)

	default:
		return nil, fmt.Errorf("unsupported compression tag: %d", tag)
	}
}

// DecompressChunk decompresses data that was compressed with the
// specified algorithm. The uncompressedSize must match the original
// data length exactly — this is verified and a mismatch returns an
// error.
func DecompressChunk(compressed []byte, tag CompressionTag, uncompressedSize int) ([]byte, error) {
	switch tag {
	case CompressionNone:
		if len(compressed) != uncompressedSize {
			return nil, fmt.Errorf("uncompressed chunk: size %d does not match expected %d",
				len(compressed), uncompressedSize)
		}
		return compressed, nil

	case CompressionLZ4:
		return decompressLZ4(compressed, uncompressedSize)

	case CompressionZstd:
		return decompressZstd(compressed, uncompressedSize)

	case CompressionBG4LZ4:
		return decompressBG4LZ4(compressed, uncompressedSize)

	default:
		return nil, fmt.Errorf("unsupported compression tag: %d", tag)
	}
}

// LZ4 compression: block-mode LZ4.

func compressLZ4(data []byte) ([]byte, error) {
	// CompressBlockBound returns the maximum compressed size.
	bound := lz4.CompressBlockBound(len(data))
	destination := make([]byte, bound)

	written, err := lz4.CompressBlock(data, destination, nil)
	if err != nil {
		return nil, fmt.Errorf("lz4 compress: %w", err)
	}

	// CompressBlock returns 0 when it determines the data is
	// incompressible. We also check whether the compressed output
	// is actually smaller than the input — if not, compression is
	// not worthwhile.
	if written == 0 || written >= len(data) {
		return nil, errIncompressible
	}

	return destination[:written], nil
}

func decompressLZ4(compressed []byte, uncompressedSize int) ([]byte, error) {
	destination := make([]byte, uncompressedSize)
	read, err := lz4.UncompressBlock(compressed, destination)
	if err != nil {
		return nil, fmt.Errorf("lz4 decompress: %w", err)
	}
	if read != uncompressedSize {
		return nil, fmt.Errorf("lz4 decompress: got %d bytes, expected %d", read, uncompressedSize)
	}
	return destination, nil
}

// Zstd compression: level 3 (the "default" level — good ratio
// without excessive CPU).

// zstdEncoder and zstdDecoder are reused across calls to avoid
// repeated initialization overhead. zstd.Encoder and zstd.Decoder
// are safe for concurrent use.
var (
	zstdEncoder *zstd.Encoder
	zstdDecoder *zstd.Decoder
)

func init() {
	var err error
	zstdEncoder, err = zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
	)
	if err != nil {
		panic("artifact: zstd encoder initialization failed: " + err.Error())
	}

	zstdDecoder, err = zstd.NewReader(nil)
	if err != nil {
		panic("artifact: zstd decoder initialization failed: " + err.Error())
	}
}

func compressZstd(data []byte) ([]byte, error) {
	compressed := zstdEncoder.EncodeAll(data, nil)
	if len(compressed) >= len(data) {
		return nil, errIncompressible
	}
	return compressed, nil
}

func decompressZstd(compressed []byte, uncompressedSize int) ([]byte, error) {
	destination := make([]byte, 0, uncompressedSize)
	result, err := zstdDecoder.DecodeAll(compressed, destination)
	if err != nil {
		return nil, fmt.Errorf("zstd decompress: %w", err)
	}
	if len(result) != uncompressedSize {
		return nil, fmt.Errorf("zstd decompress: got %d bytes, expected %d", len(result), uncompressedSize)
	}
	return result, nil
}

// ByteGrouping4 + LZ4: transpose float32 data by byte position
// before LZ4 compression. Groups all byte-0s together, then all
// byte-1s, etc. This exploits the fact that adjacent float32 values
// in neural network weights tend to have similar exponents, making
// the high-order bytes highly compressible.

func compressBG4LZ4(data []byte) ([]byte, error) {
	transposed := bg4Transpose(data)
	compressed, err := compressLZ4(transposed)
	if err != nil {
		return nil, err
	}
	return compressed, nil
}

func decompressBG4LZ4(compressed []byte, uncompressedSize int) ([]byte, error) {
	transposed, err := decompressLZ4(compressed, uncompressedSize)
	if err != nil {
		return nil, err
	}
	return bg4Untranspose(transposed), nil
}

// bg4Transpose rearranges data so that all byte-position-0 values
// come first, then all byte-position-1 values, etc., in groups of 4.
// If the input length is not a multiple of 4, trailing bytes are
// appended as-is after the transposed groups.
func bg4Transpose(data []byte) []byte {
	length := len(data)
	groupCount := length / 4
	remainder := length % 4

	output := make([]byte, length)

	// Transpose the aligned portion.
	for i := 0; i < groupCount; i++ {
		output[i] = data[i*4]
		output[groupCount+i] = data[i*4+1]
		output[groupCount*2+i] = data[i*4+2]
		output[groupCount*3+i] = data[i*4+3]
	}

	// Append any trailing bytes unchanged.
	for i := 0; i < remainder; i++ {
		output[groupCount*4+i] = data[groupCount*4+i]
	}

	return output
}

// bg4Untranspose reverses the bg4Transpose operation.
func bg4Untranspose(data []byte) []byte {
	length := len(data)
	groupCount := length / 4
	remainder := length % 4

	output := make([]byte, length)

	// Untranspose the aligned portion.
	for i := 0; i < groupCount; i++ {
		output[i*4] = data[i]
		output[i*4+1] = data[groupCount+i]
		output[i*4+2] = data[groupCount*2+i]
		output[i*4+3] = data[groupCount*3+i]
	}

	// Append any trailing bytes unchanged.
	for i := 0; i < remainder; i++ {
		output[groupCount*4+i] = data[groupCount*4+i]
	}

	return output
}

// errIncompressible is returned by compression functions when the
// compressed output is not smaller than the input. The caller should
// fall back to CompressionNone.
var errIncompressible = fmt.Errorf("data is incompressible")

// IsIncompressible returns true if the error indicates that data
// could not be compressed smaller than its original size.
func IsIncompressible(err error) bool {
	return err == errIncompressible
}

// SelectCompression probes the first chunk of data to determine the
// best compression algorithm. It tries zstd first: if the ratio
// exceeds 1.5x, zstd is selected. If the ratio is between 1.1x and
// 1.5x, LZ4 is selected (faster with acceptable ratio). Below 1.1x,
// the data is considered incompressible.
//
// The contentType parameter allows short-circuiting the probe for
// known content types. If empty, probing is always performed.
func SelectCompression(data []byte, contentType string) CompressionTag {
	// Short-circuit for known content types.
	switch contentType {
	case "text/plain", "text/html", "text/css", "text/csv",
		"text/xml", "text/markdown",
		"application/json", "application/x-ndjson",
		"application/sql", "application/x-sqlite3",
		"application/xml":
		return CompressionZstd

	case "application/x-safetensors":
		return CompressionBG4LZ4
	}

	// Probe: compress with zstd and check the ratio.
	if len(data) == 0 {
		return CompressionNone
	}

	compressed := zstdEncoder.EncodeAll(data, nil)
	ratio := float64(len(data)) / float64(len(compressed))

	switch {
	case ratio >= 1.5:
		return CompressionZstd
	case ratio >= 1.1:
		return CompressionLZ4
	default:
		return CompressionNone
	}
}

// CompressChunkAuto compresses data using the best algorithm for the
// given content type. Returns the compressed bytes and the tag used.
// If the data is incompressible, returns the original data with
// CompressionNone.
func CompressChunkAuto(data []byte, contentType string) ([]byte, CompressionTag, error) {
	tag := SelectCompression(data, contentType)

	compressed, err := CompressChunk(data, tag)
	if err != nil {
		if IsIncompressible(err) {
			return data, CompressionNone, nil
		}
		return nil, 0, err
	}

	return compressed, tag, nil
}

// Float32SliceToBytes converts a slice of float32 values to a byte
// slice in little-endian order. This is a helper for testing BG4+LZ4
// compression with realistic tensor data.
func Float32SliceToBytes(values []float32) []byte {
	result := make([]byte, len(values)*4)
	for i, value := range values {
		binary.LittleEndian.PutUint32(result[i*4:], math.Float32bits(value))
	}
	return result
}
