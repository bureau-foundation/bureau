// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema/artifact"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
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
	raw, err := artifactstore.ReadRawMessage(conn)
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return
		}
		as.writeError(conn, fmt.Sprintf("invalid request: %v", err))
		return
	}

	// Extract the action field for routing.
	var header struct {
		Action string `json:"action"`
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
		if as.authenticate(conn, raw, "store", artifact.ActionStore) == nil {
			return
		}
		as.handleStore(ctx, conn, raw)
	case "fetch":
		if as.authenticate(conn, raw, "fetch", artifact.ActionFetch) == nil {
			return
		}
		as.handleFetch(ctx, conn, raw)
	case "exists":
		if as.authenticate(conn, raw, "exists", artifact.ActionFetch) == nil {
			return
		}
		as.handleExists(ctx, conn, raw)
	case "show":
		if as.authenticate(conn, raw, "show", artifact.ActionFetch) == nil {
			return
		}
		as.handleShow(ctx, conn, raw)
	case "reconstruction":
		if as.authenticate(conn, raw, "reconstruction", artifact.ActionFetch) == nil {
			return
		}
		as.handleReconstruction(ctx, conn, raw)
	case "list":
		if as.authenticate(conn, raw, "list", artifact.ActionList) == nil {
			return
		}
		as.handleList(ctx, conn, raw)
	case "tag":
		if as.authenticate(conn, raw, "tag", artifact.ActionTag) == nil {
			return
		}
		as.handleTag(ctx, conn, raw)
	case "resolve":
		if as.authenticate(conn, raw, "resolve", artifact.ActionFetch) == nil {
			return
		}
		as.handleResolve(ctx, conn, raw)
	case "tags":
		if as.authenticate(conn, raw, "tags", artifact.ActionFetch) == nil {
			return
		}
		as.handleTags(ctx, conn, raw)
	case "delete-tag":
		if as.authenticate(conn, raw, "delete-tag", artifact.ActionTag) == nil {
			return
		}
		as.handleDeleteTag(ctx, conn, raw)
	case "pin":
		if as.authenticate(conn, raw, "pin", artifact.ActionPin) == nil {
			return
		}
		as.handlePin(ctx, conn, raw)
	case "unpin":
		if as.authenticate(conn, raw, "unpin", artifact.ActionPin) == nil {
			return
		}
		as.handleUnpin(ctx, conn, raw)
	case "gc":
		if as.authenticate(conn, raw, "gc", artifact.ActionGC) == nil {
			return
		}
		as.handleGC(ctx, conn, raw)
	case "cache-status":
		if as.authenticate(conn, raw, "cache-status", artifact.ActionList) == nil {
			return
		}
		as.handleCacheStatus(ctx, conn, raw)
	case "set-upstream":
		as.handleSetUpstream(conn, raw)
	case "set-push-targets":
		as.handleSetPushTargets(conn, raw)
	case "revoke-tokens":
		as.handleRevokeTokens(conn, raw)
	case "status":
		// Status is unauthenticated — pure liveness check that
		// reveals only uptime and artifact count.
		as.handleStatus(ctx, conn, raw)
	default:
		as.writeError(conn, fmt.Sprintf("unknown action %q", header.Action))
	}
}

// --- Authentication ---

// authenticate verifies the service token in the request and checks
// that it carries the required grant. Returns the verified token on
// success. On failure, writes an error response to conn and returns
// nil.
//
// When authConfig is nil (unit tests that don't exercise auth),
// returns a zero token to allow the handler to proceed.
func (as *ArtifactService) authenticate(conn net.Conn, raw []byte, action, grant string) *servicetoken.Token {
	if as.authConfig == nil {
		return &servicetoken.Token{}
	}

	token, err := service.VerifyRequestToken(as.authConfig, raw, action, as.logger)
	if err != nil {
		as.writeError(conn, err.Error())
		return nil
	}

	if !servicetoken.GrantsAllow(token.Grants, grant, "") {
		as.writeError(conn, fmt.Sprintf("access denied: missing grant for %s", grant))
		return nil
	}

	return token
}

// --- Store action ---

func (as *ArtifactService) handleStore(ctx context.Context, conn net.Conn, raw []byte) {
	var header artifactstore.StoreHeader
	if err := codec.Unmarshal(raw, &header); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid store header: %v", err))
		return
	}

	// Normalize and validate visibility before any writes.
	// Default to private per security-by-default design.
	header.Visibility = artifactstore.NormalizeVisibility(header.Visibility)
	if err := artifactstore.ValidateVisibility(header.Visibility); err != nil {
		as.writeError(conn, err.Error())
		return
	}

	// Store the artifact locally. The write mutex serializes all
	// writes to the store, metadata, ref index, and tag store.
	storeResult, meta, err := as.storeLocally(ctx, conn, &header)
	if err != nil {
		// storeLocally already wrote the error response to conn.
		return
	}

	// Resolve push targets and execute pushes outside the write
	// lock. Push operations are read-only against the local store
	// and can run concurrently with other writes.
	targets := as.resolvePushTargets(&header)
	var pushResults []artifactstore.PushResult
	if len(targets) > 0 {
		pushResults = as.executePushes(ctx, storeResult.FileHash, meta, targets)
		for _, result := range pushResults {
			if result.OK {
				as.logger.Info("push succeeded", "target", result.Target)
			} else {
				as.logger.Warn("push failed", "target", result.Target, "error", result.Error)
			}
		}
	}

	// Send the store response.
	response := &artifactstore.StoreResponse{
		Ref:            storeResult.Ref,
		Hash:           artifactstore.FormatHash(storeResult.FileHash),
		Size:           storeResult.Size,
		ChunkCount:     storeResult.ChunkCount,
		ContainerCount: storeResult.ContainerCount,
		Compression:    storeResult.Compression.String(),
		BytesStored:    storeResult.CompressedSize,
		PushResults:    pushResults,
	}
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := artifactstore.WriteMessage(conn, response); err != nil {
		as.logger.Debug("failed to write store response", "error", err)
	}
}

