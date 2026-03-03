// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/modelprovider"
	"github.com/bureau-foundation/bureau/lib/schema/model"
)

// embedBatchKey groups embed requests that can be merged into a single
// provider API call.
type embedBatchKey struct {
	ProviderName  string
	ProviderModel string
}

// EmbedBatchResult is the per-caller result delivered after a batch
// flushes. Exactly one value is sent on the caller's result channel.
type EmbedBatchResult struct {
	// Embeddings is the slice of embedding vectors for this caller's
	// inputs, extracted from the provider's merged response.
	Embeddings [][]byte

	// Model is the actual provider model used.
	Model string

	// Usage is the proportionally attributed token consumption for
	// this caller's inputs within the batch.
	Usage model.Usage

	// BatchSize is the number of callers in the batch. 1 when the
	// batch contained only this request.
	BatchSize int

	// Error is non-nil when the batch flush failed. When set, all
	// other fields are zero.
	Error error
}

// EmbedBatcher accumulates embedding requests and flushes them as
// merged provider API calls. Requests are grouped by
// (providerName, providerModel) — the batch key. A batch
// flushes when it reaches the provider's MaxBatchSize or when the
// flush timer fires (whichever comes first).
//
// On flush, the batcher concatenates all inputs into one provider.Embed
// call, then splits the response embeddings back to each caller by
// their input offset. Token usage is attributed proportionally by
// input count.
//
// Thread-safe: Submit is called from handler goroutines (one per socket
// connection), and flushBatch runs from timer callbacks or from Submit
// when the batch is full.
type EmbedBatcher struct {
	clock               clock.Clock
	logger              *slog.Logger
	flushDelay          time.Duration
	defaultMaxBatchSize int

	// serverCtx is the model service's root context. Provider calls
	// during flush use this context so they cancel on server shutdown
	// rather than being tied to any individual caller's context.
	serverCtx context.Context

	mu      sync.Mutex
	batches map[embedBatchKey]*pendingEmbedBatch
	closed  bool

	// entryCond is signaled whenever an entry is added to any batch.
	// Used by WaitForBatchSize for deterministic test synchronization.
	entryCond *sync.Cond
}

// pendingEmbedBatch accumulates entries until a flush trigger fires.
type pendingEmbedBatch struct {
	entries    []embedBatchEntry
	flushTimer *clock.Timer
	provider   modelprovider.Provider
	maxSize    int

	// providerModel is carried from the batch key for constructing
	// the EmbedRequest during flush.
	providerModel string
}

// embedBatchEntry is one caller's contribution to a batch.
type embedBatchEntry struct {
	input      [][]byte
	resultChan chan EmbedBatchResult
	ctx        context.Context
}

// NewEmbedBatcher creates a batcher with the given flush parameters.
// The serverCtx controls provider call cancellation on shutdown.
func NewEmbedBatcher(
	serverCtx context.Context,
	clk clock.Clock,
	logger *slog.Logger,
	flushDelay time.Duration,
	defaultMaxBatchSize int,
) *EmbedBatcher {
	batcher := &EmbedBatcher{
		serverCtx:           serverCtx,
		clock:               clk,
		logger:              logger,
		flushDelay:          flushDelay,
		defaultMaxBatchSize: defaultMaxBatchSize,
		batches:             make(map[embedBatchKey]*pendingEmbedBatch),
	}
	batcher.entryCond = sync.NewCond(&batcher.mu)
	return batcher
}

// Submit adds an embedding request to the batch for the given key.
// Returns a channel that receives exactly one EmbedBatchResult when
// the batch flushes. The channel has buffer 1 so the flush goroutine
// never blocks on send.
//
// If this entry fills the batch to maxSize, the batch is flushed
// immediately in a new goroutine. Otherwise, the flush timer handles
// it.
func (b *EmbedBatcher) Submit(
	ctx context.Context,
	key embedBatchKey,
	provider modelprovider.Provider,
	input [][]byte,
	maxBatchSize int,
) <-chan EmbedBatchResult {
	resultChan := make(chan EmbedBatchResult, 1)

	b.mu.Lock()

	if b.closed {
		b.mu.Unlock()
		resultChan <- EmbedBatchResult{Error: context.Canceled}
		return resultChan
	}

	batch, exists := b.batches[key]
	if !exists {
		effectiveMax := maxBatchSize
		if effectiveMax <= 0 {
			effectiveMax = b.defaultMaxBatchSize
		}

		batch = &pendingEmbedBatch{
			provider:      provider,
			maxSize:       effectiveMax,
			providerModel: key.ProviderModel,
		}
		batch.flushTimer = b.clock.AfterFunc(b.flushDelay, func() {
			go b.flushBatch(key)
		})
		b.batches[key] = batch
	}

	batch.entries = append(batch.entries, embedBatchEntry{
		input:      input,
		resultChan: resultChan,
		ctx:        ctx,
	})
	b.entryCond.Broadcast()

	if len(batch.entries) >= batch.maxSize {
		batch.flushTimer.Stop()
		delete(b.batches, key)
		b.mu.Unlock()
		go b.doFlush(batch)
		return resultChan
	}

	b.mu.Unlock()
	return resultChan
}

