// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/artifact"
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/testutil"
)

// testClockEpoch is the fixed time used by the fake clock in artifact
// service tests. Token timestamps are relative to this epoch.
var testClockEpoch = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

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

	tagStore, err := artifact.NewTagStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	return &ArtifactService{
		store:         store,
		metadataStore: metadataStore,
		refIndex:      artifact.NewRefIndex(),
		tagStore:      tagStore,
		artifactIndex: artifact.NewArtifactIndex(),
		clock:         clock.Fake(testClockEpoch),
		startedAt:     testClockEpoch,
		rooms:         make(map[ref.RoomID]*artifactRoomState),
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

	var response artifact.ExistsResponse
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

	var response artifact.ExistsResponse
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
		Action string `json:"action"`
	}{Action: "status"}); err != nil {
		t.Fatal(err)
	}

	var response artifact.StatusResponse
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
		Action string `json:"action"`
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
		Value int `json:"value"`
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

	if !strings.Contains(errResp.Error, "unknown artifact reference") {
		t.Errorf("error = %q, want contains 'unknown artifact reference'", errResp.Error)
	}
}

// --- Tag tests ---

func TestTagAndFetchByName(t *testing.T) {
	as := testService(t)

	content := []byte("tagged artifact content")
	ref := storeTestArtifact(t, as, content, "text/plain")

	// Create a tag pointing to the artifact.
	conn, wait := startHandler(t, as)

	tagRequest := &artifact.TagRequest{
		Action:     "tag",
		Name:       "model/latest",
		Ref:        ref,
		Optimistic: true,
	}
	if err := artifact.WriteMessage(conn, tagRequest); err != nil {
		t.Fatal(err)
	}

	var tagResp artifact.TagResponse
	if err := artifact.ReadMessage(conn, &tagResp); err != nil {
		t.Fatal(err)
	}
	wait()

	if tagResp.Name != "model/latest" {
		t.Errorf("tag name = %q, want %q", tagResp.Name, "model/latest")
	}
	if tagResp.Hash == "" {
		t.Error("tag response hash is empty")
	}

	// Fetch by tag name.
	conn2, wait2 := startHandler(t, as)

	fetchRequest := &artifact.FetchRequest{
		Action: "fetch",
		Ref:    "model/latest",
	}
	if err := artifact.WriteMessage(conn2, fetchRequest); err != nil {
		t.Fatal(err)
	}

	var fetchResp artifact.FetchResponse
	if err := artifact.ReadMessage(conn2, &fetchResp); err != nil {
		t.Fatal(err)
	}
	wait2()

	if !bytes.Equal(fetchResp.Data, content) {
		t.Error("fetched content via tag does not match")
	}
}

func TestStoreWithTag(t *testing.T) {
	as := testService(t)

	content := []byte("store with tag")
	conn, wait := startHandler(t, as)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
		Tag:         "auto/tagged",
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	var response artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	// Verify tag was created.
	tag, exists := as.tagStore.Get("auto/tagged")
	if !exists {
		t.Fatal("tag not created by store-with-tag")
	}

	// Verify tag points to the stored artifact.
	if artifact.FormatRef(tag.Target) != response.Ref {
		t.Errorf("tag target ref = %s, want %s", artifact.FormatRef(tag.Target), response.Ref)
	}
}

func TestTagCompareAndSwap(t *testing.T) {
	as := testService(t)

	content1 := []byte("version 1")
	content2 := []byte("version 2")
	ref1 := storeTestArtifact(t, as, content1, "text/plain")
	ref2 := storeTestArtifact(t, as, content2, "text/plain")

	// Create initial tag.
	conn1, wait1 := startHandler(t, as)
	if err := artifact.WriteMessage(conn1, &artifact.TagRequest{
		Action:     "tag",
		Name:       "cas/test",
		Ref:        ref1,
		Optimistic: true,
	}); err != nil {
		t.Fatal(err)
	}
	var resp1 artifact.TagResponse
	if err := artifact.ReadMessage(conn1, &resp1); err != nil {
		t.Fatal(err)
	}
	wait1()

	// CAS with wrong previous should fail.
	conn2, wait2 := startHandler(t, as)
	wrongPrevious := strings.Repeat("00", 32)
	if err := artifact.WriteMessage(conn2, &artifact.TagRequest{
		Action:           "tag",
		Name:             "cas/test",
		Ref:              ref2,
		ExpectedPrevious: wrongPrevious,
	}); err != nil {
		t.Fatal(err)
	}
	var errResp artifact.ErrorResponse
	if err := artifact.ReadMessage(conn2, &errResp); err != nil {
		t.Fatal(err)
	}
	wait2()

	if errResp.Error == "" {
		t.Fatal("expected CAS conflict error")
	}
	if !strings.Contains(errResp.Error, "tag conflict") {
		t.Errorf("error = %q, want contains 'tag conflict'", errResp.Error)
	}

	// CAS with correct previous should succeed.
	conn3, wait3 := startHandler(t, as)
	if err := artifact.WriteMessage(conn3, &artifact.TagRequest{
		Action:           "tag",
		Name:             "cas/test",
		Ref:              ref2,
		ExpectedPrevious: resp1.Hash,
	}); err != nil {
		t.Fatal(err)
	}
	var resp3 artifact.TagResponse
	if err := artifact.ReadMessage(conn3, &resp3); err != nil {
		t.Fatal(err)
	}
	wait3()

	if resp3.Previous == "" {
		t.Error("expected previous ref in response")
	}
}

