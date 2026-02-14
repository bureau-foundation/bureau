// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
)

// Client timeouts.
const (
	// clientDialTimeout is the maximum time to wait for a
	// connection to the artifact service socket.
	clientDialTimeout = 5 * time.Second

	// clientResponseTimeout is how long the client waits for the
	// server to send a response after a request completes. Covers
	// handler execution time for actions like GC that may take a
	// while.
	clientResponseTimeout = 120 * time.Second
)

// Client communicates with the artifact service over its Unix socket
// protocol. Each method opens a new connection, performs the
// request/response exchange, and closes the connection.
//
// The artifact protocol uses length-prefixed CBOR messages (4-byte
// uint32 + CBOR bytes) rather than the standard lib/service socket
// protocol, because artifact transfers interleave CBOR messages with
// raw binary data streams that a CBOR stream decoder would consume.
//
// Token injection: if the client holds a service token, it is
// included in every request via the "token" field. The server's
// authenticate helper extracts it from the raw CBOR bytes.
type Client struct {
	socketPath string
	token      []byte
}

// NewClient creates an authenticated artifact client by reading the
// service token from tokenPath. The token file contains the raw bytes
// written by the daemon at sandbox creation time.
func NewClient(socketPath, tokenPath string) (*Client, error) {
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("reading artifact service token from %s: %w", tokenPath, err)
	}
	if len(tokenBytes) == 0 {
		return nil, fmt.Errorf("artifact service token file %s is empty", tokenPath)
	}
	return &Client{
		socketPath: socketPath,
		token:      tokenBytes,
	}, nil
}

// NewClientFromToken creates a client with pre-loaded token bytes.
// If token is nil, the client sends unauthenticated requests
// (suitable for the "status" action or testing).
func NewClientFromToken(socketPath string, token []byte) *Client {
	return &Client{
		socketPath: socketPath,
		token:      token,
	}
}

// --- Public API: simple request/response ---

