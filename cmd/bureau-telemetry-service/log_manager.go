// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/log"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
)

// Default configuration for the log manager.
const (
	// defaultChunkSizeThreshold is the buffer size at which a flush
	// is triggered immediately. 1 MB balances artifact granularity
	// (not too many tiny artifacts) against latency (don't wait
	// forever for a slow-producing process).
	defaultChunkSizeThreshold = 1024 * 1024

	// defaultFlushInterval is how often the background ticker checks
	// for stale buffers. Any session with unflushed data older than
	// this interval gets flushed, even if the data hasn't reached
	// the size threshold.
	defaultFlushInterval = 10 * time.Second

	// defaultStaleTimeout is how long a session can be idle before
	// the reaper transitions it to "complete". This is the fallback
	// for crashes or disconnections where the daemon never sends a
	// complete-log action.
	defaultStaleTimeout = 5 * time.Minute

	// maxPendingChunks is the maximum number of artifact refs that
	// have been stored but not yet persisted in a state event. If
	// this limit is reached, the session stops storing new artifacts
	// until the backlog drains (i.e. a state event write succeeds).
	maxPendingChunks = 100

	// artifactContentType is the content type used when storing
	// output delta chunks in the artifact service.
	artifactContentType = "application/vnd.bureau.terminal-output"

	// defaultMaxBytesPerSession is the maximum stored output per
	// session before the eviction loop starts removing old chunks
	// from the front. 1 GB is generous for most services while
	// preventing unbounded state event growth. Configurable at
	// startup via the --max-bytes-per-session flag.
	defaultMaxBytesPerSession int64 = 1 << 30
)

// artifactStorer is the subset of artifactstore.Client needed by the
// log manager for storing output chunks. Defined as an interface for
// testability — the unit tests inject a mock that records calls and
// returns canned responses.
type artifactStorer interface {
	Store(ctx context.Context, header *artifactstore.StoreHeader, content io.Reader) (*artifactstore.StoreResponse, error)
}

// logEntityWriter is the subset of messaging.Session needed for
// writing log state events and resolving room aliases. Defined as an
// interface for testability.
type logEntityWriter interface {
	SendStateEvent(ctx context.Context, roomID ref.RoomID, eventType ref.EventType, stateKey string, content any) (ref.EventID, error)
	ResolveAlias(ctx context.Context, alias ref.RoomAlias) (ref.RoomID, error)
}

// sessionKey uniquely identifies an output stream. The same source
// may have multiple sessions if its sandbox is recreated.
type sessionKey struct {
	source    string // Source.Localpart()
	sessionID string
}

// logManager manages per-session output buffers, flushes them to the
// artifact store, and tracks the resulting chunks in m.bureau.log
// state events. Each session (source + sessionID pair) has its own
// buffer with independent locking to avoid cross-session contention.
//
// The lifecycle:
//   - HandleDeltas routes incoming output deltas to session buffers.
//   - When a buffer reaches the size threshold, it flushes immediately.
//   - A background ticker periodically flushes small buffers for low-latency visibility.
//   - A background reaper completes stale sessions (no deltas for >5 minutes).
//   - The complete-log action explicitly completes a session.
//   - Close drains all buffers during graceful shutdown.
type logManager struct {
	sessionsMu sync.RWMutex
	sessions   map[sessionKey]*sessionBuffer

	artifact artifactStorer
	writer   logEntityWriter
	clock    clock.Clock
	logger   *slog.Logger

	chunkSizeThreshold int
	flushInterval      time.Duration
	staleTimeout       time.Duration
	maxBytesPerSession int64

	// Operational counters for the status endpoint.
	flushCount           atomic.Uint64
	flushErrors          atomic.Uint64
	storeCount           atomic.Uint64
	storeErrors          atomic.Uint64
	stateEventWrites     atomic.Uint64
	stateEventErrors     atomic.Uint64
	roomResolutionErrors atomic.Uint64
	evictionCount        atomic.Uint64

	// lastError stores the most recent error message for
	// diagnostic surfacing via the status endpoint. Protected
	// by lastErrorMu to allow concurrent read from status and
	// write from flush goroutines.
	lastErrorMu sync.Mutex
	lastError   string
}

