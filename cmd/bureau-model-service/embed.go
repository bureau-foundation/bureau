// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
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

	// Resolve the model alias.
	resolution, err := ms.resolveModel(request.Model)
	if err != nil {
		return nil, err
	}

	// Extract project identity.
	project := token.Project
	if project == "" {
		return nil, fmt.Errorf("service token missing project identity")
	}

	// Select account and check quota.
	account, err := ms.registry.SelectAccount(project, resolution.ProviderName)
	if err != nil {
		return nil, fmt.Errorf("no account for project %q, provider %q", project, resolution.ProviderName)
	}

	if err := ms.quotaTracker.Check(account.AccountName, account.Quota); err != nil {
		var quotaError *modelregistry.QuotaExceededError
		if errors.As(err, &quotaError) {
			return nil, fmt.Errorf("quota exceeded: %s limit for account %q (resets %s)",
				quotaError.Window, quotaError.AccountName,
				quotaError.ResetsAt.Format(time.RFC3339))
		}
		return nil, fmt.Errorf("quota check failed: %w", err)
	}

	// Determine latency policy. Defaults to immediate when unset.
	policy := request.LatencyPolicy
	if policy == "" {
		policy = model.LatencyImmediate
	}
	if !policy.IsKnown() {
		return nil, fmt.Errorf("unknown latency policy: %q", request.LatencyPolicy)
	}

	// Look up credential and get provider.
	credential := ms.lookupCredential(resolution, account)
	provider := ms.getOrCreateProvider(resolution.ProviderName, resolution.Provider)

	// Route through the latency router. For immediate requests, this
	// calls the provider directly. For batch, it merges with other
	// requests into a single provider call. For background, it waits
	// for the provider to become idle first.
	result, err := ms.latencyRouter.SubmitEmbed(
		ctx, policy, provider,
		resolution.ProviderName, resolution.ProviderModel, credential,
		request.Input,
		resolution.Provider.MaxBatchSize,
		resolution.Provider.BatchSupport,
	)
	if err != nil {
		return nil, fmt.Errorf("provider error: %w", err)
	}

	// Calculate cost and record to quota tracker.
	cost := calculateCost(&result.Usage, resolution.Pricing)
	ms.quotaTracker.Record(account.AccountName, cost)

	// Emit telemetry.
	latency := ms.clock.Now().Sub(startTime)
	ms.emitEmbedTelemetry(token, request.Model, resolution, account, &result.Usage, result.Model, cost, latency, result.BatchSize)

	// Return the response for the socket server to encode and send.
	return &model.EmbedResponse{
		Embeddings:       result.Embeddings,
		Model:            result.Model,
		Usage:            &result.Usage,
		CostMicrodollars: cost,
	}, nil
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
) {
	if ms.telemetry == nil {
		return
	}

	var inputTokens int64
	if usage != nil {
		inputTokens = usage.InputTokens
	}

	ms.telemetry.RecordSpan(telemetry.Span{
		TraceID:   telemetry.NewTraceID(),
		SpanID:    telemetry.NewSpanID(),
		Operation: "model.embed",
		StartTime: ms.clock.Now().Add(-latency).UnixNano(),
		Duration:  latency.Nanoseconds(),
		Status:    telemetry.SpanStatusOK,
		Attributes: map[string]any{
			"project":           token.Project,
			"agent":             token.Subject.String(),
			"model_alias":       requestedAlias,
			"provider":          resolution.ProviderName,
			"provider_model":    providerModel,
			"account":           account.AccountName,
			"input_tokens":      inputTokens,
			"cost_microdollars": costMicrodollars,
			"batch_size":        batchSize,
		},
	})
}
