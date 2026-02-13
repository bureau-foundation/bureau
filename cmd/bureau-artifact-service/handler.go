// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/artifact"
	"github.com/bureau-foundation/bureau/lib/codec"
)

// Connection timeout constants. These match the values used by
// lib/service/SocketServer for consistency across Bureau services.
const (
	// readTimeout is how long we wait for the client to send its
	// initial CBOR message. A well-behaved client sends the request
	// immediately after connecting.
	readTimeout = 30 * time.Second

	// writeTimeout is how long we wait for a control message (error
	// responses, simple action results) to be written. Not used for
	// binary data streaming — those paths remove the deadline.
	writeTimeout = 10 * time.Second
)

// serve starts accepting connections on the Unix socket and
// dispatches requests. Blocks until ctx is cancelled, then stops
// accepting new connections and waits for active handlers to
// complete. This is the artifact service's equivalent of
// service.SocketServer.Serve, but with the custom connection
// handler needed for binary streaming.
func (as *ArtifactService) serve(ctx context.Context, socketPath string) error {
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale socket %s: %w", socketPath, err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	defer func() {
		listener.Close()
		os.Remove(socketPath)
	}()

	// Unblock Accept when the context is cancelled.
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	as.logger.Info("artifact socket listening", "path", socketPath)

	var activeConnections sync.WaitGroup

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if errors.Is(err, net.ErrClosed) {
				break
			}
			as.logger.Error("accept failed", "error", err)
			continue
		}

		activeConnections.Add(1)
		go func() {
			defer activeConnections.Done()
			as.handleConnection(ctx, conn)
		}()
	}

	activeConnections.Wait()
	return nil
}

// handleConnection processes one client request on a connection. The
// first message determines the action, and the handler manages the
// rest of the connection lifecycle (including binary streaming for
// store/fetch).
func (as *ArtifactService) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(readTimeout))

	// Read the first message. This is always a length-prefixed CBOR
	// message containing at minimum an "action" field.
	raw, err := artifact.ReadRawMessage(conn)
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return
		}
		as.writeError(conn, fmt.Sprintf("invalid request: %v", err))
		return
	}

	// Extract the action field for routing.
	var header struct {
		Action string `cbor:"action"`
	}
	if err := codec.Unmarshal(raw, &header); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid request: %v", err))
		return
	}
	if header.Action == "" {
		as.writeError(conn, "missing required field: action")
		return
	}

	switch header.Action {
	case "store":
		as.handleStore(ctx, conn, raw)
	case "fetch":
		as.handleFetch(ctx, conn, raw)
	case "exists":
		as.handleExists(ctx, conn, raw)
	case "show":
		as.handleShow(ctx, conn, raw)
	case "reconstruction":
		as.handleReconstruction(ctx, conn, raw)
	case "status":
		as.handleStatus(ctx, conn, raw)
	default:
		as.writeError(conn, fmt.Sprintf("unknown action %q", header.Action))
	}
}

// --- Store action ---

func (as *ArtifactService) handleStore(ctx context.Context, conn net.Conn, raw []byte) {
	var header artifact.StoreHeader
	if err := codec.Unmarshal(raw, &header); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid store header: %v", err))
		return
	}

	as.writeMu.Lock()
	defer as.writeMu.Unlock()

	var storeResult *artifact.StoreResult
	var err error

	if header.Data != nil {
		// Small artifact: data embedded in the header.
		storeResult, err = as.store.WriteContent(header.Data, header.ContentType)
	} else {
		// Large artifact: binary data follows the header. Remove
		// the read deadline — streaming can take a long time.
		conn.SetReadDeadline(time.Time{})
		reader := artifact.DataReader(conn, header.Size)
		storeResult, err = as.store.Write(reader, header.ContentType, nil)
	}

	if err != nil {
		as.writeError(conn, fmt.Sprintf("store failed: %v", err))
		return
	}

	// Persist metadata.
	meta := &artifact.ArtifactMetadata{
		FileHash:       storeResult.FileHash,
		Ref:            storeResult.Ref,
		ContentType:    header.ContentType,
		Filename:       header.Filename,
		Description:    header.Description,
		Labels:         header.Labels,
		CachePolicy:    header.CachePolicy,
		Visibility:     header.Visibility,
		TTL:            header.TTL,
		Size:           storeResult.Size,
		ChunkCount:     storeResult.ChunkCount,
		ContainerCount: storeResult.ContainerCount,
		Compression:    storeResult.Compression.String(),
		StoredAt:       as.clock.Now(),
	}
	if err := as.metadataStore.Write(meta); err != nil {
		as.writeError(conn, fmt.Sprintf("persisting metadata: %v", err))
		return
	}

	// Update the ref index.
	as.refIndex.Add(storeResult.FileHash)

	as.logger.Info("artifact stored",
		"ref", storeResult.Ref,
		"size", storeResult.Size,
		"chunks", storeResult.ChunkCount,
		"containers", storeResult.ContainerCount,
		"compression", storeResult.Compression.String(),
		"content_type", header.ContentType,
	)

	// Send the store response.
	response := &artifact.StoreResponse{
		Ref:            storeResult.Ref,
		Hash:           artifact.FormatHash(storeResult.FileHash),
		Size:           storeResult.Size,
		ChunkCount:     storeResult.ChunkCount,
		ContainerCount: storeResult.ContainerCount,
		Compression:    storeResult.Compression.String(),
		BytesStored:    storeResult.CompressedSize,
	}
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := artifact.WriteMessage(conn, response); err != nil {
		as.logger.Debug("failed to write store response", "error", err)
	}
}