func TestResolve(t *testing.T) {
	as := testService(t)

	content := []byte("resolvable")
	ref := storeTestArtifact(t, as, content, "text/plain")

	// Tag the artifact.
	hash := getStoredHash(t, as, content)
	as.tagStore.Set("named/thing", hash, nil, true, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	// Resolve by tag name.
	conn, wait := startHandler(t, as)
	if err := artifact.WriteMessage(conn, &artifact.ResolveRequest{
		Action: "resolve",
		Ref:    "named/thing",
	}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.ResolveResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if resp.Tag != "named/thing" {
		t.Errorf("tag = %q, want %q", resp.Tag, "named/thing")
	}
	if resp.Ref != ref {
		t.Errorf("ref = %q, want %q", resp.Ref, ref)
	}
}

func TestListTags(t *testing.T) {
	as := testService(t)

	content := []byte("tagged")
	ref := storeTestArtifact(t, as, content, "text/plain")
	hash := getStoredHash(t, as, content)

	// Create several tags.
	tagTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	as.tagStore.Set("prefix/a", hash, nil, true, tagTime)
	as.tagStore.Set("prefix/b", hash, nil, true, tagTime)
	as.tagStore.Set("other/c", hash, nil, true, tagTime)
	_ = ref

	// List with prefix.
	conn, wait := startHandler(t, as)
	if err := artifact.WriteMessage(conn, &artifact.TagsRequest{
		Action: "tags",
		Prefix: "prefix/",
	}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.TagsResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if len(resp.Tags) != 2 {
		t.Fatalf("tags count = %d, want 2", len(resp.Tags))
	}
}

func TestDeleteTag(t *testing.T) {
	as := testService(t)

	content := []byte("deletable")
	storeTestArtifact(t, as, content, "text/plain")
	hash := getStoredHash(t, as, content)
	as.tagStore.Set("to-delete", hash, nil, true, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	// Delete the tag.
	conn, wait := startHandler(t, as)
	if err := artifact.WriteMessage(conn, &artifact.DeleteTagRequest{
		Action: "delete-tag",
		Name:   "to-delete",
	}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.DeleteTagResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if resp.Deleted != "to-delete" {
		t.Errorf("deleted = %q, want %q", resp.Deleted, "to-delete")
	}

	// Verify tag is gone.
	if _, exists := as.tagStore.Get("to-delete"); exists {
		t.Error("tag still exists after delete")
	}

	// Verify resolve by tag name fails.
	conn2, wait2 := startHandler(t, as)
	if err := artifact.WriteMessage(conn2, &artifact.FetchRequest{
		Action: "fetch",
		Ref:    "to-delete",
	}); err != nil {
		t.Fatal(err)
	}

	var errResp artifact.ErrorResponse
	if err := artifact.ReadMessage(conn2, &errResp); err != nil {
		t.Fatal(err)
	}
	wait2()

	if errResp.Error == "" {
		t.Error("expected error when fetching by deleted tag name")
	}
}

// --- List tests ---

func TestListAllArtifacts(t *testing.T) {
	as := testService(t)

	storeTestArtifact(t, as, []byte("one"), "text/plain")
	storeTestArtifact(t, as, []byte("two"), "text/plain")
	storeTestArtifact(t, as, []byte("three"), "image/png")

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, &artifact.ListRequest{
		Action: "list",
	}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.ListResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if resp.Total != 3 {
		t.Errorf("total = %d, want 3", resp.Total)
	}
	if len(resp.Artifacts) != 3 {
		t.Errorf("artifacts = %d, want 3", len(resp.Artifacts))
	}
}

func TestListByContentType(t *testing.T) {
	as := testService(t)

	storeTestArtifactWithMeta(t, as, []byte("text1"), "text/plain", "", "", nil)
	storeTestArtifactWithMeta(t, as, []byte("text2"), "text/plain", "", "", nil)
	storeTestArtifactWithMeta(t, as, []byte("img1"), "image/png", "", "", nil)

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, &artifact.ListRequest{
		Action:      "list",
		ContentType: "text/plain",
	}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.ListResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if resp.Total != 2 {
		t.Errorf("total = %d, want 2", resp.Total)
	}
	for _, summary := range resp.Artifacts {
		if summary.ContentType != "text/plain" {
			t.Errorf("content_type = %q, want text/plain", summary.ContentType)
		}
	}
}

func TestListByLabel(t *testing.T) {
	as := testService(t)

	storeTestArtifactWithMeta(t, as, []byte("doc1"), "text/plain", "", "", []string{"docs", "important"})
	storeTestArtifactWithMeta(t, as, []byte("doc2"), "text/plain", "", "", []string{"docs"})
	storeTestArtifactWithMeta(t, as, []byte("code1"), "text/plain", "", "", []string{"code"})

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, &artifact.ListRequest{
		Action: "list",
		Label:  "docs",
	}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.ListResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if resp.Total != 2 {
		t.Errorf("total = %d, want 2", resp.Total)
	}
}

func TestListPagination(t *testing.T) {
	as := testService(t)

	// Store 5 artifacts.
	for i := 0; i < 5; i++ {
		content := []byte(string(rune('a' + i)))
		storeTestArtifact(t, as, content, "text/plain")
	}

	// Page 1: limit 2.
	conn1, wait1 := startHandler(t, as)
	if err := artifact.WriteMessage(conn1, &artifact.ListRequest{
		Action: "list",
		Limit:  2,
	}); err != nil {
		t.Fatal(err)
	}

	var resp1 artifact.ListResponse
	if err := artifact.ReadMessage(conn1, &resp1); err != nil {
		t.Fatal(err)
	}
	wait1()

	if resp1.Total != 5 {
		t.Errorf("total = %d, want 5", resp1.Total)
	}
	if len(resp1.Artifacts) != 2 {
		t.Errorf("page 1 artifacts = %d, want 2", len(resp1.Artifacts))
	}

	// Page 2: offset 2, limit 2.
	conn2, wait2 := startHandler(t, as)
	if err := artifact.WriteMessage(conn2, &artifact.ListRequest{
		Action: "list",
		Limit:  2,
		Offset: 2,
	}); err != nil {
		t.Fatal(err)
	}

	var resp2 artifact.ListResponse
	if err := artifact.ReadMessage(conn2, &resp2); err != nil {
		t.Fatal(err)
	}
	wait2()

	if len(resp2.Artifacts) != 2 {
		t.Errorf("page 2 artifacts = %d, want 2", len(resp2.Artifacts))
	}
}

func TestListEmpty(t *testing.T) {
	as := testService(t)

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, &artifact.ListRequest{
		Action: "list",
	}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.ListResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if resp.Total != 0 {
		t.Errorf("total = %d, want 0", resp.Total)
	}
	if len(resp.Artifacts) != 0 {
		t.Errorf("artifacts = %d, want 0", len(resp.Artifacts))
	}
}

func TestListSummaryFields(t *testing.T) {
	as := testService(t)

	storeTestArtifactWithMeta(t, as, []byte("detailed"), "text/plain", "report.txt", "quarterly report", []string{"finance"})

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, &artifact.ListRequest{
		Action: "list",
	}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.ListResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if len(resp.Artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(resp.Artifacts))
	}

	summary := resp.Artifacts[0]
	if summary.Hash == "" {
		t.Error("hash is empty")
	}
	if !strings.HasPrefix(summary.Ref, "art-") {
		t.Errorf("ref = %q, want art- prefix", summary.Ref)
	}
	if summary.ContentType != "text/plain" {
		t.Errorf("content_type = %q, want text/plain", summary.ContentType)
	}
	if summary.Filename != "report.txt" {
		t.Errorf("filename = %q, want report.txt", summary.Filename)
	}
	if summary.Size != int64(len("detailed")) {
		t.Errorf("size = %d, want %d", summary.Size, len("detailed"))
	}
	if len(summary.Labels) != 1 || summary.Labels[0] != "finance" {
		t.Errorf("labels = %v, want [finance]", summary.Labels)
	}
	if summary.StoredAt == "" {
		t.Error("stored_at is empty")
	}
}

func TestStoreUpdatesArtifactIndex(t *testing.T) {
	as := testService(t)

	// Store via the socket protocol (not the test helper).
	content := []byte("indexed via store handler")
	conn, wait := startHandler(t, as)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
		Labels:      []string{"indexed"},
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	var storeResp artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &storeResp); err != nil {
		t.Fatal(err)
	}
	wait()

	// Verify the artifact index was updated by listing.
	conn2, wait2 := startHandler(t, as)
	if err := artifact.WriteMessage(conn2, &artifact.ListRequest{
		Action: "list",
		Label:  "indexed",
	}); err != nil {
		t.Fatal(err)
	}

	var listResp artifact.ListResponse
	if err := artifact.ReadMessage(conn2, &listResp); err != nil {
		t.Fatal(err)
	}
	wait2()

	if listResp.Total != 1 {
		t.Errorf("list total = %d, want 1 (store should update artifact index)", listResp.Total)
	}
}

// --- Visibility tests ---

func TestStoreDefaultsToPrivateVisibility(t *testing.T) {
	as := testService(t)

	content := []byte("should be private by default")
	conn, wait := startHandler(t, as)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
		// Visibility not set — should default to "private".
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	var response artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	// Verify metadata has private visibility.
	hash, err := artifact.ParseHash(response.Hash)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := as.metadataStore.Read(hash)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Visibility != artifact.VisibilityPrivate {
		t.Errorf("visibility = %q, want %q", meta.Visibility, artifact.VisibilityPrivate)
	}
}

func TestStoreAcceptsPublicVisibility(t *testing.T) {
	as := testService(t)

	content := []byte("public dataset content")
	conn, wait := startHandler(t, as)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
		Visibility:  "public",
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	var response artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	hash, err := artifact.ParseHash(response.Hash)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := as.metadataStore.Read(hash)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Visibility != artifact.VisibilityPublic {
		t.Errorf("visibility = %q, want %q", meta.Visibility, artifact.VisibilityPublic)
	}
}

func TestStoreRejectsInvalidVisibility(t *testing.T) {
	as := testService(t)

	content := []byte("bad visibility")
	conn, wait := startHandler(t, as)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
		Visibility:  "banana",
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	var errResp artifact.ErrorResponse
	if err := artifact.ReadMessage(conn, &errResp); err != nil {
		t.Fatal(err)
	}
	wait()

	if errResp.Error == "" {
		t.Fatal("expected error response for invalid visibility")
	}
	if !strings.Contains(errResp.Error, "invalid visibility") {
		t.Errorf("error = %q, want it to contain %q", errResp.Error, "invalid visibility")
	}
}

func TestFetchResponseIncludesVisibility(t *testing.T) {
	as := testService(t)

	// Store via the handler to get proper visibility defaulting.
	content := []byte("fetch visibility test")
	conn, wait := startHandler(t, as)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
		Visibility:  "public",
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	var storeResp artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &storeResp); err != nil {
		t.Fatal(err)
	}
	wait()

	// Fetch the artifact and check visibility in the response.
	conn2, wait2 := startHandler(t, as)
	if err := artifact.WriteMessage(conn2, &artifact.FetchRequest{
		Action: "fetch",
		Ref:    storeResp.Ref,
	}); err != nil {
		t.Fatal(err)
	}

	var fetchResp artifact.FetchResponse
	if err := artifact.ReadMessage(conn2, &fetchResp); err != nil {
		t.Fatal(err)
	}
	wait2()

	if fetchResp.Visibility != artifact.VisibilityPublic {
		t.Errorf("fetch response visibility = %q, want %q", fetchResp.Visibility, artifact.VisibilityPublic)
	}
}

func TestPinPreservesVisibility(t *testing.T) {
	as := testService(t)

	// Store a public artifact via the handler.
	content := []byte("pin visibility test")
	conn, wait := startHandler(t, as)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
		Visibility:  "public",
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	var storeResp artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &storeResp); err != nil {
		t.Fatal(err)
	}
	wait()

	// Pin the artifact — this updates metadata (cache_policy).
	conn2, wait2 := startHandler(t, as)
	if err := artifact.WriteMessage(conn2, &artifact.PinRequest{
		Action: "pin",
		Ref:    storeResp.Ref,
	}); err != nil {
		t.Fatal(err)
	}

	var pinResp artifact.PinResponse
	if err := artifact.ReadMessage(conn2, &pinResp); err != nil {
		t.Fatal(err)
	}
	wait2()

	// Verify visibility was not clobbered by the pin's metadata rewrite.
	hash, err := artifact.ParseHash(storeResp.Hash)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := as.metadataStore.Read(hash)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Visibility != artifact.VisibilityPublic {
		t.Errorf("after pin, visibility = %q, want %q", meta.Visibility, artifact.VisibilityPublic)
	}
	if meta.CachePolicy != "pin" {
		t.Errorf("after pin, cache_policy = %q, want %q", meta.CachePolicy, "pin")
	}
}

func TestListIncludesVisibility(t *testing.T) {
	as := testService(t)

	// Store one public and one private (default) artifact via handler.
	for _, visibility := range []string{"public", ""} {
		content := []byte("list-vis-" + visibility)
		conn, wait := startHandler(t, as)
		header := &artifact.StoreHeader{
			Action:      "store",
			ContentType: "text/plain",
			Size:        int64(len(content)),
			Data:        content,
			Visibility:  visibility,
		}
		if err := artifact.WriteMessage(conn, header); err != nil {
			t.Fatal(err)
		}
		var resp artifact.StoreResponse
		if err := artifact.ReadMessage(conn, &resp); err != nil {
			t.Fatal(err)
		}
		wait()
	}

	// List all artifacts and check visibility is populated.
	conn, wait := startHandler(t, as)
	if err := artifact.WriteMessage(conn, &artifact.ListRequest{
		Action: "list",
	}); err != nil {
		t.Fatal(err)
	}

	var listResp artifact.ListResponse
	if err := artifact.ReadMessage(conn, &listResp); err != nil {
		t.Fatal(err)
	}
	wait()

	if listResp.Total != 2 {
		t.Fatalf("list total = %d, want 2", listResp.Total)
	}

	visibilities := map[string]bool{}
	for _, summary := range listResp.Artifacts {
		if summary.Visibility == "" {
			t.Errorf("artifact %s has empty visibility in list response", summary.Ref)
		}
		visibilities[summary.Visibility] = true
	}
	if !visibilities["public"] {
		t.Error("expected one artifact with visibility=public in list")
	}
	if !visibilities["private"] {
		t.Error("expected one artifact with visibility=private in list")
	}
}

// --- Pin/Unpin tests ---

func TestPinArtifact(t *testing.T) {
	as := testService(t)

	content := []byte("pinnable content")
	ref := storeTestArtifact(t, as, content, "text/plain")

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, &artifact.PinRequest{
		Action: "pin",
		Ref:    ref,
	}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.PinResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if resp.CachePolicy != "pin" {
		t.Errorf("cache_policy = %q, want 'pin'", resp.CachePolicy)
	}
	if resp.Hash == "" {
		t.Error("hash is empty")
	}

	// Verify metadata was updated.
	hash := getStoredHash(t, as, content)
	meta, err := as.metadataStore.Read(hash)
	if err != nil {
		t.Fatal(err)
	}
	if meta.CachePolicy != "pin" {
		t.Errorf("metadata cache_policy = %q, want 'pin'", meta.CachePolicy)
	}

	// Verify the artifact index was updated.
	entry, exists := as.artifactIndex.Get(hash)
	if !exists {
		t.Fatal("artifact not in index after pin")
	}
	if entry.CachePolicy != "pin" {
		t.Errorf("index cache_policy = %q, want 'pin'", entry.CachePolicy)
	}
}

func TestUnpinArtifact(t *testing.T) {
	as := testService(t)

	content := []byte("unpinnable content")
	ref := storeTestArtifactWithMeta(t, as, content, "text/plain", "", "", nil)

	// Pin first.
	hash := getStoredHash(t, as, content)
	meta, _ := as.metadataStore.Read(hash)
	meta.CachePolicy = "pin"
	as.metadataStore.Write(meta)
	as.artifactIndex.Put(*meta)

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, &artifact.UnpinRequest{
		Action: "unpin",
		Ref:    ref,
	}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.UnpinResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if resp.CachePolicy != "" {
		t.Errorf("cache_policy = %q, want empty", resp.CachePolicy)
	}

	// Verify metadata was updated.
	meta, err := as.metadataStore.Read(hash)
	if err != nil {
		t.Fatal(err)
	}
	if meta.CachePolicy != "" {
		t.Errorf("metadata cache_policy = %q, want empty", meta.CachePolicy)
	}
}

func TestUnpinNotPinned(t *testing.T) {
	as := testService(t)

	content := []byte("not pinned content")
	ref := storeTestArtifact(t, as, content, "text/plain")

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, &artifact.UnpinRequest{
		Action: "unpin",
		Ref:    ref,
	}); err != nil {
		t.Fatal(err)
	}

	var errResp artifact.ErrorResponse
	if err := artifact.ReadMessage(conn, &errResp); err != nil {
		t.Fatal(err)
	}
	wait()

	if errResp.Error == "" {
		t.Error("expected error unpinning non-pinned artifact")
	}
	if !strings.Contains(errResp.Error, "not pinned") {
		t.Errorf("error = %q, want contains 'not pinned'", errResp.Error)
	}
}