// newLogManager creates a log manager. If artifact is nil, the
// manager counts deltas and fans out to tail subscribers but skips
// all persistence (artifact service not available).
func newLogManager(artifact artifactStorer, writer logEntityWriter, clk clock.Clock, logger *slog.Logger) *logManager {
	return &logManager{
		sessions:           make(map[sessionKey]*sessionBuffer),
		artifact:           artifact,
		writer:             writer,
		clock:              clk,
		logger:             logger,
		chunkSizeThreshold: defaultChunkSizeThreshold,
		flushInterval:      defaultFlushInterval,
		staleTimeout:       defaultStaleTimeout,
		maxBytesPerSession: defaultMaxBytesPerSession,
	}
}

func (m *logManager) recordError(err error) {
	m.lastErrorMu.Lock()
	m.lastError = err.Error()
	m.lastErrorMu.Unlock()
}

// Stats returns a snapshot of the log manager's operational counters.
func (m *logManager) Stats() telemetry.LogManagerStats {
	m.sessionsMu.RLock()
	sessionCount := len(m.sessions)
	m.sessionsMu.RUnlock()

	m.lastErrorMu.Lock()
	lastErr := m.lastError
	m.lastErrorMu.Unlock()

	return telemetry.LogManagerStats{
		FlushCount:           m.flushCount.Load(),
		FlushErrors:          m.flushErrors.Load(),
		StoreCount:           m.storeCount.Load(),
		StoreErrors:          m.storeErrors.Load(),
		StateEventWrites:     m.stateEventWrites.Load(),
		StateEventErrors:     m.stateEventErrors.Load(),
		RoomResolutionErrors: m.roomResolutionErrors.Load(),
		EvictionCount:        m.evictionCount.Load(),
		ActiveSessions:       sessionCount,
		LastError:            lastErr,
	}
}

// sessionBuffer holds the in-flight data for one (source, sessionID)
// pair. The mutex is per-session to avoid cross-session contention
// during concurrent ingestion from multiple relays.
type sessionBuffer struct {
	mu sync.Mutex

	machine   ref.Machine
	source    ref.Entity
	sessionID string

	// Room resolution (resolved on first delta, cached).
	configRoomID ref.RoomID
	roomResolved bool

	// Current unflushed data. Pre-allocated to chunkSizeThreshold
	// capacity to avoid repeated growth allocations during append.
	data           []byte
	firstSequence  uint64
	firstTimestamp int64
	lastSequence   uint64

	// Chunks stored in artifacts but not yet persisted in the state
	// event. Drained on each successful state event write.
	pendingChunks []log.LogChunk

	// Entity state: the last successfully persisted state. Survives
	// across flushes and is the source of truth for building the
	// next state event.
	totalBytes   int64
	chunks       []log.LogChunk
	entityExists bool

	// pendingOverflow is set when pendingChunks reaches
	// maxPendingChunks. When true, new artifact stores are skipped
	// until the backlog drains via a successful state event write.
	pendingOverflow bool

	// Lifecycle.
	lastActivity time.Time
	status       log.LogStatus
}