// --- Fetch action ---

func (as *ArtifactService) handleFetch(ctx context.Context, conn net.Conn, raw []byte) {
	var request artifact.FetchRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid fetch request: %v", err))
		return
	}

	// Resolve the reference to a full hash.
	fileHash, err := as.resolveRef(request.Ref)
	if err != nil {
		as.writeError(conn, err.Error())
		return
	}

	// Load metadata for content_type, filename.
	meta, err := as.metadataStore.Read(fileHash)
	if err != nil {
		as.writeError(conn, fmt.Sprintf("reading metadata: %v", err))
		return
	}

	// Get reconstruction record for size, chunk count.
	record, err := as.store.Stat(fileHash)
	if err != nil {
		as.writeError(conn, fmt.Sprintf("reading reconstruction record: %v", err))
		return
	}

	if record.Size <= int64(artifact.SmallArtifactThreshold) {
		// Small artifact: embed content in the response.
		content, err := as.store.ReadContent(fileHash)
		if err != nil {
			as.writeError(conn, fmt.Sprintf("reading content: %v", err))
			return
		}
		response := &artifact.FetchResponse{
			Size:        record.Size,
			ContentType: meta.ContentType,
			Filename:    meta.Filename,
			Hash:        artifact.FormatHash(fileHash),
			ChunkCount:  record.ChunkCount,
			Compression: meta.Compression,
			Data:        content,
		}
		conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := artifact.WriteMessage(conn, response); err != nil {
			as.logger.Debug("failed to write fetch response", "error", err)
		}
		return
	}

	// Large artifact: send response header, then stream content.
	response := &artifact.FetchResponse{
		Size:        record.Size,
		ContentType: meta.ContentType,
		Filename:    meta.Filename,
		Hash:        artifact.FormatHash(fileHash),
		ChunkCount:  record.ChunkCount,
		Compression: meta.Compression,
		// Data is nil — content follows as a sized binary stream.
	}
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := artifact.WriteMessage(conn, response); err != nil {
		as.logger.Debug("failed to write fetch response header", "error", err)
		return
	}

	// Stream the content directly to the connection. Remove the
	// write deadline — streaming can take a long time.
	conn.SetWriteDeadline(time.Time{})
	if _, err := as.store.Read(fileHash, conn); err != nil {
		as.logger.Error("fetch stream failed",
			"ref", artifact.FormatRef(fileHash),
			"error", err,
		)
		// Connection is committed to the binary protocol. The client
		// will see io.ErrUnexpectedEOF when the connection closes.
	}
}

// --- Simple actions ---
//
// The exists, show, and reconstruction actions all share the same
// request shape (action + ref). resolveRefRequest handles the
// common unmarshal-and-resolve sequence; each handler adds its
// specific lookup.

// refRequest is the shared request type for actions that take a
// single artifact reference: exists, show, reconstruction.
type refRequest struct {
	Action string `cbor:"action"`
	Ref    string `cbor:"ref"`
}

// resolveRefRequest unmarshals a refRequest from raw CBOR and
// resolves the ref to a full file hash. Returns the hash and true
// on success. On failure, writes an error response to conn and
// returns false.
func (as *ArtifactService) resolveRefRequest(conn net.Conn, raw []byte) (artifact.Hash, bool) {
	var request refRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid request: %v", err))
		return artifact.Hash{}, false
	}

	fileHash, err := as.resolveRef(request.Ref)
	if err != nil {
		as.writeError(conn, err.Error())
		return artifact.Hash{}, false
	}

	return fileHash, true
}