// --- GC tests ---

func TestGCRemoveExpiredArtifact(t *testing.T) {
	as := testService(t)

	// Store an artifact with a short TTL and an old stored_at.
	hash := storeTestArtifactWithTTL(t, as, []byte("ephemeral"), "text/plain", "1h",
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	// Verify it exists.
	if as.artifactIndex.Len() != 1 {
		t.Fatalf("index len = %d, want 1", as.artifactIndex.Len())
	}
	_ = hash

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, &artifact.GCRequest{
		Action: "gc",
	}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.GCResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if resp.ArtifactsRemoved != 1 {
		t.Errorf("artifacts_removed = %d, want 1", resp.ArtifactsRemoved)
	}
	if resp.DryRun {
		t.Error("dry_run should be false")
	}

	// Verify the artifact is gone from the index and ref index.
	if as.artifactIndex.Len() != 0 {
		t.Errorf("index len = %d after GC, want 0", as.artifactIndex.Len())
	}
	if as.refIndex.Len() != 0 {
		t.Errorf("ref index len = %d after GC, want 0", as.refIndex.Len())
	}
}

func TestGCPreservesPinnedArtifact(t *testing.T) {
	as := testService(t)

	// Store a pinned artifact with expired TTL.
	storeTestArtifactWithTTLAndPolicy(t, as, []byte("pinned but expired"), "text/plain", "1h",
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), "pin")

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, &artifact.GCRequest{
		Action: "gc",
	}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.GCResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if resp.ArtifactsRemoved != 0 {
		t.Errorf("artifacts_removed = %d, want 0 (pinned should be protected)", resp.ArtifactsRemoved)
	}
}

