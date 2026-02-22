// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/clock"
	gofuse "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// DefaultPrefetchContainers is the number of containers to prefetch
// in reconstruction order on first access to an artifact.
const DefaultPrefetchContainers = 4

// Options configures the FUSE mount.
type Options struct {
	// Mountpoint is the directory where the filesystem is mounted.
	Mountpoint string

	// Store provides access to artifact containers and
	// reconstruction records.
	Store *artifactstore.Store

	// Cache is the optional container-level ring cache. If nil,
	// all reads go directly to disk.
	Cache *artifactstore.Cache

	// TagStore provides tag name resolution.
	TagStore *artifactstore.TagStore

	// PrefetchContainers is the number of containers to prefetch
	// in reconstruction order on first access. Zero uses
	// DefaultPrefetchContainers.
	PrefetchContainers int

	// Clock provides time for tag update timestamps. If nil,
	// defaults to clock.Real().
	Clock clock.Clock

	// WriteMu serializes write operations. The artifact service
	// passes its own write mutex so that FUSE writes and socket
	// API writes are serialized (the Store is not safe for
	// concurrent writes). If nil, an internal mutex is created.
	// Standalone mounts (no concurrent socket API) can leave this
	// nil; production mounts sharing a Store must provide one.
	WriteMu *sync.Mutex

	// AllowOther permits other users (including root) to access
	// the mount. Requires user_allow_other in /etc/fuse.conf.
	// Set this for production use where sandboxed agents running
	// as different UIDs need to read artifacts.
	AllowOther bool

	// Logger receives diagnostic messages. If nil, a no-op logger
	// is used.
	Logger *slog.Logger
}