type existsResponse struct {
	Exists bool   `cbor:"exists"          json:"exists"`
	Hash   string `cbor:"hash,omitempty"  json:"hash,omitempty"`
	Ref    string `cbor:"ref,omitempty"   json:"ref,omitempty"`
	Size   int64  `cbor:"size,omitempty"  json:"size,omitempty"`
}

func (as *ArtifactService) handleExists(ctx context.Context, conn net.Conn, raw []byte) {
	var request refRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid exists request: %v", err))
		return
	}

	fileHash, err := as.resolveRef(request.Ref)
	if err != nil {
		// Not found is a valid "false" result for exists, not an error.
		as.writeResult(conn, existsResponse{Exists: false})
		return
	}

	response := existsResponse{
		Exists: true,
		Hash:   artifact.FormatHash(fileHash),
		Ref:    artifact.FormatRef(fileHash),
	}

	record, err := as.store.Stat(fileHash)
	if err == nil {
		response.Size = record.Size
	}

	as.writeResult(conn, response)
}

func (as *ArtifactService) handleShow(ctx context.Context, conn net.Conn, raw []byte) {
	fileHash, ok := as.resolveRefRequest(conn, raw)
	if !ok {
		return
	}

	meta, err := as.metadataStore.Read(fileHash)
	if err != nil {
		as.writeError(conn, fmt.Sprintf("reading metadata: %v", err))
		return
	}

	as.writeResult(conn, meta)
}

func (as *ArtifactService) handleReconstruction(ctx context.Context, conn net.Conn, raw []byte) {
	fileHash, ok := as.resolveRefRequest(conn, raw)
	if !ok {
		return
	}

	record, err := as.store.Stat(fileHash)
	if err != nil {
		as.writeError(conn, fmt.Sprintf("reading reconstruction record: %v", err))
		return
	}

	as.writeResult(conn, record)
}

type statusResponse struct {
	UptimeSeconds float64 `cbor:"uptime_seconds" json:"uptime_seconds"`
	Artifacts     int     `cbor:"artifacts"      json:"artifacts"`
	Rooms         int     `cbor:"rooms"          json:"rooms"`
}

func (as *ArtifactService) handleStatus(ctx context.Context, conn net.Conn, raw []byte) {
	uptime := as.clock.Now().Sub(as.startedAt)

	as.writeResult(conn, statusResponse{
		UptimeSeconds: uptime.Seconds(),
		Artifacts:     as.refIndex.Len(),
		Rooms:         len(as.rooms),
	})
}

// --- Reference resolution ---

// resolveRef resolves an artifact reference to its full 32-byte file
// hash. Accepts three formats:
//   - 64-character hex string (full hash)
//   - "art-" prefix + 12 hex characters (short ref)
//   - tag names (not yet implemented — deferred to tag storage bead)
func (as *ArtifactService) resolveRef(ref string) (artifact.Hash, error) {
	// Try full hash first (64 hex chars).
	if len(ref) == 64 {
		hash, err := artifact.ParseHash(ref)
		if err == nil {
			if as.store.Exists(hash) {
				return hash, nil
			}
			return artifact.Hash{}, fmt.Errorf("artifact %s not found", ref)
		}
	}

	// Try short ref (art-<12 hex>).
	if strings.HasPrefix(ref, "art-") {
		return as.refIndex.Resolve(ref)
	}

	return artifact.Hash{}, fmt.Errorf(
		"invalid artifact reference %q: expected 64-char hex hash or art-<12 hex> ref",
		ref,
	)
}

// --- Wire helpers ---

// writeError sends an ErrorResponse to the client.
func (as *ArtifactService) writeError(conn net.Conn, message string) {
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := artifact.WriteMessage(conn, artifact.ErrorResponse{Error: message}); err != nil {
		as.logger.Debug("failed to write error response", "error", err)
	}
}

// writeResult sends a success result to the client. The value is
// encoded directly as a CBOR message — no wrapping envelope.
func (as *ArtifactService) writeResult(conn net.Conn, result any) {
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := artifact.WriteMessage(conn, result); err != nil {
		as.logger.Debug("failed to write result", "error", err)
	}
}
