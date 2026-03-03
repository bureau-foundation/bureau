// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/modelregistry"
	"github.com/bureau-foundation/bureau/lib/schema/model"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// handleEmbed processes an embedding request. This is an
// AuthActionFunc: the socket server handles the response envelope.
// The handler returns the EmbedResponse as the response data, or an
// error for a failure response.
func (ms *ModelService) handleEmbed(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	startTime := ms.clock.Now()

	// Decode the typed request.
	var request model.Request
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.Model == "" {
		return nil, fmt.Errorf("missing required field: model")
	}
	if len(request.Input) == 0 {
		return nil, fmt.Errorf("missing required field: input")
	}

	// Resolve the model alias to a fallback chain.
	chain, err := ms.resolveModelChain(request.Model)
	if err != nil {
		return nil, err
	}

	// Authorize using the primary alias.
	if err := requireModelGrant(token, model.ActionEmbed, chain[0].Alias); err != nil {
		return nil, err
	}

	// Extract project identity.
	project := token.Project
	if project == "" {
		return nil, fmt.Errorf("service token missing project identity")
	}

	// Determine latency policy. Defaults to immediate when unset.
	policy := request.LatencyPolicy
	if policy == "" {
		policy = model.LatencyImmediate
	}
	if !policy.IsKnown() {
		return nil, fmt.Errorf("unknown latency policy: %q", request.LatencyPolicy)
	}

	// Try each resolution in the chain until one succeeds.
	var lastError error
	for chainIndex, candidate := range chain {
		candidateAccount, accountErr := ms.registry.SelectAccount(project, candidate.ProviderName)
		if accountErr != nil {
			ms.logger.Warn("embed fallback chain: no account, skipping",
				"provider", candidate.ProviderName,
				"chain_index", chainIndex,
				"error", accountErr,
			)
			lastError = accountErr
			continue
		}

		if quotaErr := ms.quotaTracker.Check(candidateAccount.AccountName, candidateAccount.Quota); quotaErr != nil {
			ms.logger.Warn("embed fallback chain: quota exceeded, skipping",
				"provider", candidate.ProviderName,
				"account", candidateAccount.AccountName,
				"chain_index", chainIndex,
				"error", quotaErr,
			)
			lastError = quotaErr
			continue
		}

		credential := ms.lookupCredential(candidate, candidateAccount)
		provider := ms.getOrCreateProvider(candidate.ProviderName, candidate.Provider)

		// Use the requested latency policy for the primary, immediate
		// for fallbacks (minimize degraded-state latency).
		attemptPolicy := policy
		if chainIndex > 0 {
			attemptPolicy = model.LatencyImmediate
		}

		result, attemptErr := ms.latencyRouter.SubmitEmbed(
			ctx, attemptPolicy, provider,
			candidate.ProviderName, candidate.ProviderModel, credential,
			request.Input,
			candidate.Provider.MaxBatchSize,
			candidate.Provider.BatchSupport,
		)

		if attemptErr != nil {
			if !isRetriableProviderError(attemptErr) || ctx.Err() != nil {
				return nil, fmt.Errorf("provider error: %w", attemptErr)
			}
			ms.logger.Warn("embed provider failed, trying next in chain",
				"provider", candidate.ProviderName,
				"chain_index", chainIndex,
				"error", attemptErr,
			)
			lastError = attemptErr
			continue
		}

		if chainIndex > 0 {
			ms.logger.Warn("embed fallback activated",
				"alias", candidate.Alias,
				"primary_provider", chain[0].ProviderName,
				"fallback_provider", candidate.ProviderName,
				"chain_index", chainIndex,
			)
		}

		// Success — calculate cost, record, emit telemetry, return.
		cost := calculateCost(&result.Usage, candidate.Pricing)
		ms.quotaTracker.Record(candidateAccount.AccountName, cost)

		latency := ms.clock.Now().Sub(startTime)
		ms.emitEmbedTelemetry(token, request.Model, candidate, candidateAccount,
			&result.Usage, result.Model, cost, latency, result.BatchSize,
			chainIndex > 0, chain[0].ProviderName)

		return &model.EmbedResponse{
			Embeddings:       result.Embeddings,
			Model:            result.Model,
			Usage:            &result.Usage,
			CostMicrodollars: cost,
		}, nil
	}

	if lastError != nil {
		return nil, fmt.Errorf("all providers in fallback chain failed: %w", lastError)
	}
	return nil, fmt.Errorf("all providers in fallback chain failed")
}

// emitEmbedTelemetry records a telemetry span for a completed
// embedding request. No-op when the telemetry emitter is nil.
func (ms *ModelService) emitEmbedTelemetry(
	token *servicetoken.Token,
	requestedAlias string,
	resolution modelregistry.Resolution,
	account modelregistry.AccountSelection,
	usage *model.Usage,
	providerModel string,
	costMicrodollars int64,
	latency time.Duration,
	batchSize int,
	degraded bool,
	primaryProvider string,
) {
	if ms.telemetry == nil {
		return
	}

	var inputTokens int64
	if usage != nil {
		inputTokens = usage.InputTokens
	}

	attributes := map[string]any{
		"project":           token.Project,
		"agent":             token.Subject.String(),
		"model_alias":       requestedAlias,
		"provider":          resolution.ProviderName,
		"provider_model":    providerModel,
		"account":           account.AccountName,
		"input_tokens":      inputTokens,
		"cost_microdollars": costMicrodollars,
		"batch_size":        batchSize,
	}

	if degraded {
		attributes["fallback"] = true
		attributes["primary_provider"] = primaryProvider
	}

	ms.telemetry.RecordSpan(telemetry.Span{
		TraceID:    telemetry.NewTraceID(),
		SpanID:     telemetry.NewSpanID(),
		Operation:  "model.embed",
		StartTime:  ms.clock.Now().Add(-latency).UnixNano(),
		Duration:   latency.Nanoseconds(),
		Status:     telemetry.SpanStatusOK,
		Attributes: attributes,
	})
}
