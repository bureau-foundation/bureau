// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/codec"
)

// --- FrameWriter tests ---

func TestFrameWriterSingleFrame(t *testing.T) {
	var buffer bytes.Buffer
	fw := NewFrameWriter(&buffer)

	data := []byte("hello frames")
	n, err := fw.Write(data)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(data) {
		t.Errorf("Write returned %d, want %d", n, len(data))
	}

	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}

	// Verify wire format: 4-byte length + data + 4-byte zero terminator.
	raw := buffer.Bytes()
	expectedLength := 4 + len(data) + 4
	if len(raw) != expectedLength {
		t.Fatalf("wire bytes = %d, want %d", len(raw), expectedLength)
	}

	frameLength := binary.BigEndian.Uint32(raw[0:4])
	if frameLength != uint32(len(data)) {
		t.Errorf("frame length = %d, want %d", frameLength, len(data))
	}
	if !bytes.Equal(raw[4:4+len(data)], data) {
		t.Error("frame data mismatch")
	}

	terminator := binary.BigEndian.Uint32(raw[4+len(data):])
	if terminator != 0 {
		t.Errorf("terminator = %d, want 0", terminator)
	}
}

func TestFrameWriterMultipleWrites(t *testing.T) {
	var buffer bytes.Buffer
	fw := NewFrameWriter(&buffer)

	fw.Write([]byte("aaa"))
	fw.Write([]byte("bbb"))
	fw.Write([]byte("ccc"))
	fw.Close()

	// Read back via FrameReader.
	fr := NewFrameReader(&buffer)
	result, err := io.ReadAll(fr)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != "aaabbbccc" {
		t.Errorf("round-trip = %q, want %q", result, "aaabbbccc")
	}
}

func TestFrameWriterLargeWriteSplitsFrames(t *testing.T) {
	var buffer bytes.Buffer
	fw := NewFrameWriter(&buffer)

	// Write more than MaxFrameSize in one call.
	data := bytes.Repeat([]byte("X"), MaxFrameSize+500)
	n, err := fw.Write(data)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(data) {
		t.Errorf("Write returned %d, want %d", n, len(data))
	}
	fw.Close()

	// Verify we get at least two frames.
	raw := buffer.Bytes()
	firstFrameLength := binary.BigEndian.Uint32(raw[0:4])
	if firstFrameLength != uint32(MaxFrameSize) {
		t.Errorf("first frame = %d bytes, want %d", firstFrameLength, MaxFrameSize)
	}

	secondFrameLength := binary.BigEndian.Uint32(raw[4+MaxFrameSize : 4+MaxFrameSize+4])
	if secondFrameLength != 500 {
		t.Errorf("second frame = %d bytes, want 500", secondFrameLength)
	}

	// Read back and verify content.
	reader := bytes.NewReader(buffer.Bytes())
	fr := NewFrameReader(reader)
	result, err := io.ReadAll(fr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result, data) {
		t.Error("large write round-trip data mismatch")
	}
}

func TestFrameWriterEmpty(t *testing.T) {
	var buffer bytes.Buffer
	fw := NewFrameWriter(&buffer)
	fw.Close()

	// Should only have the 4-byte zero terminator.
	if buffer.Len() != 4 {
		t.Errorf("empty frame stream = %d bytes, want 4", buffer.Len())
	}

	fr := NewFrameReader(&buffer)
	result, err := io.ReadAll(fr)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Errorf("empty stream returned %d bytes", len(result))
	}
}

func TestFrameWriterDoubleClose(t *testing.T) {
	var buffer bytes.Buffer
	fw := NewFrameWriter(&buffer)
	fw.Close()
	// Second close should be a no-op.
	if err := fw.Close(); err != nil {
		t.Errorf("second Close returned error: %v", err)
	}
	// Should still only have one terminator.
	if buffer.Len() != 4 {
		t.Errorf("buffer after double close = %d bytes, want 4", buffer.Len())
	}
}

func TestFrameWriterWriteAfterClose(t *testing.T) {
	var buffer bytes.Buffer
	fw := NewFrameWriter(&buffer)
	fw.Close()

	_, err := fw.Write([]byte("should fail"))
	if err == nil {
		t.Error("expected error writing to closed FrameWriter")
	}
}

// --- FrameReader tests ---

