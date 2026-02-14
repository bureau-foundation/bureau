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
// These structs define the messages in the artifact transfer protocol.
// All types use json struct tags per the lib/codec convention: they
// participate in both CBOR (wire protocol) and JSON (CLI --json
// output, future JSON-mode transports). The CBOR library reads json
// tags as fallback when cbor tags are absent.

// StoreHeader is the metadata sent by the client before uploading
// artifact content.
//
// For small artifacts (data fits in a single CBOR message), the
// Data field contains the content as a CBOR byte string and Size
// is set to the byte length. For large artifacts, Data is nil and
// the content follows as a binary stream after this header.
type StoreHeader struct {
	Action      string `json:"action"`
	ContentType string `json:"content_type"`
	Filename    string `json:"filename,omitempty"`
	Token       []byte `json:"token,omitempty"`

	// Size is the total uncompressed content size in bytes, or
	// SizeUnknown (-1) if the size is not known upfront. When
	// SizeUnknown, the binary stream uses chunked framing.
	Size int64 `json:"size"`

	// Data holds the content for small artifacts (< SmallArtifactThreshold).
	// CBOR byte strings are transferred without base64 encoding.
	// Nil for large artifacts — content follows as a binary stream.
	Data []byte `json:"data,omitempty"`

	Description string   `json:"description,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Tag         string   `json:"tag,omitempty"`
	CachePolicy string   `json:"cache_policy,omitempty"`
	Visibility  string   `json:"visibility,omitempty"`
	TTL         string   `json:"ttl,omitempty"`
}

// StoreResponse is the response after a store operation completes.
type StoreResponse struct {
	Ref            string `json:"ref"`
	Hash           string `json:"hash"`
	Size           int64  `json:"size"`
	ChunkCount     int    `json:"chunk_count"`
	ContainerCount int    `json:"container_count"`
	Compression    string `json:"compression"`
	BytesStored    int64  `json:"bytes_stored"`
	BytesDeduped   int64  `json:"bytes_deduped"`
}

// FetchRequest is the request sent by the client to download
// artifact content.
type FetchRequest struct {
	Action string `json:"action"`

	// Ref is the artifact reference (e.g., "art-a3f9b2c1e7d4") or
	// tag name to resolve and fetch.
	Ref string `json:"ref"`

	// Offset and Length specify an optional byte range. When both
	// are zero, the entire artifact is fetched. When set, only the
	// specified range is streamed.
	Offset int64 `json:"offset,omitempty"`
	Length int64 `json:"length,omitempty"`
}

// FetchResponse is the metadata sent by the service before
// streaming download content.
//
// For small artifacts, the Data field contains the content as a
// CBOR byte string and no binary stream follows. For large
// artifacts, Data is nil and the content follows as a sized binary
// stream (the receiver reads exactly Size bytes).
type FetchResponse struct {
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	Filename    string `json:"filename,omitempty"`
	Hash        string `json:"hash"`
	ChunkCount  int    `json:"chunk_count"`
	Compression string `json:"compression"`

	// Data holds the content for small artifacts. Nil for large
	// artifacts — content follows as a binary stream.
	Data []byte `json:"data,omitempty"`
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

// WriteMessage encodes v as CBOR and writes it with a 4-byte length
// prefix. This is the general-purpose message writer for the artifact
// transfer protocol — the type-specific helpers (WriteStoreHeader,
// WriteFetchResponse, etc.) call through to this function.
func WriteMessage(w io.Writer, v any) error {
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

// ReadMessage reads a length-prefixed CBOR message and decodes it
// into v. Rejects messages larger than MaxHeaderSize.
func ReadMessage(r io.Reader, v any) error {
	data, err := ReadRawMessage(r)
	if err != nil {
		return err
	}
	if err := codec.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decoding message: %w", err)
	}
	return nil
}

// ReadRawMessage reads a length-prefixed CBOR message from r and
// returns the raw CBOR bytes without decoding. The caller can
// unmarshal the bytes into different types — first to extract an
// action field for dispatch, then into the action-specific request
// type. Rejects messages larger than MaxHeaderSize.
func ReadRawMessage(r io.Reader) ([]byte, error) {
	var lengthPrefix [4]byte
	if _, err := io.ReadFull(r, lengthPrefix[:]); err != nil {
		return nil, fmt.Errorf("reading message length: %w", err)
	}
	length := binary.BigEndian.Uint32(lengthPrefix[:])
	if length > MaxHeaderSize {
		return nil, fmt.Errorf("message size %d exceeds maximum %d", length, MaxHeaderSize)
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("reading message body: %w", err)
	}
	return data, nil
}

// ErrorResponse is the error message sent when an action fails.
// Used by the artifact service's connection handler for both streaming
// and simple protocol paths. Clients check for the presence of the
// "error" field to distinguish error responses from success responses.
type ErrorResponse struct {
	Error string `json:"error"`
}

// WriteStoreHeader writes a length-prefixed CBOR StoreHeader to w.
// For small artifacts where header.Data is set, this is the complete
// request. For large artifacts (Data is nil), the caller must follow
// this with either a sized write (raw bytes) or a chunked write
// (FrameWriter).
func WriteStoreHeader(w io.Writer, header *StoreHeader) error {
	header.Action = "store"
	return WriteMessage(w, header)
}

// ReadStoreHeader reads a length-prefixed CBOR StoreHeader from r.
// The stream is positioned immediately after the header, ready for
// binary data reads if this is a large artifact transfer.
func ReadStoreHeader(r io.Reader) (*StoreHeader, error) {
	var header StoreHeader
	if err := ReadMessage(r, &header); err != nil {
		return nil, fmt.Errorf("reading store header: %w", err)
	}
	if header.Action != "store" {
		return nil, fmt.Errorf("expected action \"store\", got %q", header.Action)
	}
	return &header, nil
}

// WriteStoreResponse writes a length-prefixed CBOR StoreResponse to w.
func WriteStoreResponse(w io.Writer, response *StoreResponse) error {
	return WriteMessage(w, response)
}

// ReadStoreResponse reads a length-prefixed CBOR StoreResponse from r.
func ReadStoreResponse(r io.Reader) (*StoreResponse, error) {
	var response StoreResponse
	if err := ReadMessage(r, &response); err != nil {
		return nil, fmt.Errorf("reading store response: %w", err)
	}
	return &response, nil
}

// WriteFetchRequest writes a length-prefixed CBOR FetchRequest to w.
func WriteFetchRequest(w io.Writer, request *FetchRequest) error {
	request.Action = "fetch"
	return WriteMessage(w, request)
}

// ReadFetchRequest reads a length-prefixed CBOR FetchRequest from r.
func ReadFetchRequest(r io.Reader) (*FetchRequest, error) {
	var request FetchRequest
	if err := ReadMessage(r, &request); err != nil {
		return nil, fmt.Errorf("reading fetch request: %w", err)
	}
	if request.Action != "fetch" {
		return nil, fmt.Errorf("expected action \"fetch\", got %q", request.Action)
	}
	return &request, nil
}

// WriteFetchResponse writes a length-prefixed CBOR FetchResponse to w.
func WriteFetchResponse(w io.Writer, response *FetchResponse) error {
	return WriteMessage(w, response)
}

// ReadFetchResponse reads a length-prefixed CBOR FetchResponse from r.
func ReadFetchResponse(r io.Reader) (*FetchResponse, error) {
	var response FetchResponse
	if err := ReadMessage(r, &response); err != nil {
		return nil, fmt.Errorf("reading fetch response: %w", err)
	}
	return &response, nil
}

// --- Exists protocol types ---

// ExistsResponse is the response for the "exists" action.
type ExistsResponse struct {
	Exists bool   `json:"exists"`
	Hash   string `json:"hash,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// --- Status protocol types ---

// StatusResponse is the response for the "status" action
// (unauthenticated liveness check).
type StatusResponse struct {
	UptimeSeconds float64 `json:"uptime_seconds"`
	Artifacts     int     `json:"artifacts"`
	Rooms         int     `json:"rooms"`
}

// --- Delete-tag protocol types ---

// DeleteTagResponse is the response for the "delete-tag" action.
type DeleteTagResponse struct {
	Deleted string `json:"deleted"`
}

// --- List protocol types ---

// ListRequest is the request for the "list" action (filtered
// artifact query).
type ListRequest struct {
	Action      string `json:"action"`
	ContentType string `json:"content_type,omitempty"`
	Label       string `json:"label,omitempty"`
	CachePolicy string `json:"cache_policy,omitempty"`
	Visibility  string `json:"visibility,omitempty"`
	MinSize     int64  `json:"min_size,omitempty"`
	MaxSize     int64  `json:"max_size,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	Offset      int    `json:"offset,omitempty"`
}

// ListResponse is the response for the "list" action.
type ListResponse struct {
	Artifacts []ArtifactSummary `json:"artifacts"`
	Total     int               `json:"total"`
}

// ArtifactSummary is a single artifact in a ListResponse. Contains
// enough metadata for display without a separate "show" call.
type ArtifactSummary struct {
	Hash        string   `json:"hash"`
	Ref         string   `json:"ref"`
	ContentType string   `json:"content_type"`
	Filename    string   `json:"filename,omitempty"`
	Size        int64    `json:"size"`
	Labels      []string `json:"labels,omitempty"`
	CachePolicy string   `json:"cache_policy,omitempty"`
	StoredAt    string   `json:"stored_at"`
}

// --- Tag protocol types ---

// TagRequest is the request for the "tag" action (create or
// update a mutable tag pointing to an artifact).
type TagRequest struct {
	Action           string `json:"action"`
	Name             string `json:"name"`
	Ref              string `json:"ref"`
	Optimistic       bool   `json:"optimistic,omitempty"`
	ExpectedPrevious string `json:"expected_previous,omitempty"`
}

// TagResponse is the response for the "tag" action.
type TagResponse struct {
	Name     string `json:"name"`
	Hash     string `json:"hash"`
	Ref      string `json:"ref"`
	Previous string `json:"previous,omitempty"`
}

// DeleteTagRequest is the request for the "delete-tag" action.
type DeleteTagRequest struct {
	Action string `json:"action"`
	Name   string `json:"name"`
}

// ResolveRequest is the request for the "resolve" action. The
// ref can be a full hash, short ref (art-<hex>), or tag name.
type ResolveRequest struct {
	Action string `json:"action"`
	Ref    string `json:"ref"`
}

// ResolveResponse is the response for the "resolve" action.
type ResolveResponse struct {
	Hash string `json:"hash"`
	Ref  string `json:"ref"`
	Tag  string `json:"tag,omitempty"`
}

// TagsRequest is the request for the "tags" action (list tags).
type TagsRequest struct {
	Action string `json:"action"`
	Prefix string `json:"prefix,omitempty"`
}

// TagsResponse is the response for the "tags" action.
type TagsResponse struct {
	Tags []TagEntry `json:"tags"`
}

// TagEntry is a single tag in a TagsResponse.
type TagEntry struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
	Ref  string `json:"ref"`
}

// --- Pin/Unpin protocol types ---

// PinRequest is the request for the "pin" action (mark an
// artifact as protected from GC and pin containers in the cache).
type PinRequest struct {
	Action string `json:"action"`
	Ref    string `json:"ref"`
}

// PinResponse is the response for the "pin" action.
type PinResponse struct {
	Hash        string `json:"hash"`
	Ref         string `json:"ref"`
	CachePolicy string `json:"cache_policy"`
}

// UnpinRequest is the request for the "unpin" action.
type UnpinRequest struct {
	Action string `json:"action"`
	Ref    string `json:"ref"`
}

// UnpinResponse is the same shape as PinResponse — both report the
// artifact's ref and resulting cache policy after the operation.
type UnpinResponse = PinResponse

// --- GC protocol types ---

// GCRequest is the request for the "gc" action (mark-and-sweep
// garbage collection of expired, unprotected artifacts).
type GCRequest struct {
	Action string `json:"action"`
	DryRun bool   `json:"dry_run,omitempty"`
}

// GCResponse is the response for the "gc" action.
type GCResponse struct {
	ArtifactsRemoved  int   `json:"artifacts_removed"`
	ContainersRemoved int   `json:"containers_removed"`
	BytesFreed        int64 `json:"bytes_freed"`
	DryRun            bool  `json:"dry_run"`
}

// --- Cache status protocol types ---

// CacheStatusResponse is the response for the "cache-status"
// action. Includes store-level stats and, if a cache is configured,
// cache ring stats.
type CacheStatusResponse struct {
	StoreArtifacts int   `json:"store_artifacts"`
	StoreSizeBytes int64 `json:"store_size_bytes"`
	TagCount       int   `json:"tag_count"`

	// Cache fields are present only when a cache is configured.
	CacheDeviceSize       int64 `json:"cache_device_size,omitempty"`
	CacheBlockSize        int64 `json:"cache_block_size,omitempty"`
	CacheBlockCount       int   `json:"cache_block_count,omitempty"`
	CacheLiveContainers   int   `json:"cache_live_containers,omitempty"`
	CachePinnedContainers int   `json:"cache_pinned_containers,omitempty"`
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
