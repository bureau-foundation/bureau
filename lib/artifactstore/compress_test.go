// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifactstore

import (
	"crypto/rand"
	"fmt"
	"math"
	"testing"
)

func TestCompressionTagString(t *testing.T) {
	tests := []struct {
		tag  CompressionTag
		want string
	}{
		{CompressionNone, "none"},
		{CompressionLZ4, "lz4"},
		{CompressionZstd, "zstd"},
		{CompressionBG4LZ4, "bg4_lz4"},
		{CompressionTag(99), "unknown(99)"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := tt.tag.String()
			if got != tt.want {
				t.Errorf("CompressionTag(%d).String() = %q, want %q", tt.tag, got, tt.want)
			}
		})
	}
}

func TestParseCompressionTag(t *testing.T) {
	for _, name := range []string{"none", "lz4", "zstd", "bg4_lz4"} {
		t.Run(name, func(t *testing.T) {
			tag, err := ParseCompressionTag(name)
			if err != nil {
				t.Fatalf("ParseCompressionTag(%q) failed: %v", name, err)
			}
			if tag.String() != name {
				t.Errorf("roundtrip: ParseCompressionTag(%q).String() = %q", name, tag.String())
			}
		})
	}

	t.Run("unknown", func(t *testing.T) {
		_, err := ParseCompressionTag("gzip")
		if err == nil {
			t.Error("ParseCompressionTag(\"gzip\") should fail")
		}
	})
}

func TestCompressDecompressNone(t *testing.T) {
	data := []byte("uncompressed data should pass through unchanged")

	compressed, err := CompressChunk(data, CompressionNone)
	if err != nil {
		t.Fatalf("CompressChunk(none) failed: %v", err)
	}

	// For CompressionNone, the compressed output should be the same slice.
	if &compressed[0] != &data[0] {
		t.Error("CompressionNone should return the same slice, not a copy")
	}

	decompressed, err := DecompressChunk(compressed, CompressionNone, len(data))
	if err != nil {
		t.Fatalf("DecompressChunk(none) failed: %v", err)
	}

	if string(decompressed) != string(data) {
		t.Error("none compression roundtrip failed")
	}
}

func TestCompressDecompressNoneSizeMismatch(t *testing.T) {
	data := []byte("five bytes extra")

	_, err := DecompressChunk(data, CompressionNone, len(data)+5)
	if err == nil {
		t.Error("DecompressChunk(none) should fail when size does not match")
	}
}

func TestCompressDecompressLZ4(t *testing.T) {
	// Compressible data: repeated pattern.
	data := make([]byte, 64*1024)
	for i := range data {
		data[i] = byte(i % 17)
	}

	compressed, err := CompressChunk(data, CompressionLZ4)
	if err != nil {
		t.Fatalf("CompressChunk(lz4) failed: %v", err)
	}

	if len(compressed) >= len(data) {
		t.Errorf("LZ4 did not compress: %d bytes → %d bytes", len(data), len(compressed))
	}

	decompressed, err := DecompressChunk(compressed, CompressionLZ4, len(data))
	if err != nil {
		t.Fatalf("DecompressChunk(lz4) failed: %v", err)
	}

	for i := range data {
		if decompressed[i] != data[i] {
			t.Fatalf("LZ4 roundtrip mismatch at byte %d", i)
		}
	}
}

func TestCompressDecompressZstd(t *testing.T) {
	// Text-like data: JSON.
	data := []byte(`{"event_type":"m.bureau.artifact","content":{"hash":"abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890","content_type":"application/json","size":12345}}`)
	// Repeat it to get a reasonable chunk size.
	repeated := make([]byte, 0, 64*1024)
	for len(repeated) < 64*1024 {
		repeated = append(repeated, data...)
	}

	compressed, err := CompressChunk(repeated, CompressionZstd)
	if err != nil {
		t.Fatalf("CompressChunk(zstd) failed: %v", err)
	}

	if len(compressed) >= len(repeated) {
		t.Errorf("Zstd did not compress: %d bytes → %d bytes", len(repeated), len(compressed))
	}

	ratio := float64(len(repeated)) / float64(len(compressed))
	if ratio < 2.0 {
		t.Errorf("Zstd compression ratio %.2fx is unexpectedly low for repetitive JSON", ratio)
	}

	decompressed, err := DecompressChunk(compressed, CompressionZstd, len(repeated))
	if err != nil {
		t.Fatalf("DecompressChunk(zstd) failed: %v", err)
	}

	for i := range repeated {
		if decompressed[i] != repeated[i] {
			t.Fatalf("Zstd roundtrip mismatch at byte %d", i)
		}
	}
}