// HandleDeltas routes output deltas from an ingested batch to the
// appropriate session buffers. For each delta, it finds or creates
// a session buffer, appends the data, and triggers an immediate
// flush if the buffer exceeds the size threshold.
//
// Deltas use their per-record Source and SessionID fields (not the
// batch-level Machine) as the session key, because a single batch
// may contain deltas from multiple sources.
func (m *logManager) HandleDeltas(ctx context.Context, deltas []telemetry.OutputDelta) {
	if m.artifact == nil {
		return
	}

	for i := range deltas {
		delta := &deltas[i]
		if len(delta.Data) == 0 {
			continue
		}

		key := sessionKey{
			source:    delta.Source.Localpart(),
			sessionID: delta.SessionID,
		}

		session := m.findOrCreateSession(key, delta)

		// Append data under the session lock.
		var flushData []byte
		var flushFirstSequence uint64
		var flushFirstTimestamp int64

		session.mu.Lock()
		if session.status == log.LogStatusComplete {
			// Session already completed (by reaper or complete-log).
			// Drop the delta — the process may have sent data after
			// the session was closed.
			session.mu.Unlock()
			m.logger.Warn("delta for completed session",
				"source", delta.Source,
				"session_id", delta.SessionID,
				"sequence", delta.Sequence,
			)
			continue
		}

		session.data = append(session.data, delta.Data...)
		if session.firstTimestamp == 0 {
			session.firstSequence = delta.Sequence
			session.firstTimestamp = delta.Timestamp
		}
		session.lastSequence = delta.Sequence
		session.lastActivity = m.clock.Now()

		// Check if we've crossed the size threshold.
		if len(session.data) >= m.chunkSizeThreshold {
			// Swap out the buffer under lock, pre-allocate a fresh one.
			flushData = session.data
			flushFirstSequence = session.firstSequence
			flushFirstTimestamp = session.firstTimestamp
			session.data = make([]byte, 0, m.chunkSizeThreshold)
			session.firstSequence = 0
			session.firstTimestamp = 0
		}
		session.mu.Unlock()

		// Flush outside the lock if threshold was crossed.
		if flushData != nil {
			m.flushSession(ctx, session, flushData, flushFirstSequence, flushFirstTimestamp)
		}
	}
}

// findOrCreateSession returns the session buffer for the given key,
// creating one if it doesn't exist. Uses RLock for the fast path
// (session exists) and upgrades to Lock for creation.
func (m *logManager) findOrCreateSession(key sessionKey, delta *telemetry.OutputDelta) *sessionBuffer {
	m.sessionsMu.RLock()
	session, exists := m.sessions[key]
	m.sessionsMu.RUnlock()
	if exists {
		return session
	}

	m.sessionsMu.Lock()
	// Double-check under write lock.
	session, exists = m.sessions[key]
	if exists {
		m.sessionsMu.Unlock()
		return session
	}

	session = &sessionBuffer{
		machine:   delta.Machine,
		source:    delta.Source,
		sessionID: delta.SessionID,
		data:      make([]byte, 0, m.chunkSizeThreshold),
		status:    log.LogStatusActive,
	}
	m.sessions[key] = session
	m.sessionsMu.Unlock()

	m.logger.Info("new output session",
		"source", delta.Source,
		"session_id", delta.SessionID,
		"machine", delta.Machine,
	)

	return session
}

// flushSession stores buffered data as a CAS artifact and updates
// the m.bureau.log state event. Called both from HandleDeltas (size
// threshold) and from the background flush ticker (time threshold).
//
// The caller provides the data snapshot — it must not hold the
// session lock.
func (m *logManager) flushSession(ctx context.Context, session *sessionBuffer, data []byte, firstSequence uint64, firstTimestamp int64) {
	if len(data) == 0 {
		return
	}

	// Resolve room on first flush.
	session.mu.Lock()
	if !session.roomResolved {
		session.mu.Unlock()
		m.resolveRoom(ctx, session)
		session.mu.Lock()
	}
	roomID := session.configRoomID
	roomResolved := session.roomResolved
	session.mu.Unlock()

	if !roomResolved || roomID.IsZero() {
		m.logger.Debug("skipping flush: room not resolved",
			"source", session.source,
			"session_id", session.sessionID,
		)
		m.flushErrors.Add(1)
		m.roomResolutionErrors.Add(1)
		return
	}

	// Check pending overflow before storing.
	session.mu.Lock()
	overflow := session.pendingOverflow
	session.mu.Unlock()

	if overflow {
		m.logger.Error("skipping artifact store: pending chunks at capacity",
			"source", session.source,
			"session_id", session.sessionID,
			"pending_count", maxPendingChunks,
		)
		m.flushErrors.Add(1)
		return
	}

	// Store artifact. The data is passed as a streaming reader rather
	// than embedded in the header because flush buffers are ≥1MB
	// (chunkSizeThreshold), which exceeds the artifact protocol's
	// 64KB header size limit.
	filename := fmt.Sprintf("%s-%s-%d.bin",
		session.source.Localpart(),
		session.sessionID,
		firstSequence,
	)
	header := &artifactstore.StoreHeader{
		ContentType: artifactContentType,
		Filename:    filename,
		Size:        int64(len(data)),
		Labels:      []string{"log-output", session.source.Localpart()},
	}

	response, err := m.artifact.Store(ctx, header, bytes.NewReader(data))
	if err != nil {
		m.logger.Error("artifact store failed",
			"source", session.source,
			"session_id", session.sessionID,
			"error", err,
		)
		m.storeErrors.Add(1)
		m.flushErrors.Add(1)
		m.recordError(fmt.Errorf("artifact store: %w", err))
		// Nothing was stored — no ref to track. The data is lost
		// for this chunk. The next flush will store new data.
		return
	}
	m.storeCount.Add(1)

	newChunk := log.LogChunk{
		Ref:       response.Hash,
		Sequence:  firstSequence,
		Size:      int64(len(data)),
		Timestamp: firstTimestamp,
	}

	// Build and write the state event.
	m.writeLogEntity(ctx, session, roomID, newChunk)
}