func TestFrameReaderSingleFrame(t *testing.T) {
	var buffer bytes.Buffer
	data := []byte("test frame data")

	// Manually construct a frame stream.
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	buffer.Write(header[:])
	buffer.Write(data)
	// Terminator.
	binary.BigEndian.PutUint32(header[:], 0)
	buffer.Write(header[:])

	fr := NewFrameReader(&buffer)
	result, err := io.ReadAll(fr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result, data) {
		t.Errorf("got %q, want %q", result, data)
	}
}

func TestFrameReaderSmallReads(t *testing.T) {
	// Write a frame, then read it in 1-byte chunks.
	var buffer bytes.Buffer
	fw := NewFrameWriter(&buffer)
	fw.Write([]byte("abcdef"))
	fw.Close()

	fr := NewFrameReader(&buffer)
	var result []byte
	oneByte := make([]byte, 1)
	for {
		n, err := fr.Read(oneByte)
		if n > 0 {
			result = append(result, oneByte[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if string(result) != "abcdef" {
		t.Errorf("got %q, want %q", result, "abcdef")
	}
}

func TestFrameReaderCrossBoundary(t *testing.T) {
	// Two frames, read in a single large Read call.
	var buffer bytes.Buffer
	fw := NewFrameWriter(&buffer)
	fw.Write([]byte("frame1"))
	fw.Write([]byte("frame2"))
	fw.Close()

	fr := NewFrameReader(&buffer)
	result := make([]byte, 100)
	totalRead := 0

	for {
		n, err := fr.Read(result[totalRead:])
		totalRead += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	if string(result[:totalRead]) != "frame1frame2" {
		t.Errorf("got %q, want %q", result[:totalRead], "frame1frame2")
	}
}

func TestFrameReaderOversizedFrame(t *testing.T) {
	var buffer bytes.Buffer
	// Write a frame header claiming MaxFrameSize + 1 bytes.
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(MaxFrameSize+1))
	buffer.Write(header[:])

	fr := NewFrameReader(&buffer)
	result := make([]byte, 10)
	_, err := fr.Read(result)
	if err == nil {
		t.Error("expected error for oversized frame")
	}
}

func TestFrameReaderAfterEOF(t *testing.T) {
	var buffer bytes.Buffer
	fw := NewFrameWriter(&buffer)
	fw.Close()

	fr := NewFrameReader(&buffer)
	result := make([]byte, 10)

	_, err := fr.Read(result)
	if err != io.EOF {
		t.Errorf("first Read after empty stream: got %v, want io.EOF", err)
	}

	// Second read should also return EOF.
	_, err = fr.Read(result)
	if err != io.EOF {
		t.Errorf("second Read: got %v, want io.EOF", err)
	}
}

// --- SizedReader tests ---

func TestSizedReaderExactRead(t *testing.T) {
	data := []byte("exactly ten!")
	sr := NewSizedReader(bytes.NewReader(data), int64(len(data)))

	result, err := io.ReadAll(sr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result, data) {
		t.Errorf("got %q, want %q", result, data)
	}
	if sr.Remaining() != 0 {
		t.Errorf("Remaining = %d, want 0", sr.Remaining())
	}
}

func TestSizedReaderPartialReads(t *testing.T) {
	data := []byte("0123456789")
	sr := NewSizedReader(bytes.NewReader(data), 10)

	// Read in two chunks.
	buffer := make([]byte, 5)
	n, err := sr.Read(buffer)
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if n != 5 || string(buffer) != "01234" {
		t.Errorf("first Read: got %d bytes %q", n, buffer[:n])
	}
	if sr.Remaining() != 5 {
		t.Errorf("Remaining after first Read = %d, want 5", sr.Remaining())
	}

	n, err = sr.Read(buffer)
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if n != 5 || string(buffer) != "56789" {
		t.Errorf("second Read: got %d bytes %q", n, buffer[:n])
	}

	// Should be done.
	_, err = sr.Read(buffer)
	if err != io.EOF {
		t.Errorf("third Read: got %v, want io.EOF", err)
	}
}

func TestSizedReaderUnderlyingShort(t *testing.T) {
	// Underlying reader has fewer bytes than claimed.
	data := []byte("short")
	sr := NewSizedReader(bytes.NewReader(data), 100)

	_, err := io.ReadAll(sr)
	if err != io.ErrUnexpectedEOF {
		t.Errorf("got %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestSizedReaderZeroSize(t *testing.T) {
	sr := NewSizedReader(strings.NewReader("ignored"), 0)

	result, err := io.ReadAll(sr)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Errorf("zero-size read returned %d bytes", len(result))
	}
}

func TestSizedReaderLargerBuffer(t *testing.T) {
	// Read buffer is larger than remaining data.
	data := []byte("abc")
	sr := NewSizedReader(bytes.NewReader(data), 3)

	buffer := make([]byte, 100)
	n, err := sr.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("Read returned %d, want 3", n)
	}
	if string(buffer[:n]) != "abc" {
		t.Errorf("got %q", buffer[:n])
	}
}

// --- DataReader tests ---

func TestDataReaderSized(t *testing.T) {
	data := []byte("sized transfer")
	reader := DataReader(bytes.NewReader(data), int64(len(data)))

	result, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result, data) {
		t.Error("sized DataReader mismatch")
	}
}

func TestDataReaderChunked(t *testing.T) {
	// Write frames, then read via DataReader with SizeUnknown.
	var buffer bytes.Buffer
	fw := NewFrameWriter(&buffer)
	fw.Write([]byte("chunked"))
	fw.Write([]byte(" data"))
	fw.Close()

	reader := DataReader(&buffer, SizeUnknown)
	result, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != "chunked data" {
		t.Errorf("chunked DataReader = %q", result)
	}
}

// --- Protocol type CBOR round-trip tests ---

func TestStoreHeaderCBORRoundtrip(t *testing.T) {
	original := StoreHeader{
		Action:      "store",
		ContentType: "application/octet-stream",
		Filename:    "model.safetensors",
		Size:        1024 * 1024 * 100,
		Description: "test model weights",
		Labels:      []string{"model", "test"},
		Tag:         "models/test/latest",
		CachePolicy: "pin",
		Visibility:  "private",
		TTL:         "168h",
	}

	data, err := codec.Marshal(&original)
	if err != nil {
		t.Fatal(err)
	}

	var decoded StoreHeader
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.Action != original.Action {
		t.Errorf("Action = %q, want %q", decoded.Action, original.Action)
	}
	if decoded.ContentType != original.ContentType {
		t.Errorf("ContentType = %q, want %q", decoded.ContentType, original.ContentType)
	}
	if decoded.Filename != original.Filename {
		t.Errorf("Filename = %q, want %q", decoded.Filename, original.Filename)
	}
	if decoded.Size != original.Size {
		t.Errorf("Size = %d, want %d", decoded.Size, original.Size)
	}
	if decoded.Tag != original.Tag {
		t.Errorf("Tag = %q, want %q", decoded.Tag, original.Tag)
	}
	if decoded.CachePolicy != original.CachePolicy {
		t.Errorf("CachePolicy = %q, want %q", decoded.CachePolicy, original.CachePolicy)
	}
	if decoded.TTL != original.TTL {
		t.Errorf("TTL = %q, want %q", decoded.TTL, original.TTL)
	}
}

func TestStoreHeaderSmallArtifactCBOR(t *testing.T) {
	// Small artifact: data embedded in the header.
	content := []byte("small stack trace content")
	original := StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
	}

	data, err := codec.Marshal(&original)
	if err != nil {
		t.Fatal(err)
	}

	var decoded StoreHeader
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(decoded.Data, content) {
		t.Errorf("Data = %q, want %q", decoded.Data, content)
	}
}

func TestStoreResponseCBORRoundtrip(t *testing.T) {
	original := StoreResponse{
		Ref:            "art-a3f9b2c1e7d4",
		Hash:           "a3f9b2c1e7d4000000000000000000000000000000000000000000000000abcd",
		Size:           1024 * 1024 * 100,
		ChunkCount:     1600,
		ContainerCount: 2,
		Compression:    "lz4",
		BytesStored:    80 * 1024 * 1024,
		BytesDeduped:   20 * 1024 * 1024,
	}

	data, err := codec.Marshal(&original)
	if err != nil {
		t.Fatal(err)
	}

	var decoded StoreResponse
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.Ref != original.Ref {
		t.Errorf("Ref = %q, want %q", decoded.Ref, original.Ref)
	}
	if decoded.ChunkCount != original.ChunkCount {
		t.Errorf("ChunkCount = %d, want %d", decoded.ChunkCount, original.ChunkCount)
	}
	if decoded.BytesDeduped != original.BytesDeduped {
		t.Errorf("BytesDeduped = %d, want %d", decoded.BytesDeduped, original.BytesDeduped)
	}
}

func TestFetchRequestCBORRoundtrip(t *testing.T) {
	original := FetchRequest{
		Action: "fetch",
		Ref:    "art-a3f9b2c1e7d4",
		Offset: 1024,
		Length: 4096,
	}

	data, err := codec.Marshal(&original)
	if err != nil {
		t.Fatal(err)
	}

	var decoded FetchRequest
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.Action != "fetch" {
		t.Errorf("Action = %q, want %q", decoded.Action, "fetch")
	}
	if decoded.Ref != original.Ref {
		t.Errorf("Ref = %q, want %q", decoded.Ref, original.Ref)
	}
	if decoded.Offset != original.Offset {
		t.Errorf("Offset = %d, want %d", decoded.Offset, original.Offset)
	}
	if decoded.Length != original.Length {
		t.Errorf("Length = %d, want %d", decoded.Length, original.Length)
	}
}

func TestFetchResponseSmallArtifactCBOR(t *testing.T) {
	content := []byte("small artifact content bytes")
	original := FetchResponse{
		Size:        int64(len(content)),
		ContentType: "text/plain",
		Hash:        "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		ChunkCount:  1,
		Compression: "none",
		Data:        content,
	}

	data, err := codec.Marshal(&original)
	if err != nil {
		t.Fatal(err)
	}

	var decoded FetchResponse
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(decoded.Data, content) {
		t.Error("Data mismatch in small fetch response")
	}
	if decoded.Size != original.Size {
		t.Errorf("Size = %d, want %d", decoded.Size, original.Size)
	}
}

// --- Protocol helper round-trip tests ---

func TestStoreHeaderWriteRead(t *testing.T) {
	var buffer bytes.Buffer
	header := &StoreHeader{
		ContentType: "image/png",
		Filename:    "screenshot.png",
		Size:        50000,
		CachePolicy: "ephemeral",
	}

	if err := WriteStoreHeader(&buffer, header); err != nil {
		t.Fatal(err)
	}

	decoded, err := ReadStoreHeader(&buffer)
	if err != nil {
		t.Fatal(err)
	}

	if decoded.Action != "store" {
		t.Errorf("Action = %q, want \"store\"", decoded.Action)
	}
	if decoded.ContentType != "image/png" {
		t.Errorf("ContentType = %q", decoded.ContentType)
	}
	if decoded.Size != 50000 {
		t.Errorf("Size = %d", decoded.Size)
	}
}

func TestFetchRequestWriteRead(t *testing.T) {
	var buffer bytes.Buffer
	request := &FetchRequest{
		Ref: "art-deadbeef1234",
	}

	if err := WriteFetchRequest(&buffer, request); err != nil {
		t.Fatal(err)
	}

	decoded, err := ReadFetchRequest(&buffer)
	if err != nil {
		t.Fatal(err)
	}

	if decoded.Action != "fetch" {
		t.Errorf("Action = %q, want \"fetch\"", decoded.Action)
	}
	if decoded.Ref != "art-deadbeef1234" {
		t.Errorf("Ref = %q", decoded.Ref)
	}
}

// --- End-to-end protocol sequence tests ---

func TestEndToEndSmallStore(t *testing.T) {
	// Simulate a small artifact store: header with embedded data.
	var wire bytes.Buffer
	content := []byte("small artifact: stack trace from crash")

	// Client side: send store header with data.
	header := &StoreHeader{
		ContentType: "text/plain",
		Filename:    "crash.txt",
		Size:        int64(len(content)),
		Data:        content,
		Labels:      []string{"crash", "debug"},
	}
	if err := WriteStoreHeader(&wire, header); err != nil {
		t.Fatal(err)
	}

	// Server side: read header, extract data.
	decoded, err := ReadStoreHeader(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.Data, content) {
		t.Error("server received wrong data")
	}

	// Server sends response.
	response := &StoreResponse{
		Ref:            "art-abc123def456",
		Hash:           "abc123def456000000000000000000000000000000000000000000000000ffff",
		Size:           int64(len(content)),
		ChunkCount:     1,
		ContainerCount: 1,
		Compression:    "none",
		BytesStored:    int64(len(content)),
	}
	if err := WriteStoreResponse(&wire, response); err != nil {
		t.Fatal(err)
	}

	// Client reads response.
	result, err := ReadStoreResponse(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if result.Ref != "art-abc123def456" {
		t.Errorf("response Ref = %q", result.Ref)
	}
}

func TestEndToEndLargeStoreSized(t *testing.T) {
	// Simulate a large artifact store with known size.
	var wire bytes.Buffer
	content := bytes.Repeat([]byte("large artifact data "), 5000)

	// Client: send header, then raw bytes.
	header := &StoreHeader{
		ContentType: "application/octet-stream",
		Size:        int64(len(content)),
	}
	if err := WriteStoreHeader(&wire, header); err != nil {
		t.Fatal(err)
	}
	wire.Write(content)

	// Server: read header, then read exactly Size bytes.
	decoded, err := ReadStoreHeader(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Data != nil {
		t.Error("large store should not have embedded data")
	}

	reader := DataReader(&wire, decoded.Size)
	received, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, content) {
		t.Errorf("sized transfer: received %d bytes, want %d", len(received), len(content))
	}
}

func TestEndToEndLargeStoreChunked(t *testing.T) {
	// Simulate a large artifact store with unknown size (chunked).
	var wire bytes.Buffer
	content := bytes.Repeat([]byte("streaming data "), 10000)

	// Client: send header with SizeUnknown, then framed data.
	header := &StoreHeader{
		ContentType: "application/octet-stream",
		Size:        SizeUnknown,
	}
	if err := WriteStoreHeader(&wire, header); err != nil {
		t.Fatal(err)
	}

	fw := NewFrameWriter(&wire)
	// Write in multiple chunks to exercise frame splitting.
	chunkSize := 4096
	for offset := 0; offset < len(content); offset += chunkSize {
		end := offset + chunkSize
		if end > len(content) {
			end = len(content)
		}
		fw.Write(content[offset:end])
	}
	fw.Close()

	// Server: read header, detect chunked mode, read frames.
	decoded, err := ReadStoreHeader(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Size != SizeUnknown {
		t.Errorf("Size = %d, want SizeUnknown", decoded.Size)
	}

	reader := DataReader(&wire, decoded.Size)
	received, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, content) {
		t.Errorf("chunked transfer: received %d bytes, want %d", len(received), len(content))
	}
}

func TestEndToEndFetch(t *testing.T) {
	// Simulate a fetch: client sends request, server streams response.
	var wire bytes.Buffer
	content := bytes.Repeat([]byte("fetched content "), 2000)

	// Client: send fetch request.
	request := &FetchRequest{
		Ref: "art-fetch123test",
	}
	if err := WriteFetchRequest(&wire, request); err != nil {
		t.Fatal(err)
	}

	// Server: read request.
	decodedRequest, err := ReadFetchRequest(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if decodedRequest.Ref != "art-fetch123test" {
		t.Errorf("Ref = %q", decodedRequest.Ref)
	}

	// Server: send response header, then stream data.
	response := &FetchResponse{
		Size:        int64(len(content)),
		ContentType: "application/octet-stream",
		Hash:        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ChunkCount:  500,
		Compression: "lz4",
	}
	if err := WriteFetchResponse(&wire, response); err != nil {
		t.Fatal(err)
	}
	wire.Write(content) // Sized transfer — server knows the length.

	// Client: read response header, then read data.
	decodedResponse, err := ReadFetchResponse(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if decodedResponse.Size != int64(len(content)) {
		t.Errorf("response Size = %d", decodedResponse.Size)
	}
	if decodedResponse.Data != nil {
		t.Error("large fetch should not have embedded data")
	}

	reader := DataReader(&wire, decodedResponse.Size)
	received, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, content) {
		t.Error("fetch content mismatch")
	}
}

func TestEndToEndSmallFetch(t *testing.T) {
	// Small fetch: content embedded in the response header.
	var wire bytes.Buffer
	content := []byte("tiny artifact")

	response := &FetchResponse{
		Size:        int64(len(content)),
		ContentType: "text/plain",
		Hash:        "aabbccdd00112233aabbccdd00112233aabbccdd00112233aabbccdd00112233",
		ChunkCount:  1,
		Compression: "none",
		Data:        content,
	}

	if err := WriteFetchResponse(&wire, response); err != nil {
		t.Fatal(err)
	}

	decoded, err := ReadFetchResponse(&wire)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(decoded.Data, content) {
		t.Error("small fetch data mismatch")
	}
}

// --- CBOR determinism test ---

func TestProtocolTypesDeterministicEncoding(t *testing.T) {
	// Encoding the same struct twice should produce identical bytes
	// (Core Deterministic Encoding).
	header := StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Filename:    "test.txt",
		Size:        42,
		Labels:      []string{"a", "b"},
	}

	data1, err := codec.Marshal(&header)
	if err != nil {
		t.Fatal(err)
	}
	data2, err := codec.Marshal(&header)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(data1, data2) {
		t.Error("non-deterministic encoding")
	}
}

// --- Benchmarks ---

func BenchmarkFrameWriterRead(b *testing.B) {
	data := bytes.Repeat([]byte("benchmark data for frame throughput testing "), 1000)

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var buffer bytes.Buffer
		buffer.Grow(len(data) + 1024)

		fw := NewFrameWriter(&buffer)
		fw.Write(data)
		fw.Close()

		fr := NewFrameReader(&buffer)
		io.Copy(io.Discard, fr)
	}
}

func BenchmarkSizedReaderRead(b *testing.B) {
	data := bytes.Repeat([]byte("benchmark sized reader data "), 1000)

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sr := NewSizedReader(bytes.NewReader(data), int64(len(data)))
		io.Copy(io.Discard, sr)
	}
}

func BenchmarkStoreHeaderCBOR(b *testing.B) {
	header := StoreHeader{
		Action:      "store",
		ContentType: "application/octet-stream",
		Filename:    "model.safetensors",
		Size:        10 * 1024 * 1024 * 1024,
		Description: "fine-tuned model weights",
		Labels:      []string{"model", "production"},
		Tag:         "models/gpt2/latest",
		CachePolicy: "pin",
		Visibility:  "private",
		TTL:         "720h",
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		data, _ := codec.Marshal(&header)
		var decoded StoreHeader
		codec.Unmarshal(data, &decoded)
	}
}

func BenchmarkEndToEndSizedTransfer(b *testing.B) {
	content := bytes.Repeat([]byte("X"), 64*1024) // 64KB

	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var wire bytes.Buffer
		wire.Grow(len(content) + 256)

		header := &StoreHeader{
			Action:      "store",
			ContentType: "application/octet-stream",
			Size:        int64(len(content)),
		}
		WriteStoreHeader(&wire, header)
		wire.Write(content)

		ReadStoreHeader(&wire)
		sr := NewSizedReader(&wire, int64(len(content)))
		io.Copy(io.Discard, sr)
	}
}