// Mount mounts the artifact FUSE filesystem at the configured
// mountpoint. The caller must call Unmount on the returned Server
// when done. The mountpoint directory is created if it does not exist.
func Mount(options Options) (*fuse.Server, error) {
	if options.Mountpoint == "" {
		return nil, fmt.Errorf("mountpoint is required")
	}
	if options.Store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if options.TagStore == nil {
		return nil, fmt.Errorf("tag store is required")
	}

	if options.PrefetchContainers == 0 {
		options.PrefetchContainers = DefaultPrefetchContainers
	}
	if options.Clock == nil {
		options.Clock = clock.Real()
	}
	if options.WriteMu == nil {
		options.WriteMu = &sync.Mutex{}
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelError,
		}))
	}

	// Ensure the mountpoint exists.
	if err := os.MkdirAll(options.Mountpoint, 0o755); err != nil {
		return nil, fmt.Errorf("creating mountpoint %s: %w", options.Mountpoint, err)
	}

	root := &rootNode{options: &options}

	entryTimeout := 1 * time.Second
	attrTimeout := 1 * time.Second
	negativeTimeout := 100 * time.Millisecond

	server, err := gofuse.Mount(options.Mountpoint, root, &gofuse.Options{
		EntryTimeout:    &entryTimeout,
		AttrTimeout:     &attrTimeout,
		NegativeTimeout: &negativeTimeout,
		MountOptions: fuse.MountOptions{
			FsName:     "bureau-artifact",
			Name:       "bureau",
			AllowOther: options.AllowOther,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("mounting FUSE filesystem at %s: %w", options.Mountpoint, err)
	}

	options.Logger.Info("artifact FUSE filesystem mounted", "mountpoint", options.Mountpoint)
	return server, nil
}

// rootNode is the filesystem root. It has two children: "tag" and "cas".
type rootNode struct {
	gofuse.Inode
	options *Options
}

var _ gofuse.InodeEmbedder = (*rootNode)(nil)
var _ gofuse.NodeOnAdder = (*rootNode)(nil)

func (r *rootNode) OnAdd(ctx context.Context) {
	tagDir := r.NewPersistentInode(ctx, &tagRootNode{options: r.options}, gofuse.StableAttr{Mode: syscall.S_IFDIR})
	r.AddChild("tag", tagDir, true)

	casDir := r.NewPersistentInode(ctx, &casNode{options: r.options}, gofuse.StableAttr{Mode: syscall.S_IFDIR})
	r.AddChild("cas", casDir, true)
}

// tagRootNode is the "tag/" directory. It dynamically lists the
// top-level components of all tag names.
type tagRootNode struct {
	gofuse.Inode
	options *Options
	prefix  string // empty for root, "foo/" for intermediate directories
}

var _ gofuse.InodeEmbedder = (*tagRootNode)(nil)
var _ gofuse.NodeLookuper = (*tagRootNode)(nil)
var _ gofuse.NodeReaddirer = (*tagRootNode)(nil)
var _ gofuse.NodeCreater = (*tagRootNode)(nil)

func (t *tagRootNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*gofuse.Inode, syscall.Errno) {
	fullName := t.prefix + name

	// Check if this exact name is a tag (leaf file).
	tagRecord, isTag := t.options.TagStore.Get(fullName)

	// Check if this name is a prefix of other tags (directory).
	childPrefix := fullName + "/"
	children := t.options.TagStore.List(childPrefix)
	isDir := len(children) > 0

	if isDir {
		// Directory takes precedence over a same-named tag.
		child := t.NewPersistentInode(ctx, &tagRootNode{
			options: t.options,
			prefix:  childPrefix,
		}, gofuse.StableAttr{Mode: syscall.S_IFDIR})
		out.Mode = syscall.S_IFDIR | 0o555
		return child, 0
	}

	if isTag {
		return t.makeArtifactInode(ctx, tagRecord.Target, fullName, out)
	}

	return nil, syscall.ENOENT
}

func (t *tagRootNode) Readdir(ctx context.Context) (gofuse.DirStream, syscall.Errno) {
	tags := t.options.TagStore.List(t.prefix)

	// Extract unique immediate children under our prefix.
	seen := make(map[string]bool)
	var entries []fuse.DirEntry

	for _, tag := range tags {
		// Strip our prefix to get the relative path.
		relative := strings.TrimPrefix(tag.Name, t.prefix)
		if relative == "" {
			continue
		}

		// Extract the first path component.
		component := relative
		isDirectory := false
		if slashIndex := strings.IndexByte(relative, '/'); slashIndex >= 0 {
			component = relative[:slashIndex]
			isDirectory = true
		}

		if seen[component] {
			continue
		}
		seen[component] = true

		mode := uint32(syscall.S_IFREG)
		if isDirectory {
			mode = syscall.S_IFDIR
		}

		entries = append(entries, fuse.DirEntry{
			Name: component,
			Mode: mode,
		})
	}

	return &sliceDirStream{entries: entries}, 0
}

// Create handles file creation under the tag directory. When a program
// writes to a tag path (cp, shell redirection, curl), the kernel calls
// Create on the parent directory. The write handle captures the current
// tag target (if any) for compare-and-swap on close.
func (t *tagRootNode) Create(ctx context.Context, name string, _ uint32, _ uint32, out *fuse.EntryOut) (*gofuse.Inode, gofuse.FileHandle, uint32, syscall.Errno) {
	fullName := t.prefix + name

	// Reject creation of files that conflict with existing directory
	// prefixes. Writing to "foo" when "foo/bar" exists would be
	// ambiguous in the tag namespace.
	childPrefix := fullName + "/"
	children := t.options.TagStore.List(childPrefix)
	if len(children) > 0 {
		return nil, nil, 0, syscall.EISDIR
	}

	// Capture current tag target for compare-and-swap on close.
	var previousTarget *artifactstore.Hash
	if existing, exists := t.options.TagStore.Get(fullName); exists {
		target := existing.Target
		previousTarget = &target
	}

	handle := &writeHandle{
		options:        t.options,
		tagName:        fullName,
		previousTarget: previousTarget,
	}

	node := &writeInProgressNode{handle: handle}
	child := t.NewPersistentInode(ctx, node, gofuse.StableAttr{Mode: syscall.S_IFREG})
	out.Mode = syscall.S_IFREG | 0o644

	return child, handle, 0, 0
}

func (t *tagRootNode) makeArtifactInode(ctx context.Context, fileHash artifactstore.Hash, tagName string, out *fuse.EntryOut) (*gofuse.Inode, syscall.Errno) {
	record, err := t.options.Store.Stat(fileHash)
	if err != nil {
		t.options.Logger.Error("stat failed for artifact",
			"hash", artifactstore.FormatHash(fileHash),
			"error", err,
		)
		return nil, syscall.EIO
	}

	node := &artifactFileNode{
		options:  t.options,
		fileHash: fileHash,
		record:   record,
		tagName:  tagName,
	}

	child := t.NewPersistentInode(ctx, node, gofuse.StableAttr{Mode: syscall.S_IFREG})
	out.Mode = syscall.S_IFREG | 0o444
	out.Size = uint64(record.Size)
	return child, 0
}

// casNode is the "cas/" directory. It supports lookup by full hex
// hash but does not support directory listing.
type casNode struct {
	gofuse.Inode
	options *Options
}

var _ gofuse.InodeEmbedder = (*casNode)(nil)
var _ gofuse.NodeLookuper = (*casNode)(nil)
var _ gofuse.NodeCreater = (*casNode)(nil)

func (c *casNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*gofuse.Inode, syscall.Errno) {
	fileHash, err := artifactstore.ParseHash(name)
	if err != nil {
		return nil, syscall.ENOENT
	}

	record, err := c.options.Store.Stat(fileHash)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, syscall.ENOENT
		}
		c.options.Logger.Error("stat failed for CAS lookup",
			"hash", name,
			"error", err,
		)
		return nil, syscall.EIO
	}

	node := &artifactFileNode{
		options:  c.options,
		fileHash: fileHash,
		record:   record,
	}

	child := c.NewPersistentInode(ctx, node, gofuse.StableAttr{Mode: syscall.S_IFREG})
	out.Mode = syscall.S_IFREG | 0o444
	out.Size = uint64(record.Size)
	return child, 0
}