// flushBatch is called by the timer callback. It extracts the batch
// from the map (if it still exists) and flushes it.
func (b *EmbedBatcher) flushBatch(key embedBatchKey) {
	b.mu.Lock()
	batch, exists := b.batches[key]
	if !exists {
		// Already flushed by Submit (batch hit maxSize between timer
		// schedule and timer fire). Nothing to do.
		b.mu.Unlock()
		return
	}
	delete(b.batches, key)
	b.mu.Unlock()

	b.doFlush(batch)
}

// doFlush executes the merged provider call and distributes results
// to each caller's result channel. Must be called without holding
// b.mu.
func (b *EmbedBatcher) doFlush(batch *pendingEmbedBatch) {
	// Separate live entries (context still valid) from cancelled ones.
	var liveEntries []embedBatchEntry
	for _, entry := range batch.entries {
		if entry.ctx.Err() != nil {
			entry.resultChan <- EmbedBatchResult{Error: entry.ctx.Err()}
			continue
		}
		liveEntries = append(liveEntries, entry)
	}

	if len(liveEntries) == 0 {
		b.logger.Debug("embed batch skipped (all callers cancelled)")
		return
	}

	// Build the merged input and track per-entry offsets.
	type entryOffset struct {
		startIndex int
		count      int
	}

	var mergedInput [][]byte
	offsets := make([]entryOffset, len(liveEntries))
	for i, entry := range liveEntries {
		offsets[i] = entryOffset{
			startIndex: len(mergedInput),
			count:      len(entry.input),
		}
		mergedInput = append(mergedInput, entry.input...)
	}

	totalInputCount := len(mergedInput)
	batchSize := len(liveEntries)

	b.logger.Debug("flushing embed batch",
		"batch_size", batchSize,
		"total_inputs", totalInputCount,
		"model", batch.providerModel,
	)

	// Call the provider with merged inputs.
	result, err := batch.provider.Embed(b.serverCtx, &modelprovider.EmbedRequest{
		Model: batch.providerModel,
		Input: mergedInput,
	})

	if err != nil {
		for _, entry := range liveEntries {
			entry.resultChan <- EmbedBatchResult{Error: err}
		}
		return
	}

	// Validate the response has the expected number of embeddings.
	if len(result.Embeddings) != totalInputCount {
		for _, entry := range liveEntries {
			entry.resultChan <- EmbedBatchResult{
				Error: fmt.Errorf(
					"provider returned %d embeddings for %d inputs",
					len(result.Embeddings), totalInputCount,
				),
			}
		}
		return
	}

	// Distribute results. Each caller gets their slice of embeddings
	// and a proportionally attributed share of the total token usage.
	for i, entry := range liveEntries {
		offset := offsets[i]
		embeddings := make([][]byte, offset.count)
		copy(embeddings, result.Embeddings[offset.startIndex:offset.startIndex+offset.count])

		// Proportional token attribution: this caller's share of
		// the total is (their input count / total input count).
		// Integer division may lose a few tokens to rounding, which
		// favors the user (charges slightly less than actual).
		attributedInputTokens := result.Usage.InputTokens * int64(offset.count) / int64(totalInputCount)

		entry.resultChan <- EmbedBatchResult{
			Embeddings: embeddings,
			Model:      result.Model,
			Usage: model.Usage{
				InputTokens: attributedInputTokens,
			},
			BatchSize: batchSize,
		}
	}
}

// Close stops all flush timers and flushes pending batches as a
// last-chance service. The server context may already be cancelled,
// in which case provider calls fail and waiters receive errors.
func (b *EmbedBatcher) Close() {
	b.mu.Lock()
	b.closed = true
	pending := make([]*pendingEmbedBatch, 0, len(b.batches))
	for _, batch := range b.batches {
		batch.flushTimer.Stop()
		pending = append(pending, batch)
	}
	b.batches = make(map[embedBatchKey]*pendingEmbedBatch)
	b.mu.Unlock()

	for _, batch := range pending {
		b.doFlush(batch)
	}
}

// WaitForBatchSize blocks until the batch for the given key has at
// least count entries queued. Used for deterministic test
// synchronization — ensures all goroutines have reached Submit()
// before the test advances the clock.
func (b *EmbedBatcher) WaitForBatchSize(key embedBatchKey, count int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		if batch, ok := b.batches[key]; ok && len(batch.entries) >= count {
			return
		}
		b.entryCond.Wait()
	}
}