func TestGCPreservesTaggedArtifact(t *testing.T) {
	as := testService(t)

	// Store an artifact with expired TTL and tag it.
	hash := storeTestArtifactWithTTL(t, as, []byte("tagged but expired"), "text/plain", "1h",
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	as.tagStore.Set("protected/by-tag", hash, nil, true, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, &artifact.GCRequest{
		Action: "gc",
	}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.GCResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if resp.ArtifactsRemoved != 0 {
		t.Errorf("artifacts_removed = %d, want 0 (tagged should be protected)", resp.ArtifactsRemoved)
	}
}

func TestGCPreservesNoTTL(t *testing.T) {
	as := testService(t)

	// Store an artifact with no TTL — lives forever.
	storeTestArtifact(t, as, []byte("eternal"), "text/plain")

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, &artifact.GCRequest{
		Action: "gc",
	}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.GCResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if resp.ArtifactsRemoved != 0 {
		t.Errorf("artifacts_removed = %d, want 0 (no TTL = lives forever)", resp.ArtifactsRemoved)
	}
}

func TestGCDryRun(t *testing.T) {
	as := testService(t)

	storeTestArtifactWithTTL(t, as, []byte("dry-run target"), "text/plain", "1h",
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, &artifact.GCRequest{
		Action: "gc",
		DryRun: true,
	}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.GCResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if resp.ArtifactsRemoved != 1 {
		t.Errorf("artifacts_removed = %d, want 1 (dry run should report what would be removed)", resp.ArtifactsRemoved)
	}
	if !resp.DryRun {
		t.Error("dry_run should be true")
	}

	// The artifact should still exist.
	if as.artifactIndex.Len() != 1 {
		t.Errorf("index len = %d after dry-run GC, want 1", as.artifactIndex.Len())
	}
}

// --- Cache status tests ---

func TestCacheStatus(t *testing.T) {
	as := testService(t)

	storeTestArtifact(t, as, []byte("status-a"), "text/plain")
	storeTestArtifact(t, as, []byte("status-b"), "image/png")

	conn, wait := startHandler(t, as)

	if err := artifact.WriteMessage(conn, struct {
		Action string `json:"action"`
	}{Action: "cache-status"}); err != nil {
		t.Fatal(err)
	}

	var resp artifact.CacheStatusResponse
	if err := artifact.ReadMessage(conn, &resp); err != nil {
		t.Fatal(err)
	}
	wait()

	if resp.StoreArtifacts != 2 {
		t.Errorf("store_artifacts = %d, want 2", resp.StoreArtifacts)
	}
	if resp.StoreSizeBytes <= 0 {
		t.Errorf("store_size_bytes = %d, want > 0", resp.StoreSizeBytes)
	}
	// No cache configured in testService, so cache fields should be zero.
	if resp.CacheDeviceSize != 0 {
		t.Errorf("cache_device_size = %d, want 0 (no cache)", resp.CacheDeviceSize)
	}
}

// --- Auth tests ---

// testServiceWithAuth creates an ArtifactService configured with auth.
// Returns the service, the private key (for minting tokens), and the
// public key's auth config.
func testServiceWithAuth(t *testing.T) (*ArtifactService, ed25519.PrivateKey) {
	t.Helper()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}

	storeDir := t.TempDir()
	store, err := artifact.NewStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}

	metadataStore, err := artifact.NewMetadataStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	tagStore, err := artifact.NewTagStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	testClock := clock.Fake(testClockEpoch)
	authConfig := &service.AuthConfig{
		PublicKey: publicKey,
		Audience:  "artifact",
		Blacklist: servicetoken.NewBlacklist(),
		Clock:     testClock,
	}

	as := &ArtifactService{
		store:         store,
		metadataStore: metadataStore,
		refIndex:      artifact.NewRefIndex(),
		tagStore:      tagStore,
		artifactIndex: artifact.NewArtifactIndex(),
		authConfig:    authConfig,
		clock:         testClock,
		startedAt:     testClockEpoch,
		rooms:         make(map[ref.RoomID]*artifactRoomState),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	return as, privateKey
}

// mintArtifactToken creates a signed test token with specific grants
// and audience "artifact". Timestamps are relative to testClockEpoch.
func mintArtifactToken(t *testing.T, privateKey ed25519.PrivateKey, grants []servicetoken.Grant) []byte {
	t.Helper()
	token := &servicetoken.Token{
		Subject:   "agent/tester",
		Machine:   "machine/test",
		Audience:  "artifact",
		Grants:    grants,
		ID:        "test-token",
		IssuedAt:  testClockEpoch.Add(-5 * time.Minute).Unix(),
		ExpiresAt: testClockEpoch.Add(5 * time.Minute).Unix(),
	}
	tokenBytes, err := servicetoken.Mint(privateKey, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return tokenBytes
}

// mintExpiredArtifactToken creates a signed token that has already
// expired relative to the test clock.
func mintExpiredArtifactToken(t *testing.T, privateKey ed25519.PrivateKey) []byte {
	t.Helper()
	token := &servicetoken.Token{
		Subject:   "agent/tester",
		Machine:   "machine/test",
		Audience:  "artifact",
		Grants:    []servicetoken.Grant{{Actions: []string{"artifact/*"}}},
		ID:        "expired-token",
		IssuedAt:  testClockEpoch.Add(-2 * time.Hour).Unix(),
		ExpiresAt: testClockEpoch.Add(-1 * time.Hour).Unix(),
	}
	tokenBytes, err := servicetoken.Mint(privateKey, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return tokenBytes
}

func TestAuthMissingToken(t *testing.T) {
	as, _ := testServiceWithAuth(t)

	conn, wait := startHandler(t, as)

	// Send a request with no token field.
	if err := artifact.WriteMessage(conn, struct {
		Action string `json:"action"`
		Ref    string `json:"ref"`
	}{Action: "show", Ref: "art-000000000000"}); err != nil {
		t.Fatal(err)
	}

	var errResp artifact.ErrorResponse
	if err := artifact.ReadMessage(conn, &errResp); err != nil {
		t.Fatal(err)
	}
	wait()

	if errResp.Error == "" {
		t.Fatal("expected error for missing token")
	}
	if !strings.Contains(errResp.Error, "authentication required") {
		t.Errorf("error = %q, want contains 'authentication required'", errResp.Error)
	}
}

func TestAuthWrongGrant(t *testing.T) {
	as, privateKey := testServiceWithAuth(t)

	// Store an artifact so the ref exists (the grant check happens
	// before the handler logic).
	storeTestArtifact(t, as, []byte("auth test"), "text/plain")

	// Mint a token with only artifact/fetch grant, then try to store.
	tokenBytes := mintArtifactToken(t, privateKey, []servicetoken.Grant{
		{Actions: []string{"artifact/fetch"}},
	})

	conn, wait := startHandler(t, as)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len("new content")),
		Data:        []byte("new content"),
	}
	// Manually build a CBOR message that includes the token field.
	requestMap := map[string]any{
		"action":       header.Action,
		"content_type": header.ContentType,
		"size":         header.Size,
		"data":         header.Data,
		"token":        tokenBytes,
	}
	if err := artifact.WriteMessage(conn, requestMap); err != nil {
		t.Fatal(err)
	}

	var errResp artifact.ErrorResponse
	if err := artifact.ReadMessage(conn, &errResp); err != nil {
		t.Fatal(err)
	}
	wait()

	if errResp.Error == "" {
		t.Fatal("expected access denied error")
	}
	if !strings.Contains(errResp.Error, "access denied") {
		t.Errorf("error = %q, want contains 'access denied'", errResp.Error)
	}
}

func TestAuthValidToken(t *testing.T) {
	as, privateKey := testServiceWithAuth(t)

	// Store an artifact to query.
	storeTestArtifact(t, as, []byte("authed content"), "text/plain")

	// Mint a token with artifact/fetch grant.
	tokenBytes := mintArtifactToken(t, privateKey, []servicetoken.Grant{
		{Actions: []string{"artifact/fetch"}},
	})

	conn, wait := startHandler(t, as)

	// Send an exists request with valid token.
	requestMap := map[string]any{
		"action": "exists",
		"ref":    "art-000000000000",
		"token":  tokenBytes,
	}
	if err := artifact.WriteMessage(conn, requestMap); err != nil {
		t.Fatal(err)
	}

	// Should get a valid response (not an auth error).
	var response artifact.ExistsResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	// The artifact won't be found at that ref, but the point is we
	// got past auth and got a real response, not an auth error.
	if response.Exists {
		t.Error("expected exists = false for dummy ref")
	}
}

func TestAuthExpiredToken(t *testing.T) {
	as, privateKey := testServiceWithAuth(t)

	tokenBytes := mintExpiredArtifactToken(t, privateKey)

	conn, wait := startHandler(t, as)

	requestMap := map[string]any{
		"action": "exists",
		"ref":    "art-000000000000",
		"token":  tokenBytes,
	}
	if err := artifact.WriteMessage(conn, requestMap); err != nil {
		t.Fatal(err)
	}

	var errResp artifact.ErrorResponse
	if err := artifact.ReadMessage(conn, &errResp); err != nil {
		t.Fatal(err)
	}
	wait()

	if errResp.Error == "" {
		t.Fatal("expected error for expired token")
	}
	if !strings.Contains(errResp.Error, "expired") {
		t.Errorf("error = %q, want contains 'expired'", errResp.Error)
	}
}

func TestAuthStatusWithoutToken(t *testing.T) {
	as, _ := testServiceWithAuth(t)

	// Store some artifacts so we get meaningful status.
	storeTestArtifact(t, as, []byte("one"), "text/plain")

	conn, wait := startHandler(t, as)

	// Status should work without any token even when auth is configured.
	if err := artifact.WriteMessage(conn, struct {
		Action string `json:"action"`
	}{Action: "status"}); err != nil {
		t.Fatal(err)
	}

	var response artifact.StatusResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	if response.UptimeSeconds < 0 {
		t.Errorf("uptime = %f, want >= 0", response.UptimeSeconds)
	}
	if response.Artifacts != 1 {
		t.Errorf("artifacts = %d, want 1", response.Artifacts)
	}
}

func TestAuthAllActionsRequireToken(t *testing.T) {
	as, _ := testServiceWithAuth(t)

	// Store an artifact so ref resolution works for authorized requests.
	storeTestArtifact(t, as, []byte("auth-all-test"), "text/plain")

	// Every action except "status" should fail without a token.
	actions := []struct {
		action string
		grant  string
	}{
		{"store", "artifact/store"},
		{"fetch", "artifact/fetch"},
		{"exists", "artifact/fetch"},
		{"show", "artifact/fetch"},
		{"reconstruction", "artifact/fetch"},
		{"list", "artifact/list"},
		{"tag", "artifact/tag"},
		{"resolve", "artifact/fetch"},
		{"tags", "artifact/fetch"},
		{"delete-tag", "artifact/tag"},
		{"pin", "artifact/pin"},
		{"unpin", "artifact/pin"},
		{"gc", "artifact/gc"},
		{"cache-status", "artifact/list"},
	}

	for _, action := range actions {
		t.Run(action.action, func(t *testing.T) {
			conn, wait := startHandler(t, as)

			if err := artifact.WriteMessage(conn, struct {
				Action string `json:"action"`
			}{Action: action.action}); err != nil {
				t.Fatal(err)
			}

			var errResp artifact.ErrorResponse
			if err := artifact.ReadMessage(conn, &errResp); err != nil {
				t.Fatal(err)
			}
			wait()

			if errResp.Error == "" {
				t.Errorf("action %q: expected auth error, got success", action.action)
			}
			if !strings.Contains(errResp.Error, "authentication") {
				t.Errorf("action %q: error = %q, want contains 'authentication'", action.action, errResp.Error)
			}
		})
	}
}

func TestAuthWildcardGrant(t *testing.T) {
	as, privateKey := testServiceWithAuth(t)

	// Store an artifact to query.
	ref := storeTestArtifact(t, as, []byte("wildcard test"), "text/plain")

	// Mint a token with artifact/* wildcard grant.
	tokenBytes := mintArtifactToken(t, privateKey, []servicetoken.Grant{
		{Actions: []string{"artifact/*"}},
	})

	// Try a fetch action — should succeed with wildcard grant.
	conn, wait := startHandler(t, as)

	requestMap := map[string]any{
		"action": "exists",
		"ref":    ref,
		"token":  tokenBytes,
	}
	if err := artifact.WriteMessage(conn, requestMap); err != nil {
		t.Fatal(err)
	}

	var response artifact.ExistsResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	if !response.Exists {
		t.Error("expected exists = true with wildcard grant")
	}
}

func TestAuthRevokedToken(t *testing.T) {
	as, privateKey := testServiceWithAuth(t)

	tokenBytes := mintArtifactToken(t, privateKey, []servicetoken.Grant{
		{Actions: []string{"artifact/*"}},
	})

	// Revoke the token. The expiry time is the token's ExpiresAt,
	// used by the blacklist to prune stale entries.
	as.authConfig.Blacklist.Revoke("test-token", time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))

	conn, wait := startHandler(t, as)

	requestMap := map[string]any{
		"action": "exists",
		"ref":    "art-000000000000",
		"token":  tokenBytes,
	}
	if err := artifact.WriteMessage(conn, requestMap); err != nil {
		t.Fatal(err)
	}

	var errResp artifact.ErrorResponse
	if err := artifact.ReadMessage(conn, &errResp); err != nil {
		t.Fatal(err)
	}
	wait()

	if errResp.Error == "" {
		t.Fatal("expected error for revoked token")
	}
	if !strings.Contains(errResp.Error, "revoked") {
		t.Errorf("error = %q, want contains 'revoked'", errResp.Error)
	}
}

// --- Upstream fallthrough tests ---

// startUpstreamService creates a second ArtifactService and starts it
// listening on a Unix socket. Returns the service instance (for
// storing artifacts directly) and the socket path. The listener is
// shut down via t.Cleanup.
func startUpstreamService(t *testing.T, socketPath string) *ArtifactService {
	t.Helper()

	upstream := testService(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Remove any stale socket before listening.
	os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	t.Cleanup(func() {
		listener.Close()
		os.Remove(socketPath)
	})

	// Unblock Accept when the context is cancelled.
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go upstream.handleConnection(ctx, conn)
		}
	}()

	return upstream
}