// Status returns service liveness information. This action is
// unauthenticated — it works even without a token.
func (c *Client) Status(ctx context.Context) (*StatusResponse, error) {
	var response StatusResponse
	if err := c.simpleCall(ctx, "status", nil, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// Exists checks whether an artifact exists in the store.
func (c *Client) Exists(ctx context.Context, ref string) (*ExistsResponse, error) {
	var response ExistsResponse
	if err := c.simpleCall(ctx, "exists", map[string]any{"ref": ref}, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// Show returns the full metadata for an artifact.
func (c *Client) Show(ctx context.Context, ref string) (*ArtifactMetadata, error) {
	var response ArtifactMetadata
	if err := c.simpleCall(ctx, "show", map[string]any{"ref": ref}, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// Reconstruction returns the physical layout (segments, containers)
// needed to reassemble an artifact.
func (c *Client) Reconstruction(ctx context.Context, ref string) (*ReconstructionRecord, error) {
	var response ReconstructionRecord
	if err := c.simpleCall(ctx, "reconstruction", map[string]any{"ref": ref}, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// List queries the artifact index with filters and pagination.
func (c *Client) List(ctx context.Context, filter ListRequest) (*ListResponse, error) {
	fields := map[string]any{}
	if filter.ContentType != "" {
		fields["content_type"] = filter.ContentType
	}
	if filter.Label != "" {
		fields["label"] = filter.Label
	}
	if filter.CachePolicy != "" {
		fields["cache_policy"] = filter.CachePolicy
	}
	if filter.Visibility != "" {
		fields["visibility"] = filter.Visibility
	}
	if filter.MinSize != 0 {
		fields["min_size"] = filter.MinSize
	}
	if filter.MaxSize != 0 {
		fields["max_size"] = filter.MaxSize
	}
	if filter.Limit != 0 {
		fields["limit"] = filter.Limit
	}
	if filter.Offset != 0 {
		fields["offset"] = filter.Offset
	}

	var response ListResponse
	if err := c.simpleCall(ctx, "list", fields, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// Resolve resolves a ref (hash, short ref, or tag name) to a full
// artifact hash.
func (c *Client) Resolve(ctx context.Context, ref string) (*ResolveResponse, error) {
	var response ResolveResponse
	if err := c.simpleCall(ctx, "resolve", map[string]any{"ref": ref}, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// Tag creates or updates a mutable tag pointing to an artifact.
// When optimistic is true, the tag is overwritten unconditionally.
// When optimistic is false, expectedPrevious must match the current
// target (compare-and-swap); pass nil for a new tag.
func (c *Client) Tag(ctx context.Context, name, ref string, optimistic bool, expectedPrevious string) (*TagResponse, error) {
	fields := map[string]any{
		"name": name,
		"ref":  ref,
	}
	if optimistic {
		fields["optimistic"] = true
	}
	if expectedPrevious != "" {
		fields["expected_previous"] = expectedPrevious
	}

	var response TagResponse
	if err := c.simpleCall(ctx, "tag", fields, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// DeleteTag removes a tag by name.
func (c *Client) DeleteTag(ctx context.Context, name string) (*DeleteTagResponse, error) {
	var response DeleteTagResponse
	if err := c.simpleCall(ctx, "delete-tag", map[string]any{"name": name}, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// Tags lists tags, optionally filtered by prefix.
func (c *Client) Tags(ctx context.Context, prefix string) (*TagsResponse, error) {
	fields := map[string]any{}
	if prefix != "" {
		fields["prefix"] = prefix
	}

	var response TagsResponse
	if err := c.simpleCall(ctx, "tags", fields, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// Pin marks an artifact as protected from GC. If a cache is
// configured, also pins the artifact's containers in the cache.
func (c *Client) Pin(ctx context.Context, ref string) (*PinResponse, error) {
	var response PinResponse
	if err := c.simpleCall(ctx, "pin", map[string]any{"ref": ref}, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// Unpin removes GC protection from an artifact.
func (c *Client) Unpin(ctx context.Context, ref string) (*UnpinResponse, error) {
	var response UnpinResponse
	if err := c.simpleCall(ctx, "unpin", map[string]any{"ref": ref}, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// GC runs mark-and-sweep garbage collection. When dryRun is true,
// reports what would be collected without actually deleting.
func (c *Client) GC(ctx context.Context, dryRun bool) (*GCResponse, error) {
	fields := map[string]any{}
	if dryRun {
		fields["dry_run"] = true
	}

	var response GCResponse
	if err := c.simpleCall(ctx, "gc", fields, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// CacheStatus returns store and cache statistics.
func (c *Client) CacheStatus(ctx context.Context) (*CacheStatusResponse, error) {
	var response CacheStatusResponse
	if err := c.simpleCall(ctx, "cache-status", nil, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// --- Public API: streaming ---

// Store uploads an artifact to the service. For small content
// (len(content) <= SmallArtifactThreshold and content implements
// io.Seeker or the caller pre-loaded the bytes), embed the data in
// header.Data and pass nil for content. For large content, leave
// header.Data nil and pass the content reader; header.Size must be
// set to the content length or SizeUnknown for chunked transfer.
func (c *Client) Store(ctx context.Context, header *StoreHeader, content io.Reader) (*StoreResponse, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Inject token.
	header.Token = c.token

	// Send the store header.
	if err := WriteStoreHeader(conn, header); err != nil {
		return nil, fmt.Errorf("writing store header: %w", err)
	}

	// Stream binary content for large artifacts.
	if header.Data == nil && content != nil {
		if header.Size >= 0 {
			// Sized transfer: raw bytes, no framing.
			written, err := io.CopyN(conn, content, header.Size)
			if err != nil {
				return nil, fmt.Errorf("streaming content (%d/%d bytes written): %w",
					written, header.Size, err)
			}
		} else {
			// Chunked transfer: length-prefixed frames.
			frameWriter := NewFrameWriter(conn)
			if _, err := io.Copy(frameWriter, content); err != nil {
				return nil, fmt.Errorf("streaming chunked content: %w", err)
			}
			if err := frameWriter.Close(); err != nil {
				return nil, fmt.Errorf("closing chunked stream: %w", err)
			}
		}
	}

	// Read the store response.
	conn.SetReadDeadline(time.Now().Add(clientResponseTimeout))
	raw, err := ReadRawMessage(conn)
	if err != nil {
		return nil, fmt.Errorf("reading store response: %w", err)
	}
	if err := checkError(raw); err != nil {
		return nil, err
	}

	var response StoreResponse
	if err := codec.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decoding store response: %w", err)
	}
	return &response, nil
}

// FetchResult holds the metadata and content from a fetch operation.
// The caller MUST close Content when done to release the underlying
// connection.
type FetchResult struct {
	Response FetchResponse

	// Content is the artifact data reader. For small artifacts
	// (Response.Data is non-nil), this reads from the embedded
	// byte slice. For large artifacts, this reads from the
	// underlying socket connection.
	Content io.ReadCloser
}

// Fetch downloads an artifact by ref. Returns the metadata and a
// content reader. The caller MUST close FetchResult.Content when done.
//
// For byte-range requests, use FetchRange instead.
func (c *Client) Fetch(ctx context.Context, ref string) (*FetchResult, error) {
	return c.fetchInternal(ctx, ref, 0, 0)
}

// FetchRange downloads a byte range from an artifact. Offset is the
// starting byte position; length is the number of bytes to read.
func (c *Client) FetchRange(ctx context.Context, ref string, offset, length int64) (*FetchResult, error) {
	return c.fetchInternal(ctx, ref, offset, length)
}

func (c *Client) fetchInternal(ctx context.Context, ref string, offset, length int64) (*FetchResult, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}

	// Build and send the fetch request.
	request := c.buildRequest("fetch", map[string]any{"ref": ref})
	if offset > 0 {
		request["offset"] = offset
	}
	if length > 0 {
		request["length"] = length
	}
	if err := WriteMessage(conn, request); err != nil {
		conn.Close()
		return nil, fmt.Errorf("writing fetch request: %w", err)
	}

	// Read the fetch response header.
	raw, err := ReadRawMessage(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading fetch response: %w", err)
	}
	if err := checkError(raw); err != nil {
		conn.Close()
		return nil, err
	}

	var response FetchResponse
	if err := codec.Unmarshal(raw, &response); err != nil {
		conn.Close()
		return nil, fmt.Errorf("decoding fetch response: %w", err)
	}

	// For small artifacts, data is embedded in the response.
	if response.Data != nil {
		conn.Close()
		return &FetchResult{
			Response: response,
			Content:  io.NopCloser(newBytesReader(response.Data)),
		}, nil
	}

	// For large artifacts, the binary stream follows on the
	// connection. Wrap the connection so closing the FetchResult
	// closes the connection.
	dataReader := DataReader(conn, response.Size)
	return &FetchResult{
		Response: response,
		Content:  &connReader{reader: dataReader, conn: conn},
	}, nil
}

// --- Internal helpers ---

// dial establishes a connection to the artifact service socket.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	dialer := net.Dialer{Timeout: clientDialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, fmt.Errorf("connecting to artifact service at %s: %w", c.socketPath, err)
	}
	return conn, nil
}

// buildRequest constructs a CBOR request map with action, token, and
// caller-provided fields.
func (c *Client) buildRequest(action string, fields map[string]any) map[string]any {
	capacity := 2
	if fields != nil {
		capacity += len(fields)
	}
	request := make(map[string]any, capacity)
	for key, value := range fields {
		request[key] = value
	}
	request["action"] = action
	if c.token != nil {
		request["token"] = c.token
	}
	return request
}

// simpleCall handles the common pattern: dial, send request, read
// response, check for errors, decode into result.
func (c *Client) simpleCall(ctx context.Context, action string, fields map[string]any, result any) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	request := c.buildRequest(action, fields)
	if err := WriteMessage(conn, request); err != nil {
		return fmt.Errorf("%s: writing request: %w", action, err)
	}

	conn.SetReadDeadline(time.Now().Add(clientResponseTimeout))
	raw, err := ReadRawMessage(conn)
	if err != nil {
		return fmt.Errorf("%s: reading response: %w", action, err)
	}
	if err := checkError(raw); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}

	if result != nil {
		if err := codec.Unmarshal(raw, result); err != nil {
			return fmt.Errorf("%s: decoding response: %w", action, err)
		}
	}
	return nil
}

// checkError inspects raw CBOR bytes for an ErrorResponse. If the
// "error" field is present and non-empty, returns it as an error.
func checkError(raw []byte) error {
	var errResp ErrorResponse
	if err := codec.Unmarshal(raw, &errResp); err != nil {
		// Can't decode — not an error response, caller will
		// decode into the expected type.
		return nil
	}
	if errResp.Error != "" {
		return &ServiceError{Message: errResp.Error}
	}
	return nil
}

// ServiceError is returned when the artifact service responds with
// an error message.
type ServiceError struct {
	Message string
}

func (e *ServiceError) Error() string {
	return e.Message
}

// connReader wraps an io.Reader and closes the underlying connection
// when Close is called. Used for streaming fetch responses where the
// connection must stay open until the caller finishes reading.
type connReader struct {
	reader io.Reader
	conn   net.Conn
}

func (cr *connReader) Read(p []byte) (int, error) {
	return cr.reader.Read(p)
}

func (cr *connReader) Close() error {
	return cr.conn.Close()
}

// newBytesReader creates a bytes.Reader without importing bytes.
// Using a simple wrapper to avoid an import for one call site.
type bytesReader struct {
	data   []byte
	offset int
}

func newBytesReader(data []byte) *bytesReader {
	return &bytesReader{data: data}
}

func (r *bytesReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	count := copy(p, r.data[r.offset:])
	r.offset += count
	return count, nil
}