// storeLocally writes the artifact to the local store, persists
// metadata, updates indexes, and creates a tag if requested. Holds
// the write mutex for the duration. On error, writes the error
// response to conn and returns the error.
func (as *ArtifactService) storeLocally(ctx context.Context, conn net.Conn, header *artifactstore.StoreHeader) (*artifactstore.StoreResult, *artifactstore.ArtifactMetadata, error) {
	as.writeMu.Lock()
	defer as.writeMu.Unlock()

	var storeResult *artifactstore.StoreResult
	var err error

	if header.Data != nil {
		// Small artifact: data embedded in the header.
		storeResult, err = as.store.WriteContent(header.Data, header.ContentType)
	} else {
		// Large artifact: binary data follows the header. Remove
		// the read deadline — streaming can take a long time.
		conn.SetReadDeadline(time.Time{})
		reader := artifactstore.DataReader(conn, header.Size)
		storeResult, err = as.store.Write(reader, header.ContentType, nil)
	}

	if err != nil {
		as.writeError(conn, fmt.Sprintf("store failed: %v", err))
		return nil, nil, err
	}

	// Persist metadata.
	meta := &artifactstore.ArtifactMetadata{
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
		return nil, nil, err
	}

	// Update the ref index and artifact index.
	as.refIndex.Add(storeResult.FileHash)
	as.artifactIndex.Put(*meta)

	// If the store request included a tag, create it now.
	// Store-with-tag uses optimistic mode (last writer wins) because
	// the caller explicitly asked to overwrite the tag with a fresh
	// artifact.
	if header.Tag != "" {
		if err := as.tagStore.Set(header.Tag, storeResult.FileHash, nil, true, as.clock.Now()); err != nil {
			as.writeError(conn, fmt.Sprintf("creating tag %q: %v", header.Tag, err))
			return nil, nil, err
		}
		as.logger.Info("tag created via store",
			"tag", header.Tag,
			"ref", storeResult.Ref,
		)
	}

	as.logger.Info("artifact stored",
		"ref", storeResult.Ref,
		"size", storeResult.Size,
		"chunks", storeResult.ChunkCount,
		"containers", storeResult.ContainerCount,
		"compression", storeResult.Compression.String(),
		"content_type", header.ContentType,
	)

	return storeResult, meta, nil
}

// --- Fetch action ---