// Create handles file creation under the CAS directory. The filename
// must be a valid hex hash. After the write completes, the content
// hash is verified against the filename — a mismatch returns EIO.
// The artifact is still stored (data is never lost) but the caller
// is notified of the mismatch.
func (c *casNode) Create(ctx context.Context, name string, _ uint32, _ uint32, out *fuse.EntryOut) (*gofuse.Inode, gofuse.FileHandle, uint32, syscall.Errno) {
	expectedHash, err := artifactstore.ParseHash(name)
	if err != nil {
		return nil, nil, 0, syscall.EINVAL
	}

	handle := &writeHandle{
		options:      c.options,
		expectedHash: &expectedHash,
	}

	node := &writeInProgressNode{handle: handle}
	child := c.NewPersistentInode(ctx, node, gofuse.StableAttr{Mode: syscall.S_IFREG})
	out.Mode = syscall.S_IFREG | 0o644

	return child, handle, 0, 0
}

// artifactFileNode represents a single artifact as a regular file.
// It loads the chunk table lazily on first Open and serves reads by
// resolving byte offsets to container chunks. For tagged artifacts,
// tagName is set so that Open-for-write can create a writeHandle
// that updates the tag on close.
type artifactFileNode struct {
	gofuse.Inode
	options  *Options
	fileHash artifactstore.Hash
	record   *artifactstore.ReconstructionRecord

	// tagName is the tag name if this node was created via tag
	// lookup. Empty for CAS-created nodes. Used by Open to
	// determine whether write mode is supported.
	tagName string

	// mu protects chunks (lazy initialization).
	mu     sync.Mutex
	chunks []chunkEntry

	// prefetchOnce ensures container prefetching fires at most
	// once per file node.
	prefetchOnce sync.Once
}

var _ gofuse.InodeEmbedder = (*artifactFileNode)(nil)
var _ gofuse.NodeGetattrer = (*artifactFileNode)(nil)
var _ gofuse.NodeSetattrer = (*artifactFileNode)(nil)
var _ gofuse.NodeOpener = (*artifactFileNode)(nil)
var _ gofuse.NodeReader = (*artifactFileNode)(nil)

func (a *artifactFileNode) Getattr(ctx context.Context, f gofuse.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFREG | 0o444
	out.Size = uint64(a.record.Size)
	out.Blocks = (out.Size + 511) / 512
	out.Blksize = 65536 // 64 KiB, matching target chunk size
	return 0
}

// Setattr handles truncation requests. When the kernel opens an
// existing file with O_TRUNC, it sends SETATTR(size=0) before or
// after Open. For tagged artifacts opened for writing, truncation
// is a no-op (the write handle starts empty). For read-only access,
// truncation is rejected.
func (a *artifactFileNode) Setattr(_ context.Context, _ gofuse.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if _, ok := in.GetSize(); ok {
		// Truncation is only meaningful for tagged artifacts that
		// support writes. Truncating a CAS entry makes no sense.
		if a.tagName == "" {
			return syscall.EROFS
		}
		// For tagged artifacts, truncation during open-for-write is
		// a no-op: the write handle maintains its own buffer.
	}
	out.Mode = syscall.S_IFREG | 0o444
	out.Size = uint64(a.record.Size)
	out.Blocks = (out.Size + 511) / 512
	out.Blksize = 65536
	return 0
}

