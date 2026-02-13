// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/artifact"
	"github.com/bureau-foundation/bureau/lib/clock"
)

// testService creates an ArtifactService backed by temporary
// directories for testing. The Store, MetadataStore, and RefIndex
// are real — no mocking. The Matrix session and sync machinery are
// not needed for handler tests since the handlers operate on the
// local store.
func testService(t *testing.T) *ArtifactService {
	t.Helper()

	storeDir := t.TempDir()
	store, err := artifact.NewStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	metadataStore, err := artifact.NewMetadataStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	return &ArtifactService{
		store:         store,
		metadataStore: metadataStore,
		refIndex:      artifact.NewRefIndex(),
		clock:         clock.Real(),
		startedAt:     time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		rooms:         make(map[string]*artifactRoomState),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// startHandler launches handleConnection in a goroutine against one
// end of a net.Pipe and returns the client end plus a wait function.
// The test writes requests and reads responses via the returned
// connection. The wait function blocks until the handler goroutine
// exits — call it AFTER reading the full response, since net.Pipe is
// synchronous (writes block until reads happen).
//
// t.Cleanup closes the client connection and waits for the handler,
// ensuring clean shutdown even on early test failure.
func startHandler(t *testing.T, as *ArtifactService) (net.Conn, func()) {
	t.Helper()
	clientConn, serverConn := net.Pipe()

	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		as.handleConnection(t.Context(), serverConn)
	}()

	t.Cleanup(func() {
		clientConn.Close()
		done.Wait()
	})

	return clientConn, done.Wait
}

// --- Store tests ---

func TestStoreSmallArtifact(t *testing.T) {
	as := testService(t)

	content := []byte("hello, artifact service!")
	conn, wait := startHandler(t, as)

	// Send a store header with embedded data.
	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
		Description: "test artifact",
		Labels:      []string{"test"},
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	// Read the store response. This unblocks the handler's write
	// on the synchronous net.Pipe.
	var response artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}

	wait()

	if !strings.HasPrefix(response.Ref, "art-") {
		t.Errorf("ref = %q, want art- prefix", response.Ref)
	}
	if response.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", response.Size, len(content))
	}
	if response.ChunkCount != 1 {
		t.Errorf("chunk_count = %d, want 1", response.ChunkCount)
	}
	if response.Hash == "" {
		t.Error("hash is empty")
	}

	// Verify the ref index was updated.
	if as.refIndex.Len() != 1 {
		t.Errorf("refIndex.Len() = %d, want 1", as.refIndex.Len())
	}
}

func TestStoreLargeArtifactSized(t *testing.T) {
	as := testService(t)

	// Content larger than SmallArtifactThreshold (256KB).
	content := make([]byte, 300*1024)
	rand.Read(content)

	conn, wait := startHandler(t, as)

	// Send store header without embedded data (large artifact).
	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "application/octet-stream",
		Size:        int64(len(content)),
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	// Stream the content as a sized transfer (write raw bytes).
	if _, err := conn.Write(content); err != nil {
		t.Fatal(err)
	}

	// Read the store response.
	var response artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}

	wait()

	if response.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", response.Size, len(content))
	}
	if response.ChunkCount < 1 {
		t.Errorf("chunk_count = %d, want >= 1", response.ChunkCount)
	}
}

func TestStoreLargeArtifactChunked(t *testing.T) {
	as := testService(t)

	content := make([]byte, 300*1024)
	rand.Read(content)

	conn, wait := startHandler(t, as)

	// Send store header with SizeUnknown (chunked transfer).
	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "application/octet-stream",
		Size:        artifact.SizeUnknown,
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	// Stream content using FrameWriter.
	frameWriter := artifact.NewFrameWriter(conn)
	if _, err := frameWriter.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := frameWriter.Close(); err != nil {
		t.Fatal(err)
	}

	// Read the store response.
	var response artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}

	wait()

	if response.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", response.Size, len(content))
	}
}

// --- Fetch tests ---

func TestFetchSmallArtifact(t *testing.T) {
	as := testService(t)

	// Store an artifact first.
	content := []byte("fetchable content")
	ref := storeTestArtifact(t, as, content, "text/plain")

	conn, wait := startHandler(t, as)

	request := &artifact.FetchRequest{
		Action: "fetch",
		Ref:    ref,
	}
	if err := artifact.WriteMessage(conn, request); err != nil {
		t.Fatal(err)
	}

	// Read the fetch response.
	var response artifact.FetchResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}

	wait()

	if response.Data == nil {
		t.Fatal("expected embedded data for small artifact")
	}
	if !bytes.Equal(response.Data, content) {
		t.Error("fetched content does not match stored content")
	}
	if response.ContentType != "text/plain" {
		t.Errorf("content_type = %q, want 'text/plain'", response.ContentType)
	}
	if response.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", response.Size, len(content))
	}
}