func (as *ArtifactService) handleFetch(ctx context.Context, conn net.Conn, raw []byte) {
	var request artifactstore.FetchRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid fetch request: %v", err))
		return
	}

	// Resolve the reference to a full hash.
	fileHash, err := as.resolveRef(request.Ref)
	if err != nil {
		// Local miss. If an upstream is configured and the ref is
		// eligible for forwarding, fetch from the upstream, store
		// locally, then serve from the local store.
		if as.isUpstreamRef(request.Ref) {
			if upstreamHash, ok := as.fetchFromUpstream(ctx, conn, request.Ref); ok {
				fileHash = upstreamHash
			} else {
				// fetchFromUpstream already wrote an error or
				// response to conn.
				return
			}
		} else {
			as.writeError(conn, err.Error())
			return
		}
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

	if record.Size <= int64(artifactstore.SmallArtifactThreshold) {
		// Small artifact: embed content in the response.
		content, err := as.store.ReadContent(fileHash)
		if err != nil {
			as.writeError(conn, fmt.Sprintf("reading content: %v", err))
			return
		}
		response := &artifactstore.FetchResponse{
			Size:        record.Size,
			ContentType: meta.ContentType,
			Filename:    meta.Filename,
			Hash:        artifactstore.FormatHash(fileHash),
			ChunkCount:  record.ChunkCount,
			Compression: meta.Compression,
			Visibility:  meta.Visibility,
			Data:        content,
		}
		conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := artifactstore.WriteMessage(conn, response); err != nil {
			as.logger.Debug("failed to write fetch response", "error", err)
		}
		return
	}

	// Large artifact: send response header, then stream content.
	response := &artifactstore.FetchResponse{
		Size:        record.Size,
		ContentType: meta.ContentType,
		Filename:    meta.Filename,
		Hash:        artifactstore.FormatHash(fileHash),
		ChunkCount:  record.ChunkCount,
		Compression: meta.Compression,
		Visibility:  meta.Visibility,
		// Data is nil — content follows as a sized binary stream.
	}
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := artifactstore.WriteMessage(conn, response); err != nil {
		as.logger.Debug("failed to write fetch response header", "error", err)
		return
	}

	// Stream the content directly to the connection. Remove the
	// write deadline — streaming can take a long time.
	conn.SetWriteDeadline(time.Time{})
	if _, err := as.store.Read(fileHash, conn); err != nil {
		as.logger.Error("fetch stream failed",
			"ref", artifactstore.FormatRef(fileHash),
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
	Action string `json:"action"`
	Ref    string `json:"ref"`
}

// resolveRefRequest unmarshals a refRequest from raw CBOR and
// resolves the ref to a full file hash. Returns the hash and true
// on success. On failure, writes an error response to conn and
// returns false.
func (as *ArtifactService) resolveRefRequest(conn net.Conn, raw []byte) (artifactstore.Hash, bool) {
	var request refRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid request: %v", err))
		return artifactstore.Hash{}, false
	}

	fileHash, err := as.resolveRef(request.Ref)
	if err != nil {
		as.writeError(conn, err.Error())
		return artifactstore.Hash{}, false
	}

	return fileHash, true
}

func (as *ArtifactService) handleExists(ctx context.Context, conn net.Conn, raw []byte) {
	var request refRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid exists request: %v", err))
		return
	}

	fileHash, err := as.resolveRef(request.Ref)
	if err != nil {
		// Local miss. Check upstream if configured and ref is eligible.
		if as.isUpstreamRef(request.Ref) {
			upstreamSocket, upstreamToken := as.getUpstream()
			if upstreamSocket != "" {
				upstream := artifactstore.NewClientFromToken(upstreamSocket, upstreamToken)
				upstreamResult, upstreamErr := upstream.Exists(ctx, request.Ref)
				if upstreamErr == nil && upstreamResult.Exists {
					as.writeResult(conn, *upstreamResult)
					return
				}
			}
		}
		as.writeResult(conn, artifactstore.ExistsResponse{Exists: false})
		return
	}

	response := artifactstore.ExistsResponse{
		Exists: true,
		Hash:   artifactstore.FormatHash(fileHash),
		Ref:    artifactstore.FormatRef(fileHash),
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

func (as *ArtifactService) handleStatus(ctx context.Context, conn net.Conn, raw []byte) {
	uptime := as.clock.Now().Sub(as.startedAt)

	as.writeResult(conn, artifactstore.StatusResponse{
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
//   - tag name (looked up in the tag store)
func (as *ArtifactService) resolveRef(ref string) (artifactstore.Hash, error) {
	// Try full hash first (64 hex chars).
	if len(ref) == 64 {
		hash, err := artifactstore.ParseHash(ref)
		if err == nil {
			if as.store.Exists(hash) {
				return hash, nil
			}
			return artifactstore.Hash{}, fmt.Errorf("artifact %s not found", ref)
		}
	}

	// Try short ref (art-<12 hex>).
	if strings.HasPrefix(ref, "art-") {
		return as.refIndex.Resolve(ref)
	}

	// Try tag name.
	if tag, exists := as.tagStore.Get(ref); exists {
		return tag.Target, nil
	}

	return artifactstore.Hash{}, fmt.Errorf(
		"unknown artifact reference %q: not a hash, short ref (art-<hex>), or tag name",
		ref,
	)
}

// --- List action ---

func (as *ArtifactService) handleList(ctx context.Context, conn net.Conn, raw []byte) {
	var request artifactstore.ListRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid list request: %v", err))
		return
	}

	filter := artifactstore.ArtifactFilter{
		ContentType: request.ContentType,
		Label:       request.Label,
		CachePolicy: request.CachePolicy,
		Visibility:  request.Visibility,
		MinSize:     request.MinSize,
		MaxSize:     request.MaxSize,
		Limit:       request.Limit,
		Offset:      request.Offset,
	}

	entries, total := as.artifactIndex.List(filter)

	summaries := make([]artifactstore.ArtifactSummary, len(entries))
	for i, entry := range entries {
		summaries[i] = artifactstore.ArtifactSummary{
			Hash:        artifactstore.FormatHash(entry.FileHash),
			Ref:         artifactstore.FormatRef(entry.FileHash),
			ContentType: entry.Metadata.ContentType,
			Filename:    entry.Metadata.Filename,
			Size:        entry.Metadata.Size,
			Labels:      entry.Metadata.Labels,
			CachePolicy: entry.Metadata.CachePolicy,
			Visibility:  entry.Metadata.Visibility,
			StoredAt:    entry.Metadata.StoredAt.UTC().Format(time.RFC3339),
		}
	}

	as.writeResult(conn, artifactstore.ListResponse{
		Artifacts: summaries,
		Total:     total,
	})
}

// --- Tag actions ---

func (as *ArtifactService) handleTag(ctx context.Context, conn net.Conn, raw []byte) {
	var request artifactstore.TagRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid tag request: %v", err))
		return
	}

	if request.Name == "" {
		as.writeError(conn, "missing required field: name")
		return
	}
	if request.Ref == "" {
		as.writeError(conn, "missing required field: ref")
		return
	}

	// Resolve the ref to a full hash (verifies the artifact exists).
	fileHash, err := as.resolveRef(request.Ref)
	if err != nil {
		as.writeError(conn, err.Error())
		return
	}

	// Parse expected_previous if provided (for compare-and-swap).
	var expectedPrevious *artifactstore.Hash
	if request.ExpectedPrevious != "" {
		parsed, err := artifactstore.ParseHash(request.ExpectedPrevious)
		if err != nil {
			as.writeError(conn, fmt.Sprintf("invalid expected_previous hash: %v", err))
			return
		}
		expectedPrevious = &parsed
	}

	// Record the previous target before the update (for the response).
	var previousRef string
	if existing, exists := as.tagStore.Get(request.Name); exists {
		previousRef = artifactstore.FormatRef(existing.Target)
	}

	as.writeMu.Lock()
	err = as.tagStore.Set(request.Name, fileHash, expectedPrevious, request.Optimistic, as.clock.Now())
	as.writeMu.Unlock()
	if err != nil {
		as.writeError(conn, err.Error())
		return
	}

	as.logger.Info("tag set",
		"tag", request.Name,
		"ref", artifactstore.FormatRef(fileHash),
	)

	as.writeResult(conn, artifactstore.TagResponse{
		Name:     request.Name,
		Hash:     artifactstore.FormatHash(fileHash),
		Ref:      artifactstore.FormatRef(fileHash),
		Previous: previousRef,
	})
}

func (as *ArtifactService) handleDeleteTag(ctx context.Context, conn net.Conn, raw []byte) {
	var request artifactstore.DeleteTagRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid delete-tag request: %v", err))
		return
	}

	if request.Name == "" {
		as.writeError(conn, "missing required field: name")
		return
	}

	as.writeMu.Lock()
	err := as.tagStore.Delete(request.Name)
	as.writeMu.Unlock()
	if err != nil {
		as.writeError(conn, err.Error())
		return
	}

	as.logger.Info("tag deleted", "tag", request.Name)

	as.writeResult(conn, artifactstore.DeleteTagResponse{Deleted: request.Name})
}

func (as *ArtifactService) handleResolve(ctx context.Context, conn net.Conn, raw []byte) {
	var request artifactstore.ResolveRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid resolve request: %v", err))
		return
	}

	if request.Ref == "" {
		as.writeError(conn, "missing required field: ref")
		return
	}

	fileHash, err := as.resolveRef(request.Ref)
	if err != nil {
		as.writeError(conn, err.Error())
		return
	}

	// Check if the ref was a tag name.
	response := artifactstore.ResolveResponse{
		Hash: artifactstore.FormatHash(fileHash),
		Ref:  artifactstore.FormatRef(fileHash),
	}
	if _, exists := as.tagStore.Get(request.Ref); exists {
		response.Tag = request.Ref
	}

	as.writeResult(conn, response)
}

func (as *ArtifactService) handleTags(ctx context.Context, conn net.Conn, raw []byte) {
	var request artifactstore.TagsRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid tags request: %v", err))
		return
	}

	records := as.tagStore.List(request.Prefix)

	tags := make([]artifactstore.TagEntry, len(records))
	for i, record := range records {
		tags[i] = artifactstore.TagEntry{
			Name: record.Name,
			Hash: artifactstore.FormatHash(record.Target),
			Ref:  artifactstore.FormatRef(record.Target),
		}
	}

	as.writeResult(conn, artifactstore.TagsResponse{Tags: tags})
}