func (a *artifactFileNode) Open(ctx context.Context, flags uint32) (gofuse.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0 {
		// Write mode: only supported for tagged artifacts. CAS
		// entries are immutable — overwriting them makes no sense.
		if a.tagName == "" {
			return nil, 0, syscall.EROFS
		}

		// Capture the current target for compare-and-swap on close.
		previousTarget := a.fileHash
		handle := &writeHandle{
			options:        a.options,
			tagName:        a.tagName,
			previousTarget: &previousTarget,
		}
		return handle, 0, 0
	}

	// Read mode: build the chunk table if not already built.
	if err := a.ensureChunkTable(); err != nil {
		a.options.Logger.Error("failed to build chunk table",
			"hash", artifactstore.FormatHash(a.fileHash),
			"error", err,
		)
		return nil, 0, syscall.EIO
	}

	// Enable kernel page cache. Artifact content is immutable, so
	// the cache is always valid.
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (a *artifactFileNode) Read(ctx context.Context, f gofuse.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if err := a.ensureChunkTable(); err != nil {
		return nil, syscall.EIO
	}

	provider := &storeProvider{store: a.options.Store, cache: a.options.Cache}

	// Trigger prefetch on first access.
	a.triggerPrefetch(provider, off)

	bytesRead, err := readAt(a.chunks, provider, dest, off, a.record.Size)
	if err != nil {
		a.options.Logger.Error("read failed",
			"hash", artifactstore.FormatHash(a.fileHash),
			"offset", off,
			"error", err,
		)
		return nil, syscall.EIO
	}

	return fuse.ReadResultData(dest[:bytesRead]), 0
}

func (a *artifactFileNode) ensureChunkTable() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.chunks != nil {
		return nil
	}

	provider := &storeProvider{store: a.options.Store, cache: a.options.Cache}
	chunks, err := buildChunkTable(a.record, provider)
	if err != nil {
		return fmt.Errorf("building chunk table for %s: %w",
			artifactstore.FormatHash(a.fileHash), err)
	}

	a.chunks = chunks
	return nil
}

// triggerPrefetch kicks off background loading of the next N
// containers in reconstruction order starting from the container
// that contains the given offset. This amortizes the cost of
// sequential reads across container boundaries. Only fires once
// per file node — subsequent reads benefit from the container
// cache and kernel page cache.
func (a *artifactFileNode) triggerPrefetch(provider containerProvider, offset int64) {
	a.prefetchOnce.Do(func() {
		go a.prefetchContainers(provider, offset)
	})
}

func (a *artifactFileNode) prefetchContainers(provider containerProvider, offset int64) {
	if a.options.PrefetchContainers <= 0 {
		return
	}

	chunkIndex := findChunk(a.chunks, offset)
	if chunkIndex < 0 {
		return
	}

	// Find which containers to prefetch: starting from the current
	// chunk's container, collect the next N distinct containers in
	// reconstruction order.
	seen := make(map[artifactstore.Hash]bool)
	var toPrefetch []artifactstore.Hash

	for i := chunkIndex; i < len(a.chunks) && len(toPrefetch) < a.options.PrefetchContainers; i++ {
		hash := a.chunks[i].container
		if seen[hash] {
			continue
		}
		seen[hash] = true
		toPrefetch = append(toPrefetch, hash)
	}

	for _, hash := range toPrefetch {
		// Loading the container through the provider populates
		// the cache as a side effect.
		if _, err := provider.containerBytes(hash); err != nil {
			a.options.Logger.Warn("prefetch failed",
				"container", artifactstore.FormatHash(hash),
				"error", err,
			)
			return
		}
	}
}

// sliceDirStream implements fs.DirStream from a slice of entries.
type sliceDirStream struct {
	entries []fuse.DirEntry
	index   int
}

func (s *sliceDirStream) HasNext() bool {
	return s.index < len(s.entries)
}

func (s *sliceDirStream) Next() (fuse.DirEntry, syscall.Errno) {
	if s.index >= len(s.entries) {
		return fuse.DirEntry{}, syscall.EINVAL
	}
	entry := s.entries[s.index]
	s.index++
	return entry, 0
}

func (s *sliceDirStream) Close() {}