// writeLogEntity builds the m.bureau.log state event content from
// the session's persisted chunks, pending chunks, and the new chunk,
// then writes it. On success, pending chunks are drained and the
// entity state is updated. On failure, the new chunk is added to
// pending chunks for retry on the next flush.
func (m *logManager) writeLogEntity(ctx context.Context, session *sessionBuffer, roomID ref.RoomID, newChunk log.LogChunk) {
	session.mu.Lock()

	// Build the full chunk list: persisted + pending + new.
	allChunks := make([]log.LogChunk, 0, len(session.chunks)+len(session.pendingChunks)+1)
	allChunks = append(allChunks, session.chunks...)
	allChunks = append(allChunks, session.pendingChunks...)
	allChunks = append(allChunks, newChunk)

	// Compute total bytes from all chunks.
	var totalBytes int64
	for i := range allChunks {
		totalBytes += allChunks[i].Size
	}

	content := log.LogContent{
		Version:    log.LogContentVersion,
		SessionID:  session.sessionID,
		Source:     session.source,
		Format:     log.LogFormatRaw,
		Status:     session.status,
		TotalBytes: totalBytes,
		Chunks:     allChunks,
	}

	session.mu.Unlock()

	// Write the state event (network call, do not hold lock).
	_, err := m.writer.SendStateEvent(ctx, roomID, log.EventTypeLog, session.sessionID, content)

	session.mu.Lock()
	defer session.mu.Unlock()

	m.flushCount.Add(1)
	if err != nil {
		m.stateEventErrors.Add(1)
		m.recordError(fmt.Errorf("state event write: %w", err))
		m.logger.Error("state event write failed",
			"source", session.source,
			"session_id", session.sessionID,
			"room_id", roomID,
			"error", err,
		)

		// Add the new chunk to pending for retry.
		session.pendingChunks = append(session.pendingChunks, newChunk)
		if len(session.pendingChunks) >= maxPendingChunks {
			session.pendingOverflow = true
			m.logger.Error("pending chunks at capacity, artifact storage paused for session",
				"source", session.source,
				"session_id", session.sessionID,
				"pending_count", len(session.pendingChunks),
			)
		}

		// Check for M_NOT_FOUND — invalidate room cache so next flush re-resolves.
		// The messaging library wraps errors; check the error string.
		if isNotFoundError(err) {
			session.roomResolved = false
			session.configRoomID = ref.RoomID{}
			m.logger.Warn("room not found, clearing room cache",
				"source", session.source,
				"session_id", session.sessionID,
			)
		}
		return
	}

	// Success: drain pending chunks and update entity state.
	m.stateEventWrites.Add(1)
	session.chunks = allChunks
	session.totalBytes = totalBytes
	session.pendingChunks = session.pendingChunks[:0]
	session.pendingOverflow = false
	session.entityExists = true
}

