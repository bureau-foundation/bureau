// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/modelprovider"
	"github.com/bureau-foundation/bureau/lib/schema/model"
)

// LatencyConfig configures the latency router's batching and gating
// behavior. The same flush delay and default batch size apply to both
// the EmbedBatcher (request merging) and CompletionGate (timing
// coordination).
type LatencyConfig struct {
	// FlushDelay is the maximum time to accumulate requests before
	// flushing a partial batch or opening a completion gate. Lower
	// values reduce latency for partial batches; higher values allow
	// more requests to accumulate for better GPU utilization.
	FlushDelay time.Duration

	// DefaultMaxBatchSize is the batch/gate size used when the
	// provider declares MaxBatchSize=0 (no preference). Controls
	// both embed batch accumulation and completion gate sizing.
	DefaultMaxBatchSize int
}

// DefaultLatencyConfig returns production defaults: 50ms flush delay,
// 64-request default batch size.
func DefaultLatencyConfig() LatencyConfig {
	return LatencyConfig{
		FlushDelay:          50 * time.Millisecond,
		DefaultMaxBatchSize: 64,
	}
}

// LatencyRouter is the single entry point for latency-aware request
// routing. Handlers call SubmitEmbed or GateComplete instead of
// calling providers directly. The router owns the three latency
// components:
//
//   - EmbedBatcher: merges batch/background embed requests into
//     single provider API calls, splitting results back to callers.
//   - CompletionGate: groups batch completion requests and releases
//     them simultaneously so the inference engine batches internally.
//   - BackgroundScheduler: defers background requests until the
//     provider has no active immediate/batch requests in flight.
//
// Thread-safe: all methods are safe for concurrent use from handler
// goroutines.
type LatencyRouter struct {
	embedBatcher        *EmbedBatcher
	completionGate      *CompletionGate
	backgroundScheduler *BackgroundScheduler
	logger              *slog.Logger
}

// NewLatencyRouter creates a router with the given configuration.
// The serverCtx controls provider call cancellation during batch
// flushes — it should be the model service's root context.
func NewLatencyRouter(
	serverCtx context.Context,
	clk clock.Clock,
	logger *slog.Logger,
	config LatencyConfig,
) *LatencyRouter {
	return &LatencyRouter{
		embedBatcher: NewEmbedBatcher(
			serverCtx, clk, logger,
			config.FlushDelay, config.DefaultMaxBatchSize,
		),
		completionGate: NewCompletionGate(
			clk, logger,
			config.FlushDelay, config.DefaultMaxBatchSize,
		),
		backgroundScheduler: NewBackgroundScheduler(logger),
		logger:              logger,
	}
}

// SubmitEmbed routes an embedding request through the appropriate
// latency path and returns the result.
//
// Routing by policy:
//   - immediate: calls provider.Embed directly, no batching.
//   - batch: submits to the EmbedBatcher, which merges this request
//     with other requests for the same (provider, model, credential)
//     key and makes a single provider call.
//   - background: waits for the provider to become idle (no active
//     immediate/batch requests), then submits through the batcher.
//
// When batchSupport is false, batch and background policies degrade
// to immediate — the provider's API may accept multiple inputs from
// a single caller, but we don't merge across callers because the
// provider doesn't support it.
func (r *LatencyRouter) SubmitEmbed(
	ctx context.Context,
	policy model.LatencyPolicy,
	provider modelprovider.Provider,
	providerName string,
	providerModel string,
	credential string,
	input [][]byte,
	maxBatchSize int,
	batchSupport bool,
) (EmbedBatchResult, error) {
	// Degrade to immediate when the provider doesn't support batching.
	// The batcher merges multiple callers' inputs into one provider
	// call — if the provider can't handle that, we skip it entirely.
	effectivePolicy := policy
	if !batchSupport && effectivePolicy != model.LatencyImmediate {
		effectivePolicy = model.LatencyImmediate
		r.logger.Debug("degrading latency policy to immediate (no batch support)",
			"original_policy", string(policy),
			"provider", providerName,
		)
	}

	switch effectivePolicy {
	case model.LatencyImmediate:
		return r.embedDirect(ctx, provider, providerModel, credential, input)

	case model.LatencyBatch:
		return r.embedBatched(ctx, providerName, providerModel, credential, provider, input, maxBatchSize)

	case model.LatencyBackground:
		if err := r.backgroundScheduler.WaitForIdle(ctx, providerName); err != nil {
			return EmbedBatchResult{}, err
		}
		return r.embedBatched(ctx, providerName, providerModel, credential, provider, input, maxBatchSize)

	default:
		return EmbedBatchResult{}, fmt.Errorf("unknown latency policy: %q", policy)
	}
}