func BenchmarkFrameWriterLargePayload(b *testing.B) {
	// Benchmark frame writing with multi-MB payloads to measure
	// frame splitting overhead.
	data := bytes.Repeat([]byte("X"), 4*1024*1024) // 4MB

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var buffer bytes.Buffer
		buffer.Grow(len(data) + len(data)/MaxFrameSize*4 + 4)

		fw := NewFrameWriter(&buffer)
		fw.Write(data)
		fw.Close()
	}
}

func BenchmarkCBORByteStringVsBase64(b *testing.B) {
	// Compare CBOR native byte strings (what we use) vs a
	// hypothetical JSON base64 approach for small artifact data.
	content := bytes.Repeat([]byte("small artifact data"), 100)

	b.Run("cbor_bytes", func(b *testing.B) {
		b.SetBytes(int64(len(content)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			type msg struct {
				Data []byte `cbor:"data"`
			}
			data, _ := codec.Marshal(&msg{Data: content})
			var decoded msg
			codec.Unmarshal(data, &decoded)
		}
	})

	b.Run("cbor_with_string", func(b *testing.B) {
		b.SetBytes(int64(len(content)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			encoded := fmt.Sprintf("%x", content)
			type msg struct {
				Data string `cbor:"data"`
			}
			data, _ := codec.Marshal(&msg{Data: encoded})
			var decoded msg
			codec.Unmarshal(data, &decoded)
		}
	})
}