func TestFetchFallthroughToUpstream(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	upstreamSocketPath := filepath.Join(socketDir, "upstream.sock")

	// Start the upstream artifact service.
	upstream := startUpstreamService(t, upstreamSocketPath)

	// Store an artifact in the upstream.
	content := []byte("content only in upstream")
	upstreamRef := storeTestArtifact(t, upstream, content, "text/plain")

	// Create the local service with the upstream configured.
	local := testService(t)
	local.upstreamSocket = upstreamSocketPath

	// Fetch from the local service. It should miss locally, fall
	// through to the upstream, fetch the content, re-store locally,
	// and return the content.
	conn, wait := startHandler(t, local)

	request := &artifact.FetchRequest{
		Action: "fetch",
		Ref:    upstreamRef,
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
		t.Errorf("fetched content = %q, want %q", string(response.Data), string(content))
	}
	if response.ContentType != "text/plain" {
		t.Errorf("content_type = %q, want 'text/plain'", response.ContentType)
	}

	// Verify the content was stored locally (second fetch should
	// not require upstream).
	local.upstreamSocket = "" // remove upstream
	conn2, wait2 := startHandler(t, local)

	if err := artifact.WriteMessage(conn2, request); err != nil {
		t.Fatal(err)
	}

	var response2 artifact.FetchResponse
	if err := artifact.ReadMessage(conn2, &response2); err != nil {
		t.Fatal(err)
	}
	wait2()

	if !bytes.Equal(response2.Data, content) {
		t.Error("second fetch (from local store) returned different content")
	}
}