func TestFetchLargeArtifact(t *testing.T) {
	as := testService(t)

	// Store a large artifact.
	content := make([]byte, 300*1024)
	rand.Read(content)
	ref := storeTestArtifact(t, as, content, "application/octet-stream")

	conn, wait := startHandler(t, as)

	request := &artifact.FetchRequest{
		Action: "fetch",
		Ref:    ref,
	}
	if err := artifact.WriteMessage(conn, request); err != nil {
		t.Fatal(err)
	}

	// Read the fetch response header.
	var response artifact.FetchResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}

	if response.Data != nil {
		t.Fatal("expected nil Data for large artifact (binary stream follows)")
	}
	if response.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", response.Size, len(content))
	}

	// Read the binary stream (sized — exactly Size bytes).
	fetched := make([]byte, response.Size)
	if _, err := io.ReadFull(conn, fetched); err != nil {
		t.Fatalf("reading binary stream: %v", err)
	}

	wait()

	if !bytes.Equal(fetched, content) {
		t.Error("fetched content does not match stored content")
	}
}

func TestFetchByFullHash(t *testing.T) {
	as := testService(t)

	content := []byte("fetch by hash")
	storeTestArtifact(t, as, content, "text/plain")

	// Get the full hash from the ref index.
	hash := getStoredHash(t, as, content)

	conn, wait := startHandler(t, as)

	// Fetch using the full 64-char hex hash.
	request := &artifact.FetchRequest{
		Action: "fetch",
		Ref:    artifact.FormatHash(hash),
	}
	if err := artifact.WriteMessage(conn, request); err != nil {
		t.Fatal(err)
	}

	var response artifact.FetchResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}

	wait()

	if !bytes.Equal(response.Data, content) {
		t.Error("fetched content does not match")
	}
}

func TestFetchNotFound(t *testing.T) {
	as := testService(t)

	conn, wait := startHandler(t, as)

	request := &artifact.FetchRequest{
		Action: "fetch",
		Ref:    "art-000000000000",
	}
	if err := artifact.WriteMessage(conn, request); err != nil {
		t.Fatal(err)
	}

	var errResp artifact.ErrorResponse
	if err := artifact.ReadMessage(conn, &errResp); err != nil {
		t.Fatal(err)
	}

	wait()

	if errResp.Error == "" {
		t.Error("expected error response for missing artifact")
	}
	if !strings.Contains(errResp.Error, "not found") {
		t.Errorf("error = %q, want contains 'not found'", errResp.Error)
	}
}

// --- Exists tests ---

func TestExistsFound(t *testing.T) {
	as := testService(t)

	content := []byte("existing artifact")
	ref := storeTestArtifact(t, as, content, "text/plain")

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, refRequest{Action: "exists", Ref: ref}); err != nil {
		t.Fatal(err)
	}

	var response existsResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}

	wait()

	if !response.Exists {
		t.Error("expected exists = true")
	}
	if response.Hash == "" {
		t.Error("hash is empty")
	}
	if response.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", response.Size, len(content))
	}
}

func TestExistsNotFound(t *testing.T) {
	as := testService(t)

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, refRequest{Action: "exists", Ref: "art-000000000000"}); err != nil {
		t.Fatal(err)
	}

	var response existsResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}

	wait()

	if response.Exists {
		t.Error("expected exists = false")
	}
}

// --- Show tests ---

func TestShow(t *testing.T) {
	as := testService(t)

	content := []byte("artifact with metadata")
	ref := storeTestArtifactWithMeta(t, as, content, "text/plain", "notes.txt", "important notes", []string{"docs"})

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, refRequest{Action: "show", Ref: ref}); err != nil {
		t.Fatal(err)
	}

	var meta artifact.ArtifactMetadata
	if err := artifact.ReadMessage(conn, &meta); err != nil {
		t.Fatal(err)
	}

	wait()

	if meta.ContentType != "text/plain" {
		t.Errorf("content_type = %q, want 'text/plain'", meta.ContentType)
	}
	if meta.Filename != "notes.txt" {
		t.Errorf("filename = %q, want 'notes.txt'", meta.Filename)
	}
	if meta.Description != "important notes" {
		t.Errorf("description = %q, want 'important notes'", meta.Description)
	}
	if len(meta.Labels) != 1 || meta.Labels[0] != "docs" {
		t.Errorf("labels = %v, want [docs]", meta.Labels)
	}
	if meta.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", meta.Size, len(content))
	}
}