// embedDirect calls the provider without batching. Used for immediate
// policy and as the fallback when batch support is disabled.
func (r *LatencyRouter) embedDirect(
	ctx context.Context,
	provider modelprovider.Provider,
	providerModel string,
	credential string,
	input [][]byte,
) (EmbedBatchResult, error) {
	result, err := provider.Embed(ctx, &modelprovider.EmbedRequest{
		Model:      providerModel,
		Input:      input,
		Credential: credential,
	})
	if err != nil {
		return EmbedBatchResult{}, err
	}
	return EmbedBatchResult{
		Embeddings: result.Embeddings,
		Model:      result.Model,
		Usage:      result.Usage,
		BatchSize:  1,
	}, nil
}

// embedBatched submits through the EmbedBatcher and blocks until the
// batch flushes.
func (r *LatencyRouter) embedBatched(
	ctx context.Context,
	providerName string,
	providerModel string,
	credential string,
	provider modelprovider.Provider,
	input [][]byte,
	maxBatchSize int,
) (EmbedBatchResult, error) {
	key := embedBatchKey{
		ProviderName:  providerName,
		ProviderModel: providerModel,
		Credential:    credential,
	}

	resultChan := r.embedBatcher.Submit(ctx, key, provider, input, maxBatchSize)
	result := <-resultChan
	if result.Error != nil {
		return EmbedBatchResult{}, result.Error
	}
	return result, nil
}

// GateComplete applies latency policy to a completion request.
//
// Routing by policy:
//   - immediate: returns nil immediately, no gating.
//   - batch: blocks until the CompletionGate opens for this
//     (provider, model) key. The gate opens when enough concurrent
//     requests accumulate or the flush timer fires, releasing all
//     waiters simultaneously so the inference engine sees concurrent
//     requests it can batch internally.
//   - background: blocks until the provider is idle (no active
//     immediate/batch requests). Background completions proceed
//     individually — they are not gated because each gets its own
//     provider.Complete call.
//
// Called BEFORE the stream ack — the client's OpenStream blocks
// until this returns. This is correct: batch/background clients
// expect latency, and gating before ack prevents the client from
// reading a stream that won't produce data until the provider call
// actually starts.
func (r *LatencyRouter) GateComplete(
	ctx context.Context,
	policy model.LatencyPolicy,
	providerName string,
	providerModel string,
	maxBatchSize int,
) error {
	switch policy {
	case model.LatencyImmediate:
		return nil

	case model.LatencyBatch:
		key := completionGateKey{
			ProviderName:  providerName,
			ProviderModel: providerModel,
		}
		return r.completionGate.Wait(ctx, key, maxBatchSize)

	case model.LatencyBackground:
		return r.backgroundScheduler.WaitForIdle(ctx, providerName)

	default:
		return fmt.Errorf("unknown latency policy: %q", policy)
	}
}

// RecordActiveStart increments the active request count for a
// provider. Called by the handler when a non-background request
// begins its provider API call.
func (r *LatencyRouter) RecordActiveStart(providerName string) {
	r.backgroundScheduler.RecordStart(providerName)
}

// RecordActiveEnd decrements the active request count for a provider
// and wakes any waiting background requests. Called by the handler
// when a non-background request completes.
func (r *LatencyRouter) RecordActiveEnd(providerName string) {
	r.backgroundScheduler.RecordEnd(providerName)
}

// Close shuts down all latency components in order:
//  1. EmbedBatcher: stops timers, flushes pending batches (last-chance
//     service so partially accumulated requests get results).
//  2. CompletionGate: releases all pending waiters (they'll find
//     their context cancelled from server shutdown and return errors).
//  3. BackgroundScheduler: wakes all background waiters (same — they
//     see ctx.Err() and return errors).
//
// Called during shutdown before closing provider HTTP clients.
func (r *LatencyRouter) Close() {
	r.embedBatcher.Close()
	r.completionGate.Close()
	r.backgroundScheduler.Close()
}