func TestCompressIncompressibleLZ4(t *testing.T) {
	// Random data is incompressible.
	data := make([]byte, 64*1024)
	rand.Read(data)

	_, err := CompressChunk(data, CompressionLZ4)
	if err == nil {
		t.Fatal("LZ4 should return incompressible error for random data")
	}
	if !IsIncompressible(err) {
		t.Errorf("expected incompressible error, got: %v", err)
	}
}

func TestCompressIncompressibleZstd(t *testing.T) {
	// Random data is incompressible for zstd too, but zstd's framing
	// overhead means small random chunks may barely exceed input size.
	data := make([]byte, 64*1024)
	rand.Read(data)

	_, err := CompressChunk(data, CompressionZstd)
	if err == nil {
		t.Fatal("Zstd should return incompressible error for random data")
	}
	if !IsIncompressible(err) {
		t.Errorf("expected incompressible error, got: %v", err)
	}
}

func TestCompressDecompressBG4LZ4(t *testing.T) {
	// Generate fake float32 tensor data: values close in magnitude
	// (simulating a neural network layer's weights).
	values := make([]float32, 64*1024/4)
	for i := range values {
		// Weights clustered around 0.0 with small variance.
		values[i] = float32(i%256-128) * 0.001
	}
	data := Float32SliceToBytes(values)

	compressed, err := CompressChunk(data, CompressionBG4LZ4)
	if err != nil {
		// BG4+LZ4 on synthetic tensor data may or may not compress
		// depending on the patterns. If incompressible, that's valid.
		if IsIncompressible(err) {
			t.Skip("BG4+LZ4: synthetic data was incompressible")
		}
		t.Fatalf("CompressChunk(bg4_lz4) failed: %v", err)
	}

	decompressed, err := DecompressChunk(compressed, CompressionBG4LZ4, len(data))
	if err != nil {
		t.Fatalf("DecompressChunk(bg4_lz4) failed: %v", err)
	}

	for i := range data {
		if decompressed[i] != data[i] {
			t.Fatalf("BG4+LZ4 roundtrip mismatch at byte %d", i)
		}
	}
}

func TestBG4TransposeRoundtrip(t *testing.T) {
	// Test the transpose/untranspose cycle independently of compression.
	sizes := []int{0, 1, 3, 4, 7, 8, 12, 100, 1024, 65537}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("size=%d", size), func(t *testing.T) {
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i * 37)
			}

			transposed := bg4Transpose(data)
			if len(transposed) != len(data) {
				t.Fatalf("transposed length %d != original %d", len(transposed), len(data))
			}

			recovered := bg4Untranspose(transposed)
			if len(recovered) != len(data) {
				t.Fatalf("recovered length %d != original %d", len(recovered), len(data))
			}

			for i := range data {
				if recovered[i] != data[i] {
					t.Fatalf("BG4 roundtrip mismatch at byte %d (size=%d)", i, size)
				}
			}
		})
	}
}

func TestBG4TransposeGrouping(t *testing.T) {
	// Verify that transpose actually groups bytes by position.
	// Input: [A0 A1 A2 A3] [B0 B1 B2 B3] [C0 C1 C2 C3]
	// Expected: [A0 B0 C0] [A1 B1 C1] [A2 B2 C2] [A3 B3 C3]
	input := []byte{
		0x10, 0x11, 0x12, 0x13, // float "A"
		0x20, 0x21, 0x22, 0x23, // float "B"
		0x30, 0x31, 0x32, 0x33, // float "C"
	}

	expected := []byte{
		0x10, 0x20, 0x30, // byte-position 0 from each group
		0x11, 0x21, 0x31, // byte-position 1
		0x12, 0x22, 0x32, // byte-position 2
		0x13, 0x23, 0x33, // byte-position 3
	}

	transposed := bg4Transpose(input)
	for i := range expected {
		if transposed[i] != expected[i] {
			t.Errorf("bg4Transpose[%d] = 0x%02x, want 0x%02x", i, transposed[i], expected[i])
		}
	}
}

func TestFloat32SliceToBytes(t *testing.T) {
	values := []float32{1.0, -1.0, 0.0, math.MaxFloat32}
	result := Float32SliceToBytes(values)

	if len(result) != 16 {
		t.Fatalf("Float32SliceToBytes: got %d bytes, want 16", len(result))
	}

	// Verify 1.0 in little-endian: 0x3F800000 → [00 00 80 3F]
	if result[0] != 0x00 || result[1] != 0x00 || result[2] != 0x80 || result[3] != 0x3F {
		t.Errorf("Float32SliceToBytes(1.0) = [%02x %02x %02x %02x], want [00 00 80 3F]",
			result[0], result[1], result[2], result[3])
	}
}