// --- Pin/Unpin actions ---

func (as *ArtifactService) handlePin(ctx context.Context, conn net.Conn, raw []byte) {
	var request artifactstore.PinRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid pin request: %v", err))
		return
	}

	if request.Ref == "" {
		as.writeError(conn, "missing required field: ref")
		return
	}

	fileHash, err := as.resolveRef(request.Ref)
	if err != nil {
		as.writeError(conn, err.Error())
		return
	}

	as.writeMu.Lock()
	defer as.writeMu.Unlock()

	// Update metadata to set cache_policy = "pin".
	meta, err := as.metadataStore.Read(fileHash)
	if err != nil {
		as.writeError(conn, fmt.Sprintf("reading metadata: %v", err))
		return
	}

	meta.CachePolicy = "pin"
	if err := as.metadataStore.Write(meta); err != nil {
		as.writeError(conn, fmt.Sprintf("updating metadata: %v", err))
		return
	}
	as.artifactIndex.Put(*meta)

	// If cache is configured, pin all containers.
	if as.cache != nil {
		record, err := as.store.Stat(fileHash)
		if err != nil {
			as.writeError(conn, fmt.Sprintf("reading reconstruction record: %v", err))
			return
		}

		for _, segment := range record.Segments {
			containerData, err := as.readContainerBytes(segment.Container)
			if err != nil {
				as.writeError(conn, fmt.Sprintf("reading container %s: %v",
					artifactstore.FormatHash(segment.Container), err))
				return
			}
			if err := as.cache.Pin(segment.Container, containerData); err != nil {
				as.writeError(conn, fmt.Sprintf("pinning container %s: %v",
					artifactstore.FormatHash(segment.Container), err))
				return
			}
		}
	}

	as.logger.Info("artifact pinned",
		"ref", artifactstore.FormatRef(fileHash),
	)

	as.writeResult(conn, artifactstore.PinResponse{
		Hash:        artifactstore.FormatHash(fileHash),
		Ref:         artifactstore.FormatRef(fileHash),
		CachePolicy: "pin",
	})
}

func (as *ArtifactService) handleUnpin(ctx context.Context, conn net.Conn, raw []byte) {
	var request artifactstore.UnpinRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid unpin request: %v", err))
		return
	}

	if request.Ref == "" {
		as.writeError(conn, "missing required field: ref")
		return
	}

	fileHash, err := as.resolveRef(request.Ref)
	if err != nil {
		as.writeError(conn, err.Error())
		return
	}

	as.writeMu.Lock()
	defer as.writeMu.Unlock()

	meta, err := as.metadataStore.Read(fileHash)
	if err != nil {
		as.writeError(conn, fmt.Sprintf("reading metadata: %v", err))
		return
	}

	if meta.CachePolicy != "pin" {
		as.writeError(conn, fmt.Sprintf("artifact %s is not pinned (cache_policy=%q)",
			artifactstore.FormatRef(fileHash), meta.CachePolicy))
		return
	}

	meta.CachePolicy = ""
	if err := as.metadataStore.Write(meta); err != nil {
		as.writeError(conn, fmt.Sprintf("updating metadata: %v", err))
		return
	}
	as.artifactIndex.Put(*meta)

	// If cache is configured, unpin containers.
	if as.cache != nil {
		record, err := as.store.Stat(fileHash)
		if err != nil {
			as.writeError(conn, fmt.Sprintf("reading reconstruction record: %v", err))
			return
		}

		for _, segment := range record.Segments {
			// Unpin is best-effort per container — the container
			// might not have been pinned in the cache (e.g. if it
			// was pinned before cache was configured).
			as.cache.Unpin(segment.Container)
		}
	}

	as.logger.Info("artifact unpinned",
		"ref", artifactstore.FormatRef(fileHash),
	)

	as.writeResult(conn, artifactstore.UnpinResponse{
		Hash:        artifactstore.FormatHash(fileHash),
		Ref:         artifactstore.FormatRef(fileHash),
		CachePolicy: "",
	})
}