// resolveRoom resolves the machine config room alias and caches the
// result on the session buffer.
func (m *logManager) resolveRoom(ctx context.Context, session *sessionBuffer) {
	alias := session.machine.RoomAlias()

	roomID, err := m.writer.ResolveAlias(ctx, alias)

	session.mu.Lock()
	defer session.mu.Unlock()

	session.roomResolved = true
	if err != nil {
		m.logger.Warn("room alias resolution failed",
			"alias", alias,
			"source", session.source,
			"session_id", session.sessionID,
			"error", err,
		)
		return
	}

	session.configRoomID = roomID
	m.logger.Debug("resolved machine config room",
		"alias", alias,
		"room_id", roomID,
		"source", session.source,
	)
}

// Run starts the background flush and reaper tickers. Blocks until
// ctx is cancelled.
func (m *logManager) Run(ctx context.Context) {
	flushTicker := m.clock.NewTicker(m.flushInterval)
	defer flushTicker.Stop()

	reaperTicker := m.clock.NewTicker(1 * time.Minute)
	defer reaperTicker.Stop()

	for {
		select {
		case <-flushTicker.C:
			m.tickFlush(ctx)
		case <-reaperTicker.C:
			m.tickReaper(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// tickFlush scans all sessions and flushes any with non-empty data
// that hasn't been flushed within the flush interval.
func (m *logManager) tickFlush(ctx context.Context) {
	now := m.clock.Now()
	threshold := now.Add(-m.flushInterval)

	m.sessionsMu.RLock()
	sessions := make([]*sessionBuffer, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.sessionsMu.RUnlock()

	for _, session := range sessions {
		session.mu.Lock()
		if len(session.data) == 0 || session.lastActivity.After(threshold) {
			session.mu.Unlock()
			continue
		}

		// Swap out the buffer.
		data := session.data
		firstSequence := session.firstSequence
		firstTimestamp := session.firstTimestamp
		session.data = make([]byte, 0, m.chunkSizeThreshold)
		session.firstSequence = 0
		session.firstTimestamp = 0
		session.mu.Unlock()

		m.flushSession(ctx, session, data, firstSequence, firstTimestamp)
	}
}

// tickReaper scans all sessions and completes any that have been
// idle for longer than staleTimeout. After the stale scan, it runs
// the eviction loop: sessions whose totalBytes exceeds the max are
// trimmed from the front.
func (m *logManager) tickReaper(ctx context.Context) {
	now := m.clock.Now()
	staleThreshold := now.Add(-m.staleTimeout)

	m.sessionsMu.RLock()
	var stale []*sessionBuffer
	var staleKeys []sessionKey
	var oversized []*sessionBuffer
	for key, session := range m.sessions {
		session.mu.Lock()
		isStale := (session.status == log.LogStatusActive || session.status == log.LogStatusRotating) &&
			session.lastActivity.Before(staleThreshold)
		needsEviction := (session.status == log.LogStatusActive || session.status == log.LogStatusRotating) &&
			session.totalBytes > m.maxBytesPerSession
		session.mu.Unlock()
		if isStale {
			stale = append(stale, session)
			staleKeys = append(staleKeys, key)
		} else if needsEviction {
			// Only evict non-stale sessions. Stale sessions are
			// being completed, which removes them entirely.
			oversized = append(oversized, session)
		}
	}
	m.sessionsMu.RUnlock()

	for i, session := range stale {
		m.completeSession(ctx, session, staleKeys[i])
		m.logger.Info("reaped stale session",
			"source", session.source,
			"session_id", session.sessionID,
		)
	}

	for _, session := range oversized {
		m.evictSession(ctx, session)
	}
}

// evictSession removes the oldest chunks from a session until its
// totalBytes is at or below maxBytesPerSession. The last chunk is
// never removed (so the session always has some recent output). After
// trimming the in-memory state, writes the updated m.bureau.log state
// event.
func (m *logManager) evictSession(ctx context.Context, session *sessionBuffer) {
	session.mu.Lock()

	// Re-check under lock — a concurrent flush may have changed totalBytes.
	if session.totalBytes <= m.maxBytesPerSession {
		session.mu.Unlock()
		return
	}

	// Remove chunks from the front until totalBytes is within the
	// limit. Keep at least one chunk (never evict everything).
	evicted := 0
	evictedBytes := int64(0)
	for evicted < len(session.chunks)-1 && session.totalBytes-evictedBytes > m.maxBytesPerSession {
		evictedBytes += session.chunks[evicted].Size
		evicted++
	}

	if evicted == 0 {
		session.mu.Unlock()
		return
	}

	// Trim the chunk list and update totalBytes.
	session.chunks = session.chunks[evicted:]
	session.totalBytes -= evictedBytes
	if session.status == log.LogStatusActive {
		session.status = log.LogStatusRotating
	}

	roomID := session.configRoomID
	roomResolved := session.roomResolved

	m.logger.Info("evicting old output chunks",
		"source", session.source,
		"session_id", session.sessionID,
		"chunks_evicted", evicted,
		"bytes_evicted", evictedBytes,
		"chunks_remaining", len(session.chunks),
		"total_bytes_remaining", session.totalBytes,
	)

	session.mu.Unlock()

	m.evictionCount.Add(1)

	if !roomResolved || roomID.IsZero() {
		return
	}

	m.writeEvictedEntity(ctx, session, roomID)
}

// writeEvictedEntity writes the m.bureau.log state event after chunk
// eviction. Builds the event from the session's current in-memory
// state (chunks + pendingChunks, which reflects the post-eviction
// trim). On success, updates the session's persisted state.
func (m *logManager) writeEvictedEntity(ctx context.Context, session *sessionBuffer, roomID ref.RoomID) {
	session.mu.Lock()

	allChunks := make([]log.LogChunk, 0, len(session.chunks)+len(session.pendingChunks))
	allChunks = append(allChunks, session.chunks...)
	allChunks = append(allChunks, session.pendingChunks...)

	var totalBytes int64
	for i := range allChunks {
		totalBytes += allChunks[i].Size
	}

	content := log.LogContent{
		Version:    log.LogContentVersion,
		SessionID:  session.sessionID,
		Source:     session.source,
		Format:     log.LogFormatRaw,
		Status:     session.status,
		TotalBytes: totalBytes,
		Chunks:     allChunks,
	}

	session.mu.Unlock()

	_, err := m.writer.SendStateEvent(ctx, roomID, log.EventTypeLog, session.sessionID, content)

	session.mu.Lock()
	defer session.mu.Unlock()

	if err != nil {
		m.stateEventErrors.Add(1)
		m.recordError(fmt.Errorf("eviction state event write: %w", err))
		m.logger.Error("eviction state event write failed",
			"source", session.source,
			"session_id", session.sessionID,
			"error", err,
		)
		return
	}

	m.stateEventWrites.Add(1)
	session.chunks = allChunks
	session.totalBytes = totalBytes
	session.pendingChunks = session.pendingChunks[:0]
	session.pendingOverflow = false
	session.entityExists = true
}

// CompleteLog flushes remaining data and transitions sessions to
// "complete". When sessionID is non-empty, completes the specific
// session. When sessionID is empty, completes ALL active sessions
// for the given source — this is the path used by the daemon on
// sandbox exit, since the daemon doesn't know the session ID
// (generated by the launcher). Returns nil if no matching sessions
// exist (idempotent).
func (m *logManager) CompleteLog(ctx context.Context, sourceLocalpart, sessionID string) error {
	if sessionID != "" {
		return m.completeByKey(ctx, sessionKey{source: sourceLocalpart, sessionID: sessionID})
	}

	// Source-only: collect all sessions matching this source.
	m.sessionsMu.RLock()
	var targets []sessionKey
	for key := range m.sessions {
		if key.source == sourceLocalpart {
			targets = append(targets, key)
		}
	}
	m.sessionsMu.RUnlock()

	for _, key := range targets {
		if err := m.completeByKey(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

// completeByKey completes a single session identified by its key.
// Returns nil if the session doesn't exist or is already complete.
func (m *logManager) completeByKey(ctx context.Context, key sessionKey) error {
	m.sessionsMu.RLock()
	session, exists := m.sessions[key]
	m.sessionsMu.RUnlock()

	if !exists {
		return nil
	}

	session.mu.Lock()
	if session.status == log.LogStatusComplete {
		session.mu.Unlock()
		return nil
	}
	session.mu.Unlock()

	m.completeSession(ctx, session, key)
	return nil
}

// completeSession flushes remaining data, transitions the session to
// complete, writes the final state event, and removes it from the
// sessions map.
func (m *logManager) completeSession(ctx context.Context, session *sessionBuffer, key sessionKey) {
	// Flush any remaining data.
	session.mu.Lock()
	var data []byte
	var firstSequence uint64
	var firstTimestamp int64

	if len(session.data) > 0 {
		data = session.data
		firstSequence = session.firstSequence
		firstTimestamp = session.firstTimestamp
		session.data = nil
		session.firstSequence = 0
		session.firstTimestamp = 0
	}
	session.mu.Unlock()

	if data != nil {
		m.flushSession(ctx, session, data, firstSequence, firstTimestamp)
	}

	// Transition to complete and write the final state event.
	session.mu.Lock()
	session.status = log.LogStatusComplete
	roomID := session.configRoomID
	roomResolved := session.roomResolved
	hasEntity := session.entityExists || len(session.pendingChunks) > 0
	session.mu.Unlock()

	if roomResolved && !roomID.IsZero() && hasEntity {
		m.writeCompleteEntity(ctx, session, roomID)
	}

	// Remove from the sessions map.
	m.sessionsMu.Lock()
	delete(m.sessions, key)
	m.sessionsMu.Unlock()
}

// writeCompleteEntity writes the final m.bureau.log state event with
// status "complete". Does not create a new chunk — just updates the
// status field of the existing entity.
func (m *logManager) writeCompleteEntity(ctx context.Context, session *sessionBuffer, roomID ref.RoomID) {
	session.mu.Lock()

	// Build the final chunk list from persisted + pending.
	allChunks := make([]log.LogChunk, 0, len(session.chunks)+len(session.pendingChunks))
	allChunks = append(allChunks, session.chunks...)
	allChunks = append(allChunks, session.pendingChunks...)

	var totalBytes int64
	for i := range allChunks {
		totalBytes += allChunks[i].Size
	}

	content := log.LogContent{
		Version:    log.LogContentVersion,
		SessionID:  session.sessionID,
		Source:     session.source,
		Format:     log.LogFormatRaw,
		Status:     log.LogStatusComplete,
		TotalBytes: totalBytes,
		Chunks:     allChunks,
	}

	session.mu.Unlock()

	_, err := m.writer.SendStateEvent(ctx, roomID, log.EventTypeLog, session.sessionID, content)
	if err != nil {
		m.logger.Error("failed to write complete state event",
			"source", session.source,
			"session_id", session.sessionID,
			"error", err,
		)
		return
	}

	session.mu.Lock()
	session.chunks = allChunks
	session.totalBytes = totalBytes
	session.pendingChunks = session.pendingChunks[:0]
	session.pendingOverflow = false
	session.mu.Unlock()
}

// Close drains all active session buffers during graceful shutdown.
// Stores artifacts and updates entities for any non-empty buffers.
// Does NOT transition sessions to "complete" — the processes may
// still be running; only the telemetry service is shutting down.
func (m *logManager) Close(ctx context.Context) {
	m.sessionsMu.RLock()
	sessions := make([]*sessionBuffer, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.sessionsMu.RUnlock()

	var flushed int
	for _, session := range sessions {
		session.mu.Lock()
		if len(session.data) == 0 {
			session.mu.Unlock()
			continue
		}

		data := session.data
		firstSequence := session.firstSequence
		firstTimestamp := session.firstTimestamp
		session.data = nil
		session.firstSequence = 0
		session.firstTimestamp = 0
		session.mu.Unlock()

		m.flushSession(ctx, session, data, firstSequence, firstTimestamp)
		flushed++
	}

	m.logger.Info("log manager shutdown drain complete",
		"sessions_flushed", flushed,
	)
}

// isNotFoundError checks whether an error represents a Matrix
// M_NOT_FOUND response. The messaging library wraps HTTP errors in
// fmt.Errorf, so we check the error string.
func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "M_NOT_FOUND") || strings.Contains(message, "404")
}