func TestSelectCompressionKnownTypes(t *testing.T) {
	// Known text types should return zstd without probing.
	textTypes := []string{
		"text/plain", "application/json", "application/sql",
		"application/x-ndjson", "application/xml",
	}
	for _, contentType := range textTypes {
		tag := SelectCompression(nil, contentType)
		if tag != CompressionZstd {
			t.Errorf("SelectCompression(contentType=%q) = %s, want zstd", contentType, tag)
		}
	}

	// Safetensors should return BG4+LZ4.
	tag := SelectCompression(nil, "application/x-safetensors")
	if tag != CompressionBG4LZ4 {
		t.Errorf("SelectCompression(safetensors) = %s, want bg4_lz4", tag)
	}
}

func TestSelectCompressionProbe(t *testing.T) {
	// Highly compressible data: should select zstd.
	compressible := make([]byte, 64*1024)
	for i := range compressible {
		compressible[i] = byte(i % 5)
	}
	tag := SelectCompression(compressible, "")
	if tag != CompressionZstd {
		t.Errorf("SelectCompression(compressible) = %s, want zstd", tag)
	}

	// Random data: should select none.
	random := make([]byte, 64*1024)
	rand.Read(random)
	tag = SelectCompression(random, "")
	if tag != CompressionNone {
		t.Errorf("SelectCompression(random) = %s, want none", tag)
	}
}

func TestSelectCompressionEmpty(t *testing.T) {
	tag := SelectCompression(nil, "")
	if tag != CompressionNone {
		t.Errorf("SelectCompression(empty) = %s, want none", tag)
	}
}

func TestCompressChunkAutoFallback(t *testing.T) {
	// Random data: CompressChunkAuto should fall back to CompressionNone.
	data := make([]byte, 64*1024)
	rand.Read(data)

	compressed, tag, err := CompressChunkAuto(data, "")
	if err != nil {
		t.Fatalf("CompressChunkAuto failed: %v", err)
	}

	if tag != CompressionNone {
		t.Errorf("tag = %s, want none for random data", tag)
	}

	// For CompressionNone, compressed should be the original data.
	if len(compressed) != len(data) {
		t.Errorf("compressed size %d != original %d for none", len(compressed), len(data))
	}
}

func TestCompressChunkUnsupportedTag(t *testing.T) {
	_, err := CompressChunk([]byte("data"), CompressionTag(99))
	if err == nil {
		t.Error("CompressChunk with unknown tag should fail")
	}
}

func TestDecompressChunkUnsupportedTag(t *testing.T) {
	_, err := DecompressChunk([]byte("data"), CompressionTag(99), 4)
	if err == nil {
		t.Error("DecompressChunk with unknown tag should fail")
	}
}

// Benchmarks for compression. Run with:
//
//	bazel run //lib/artifact:artifact_test -- \
//	    -test.bench=BenchmarkCompress -test.benchmem -test.count=10 -test.run='^$'

func BenchmarkCompressLZ4(b *testing.B) {
	data := make([]byte, 64*1024)
	for i := range data {
		data[i] = byte(i % 17)
	}

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		CompressChunk(data, CompressionLZ4)
	}
}

func BenchmarkDecompressLZ4(b *testing.B) {
	data := make([]byte, 64*1024)
	for i := range data {
		data[i] = byte(i % 17)
	}
	compressed, err := CompressChunk(data, CompressionLZ4)
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		DecompressChunk(compressed, CompressionLZ4, len(data))
	}
}

func BenchmarkCompressZstd(b *testing.B) {
	data := make([]byte, 64*1024)
	for i := range data {
		data[i] = byte(i % 17)
	}

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		CompressChunk(data, CompressionZstd)
	}
}

func BenchmarkDecompressZstd(b *testing.B) {
	data := make([]byte, 64*1024)
	for i := range data {
		data[i] = byte(i % 17)
	}
	compressed, err := CompressChunk(data, CompressionZstd)
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		DecompressChunk(compressed, CompressionZstd, len(data))
	}
}

func BenchmarkBG4Transpose(b *testing.B) {
	data := make([]byte, 64*1024)
	for i := range data {
		data[i] = byte(i)
	}

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		bg4Transpose(data)
	}
}
