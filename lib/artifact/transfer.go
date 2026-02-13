// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/bureau-foundation/bureau/lib/codec"
)

// Transfer protocol constants.
const (
	// MaxFrameSize is the maximum size of a single data frame in
	// chunked transfer mode. Frames are length-prefixed with a
	// 4-byte uint32, so the theoretical limit is ~4GB. We cap at
	// 1MB to keep memory bounded and allow concurrent processing
	// of frames as they arrive.
	MaxFrameSize = 1024 * 1024

	// SizeUnknown indicates the content size is not known upfront.
	// When Size is SizeUnknown, the binary stream uses chunked
	// framing (length-prefixed frames with a zero-length
	// terminator). When Size >= 0, the receiver reads exactly
	// that many raw bytes with no framing overhead.
	SizeUnknown int64 = -1
)

// --- Protocol types ---
//
// These structs define the CBOR-encoded messages in the artifact
// transfer protocol. Each uses cbor struct tags; json tags serve as
// automatic fallback for the CBOR library (fxamacker/cbor reads
// json tags when cbor tags are absent) and for debugging output.

// StoreHeader is the CBOR metadata sent by the client before
// uploading artifact content.
//
// For small artifacts (data fits in a single CBOR message), the
// Data field contains the content as a CBOR byte string and Size
// is set to the byte length. For large artifacts, Data is nil and
// the content follows as a binary stream after this header.
type StoreHeader struct {
	Action      string `cbor:"action"       json:"action"`
	ContentType string `cbor:"content_type" json:"content_type"`
	Filename    string `cbor:"filename,omitempty" json:"filename,omitempty"`

	// Size is the total uncompressed content size in bytes, or
	// SizeUnknown (-1) if the size is not known upfront. When
	// SizeUnknown, the binary stream uses chunked framing.
	Size int64 `cbor:"size" json:"size"`

	// Data holds the content for small artifacts (< SmallArtifactThreshold).
	// CBOR byte strings are transferred without base64 encoding.
	// Nil for large artifacts — content follows as a binary stream.
	Data []byte `cbor:"data,omitempty" json:"data,omitempty"`

	Description string   `cbor:"description,omitempty" json:"description,omitempty"`
	Labels      []string `cbor:"labels,omitempty"      json:"labels,omitempty"`
	Tag         string   `cbor:"tag,omitempty"          json:"tag,omitempty"`
	CachePolicy string   `cbor:"cache_policy,omitempty" json:"cache_policy,omitempty"`
	Visibility  string   `cbor:"visibility,omitempty"   json:"visibility,omitempty"`
	TTL         string   `cbor:"ttl,omitempty"          json:"ttl,omitempty"`
}

// StoreResponse is the CBOR response after a store operation completes.
type StoreResponse struct {
	Ref            string `cbor:"ref"             json:"ref"`
	Hash           string `cbor:"hash"            json:"hash"`
	Size           int64  `cbor:"size"            json:"size"`
	ChunkCount     int    `cbor:"chunk_count"     json:"chunk_count"`
	ContainerCount int    `cbor:"container_count" json:"container_count"`
	Compression    string `cbor:"compression"     json:"compression"`
	BytesStored    int64  `cbor:"bytes_stored"    json:"bytes_stored"`
	BytesDeduped   int64  `cbor:"bytes_deduped"   json:"bytes_deduped"`
}

// FetchRequest is the CBOR request sent by the client to download
// artifact content.
type FetchRequest struct {
	Action string `cbor:"action" json:"action"`

	// Ref is the artifact reference (e.g., "art-a3f9b2c1e7d4") or
	// tag name to resolve and fetch.
	Ref string `cbor:"ref" json:"ref"`

	// Offset and Length specify an optional byte range. When both
	// are zero, the entire artifact is fetched. When set, only the
	// specified range is streamed.
	Offset int64 `cbor:"offset,omitempty" json:"offset,omitempty"`
	Length int64 `cbor:"length,omitempty" json:"length,omitempty"`
}