// readContainerBytes reads raw container bytes from disk.
func (as *ArtifactService) readContainerBytes(containerHash artifactstore.Hash) ([]byte, error) {
	hex := artifactstore.FormatHash(containerHash)
	containerPath := as.store.ContainerPath(containerHash)
	data, err := os.ReadFile(containerPath)
	if err != nil {
		return nil, fmt.Errorf("reading container %s: %w", hex, err)
	}
	return data, nil
}

// --- GC action ---

func (as *ArtifactService) handleGC(ctx context.Context, conn net.Conn, raw []byte) {
	var request artifactstore.GCRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		as.writeError(conn, fmt.Sprintf("invalid gc request: %v", err))
		return
	}

	as.writeMu.Lock()
	defer as.writeMu.Unlock()

	now := as.clock.Now()

	// Build the set of hashes protected by tags.
	taggedHashes := as.tagStore.Names()

	// Walk all artifacts in the index. Partition into protected and
	// candidates for removal. Page through in MaxListLimit-sized batches
	// to handle stores larger than the default list limit.
	var allArtifacts []artifactstore.ArtifactEntry
	offset := 0
	for {
		entries, total := as.artifactIndex.List(artifactstore.ArtifactFilter{
			Limit:  artifactstore.MaxListLimit,
			Offset: offset,
		})
		allArtifacts = append(allArtifacts, entries...)
		offset += len(entries)
		if offset >= total || len(entries) == 0 {
			break
		}
	}

	var removable []artifactstore.ArtifactEntry
	protected := make(map[artifactstore.Hash]struct{})

	for _, entry := range allArtifacts {
		// Protected: pinned artifacts.
		if entry.Metadata.CachePolicy == "pin" {
			protected[entry.FileHash] = struct{}{}
			continue
		}

		// Protected: artifacts referenced by a tag.
		if _, isTagged := taggedHashes[entry.FileHash]; isTagged {
			protected[entry.FileHash] = struct{}{}
			continue
		}

		// Protected: artifacts whose TTL has not expired.
		if entry.Metadata.TTL != "" {
			ttlDuration, err := parseTTL(entry.Metadata.TTL)
			if err != nil {
				// Unparseable TTL: protect the artifact rather than
				// silently deleting it.
				as.logger.Warn("unparseable TTL, protecting artifact",
					"ref", entry.Metadata.Ref,
					"ttl", entry.Metadata.TTL,
					"error", err,
				)
				protected[entry.FileHash] = struct{}{}
				continue
			}
			if entry.Metadata.StoredAt.Add(ttlDuration).After(now) {
				protected[entry.FileHash] = struct{}{}
				continue
			}
		} else {
			// No TTL: artifact lives forever (protected from GC).
			protected[entry.FileHash] = struct{}{}
			continue
		}

		// Not protected and TTL expired: candidate for removal.
		removable = append(removable, entry)
	}

	if request.DryRun {
		var bytesFreed int64
		for _, entry := range removable {
			bytesFreed += entry.Metadata.Size
		}

		as.writeResult(conn, artifactstore.GCResponse{
			ArtifactsRemoved:  len(removable),
			ContainersRemoved: 0,
			BytesFreed:        bytesFreed,
			DryRun:            true,
		})
		return
	}

	// Build the live container set from all remaining (non-removable)
	// artifacts' reconstruction records.
	liveContainers := make(map[artifactstore.Hash]struct{})
	for hash := range protected {
		record, err := as.store.Stat(hash)
		if err != nil {
			continue
		}
		for _, segment := range record.Segments {
			liveContainers[segment.Container] = struct{}{}
		}
	}

	// Delete each removable artifact.
	totalContainersRemoved := 0
	var totalBytesFreed int64
	var artifactsRemoved int

	for _, entry := range removable {
		deletedContainers, err := as.store.Delete(entry.FileHash, liveContainers)
		if err != nil {
			as.logger.Error("gc: failed to delete artifact",
				"ref", entry.Metadata.Ref,
				"error", err,
			)
			continue
		}

		if err := as.metadataStore.Delete(entry.FileHash); err != nil {
			as.logger.Error("gc: failed to delete metadata",
				"ref", entry.Metadata.Ref,
				"error", err,
			)
		}

		as.refIndex.Remove(entry.FileHash)
		as.artifactIndex.Remove(entry.FileHash)

		totalContainersRemoved += len(deletedContainers)
		totalBytesFreed += entry.Metadata.Size
		artifactsRemoved++
	}

	as.logger.Info("gc completed",
		"artifacts_removed", artifactsRemoved,
		"containers_removed", totalContainersRemoved,
		"bytes_freed", totalBytesFreed,
	)

	as.writeResult(conn, artifactstore.GCResponse{
		ArtifactsRemoved:  artifactsRemoved,
		ContainersRemoved: totalContainersRemoved,
		BytesFreed:        totalBytesFreed,
		DryRun:            false,
	})
}