func TestFetchFallthroughByFullHash(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	upstreamSocketPath := filepath.Join(socketDir, "upstream.sock")

	upstream := startUpstreamService(t, upstreamSocketPath)

	content := []byte("upstream fetch by hash")
	storeTestArtifact(t, upstream, content, "text/plain")

	// Get the full hash.
	hash := getStoredHash(t, upstream, content)
	fullHash := artifact.FormatHash(hash)

	local := testService(t)
	local.upstreamSocket = upstreamSocketPath

	conn, wait := startHandler(t, local)

	request := &artifact.FetchRequest{
		Action: "fetch",
		Ref:    fullHash,
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
		t.Error("fetched content via full hash does not match")
	}
}

func TestFetchFallthroughTagsNeverForwarded(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	upstreamSocketPath := filepath.Join(socketDir, "upstream.sock")

	upstream := startUpstreamService(t, upstreamSocketPath)

	content := []byte("upstream tagged content")
	storeTestArtifact(t, upstream, content, "text/plain")
	hash := getStoredHash(t, upstream, content)
	upstream.tagStore.Set("upstream/tag", hash, nil, true, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	local := testService(t)
	local.upstreamSocket = upstreamSocketPath

	conn, wait := startHandler(t, local)

	// Fetch by tag name — should NOT fall through to upstream because
	// tags are local to each service instance.
	request := &artifact.FetchRequest{
		Action: "fetch",
		Ref:    "upstream/tag",
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
		t.Error("expected error when fetching by tag name (tags should not be forwarded)")
	}
}

func TestExistsFallthroughToUpstream(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	upstreamSocketPath := filepath.Join(socketDir, "upstream.sock")

	upstream := startUpstreamService(t, upstreamSocketPath)

	content := []byte("exists in upstream only")
	upstreamRef := storeTestArtifact(t, upstream, content, "text/plain")

	local := testService(t)
	local.upstreamSocket = upstreamSocketPath

	conn, wait := startHandler(t, local)

	if err := artifact.WriteMessage(conn, refRequest{Action: "exists", Ref: upstreamRef}); err != nil {
		t.Fatal(err)
	}

	var response artifact.ExistsResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	if !response.Exists {
		t.Error("expected exists = true from upstream fallthrough")
	}
}

func TestExistsFallthroughMiss(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	upstreamSocketPath := filepath.Join(socketDir, "upstream.sock")

	startUpstreamService(t, upstreamSocketPath)

	local := testService(t)
	local.upstreamSocket = upstreamSocketPath

	conn, wait := startHandler(t, local)

	// Check a ref that doesn't exist anywhere.
	if err := artifact.WriteMessage(conn, refRequest{Action: "exists", Ref: "art-000000000000"}); err != nil {
		t.Fatal(err)
	}

	var response artifact.ExistsResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	if response.Exists {
		t.Error("expected exists = false when ref doesn't exist in either local or upstream")
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
		Visibility:     artifact.VisibilityPrivate,
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
	as.artifactIndex.Put(*meta)
	return result.Ref
}

// storeTestArtifactWithTTL stores an artifact with the given TTL and
// storedAt timestamp. Returns the file hash.
func storeTestArtifactWithTTL(t *testing.T, as *ArtifactService, content []byte, contentType, ttl string, storedAt time.Time) artifact.Hash {
	t.Helper()
	return storeTestArtifactWithTTLAndPolicy(t, as, content, contentType, ttl, storedAt, "")
}

// storeTestArtifactWithTTLAndPolicy stores an artifact with TTL,
// storedAt, and cache policy. Returns the file hash.
func storeTestArtifactWithTTLAndPolicy(t *testing.T, as *ArtifactService, content []byte, contentType, ttl string, storedAt time.Time, cachePolicy string) artifact.Hash {
	t.Helper()

	result, err := as.store.WriteContent(content, contentType)
	if err != nil {
		t.Fatalf("storing test artifact: %v", err)
	}

	meta := &artifact.ArtifactMetadata{
		FileHash:       result.FileHash,
		Ref:            result.Ref,
		ContentType:    contentType,
		Visibility:     artifact.VisibilityPrivate,
		Size:           result.Size,
		ChunkCount:     result.ChunkCount,
		ContainerCount: result.ContainerCount,
		Compression:    result.Compression.String(),
		TTL:            ttl,
		CachePolicy:    cachePolicy,
		StoredAt:       storedAt,
	}
	if err := as.metadataStore.Write(meta); err != nil {
		t.Fatalf("writing test metadata: %v", err)
	}

	as.refIndex.Add(result.FileHash)
	as.artifactIndex.Put(*meta)
	return result.FileHash
}

// getStoredHash computes the file hash for content, matching the
// hashing that Store.WriteContent uses.
func getStoredHash(t *testing.T, as *ArtifactService, content []byte) artifact.Hash {
	t.Helper()
	chunkHash := artifact.HashChunk(content)
	return artifact.HashFile(chunkHash)
}

// --- Push target tests ---

// startPushTargetService creates an ArtifactService listening on a Unix
// socket, suitable as a push target. Unlike startUpstreamService, this
// is named to clarify its role in push tests. Reuses the same pattern.
func startPushTargetService(t *testing.T, socketPath string) *ArtifactService {
	t.Helper()
	return startUpstreamService(t, socketPath)
}

func TestSetPushTargets(t *testing.T) {
	as, signingKey := testServiceWithAuth(t)

	// Sign a push targets config with the same key the service
	// verifies against.
	config := &servicetoken.PushTargetsConfig{
		Targets: map[string]servicetoken.PushTarget{
			"machine/gpu-server-1": {
				SocketPath: "/tmp/push-gpu.sock",
			},
			"machine/workstation": {
				SocketPath: "/tmp/push-workstation.sock",
				Token:      []byte("test-token"),
			},
		},
		IssuedAt: testClockEpoch.Unix(),
	}

	signedConfig, err := servicetoken.SignPushTargetsConfig(signingKey, config)
	if err != nil {
		t.Fatalf("SignPushTargetsConfig: %v", err)
	}

	conn, wait := startHandler(t, as)

	envelope := map[string]any{
		"action":        "set-push-targets",
		"signed_config": signedConfig,
	}
	if err := artifact.WriteMessage(conn, envelope); err != nil {
		t.Fatal(err)
	}

	var response map[string]bool
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	if !response["ok"] {
		t.Error("expected ok response")
	}

	// Verify push targets were stored.
	as.pushTargetsMu.RLock()
	defer as.pushTargetsMu.RUnlock()

	if len(as.pushTargets) != 2 {
		t.Fatalf("pushTargets has %d entries, want 2", len(as.pushTargets))
	}
	gpu := as.pushTargets["machine/gpu-server-1"]
	if gpu.SocketPath != "/tmp/push-gpu.sock" {
		t.Errorf("gpu SocketPath = %q, want %q", gpu.SocketPath, "/tmp/push-gpu.sock")
	}
	if gpu.Token != nil {
		t.Errorf("gpu Token = %v, want nil", gpu.Token)
	}
	workstation := as.pushTargets["machine/workstation"]
	if workstation.SocketPath != "/tmp/push-workstation.sock" {
		t.Errorf("workstation SocketPath = %q, want %q", workstation.SocketPath, "/tmp/push-workstation.sock")
	}
	if string(workstation.Token) != "test-token" {
		t.Errorf("workstation Token = %q, want %q", string(workstation.Token), "test-token")
	}
}

func TestStorePushToTarget(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	targetSocketPath := filepath.Join(socketDir, "target.sock")

	// Start a target service that will receive the pushed artifact.
	target := startPushTargetService(t, targetSocketPath)

	// Create the local service and configure push targets.
	local := testService(t)
	local.pushTargets = map[string]servicetoken.PushTarget{
		"machine/gpu-server-1": {
			SocketPath: targetSocketPath,
		},
	}

	// Store with push_targets set.
	content := []byte("pushable artifact content")
	conn, wait := startHandler(t, local)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
		Description: "pushed artifact",
		PushTargets: []string{"machine/gpu-server-1"},
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	var response artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	// Verify local store succeeded.
	if !strings.HasPrefix(response.Ref, "art-") {
		t.Errorf("ref = %q, want art- prefix", response.Ref)
	}

	// Verify push results.
	if len(response.PushResults) != 1 {
		t.Fatalf("push_results has %d entries, want 1", len(response.PushResults))
	}
	if !response.PushResults[0].OK {
		t.Errorf("push result not OK: %s", response.PushResults[0].Error)
	}
	if response.PushResults[0].Target != "machine/gpu-server-1" {
		t.Errorf("push target = %q, want %q", response.PushResults[0].Target, "machine/gpu-server-1")
	}

	// Verify the artifact appears on the target service.
	if target.refIndex.Len() != 1 {
		t.Errorf("target refIndex.Len() = %d, want 1", target.refIndex.Len())
	}

	// Fetch from the target and verify content matches.
	hash := getStoredHash(t, target, content)
	targetContent, err := target.store.ReadContent(hash)
	if err != nil {
		t.Fatalf("reading content from target: %v", err)
	}
	if !bytes.Equal(targetContent, content) {
		t.Error("target content does not match pushed content")
	}
}

func TestStorePushToMultipleTargets(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	target1SocketPath := filepath.Join(socketDir, "target1.sock")
	target2SocketPath := filepath.Join(socketDir, "target2.sock")

	target1 := startPushTargetService(t, target1SocketPath)
	target2 := startPushTargetService(t, target2SocketPath)

	local := testService(t)
	local.pushTargets = map[string]servicetoken.PushTarget{
		"machine/gpu-1": {SocketPath: target1SocketPath},
		"machine/gpu-2": {SocketPath: target2SocketPath},
	}

	content := []byte("multi-push artifact")
	conn, wait := startHandler(t, local)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
		PushTargets: []string{"machine/gpu-1", "machine/gpu-2"},
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	var response artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	// Both pushes should succeed.
	if len(response.PushResults) != 2 {
		t.Fatalf("push_results has %d entries, want 2", len(response.PushResults))
	}
	for _, result := range response.PushResults {
		if !result.OK {
			t.Errorf("push to %q failed: %s", result.Target, result.Error)
		}
	}

	// Verify both targets have the artifact.
	if target1.refIndex.Len() != 1 {
		t.Errorf("target1 refIndex.Len() = %d, want 1", target1.refIndex.Len())
	}
	if target2.refIndex.Len() != 1 {
		t.Errorf("target2 refIndex.Len() = %d, want 1", target2.refIndex.Len())
	}
}

func TestStorePushTargetUnreachable(t *testing.T) {
	local := testService(t)
	local.pushTargets = map[string]servicetoken.PushTarget{
		"machine/reachable": {SocketPath: "/nonexistent/socket.sock"},
	}

	content := []byte("push to unreachable target")
	conn, wait := startHandler(t, local)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
		PushTargets: []string{"machine/reachable"},
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	var response artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	// Local store should succeed.
	if !strings.HasPrefix(response.Ref, "art-") {
		t.Errorf("ref = %q, want art- prefix", response.Ref)
	}

	// Push should fail (socket doesn't exist).
	if len(response.PushResults) != 1 {
		t.Fatalf("push_results has %d entries, want 1", len(response.PushResults))
	}
	if response.PushResults[0].OK {
		t.Error("expected push to fail for unreachable socket")
	}
	if response.PushResults[0].Error == "" {
		t.Error("expected error message for unreachable push target")
	}
}

func TestStorePushTargetUnknown(t *testing.T) {
	local := testService(t)
	// No push targets configured at all.

	content := []byte("push to unknown target")
	conn, wait := startHandler(t, local)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
		PushTargets: []string{"machine/unknown"},
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	var response artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	// Local store succeeds.
	if !strings.HasPrefix(response.Ref, "art-") {
		t.Errorf("ref = %q, want art- prefix", response.Ref)
	}

	// Push reports error for unknown target.
	if len(response.PushResults) != 1 {
		t.Fatalf("push_results has %d entries, want 1", len(response.PushResults))
	}
	if response.PushResults[0].OK {
		t.Error("expected push to fail for unknown target")
	}
	if !strings.Contains(response.PushResults[0].Error, "unknown push target") {
		t.Errorf("error = %q, want contains 'unknown push target'", response.PushResults[0].Error)
	}
}

func TestStoreReplicatePolicy(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	upstreamSocketPath := filepath.Join(socketDir, "upstream.sock")

	// Start an upstream service.
	upstream := startUpstreamService(t, upstreamSocketPath)

	local := testService(t)
	local.upstreamSocket = upstreamSocketPath

	// Store with cache_policy = "replicate". This should push to upstream.
	content := []byte("replicated artifact content")
	conn, wait := startHandler(t, local)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
		CachePolicy: "replicate",
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	var response artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	// Local store should succeed.
	if !strings.HasPrefix(response.Ref, "art-") {
		t.Errorf("ref = %q, want art- prefix", response.Ref)
	}

	// Push to upstream should succeed.
	if len(response.PushResults) != 1 {
		t.Fatalf("push_results has %d entries, want 1", len(response.PushResults))
	}
	if !response.PushResults[0].OK {
		t.Errorf("replicate push failed: %s", response.PushResults[0].Error)
	}
	if response.PushResults[0].Target != "upstream" {
		t.Errorf("push target = %q, want %q", response.PushResults[0].Target, "upstream")
	}

	// Verify the artifact appears on the upstream.
	if upstream.refIndex.Len() != 1 {
		t.Errorf("upstream refIndex.Len() = %d, want 1", upstream.refIndex.Len())
	}
}

func TestStoreReplicateNoUpstream(t *testing.T) {
	local := testService(t)
	// No upstream configured.

	content := []byte("replicate without upstream")
	conn, wait := startHandler(t, local)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
		CachePolicy: "replicate",
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	var response artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	// Local store should succeed.
	if !strings.HasPrefix(response.Ref, "art-") {
		t.Errorf("ref = %q, want art- prefix", response.Ref)
	}

	// No push results since there's no upstream to replicate to.
	if len(response.PushResults) != 0 {
		t.Errorf("push_results has %d entries, want 0 (no upstream)", len(response.PushResults))
	}
}

func TestStorePushLargeArtifact(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	targetSocketPath := filepath.Join(socketDir, "target.sock")

	target := startPushTargetService(t, targetSocketPath)

	local := testService(t)
	local.pushTargets = map[string]servicetoken.PushTarget{
		"machine/gpu-server-1": {SocketPath: targetSocketPath},
	}

	// Content larger than SmallArtifactThreshold (256 KiB) to
	// exercise the streaming push path via io.Pipe.
	content := make([]byte, 300*1024)
	rand.Read(content)

	conn, wait := startHandler(t, local)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "application/octet-stream",
		Size:        int64(len(content)),
		PushTargets: []string{"machine/gpu-server-1"},
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	// Stream the content (large artifact, sized transfer).
	if _, err := conn.Write(content); err != nil {
		t.Fatal(err)
	}

	var response artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	// Verify local store.
	if response.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", response.Size, len(content))
	}

	// Verify push succeeded.
	if len(response.PushResults) != 1 {
		t.Fatalf("push_results has %d entries, want 1", len(response.PushResults))
	}
	if !response.PushResults[0].OK {
		t.Errorf("push failed: %s", response.PushResults[0].Error)
	}

	// Verify the artifact appears on the target. For large artifacts
	// (which go through CDC), we verify via the ref index and fetch
	// rather than computing the hash directly.
	if target.refIndex.Len() != 1 {
		t.Fatalf("target refIndex.Len() = %d, want 1", target.refIndex.Len())
	}

	// Fetch from the target via its handler to verify content matches.
	targetConn, targetWait := startHandler(t, target)
	fetchRequest := &artifact.FetchRequest{
		Action: "fetch",
		Ref:    response.Ref,
	}
	if err := artifact.WriteMessage(targetConn, fetchRequest); err != nil {
		t.Fatal(err)
	}

	var fetchResponse artifact.FetchResponse
	if err := artifact.ReadMessage(targetConn, &fetchResponse); err != nil {
		t.Fatal(err)
	}

	// Large artifact: read the binary stream following the header.
	if fetchResponse.Data != nil {
		// If the server embedded the data (shouldn't for 300 KiB but
		// handle it gracefully).
		if !bytes.Equal(fetchResponse.Data, content) {
			t.Error("target embedded content does not match")
		}
	} else {
		fetched := make([]byte, fetchResponse.Size)
		if _, err := io.ReadFull(targetConn, fetched); err != nil {
			t.Fatalf("reading stream from target: %v", err)
		}
		if !bytes.Equal(fetched, content) {
			t.Error("target streamed content does not match")
		}
	}
	targetWait()
}

func TestStoreNoPushTargets(t *testing.T) {
	local := testService(t)

	// Store without any push targets specified. Verify no push
	// results in the response.
	content := []byte("no push targets")
	conn, wait := startHandler(t, local)

	header := &artifact.StoreHeader{
		Action:      "store",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Data:        content,
	}
	if err := artifact.WriteMessage(conn, header); err != nil {
		t.Fatal(err)
	}

	var response artifact.StoreResponse
	if err := artifact.ReadMessage(conn, &response); err != nil {
		t.Fatal(err)
	}
	wait()

	if !strings.HasPrefix(response.Ref, "art-") {
		t.Errorf("ref = %q, want art- prefix", response.Ref)
	}
	if len(response.PushResults) != 0 {
		t.Errorf("push_results has %d entries, want 0", len(response.PushResults))
	}
}