// FetchResponse is the CBOR metadata sent by the service before
// streaming download content.
//
// For small artifacts, the Data field contains the content as a
// CBOR byte string and no binary stream follows. For large
// artifacts, Data is nil and the content follows as a sized binary
// stream (the receiver reads exactly Size bytes).
type FetchResponse struct {
	Size        int64  `cbor:"size"         json:"size"`
	ContentType string `cbor:"content_type" json:"content_type"`
	Filename    string `cbor:"filename,omitempty" json:"filename,omitempty"`
	Hash        string `cbor:"hash"         json:"hash"`
	ChunkCount  int    `cbor:"chunk_count"  json:"chunk_count"`
	Compression string `cbor:"compression"  json:"compression"`

	// Data holds the content for small artifacts. Nil for large
	// artifacts — content follows as a binary stream.
	Data []byte `cbor:"data,omitempty" json:"data,omitempty"`
}

// --- Frame writer ---

// FrameWriter writes binary data as length-prefixed frames to an
// underlying writer. Each frame is a 4-byte big-endian uint32 length
// followed by that many bytes. Close writes a zero-length terminator
// frame to signal end-of-stream.
//
// FrameWriter implements io.WriteCloser. The caller writes arbitrary
// amounts of data; FrameWriter splits into frames of at most
// MaxFrameSize bytes.
//
// The framing protocol is independent of CBOR — it handles the raw
// binary portion of the transfer, sitting between the CBOR header
// and the CBOR response in the protocol sequence.
type FrameWriter struct {
	writer io.Writer
	closed bool
}

// NewFrameWriter creates a frame writer that writes to w.
func NewFrameWriter(w io.Writer) *FrameWriter {
	return &FrameWriter{writer: w}
}

// Write splits p into frames of at most MaxFrameSize bytes and writes
// them to the underlying writer. Each frame is length-prefixed.
func (fw *FrameWriter) Write(p []byte) (int, error) {
	if fw.closed {
		return 0, fmt.Errorf("write to closed FrameWriter")
	}

	totalWritten := 0
	for len(p) > 0 {
		frameSize := len(p)
		if frameSize > MaxFrameSize {
			frameSize = MaxFrameSize
		}

		if err := fw.writeFrame(p[:frameSize]); err != nil {
			return totalWritten, err
		}
		totalWritten += frameSize
		p = p[frameSize:]
	}
	return totalWritten, nil
}

// Close writes a zero-length terminator frame to signal end-of-stream.
// The underlying writer is NOT closed — the caller manages its lifecycle.
func (fw *FrameWriter) Close() error {
	if fw.closed {
		return nil
	}
	fw.closed = true

	var header [4]byte
	// header is already zero — zero-length terminator.
	_, err := fw.writer.Write(header[:])
	return err
}

// writeFrame writes a single length-prefixed frame.
func (fw *FrameWriter) writeFrame(data []byte) error {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if _, err := fw.writer.Write(header[:]); err != nil {
		return fmt.Errorf("writing frame header: %w", err)
	}
	if _, err := fw.writer.Write(data); err != nil {
		return fmt.Errorf("writing frame data: %w", err)
	}
	return nil
}

// --- Frame reader ---

// FrameReader reads binary data from a sequence of length-prefixed
// frames. Returns io.EOF after reading the zero-length terminator.
//
// FrameReader implements io.Reader. The caller reads arbitrary
// amounts; FrameReader handles frame boundary crossing transparently.
type FrameReader struct {
	reader         io.Reader
	frameRemaining int // bytes remaining in the current frame
	done           bool
}

// NewFrameReader creates a frame reader that reads from r.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{reader: r}
}