// parseTTL parses a TTL string into a duration. Supports Go duration
// strings (e.g. "168h", "30m") and a "Nd" shorthand for days (e.g.
// "30d" = 30 * 24 hours).
func parseTTL(ttl string) (time.Duration, error) {
	// Try "Nd" day format first.
	if len(ttl) >= 2 && ttl[len(ttl)-1] == 'd' {
		dayStr := ttl[:len(ttl)-1]
		var days int
		if _, err := fmt.Sscanf(dayStr, "%d", &days); err == nil && days > 0 {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}

	return time.ParseDuration(ttl)
}

// --- Cache status action ---

func (as *ArtifactService) handleCacheStatus(ctx context.Context, conn net.Conn, raw []byte) {
	stats := as.artifactIndex.Stats()

	response := artifactstore.CacheStatusResponse{
		StoreArtifacts: stats.Total,
		StoreSizeBytes: stats.TotalSize,
		TagCount:       as.tagStore.Len(),
	}

	if as.cache != nil {
		cacheStats := as.cache.Stats()
		response.CacheDeviceSize = cacheStats.DeviceSize
		response.CacheBlockSize = cacheStats.BlockSize
		response.CacheBlockCount = cacheStats.BlockCount
		response.CacheLiveContainers = cacheStats.LiveContainers
		response.CachePinnedContainers = cacheStats.PinnedContainers
	}

	as.writeResult(conn, response)
}

// --- Wire helpers ---

// handleRevokeTokens processes a signed token revocation request from
// the daemon. Verifies the signature using the daemon's public key
// (from authConfig) and adds the specified token IDs to the blacklist.
// This is an unauthenticated action — the signed revocation blob
// itself is the authentication.
func (as *ArtifactService) handleRevokeTokens(conn net.Conn, raw []byte) {
	if as.authConfig == nil {
		as.writeError(conn, "revocation requires auth config")
		return
	}

	var envelope struct {
		Revocation []byte `cbor:"revocation"`
	}
	if err := codec.Unmarshal(raw, &envelope); err != nil {
		as.writeError(conn, fmt.Sprintf("decoding revocation envelope: %v", err))
		return
	}
	if envelope.Revocation == nil {
		as.writeError(conn, "missing required field: revocation")
		return
	}

	request, err := servicetoken.VerifyRevocation(as.authConfig.PublicKey, envelope.Revocation)
	if err != nil {
		as.logger.Warn("revocation request verification failed", "error", err)
		as.writeError(conn, "revocation verification failed")
		return
	}

	for _, entry := range request.Entries {
		as.authConfig.Blacklist.Revoke(entry.TokenID, time.Unix(entry.ExpiresAt, 0))
	}

	as.logger.Info("tokens revoked via daemon push",
		"count", len(request.Entries),
	)

	as.writeResult(conn, map[string]bool{"ok": true})
}

// --- Upstream (shared cache) ---

// handleSetUpstream processes a signed upstream configuration update
// from the daemon. Verifies the signature using the daemon's public
// key (from authConfig) and updates the upstream socket path.
// This is an unauthenticated action — the signed configuration blob
// itself is the authentication.
func (as *ArtifactService) handleSetUpstream(conn net.Conn, raw []byte) {
	if as.authConfig == nil {
		as.writeError(conn, "set-upstream requires auth config")
		return
	}

	var envelope struct {
		SignedConfig []byte `cbor:"signed_config"`
	}
	if err := codec.Unmarshal(raw, &envelope); err != nil {
		as.writeError(conn, fmt.Sprintf("decoding set-upstream envelope: %v", err))
		return
	}
	if envelope.SignedConfig == nil {
		as.writeError(conn, "missing required field: signed_config")
		return
	}

	config, err := servicetoken.VerifyUpstreamConfig(as.authConfig.PublicKey, envelope.SignedConfig)
	if err != nil {
		as.logger.Warn("upstream config verification failed", "error", err)
		as.writeError(conn, "upstream config verification failed")
		return
	}

	as.upstreamMu.Lock()
	previousSocket := as.upstreamSocket
	as.upstreamSocket = config.UpstreamSocket
	as.upstreamToken = config.ServiceToken
	as.upstreamMu.Unlock()

	if config.UpstreamSocket == "" {
		as.logger.Info("upstream cleared",
			"previous", previousSocket,
		)
	} else {
		as.logger.Info("upstream configured",
			"socket", config.UpstreamSocket,
			"previous", previousSocket,
			"issued_at", config.IssuedAt,
		)
	}

	as.writeResult(conn, map[string]bool{"ok": true})
}

// getUpstreamSocket returns the current upstream socket path, or empty
// string if no upstream is configured. Thread-safe.
func (as *ArtifactService) getUpstreamSocket() string {
	as.upstreamMu.RLock()
	defer as.upstreamMu.RUnlock()
	return as.upstreamSocket
}

// getUpstream returns the current upstream socket path and service
// token atomically. Both values are set together by handleSetUpstream.
// The token may be nil for remote upstream connections where the
// tunnel handler injects authentication.
func (as *ArtifactService) getUpstream() (string, []byte) {
	as.upstreamMu.RLock()
	defer as.upstreamMu.RUnlock()
	return as.upstreamSocket, as.upstreamToken
}

// --- Push targets ---

// handleSetPushTargets processes a signed push targets configuration
// from the daemon. Verifies the signature and replaces the current
// push target mapping. This is an unauthenticated action — the
// signed configuration blob itself is the authentication.
func (as *ArtifactService) handleSetPushTargets(conn net.Conn, raw []byte) {
	if as.authConfig == nil {
		as.writeError(conn, "set-push-targets requires auth config")
		return
	}

	var envelope struct {
		SignedConfig []byte `cbor:"signed_config"`
	}
	if err := codec.Unmarshal(raw, &envelope); err != nil {
		as.writeError(conn, fmt.Sprintf("decoding set-push-targets envelope: %v", err))
		return
	}
	if envelope.SignedConfig == nil {
		as.writeError(conn, "missing required field: signed_config")
		return
	}

	config, err := servicetoken.VerifyPushTargetsConfig(as.authConfig.PublicKey, envelope.SignedConfig)
	if err != nil {
		as.logger.Warn("push targets config verification failed", "error", err)
		as.writeError(conn, "push targets config verification failed")
		return
	}

	as.pushTargetsMu.Lock()
	previous := as.pushTargets
	as.pushTargets = config.Targets
	if as.pushTargets == nil {
		as.pushTargets = make(map[string]servicetoken.PushTarget)
	}
	as.pushTargetsMu.Unlock()

	// Log the changes for operational visibility.
	for machine := range config.Targets {
		if _, existed := previous[machine]; !existed {
			as.logger.Info("push target added", "machine", machine)
		}
	}
	for machine := range previous {
		if _, exists := config.Targets[machine]; !exists {
			as.logger.Info("push target removed", "machine", machine)
		}
	}

	as.logger.Info("push targets configured",
		"targets", len(config.Targets),
		"issued_at", config.IssuedAt,
	)

	as.writeResult(conn, map[string]bool{"ok": true})
}

// resolvedPushTarget holds the resolved socket, token, and name for
// a single push destination.
type resolvedPushTarget struct {
	name       string
	socketPath string
	token      []byte
}

// resolvePushTargets determines which targets an artifact should be
// pushed to based on the store request headers. Two sources of push
// targets:
//   - Explicit: header.PushTargets lists machine localparts
//   - Implicit: header.CachePolicy == "replicate" triggers push to upstream
//
// Returns the resolved targets. Unknown machine localparts are
// included with an empty socketPath so executePushes can report
// the error in PushResult.
func (as *ArtifactService) resolvePushTargets(header *artifactstore.StoreHeader) []resolvedPushTarget {
	var targets []resolvedPushTarget

	if len(header.PushTargets) > 0 {
		as.pushTargetsMu.RLock()
		for _, machine := range header.PushTargets {
			target, exists := as.pushTargets[machine]
			if exists {
				targets = append(targets, resolvedPushTarget{
					name:       machine,
					socketPath: target.SocketPath,
					token:      target.Token,
				})
			} else {
				targets = append(targets, resolvedPushTarget{
					name: machine,
					// Empty socketPath signals an unknown target.
				})
			}
		}
		as.pushTargetsMu.RUnlock()
	}

	if header.CachePolicy == "replicate" {
		upstreamSocket, upstreamToken := as.getUpstream()
		if upstreamSocket != "" {
			targets = append(targets, resolvedPushTarget{
				name:       "upstream",
				socketPath: upstreamSocket,
				token:      upstreamToken,
			})
		}
	}

	return targets
}

// executePushes pushes an artifact to all resolved targets in
// parallel. Returns a PushResult for each target.
func (as *ArtifactService) executePushes(ctx context.Context, fileHash artifactstore.Hash, meta *artifactstore.ArtifactMetadata, targets []resolvedPushTarget) []artifactstore.PushResult {
	results := make([]artifactstore.PushResult, len(targets))
	var waitGroup sync.WaitGroup

	for i, target := range targets {
		waitGroup.Add(1)
		go func(index int, pushTarget resolvedPushTarget) {
			defer waitGroup.Done()
			results[index] = as.pushToTarget(ctx, fileHash, meta, pushTarget)
		}(i, target)
	}

	waitGroup.Wait()
	return results
}

// pushToTarget pushes an artifact to a single target. If the target
// has an empty socketPath (unknown machine), returns an error result
// without attempting a connection. For known targets, reads the
// artifact content and stores it on the remote service.
func (as *ArtifactService) pushToTarget(ctx context.Context, fileHash artifactstore.Hash, meta *artifactstore.ArtifactMetadata, target resolvedPushTarget) artifactstore.PushResult {
	if target.socketPath == "" {
		return artifactstore.PushResult{
			Target: target.name,
			OK:     false,
			Error:  fmt.Sprintf("unknown push target %q", target.name),
		}
	}

	client := artifactstore.NewClientFromToken(target.socketPath, target.token)

	// Build the store header from the local artifact's metadata.
	// No PushTargets (prevent recursive push) and no "replicate"
	// cache policy (prevent recursive replication).
	header := &artifactstore.StoreHeader{
		ContentType: meta.ContentType,
		Filename:    meta.Filename,
		Description: meta.Description,
		Labels:      meta.Labels,
		Visibility:  meta.Visibility,
		TTL:         meta.TTL,
	}

	record, err := as.store.Stat(fileHash)
	if err != nil {
		return artifactstore.PushResult{
			Target: target.name,
			OK:     false,
			Error:  fmt.Sprintf("reading reconstruction record: %v", err),
		}
	}

	if record.Size <= int64(artifactstore.SmallArtifactThreshold) {
		// Small artifact: read content and embed in the header.
		content, err := as.store.ReadContent(fileHash)
		if err != nil {
			return artifactstore.PushResult{
				Target: target.name,
				OK:     false,
				Error:  fmt.Sprintf("reading content for push: %v", err),
			}
		}
		header.Data = content

		_, err = client.Store(ctx, header, nil)
		if err != nil {
			return artifactstore.PushResult{
				Target: target.name,
				OK:     false,
				Error:  fmt.Sprintf("pushing artifact: %v", err),
			}
		}
	} else {
		// Large artifact: stream content via io.Pipe.
		header.Size = record.Size

		pipeReader, pipeWriter := io.Pipe()
		go func() {
			_, err := as.store.Read(fileHash, pipeWriter)
			pipeWriter.CloseWithError(err)
		}()

		_, err = client.Store(ctx, header, pipeReader)
		if err != nil {
			pipeReader.Close()
			return artifactstore.PushResult{
				Target: target.name,
				OK:     false,
				Error:  fmt.Sprintf("pushing artifact: %v", err),
			}
		}
	}

	as.logger.Info("artifact pushed to target",
		"target", target.name,
		"ref", artifactstore.FormatRef(fileHash),
		"size", record.Size,
	)

	return artifactstore.PushResult{
		Target: target.name,
		OK:     true,
	}
}

// isUpstreamRef returns true if the ref format is eligible for upstream
// forwarding. Only hash-format (64 hex chars) and short-ref-format
// (art-<hex>) refs are forwarded. Tags are local to each service
// instance — forwarding tag resolution would cause confusing
// cross-instance name collisions.
func (as *ArtifactService) isUpstreamRef(ref string) bool {
	if as.getUpstreamSocket() == "" {
		return false
	}
	// Full 64-char hex hash.
	if len(ref) == 64 {
		return true
	}
	// Short ref: art-<12 hex>.
	if strings.HasPrefix(ref, "art-") {
		return true
	}
	return false
}

// fetchFromUpstream fetches an artifact from the upstream shared cache,
// stores it locally, and returns the local file hash. On success,
// returns the hash and true. On failure, writes an error to conn and
// returns false.
//
// The re-store approach fetches the full content and writes it through
// the local store's normal CDC+compression pipeline. CDC is
// deterministic, so identical containers are produced and deduplicated.
func (as *ArtifactService) fetchFromUpstream(ctx context.Context, conn net.Conn, ref string) (artifactstore.Hash, bool) {
	upstreamSocket, upstreamToken := as.getUpstream()
	if upstreamSocket == "" {
		as.writeError(conn, fmt.Sprintf("artifact %s not found", ref))
		return artifactstore.Hash{}, false
	}

	upstream := artifactstore.NewClientFromToken(upstreamSocket, upstreamToken)
	result, err := upstream.Fetch(ctx, ref)
	if err != nil {
		as.writeError(conn, fmt.Sprintf("artifact %s not found locally; upstream fetch failed: %v", ref, err))
		return artifactstore.Hash{}, false
	}
	defer result.Content.Close()

	// Read the full content from the upstream.
	content, err := io.ReadAll(result.Content)
	if err != nil {
		as.writeError(conn, fmt.Sprintf("reading upstream content for %s: %v", ref, err))
		return artifactstore.Hash{}, false
	}

	// Store locally through the standard write pipeline.
	as.writeMu.Lock()
	storeResult, err := as.store.WriteContent(content, result.Response.ContentType)
	if err != nil {
		as.writeMu.Unlock()
		as.writeError(conn, fmt.Sprintf("storing upstream artifact %s locally: %v", ref, err))
		return artifactstore.Hash{}, false
	}

	// Persist metadata. We carry forward the content type, filename,
	// and visibility from the upstream response. Other fields (labels,
	// description, cache policy) are not available from the fetch
	// response — they belong to the upstream's metadata. The local
	// copy gets minimal metadata reflecting its origin as a cache fill.
	//
	// Visibility must be preserved: a private artifact at the upstream
	// must remain private in the local cache. Default to private if
	// the upstream didn't include visibility (older service version).
	meta := &artifactstore.ArtifactMetadata{
		FileHash:       storeResult.FileHash,
		Ref:            storeResult.Ref,
		ContentType:    result.Response.ContentType,
		Filename:       result.Response.Filename,
		Visibility:     artifactstore.NormalizeVisibility(result.Response.Visibility),
		Size:           storeResult.Size,
		ChunkCount:     storeResult.ChunkCount,
		ContainerCount: storeResult.ContainerCount,
		Compression:    storeResult.Compression.String(),
		StoredAt:       as.clock.Now(),
	}
	if err := as.metadataStore.Write(meta); err != nil {
		as.writeMu.Unlock()
		as.writeError(conn, fmt.Sprintf("persisting metadata for upstream artifact %s: %v", ref, err))
		return artifactstore.Hash{}, false
	}

	as.refIndex.Add(storeResult.FileHash)
	as.artifactIndex.Put(*meta)
	as.writeMu.Unlock()

	as.logger.Info("artifact fetched from upstream and stored locally",
		"ref", storeResult.Ref,
		"upstream", upstreamSocket,
		"size", storeResult.Size,
		"chunks", storeResult.ChunkCount,
		"containers", storeResult.ContainerCount,
	)

	return storeResult.FileHash, true
}

// writeError sends an ErrorResponse to the client.
func (as *ArtifactService) writeError(conn net.Conn, message string) {
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := artifactstore.WriteMessage(conn, artifactstore.ErrorResponse{Error: message}); err != nil {
		as.logger.Debug("failed to write error response", "error", err)
	}
}

// writeResult sends a success result to the client. The value is
// encoded directly as a CBOR message — no wrapping envelope.
func (as *ArtifactService) writeResult(conn net.Conn, result any) {
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := artifactstore.WriteMessage(conn, result); err != nil {
		as.logger.Debug("failed to write result", "error", err)
	}
}