// --- Status tests ---

func TestStatus(t *testing.T) {
	as := testService(t)

	// Store a couple of artifacts.
	storeTestArtifact(t, as, []byte("one"), "text/plain")
	storeTestArtifact(t, as, []byte("two"), "text/plain")

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, struct {
		Action string `cbor:"action"`
	}{Action: "status"}); err != nil {
		t.Fatal(err)
	}

	var response statusResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}

	wait()

	if response.UptimeSeconds < 0 {
		t.Errorf("uptime = %f, want >= 0", response.UptimeSeconds)
	}
	if response.Artifacts != 2 {
		t.Errorf("artifacts = %d, want 2", response.Artifacts)
	}
}

// --- Error handling tests ---

func TestUnknownAction(t *testing.T) {
	as := testService(t)

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, struct {
		Action string `cbor:"action"`
	}{Action: "bogus"}); err != nil {
		t.Fatal(err)
	}

	var errResp artifact.ErrorResponse
	if err := artifact.ReadMessage(conn, &errResp); err != nil {
		t.Fatal(err)
	}

	wait()

	if !strings.Contains(errResp.Error, "unknown action") {
		t.Errorf("error = %q, want contains 'unknown action'", errResp.Error)
	}
}

func TestMissingAction(t *testing.T) {
	as := testService(t)

	conn, wait := startHandler(t, as)

	// Send a CBOR message without an action field.
	if err := artifact.WriteMessage(conn, struct {
		Value int `cbor:"value"`
	}{Value: 42}); err != nil {
		t.Fatal(err)
	}

	var errResp artifact.ErrorResponse
	if err := artifact.ReadMessage(conn, &errResp); err != nil {
		t.Fatal(err)
	}

	wait()

	if !strings.Contains(errResp.Error, "missing required field: action") {
		t.Errorf("error = %q, want contains 'missing required field: action'", errResp.Error)
	}
}

func TestInvalidRef(t *testing.T) {
	as := testService(t)

	conn, wait := startHandler(t, as)

	// Send a fetch with an invalid ref format.
	request := &artifact.FetchRequest{
		Action: "fetch",
		Ref:    "not-a-valid-ref",
	}
	if err := artifact.WriteMessage(conn, request); err != nil {
		t.Fatal(err)
	}

	var errResp artifact.ErrorResponse
	if err := artifact.ReadMessage(conn, &errResp); err != nil {
		t.Fatal(err)
	}

	wait()

	if !strings.Contains(errResp.Error, "invalid artifact reference") {
		t.Errorf("error = %q, want contains 'invalid artifact reference'", errResp.Error)
	}
}

// --- Test helpers ---

// storeTestArtifact stores content directly through the service's
// store and metadata pipeline (bypassing the socket protocol) and
// returns the short ref.
func storeTestArtifact(t *testing.T, as *ArtifactService, content []byte, contentType string) string {
	t.Helper()
	return storeTestArtifactWithMeta(t, as, content, contentType, "", "", nil)
}

func storeTestArtifactWithMeta(t *testing.T, as *ArtifactService, content []byte, contentType, filename, description string, labels []string) string {
	t.Helper()

	result, err := as.store.WriteContent(content, contentType)
	if err != nil {
		t.Fatalf("storing test artifact: %v", err)
	}

	meta := &artifact.ArtifactMetadata{
		FileHash:       result.FileHash,
		Ref:            result.Ref,
		ContentType:    contentType,
		Filename:       filename,
		Description:    description,
		Labels:         labels,
		Size:           result.Size,
		ChunkCount:     result.ChunkCount,
		ContainerCount: result.ContainerCount,
		Compression:    result.Compression.String(),
		StoredAt:       time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := as.metadataStore.Write(meta); err != nil {
		t.Fatalf("writing test metadata: %v", err)
	}

	as.refIndex.Add(result.FileHash)
	return result.Ref
}

// getStoredHash computes the file hash for content, matching the
// hashing that Store.WriteContent uses.
func getStoredHash(t *testing.T, as *ArtifactService, content []byte) artifact.Hash {
	t.Helper()
	chunkHash := artifact.HashChunk(content)
	return artifact.HashFile(chunkHash)
}