// Read fills p with data from the frame stream, reading across frame
// boundaries as needed. Returns io.EOF after the terminator frame.
func (fr *FrameReader) Read(p []byte) (int, error) {
	if fr.done {
		return 0, io.EOF
	}

	totalRead := 0
	for len(p) > 0 {
		// If the current frame is exhausted, read the next header.
		if fr.frameRemaining == 0 {
			var header [4]byte
			if _, err := io.ReadFull(fr.reader, header[:]); err != nil {
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					// Connection closed without terminator.
					fr.done = true
					if totalRead > 0 {
						return totalRead, nil
					}
					return 0, io.ErrUnexpectedEOF
				}
				return totalRead, err
			}
			fr.frameRemaining = int(binary.BigEndian.Uint32(header[:]))
			if fr.frameRemaining == 0 {
				// Zero-length terminator.
				fr.done = true
				if totalRead > 0 {
					return totalRead, nil
				}
				return 0, io.EOF
			}
			if fr.frameRemaining > MaxFrameSize {
				return totalRead, fmt.Errorf("frame size %d exceeds maximum %d",
					fr.frameRemaining, MaxFrameSize)
			}
		}

		// Read up to the lesser of what the caller wants and what
		// remains in this frame.
		readSize := len(p)
		if readSize > fr.frameRemaining {
			readSize = fr.frameRemaining
		}

		bytesRead, err := fr.reader.Read(p[:readSize])
		totalRead += bytesRead
		p = p[bytesRead:]
		fr.frameRemaining -= bytesRead

		if err != nil {
			if err == io.EOF {
				fr.done = true
			}
			return totalRead, err
		}
	}
	return totalRead, nil
}

// --- Sized stream reader ---

// SizedReader reads exactly Size bytes from the underlying reader,
// then returns io.EOF. This is the reader for sized transfers where
// the content length is known upfront and no framing is needed.
type SizedReader struct {
	reader    io.Reader
	remaining int64
}

// NewSizedReader wraps r to read exactly size bytes.
func NewSizedReader(r io.Reader, size int64) *SizedReader {
	return &SizedReader{reader: r, remaining: size}
}

// Read reads up to len(p) bytes, bounded by the remaining byte count.
func (sr *SizedReader) Read(p []byte) (int, error) {
	if sr.remaining <= 0 {
		return 0, io.EOF
	}

	if int64(len(p)) > sr.remaining {
		p = p[:sr.remaining]
	}

	bytesRead, err := sr.reader.Read(p)
	sr.remaining -= int64(bytesRead)

	if err == io.EOF && sr.remaining > 0 {
		return bytesRead, io.ErrUnexpectedEOF
	}
	if sr.remaining == 0 && err == nil {
		// All bytes consumed. Next read will return io.EOF.
		return bytesRead, nil
	}
	return bytesRead, err
}

// Remaining returns the number of bytes left to read.
func (sr *SizedReader) Remaining() int64 {
	return sr.remaining
}

// --- Protocol helpers ---
//
// CBOR messages are length-prefixed on the wire: a 4-byte big-endian
// uint32 length followed by that many bytes of CBOR data. This avoids
// the CBOR stream decoder's read-ahead buffering, which would consume
// bytes from the binary data stream that follows the header.
//
// Wire layout for a store upload:
//   [4-byte header length][CBOR StoreHeader][binary data stream][4-byte response length][CBOR StoreResponse]
//
// Wire layout for a fetch download:
//   [4-byte request length][CBOR FetchRequest][4-byte header length][CBOR FetchResponse][binary data stream]

// MaxHeaderSize is the maximum size of a length-prefixed CBOR header.
// 64KB is generous for metadata — the largest realistic header is a
// StoreHeader with long labels, description, and filename.
const MaxHeaderSize = 64 * 1024

// writeMessage encodes v as CBOR and writes it with a 4-byte length
// prefix.
func writeMessage(w io.Writer, v any) error {
	data, err := codec.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding message: %w", err)
	}
	var lengthPrefix [4]byte
	binary.BigEndian.PutUint32(lengthPrefix[:], uint32(len(data)))
	if _, err := w.Write(lengthPrefix[:]); err != nil {
		return fmt.Errorf("writing message length: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("writing message body: %w", err)
	}
	return nil
}

// readMessage reads a length-prefixed CBOR message and decodes it
// into v. Rejects messages larger than MaxHeaderSize.
func readMessage(r io.Reader, v any) error {
	var lengthPrefix [4]byte
	if _, err := io.ReadFull(r, lengthPrefix[:]); err != nil {
		return fmt.Errorf("reading message length: %w", err)
	}
	length := binary.BigEndian.Uint32(lengthPrefix[:])
	if length > MaxHeaderSize {
		return fmt.Errorf("message size %d exceeds maximum %d", length, MaxHeaderSize)
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return fmt.Errorf("reading message body: %w", err)
	}
	if err := codec.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decoding message: %w", err)
	}
	return nil
}

// WriteStoreHeader writes a length-prefixed CBOR StoreHeader to w.
// For small artifacts where header.Data is set, this is the complete
// request. For large artifacts (Data is nil), the caller must follow
// this with either a sized write (raw bytes) or a chunked write
// (FrameWriter).
func WriteStoreHeader(w io.Writer, header *StoreHeader) error {
	header.Action = "store"
	return writeMessage(w, header)
}

// ReadStoreHeader reads a length-prefixed CBOR StoreHeader from r.
// The stream is positioned immediately after the header, ready for
// binary data reads if this is a large artifact transfer.
func ReadStoreHeader(r io.Reader) (*StoreHeader, error) {
	var header StoreHeader
	if err := readMessage(r, &header); err != nil {
		return nil, fmt.Errorf("reading store header: %w", err)
	}
	if header.Action != "store" {
		return nil, fmt.Errorf("expected action \"store\", got %q", header.Action)
	}
	return &header, nil
}

// WriteStoreResponse writes a length-prefixed CBOR StoreResponse to w.
func WriteStoreResponse(w io.Writer, response *StoreResponse) error {
	return writeMessage(w, response)
}

// ReadStoreResponse reads a length-prefixed CBOR StoreResponse from r.
func ReadStoreResponse(r io.Reader) (*StoreResponse, error) {
	var response StoreResponse
	if err := readMessage(r, &response); err != nil {
		return nil, fmt.Errorf("reading store response: %w", err)
	}
	return &response, nil
}

// WriteFetchRequest writes a length-prefixed CBOR FetchRequest to w.
func WriteFetchRequest(w io.Writer, request *FetchRequest) error {
	request.Action = "fetch"
	return writeMessage(w, request)
}

// ReadFetchRequest reads a length-prefixed CBOR FetchRequest from r.
func ReadFetchRequest(r io.Reader) (*FetchRequest, error) {
	var request FetchRequest
	if err := readMessage(r, &request); err != nil {
		return nil, fmt.Errorf("reading fetch request: %w", err)
	}
	if request.Action != "fetch" {
		return nil, fmt.Errorf("expected action \"fetch\", got %q", request.Action)
	}
	return &request, nil
}

// WriteFetchResponse writes a length-prefixed CBOR FetchResponse to w.
func WriteFetchResponse(w io.Writer, response *FetchResponse) error {
	return writeMessage(w, response)
}

// ReadFetchResponse reads a length-prefixed CBOR FetchResponse from r.
func ReadFetchResponse(r io.Reader) (*FetchResponse, error) {
	var response FetchResponse
	if err := readMessage(r, &response); err != nil {
		return nil, fmt.Errorf("reading fetch response: %w", err)
	}
	return &response, nil
}

// DataReader returns the appropriate io.Reader for the binary data
// stream based on the transfer header's Size field. If size >= 0,
// returns a SizedReader that reads exactly that many bytes. If
// size == SizeUnknown, returns a FrameReader that reads
// length-prefixed frames until the zero-length terminator.
//
// This is the receiver-side complement to the sender's choice of
// io.CopyN (sized) or FrameWriter (chunked).
func DataReader(r io.Reader, size int64) io.Reader {
	if size == SizeUnknown {
		return NewFrameReader(r)
	}
	return NewSizedReader(r, size)
}
