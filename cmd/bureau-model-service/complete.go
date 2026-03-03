// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/modelprovider"
	"github.com/bureau-foundation/bureau/lib/modelregistry"
	"github.com/bureau-foundation/bureau/lib/schema/model"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// handleComplete processes a streaming completion request. This is an
// AuthStreamFunc: the socket server has already verified the token and
// cleared deadlines. The handler owns the connection until it returns.
//
// Protocol:
//  1. Decode the Request from the initial handshake raw bytes
//  2. Resolve the alias, select an account, check quota, get credential
//  3. Apply latency policy: gate for batch, wait for idle for background
//  4. Send ack (Response{OK: true}) to complete the OpenStream handshake
//  5. Call the provider and stream response chunks to the client
//  6. Record cost and emit telemetry
//
// On pre-ack errors (bad request, unknown alias, quota exceeded,
// cancelled while gating), the handler sends a stream rejection via
// SendError. The client's OpenStream sees a *ServiceError.
//
// On post-ack errors (provider failure, stream interruption), the
// handler sends a model.Response{Type: "error"} on the stream. The
// client processes this as a normal stream message.
func (ms *ModelService) handleComplete(ctx context.Context, token *servicetoken.Token, raw []byte, conn net.Conn) {
	startTime := ms.clock.Now()
	stream := service.NewServiceStream(conn)

	// Decode the typed request from the initial handshake bytes.
	var request model.Request
	if err := codec.Unmarshal(raw, &request); err != nil {
		stream.SendError(fmt.Sprintf("invalid request: %v", err))
		return
	}

	if request.Model == "" {
		stream.SendError("missing required field: model")
		return
	}
	if len(request.Messages) == 0 {
		stream.SendError("missing required field: messages")
		return
	}

	// Load continuation history if this is a multi-turn request.
	// When continuation_id is present, stored conversation history
	// is prepended to the request messages so the model sees the
	// full context. Continuations are opt-in: requests without a
	// continuation_id create no server-side state.
	var contKey continuationKey
	providerMessages := request.Messages
	if request.ContinuationID != "" {
		contKey = continuationKey{
			agent:          token.Subject.String(),
			continuationID: request.ContinuationID,
		}
		if history := ms.continuations.Load(contKey); history != nil {
			combined := make([]model.Message, 0, len(history)+len(request.Messages))
			combined = append(combined, history...)
			combined = append(combined, request.Messages...)
			providerMessages = combined
		}
	}

	// Resolve the model alias to a fallback chain (primary + fallbacks).
	chain, err := ms.resolveModelChain(request.Model)
	if err != nil {
		stream.SendError(err.Error())
		return
	}

	// Authorize using the primary alias. All chain entries share the
	// same alias name, so one grant check covers the whole chain.
	if err := requireModelGrant(token, model.ActionComplete, chain[0].Alias); err != nil {
		stream.SendError(err.Error())
		return
	}

	// Extract the project identity from the service token for
	// per-project accounting and credential selection.
	project := token.Project
	if project == "" {
		stream.SendError("service token missing project identity")
		return
	}

	// Determine latency policy. Defaults to immediate when unset.
	policy := request.LatencyPolicy
	if policy == "" {
		policy = model.LatencyImmediate
	}
	if !policy.IsKnown() {
		stream.SendError(fmt.Sprintf("unknown latency policy: %q", request.LatencyPolicy))
		return
	}

	// Apply latency gating for the primary provider. Fallback
	// attempts use immediate mode — when degraded, minimize latency
	// rather than waiting for batching.
	primaryResolution := chain[0]
	if policy != model.LatencyBackground {
		ms.latencyRouter.RecordActiveStart(primaryResolution.ProviderName)
		defer ms.latencyRouter.RecordActiveEnd(primaryResolution.ProviderName)
	}
	if err := ms.latencyRouter.GateComplete(
		ctx, policy,
		primaryResolution.ProviderName, primaryResolution.ProviderModel,
		primaryResolution.Provider.MaxBatchSize,
	); err != nil {
		stream.SendError(fmt.Sprintf("cancelled while waiting: %v", err))
		return
	}

	// Send the stream ack to complete the OpenStream handshake.
	// After this point, the client's OpenStream returns and the
	// client starts reading stream messages. All errors from here
	// on must go as model.Response{Type: "error"} on the stream.
	if err := stream.SendAck(); err != nil {
		ms.logger.Error("failed to send stream ack",
			"subject", token.Subject,
			"error", err,
		)
		return
	}

	// Try each resolution in the chain until one succeeds.
	var completionStream modelprovider.CompletionStream
	var resolution modelregistry.Resolution
	var account modelregistry.AccountSelection
	degraded := false

	for chainIndex, candidate := range chain {
		// Select account and credential for this provider.
		candidateAccount, accountErr := ms.registry.SelectAccount(project, candidate.ProviderName)
		if accountErr != nil {
			ms.logger.Warn("fallback chain: no account for provider, skipping",
				"alias", candidate.Alias,
				"provider", candidate.ProviderName,
				"chain_index", chainIndex,
				"error", accountErr,
			)
			continue
		}

		if quotaErr := ms.quotaTracker.Check(candidateAccount.AccountName, candidateAccount.Quota); quotaErr != nil {
			ms.logger.Warn("fallback chain: quota exceeded, skipping",
				"alias", candidate.Alias,
				"provider", candidate.ProviderName,
				"account", candidateAccount.AccountName,
				"chain_index", chainIndex,
				"error", quotaErr,
			)
			continue
		}

		provider := ms.getOrCreateProvider(candidate.ProviderName, candidate.Provider)

		attemptStream, attemptErr := provider.Complete(ctx, &modelprovider.CompleteRequest{
			Model:    candidate.ProviderModel,
			Messages: providerMessages,
			Stream:   request.Stream,
		})

		if attemptErr == nil {
			completionStream = attemptStream
			resolution = candidate
			account = candidateAccount
			degraded = chainIndex > 0
			if degraded {
				ms.logger.Warn("fallback activated",
					"alias", candidate.Alias,
					"primary_provider", chain[0].ProviderName,
					"fallback_provider", candidate.ProviderName,
					"fallback_model", candidate.ProviderModel,
					"chain_index", chainIndex,
				)
			}
			break
		}

		// Check if the error is retriable (connection/server failure).
		if !isRetriableProviderError(attemptErr) || ctx.Err() != nil {
			stream.Send(model.Response{
				Type:  model.ResponseError,
				Error: fmt.Sprintf("provider error: %v", attemptErr),
			})
			return
		}

		ms.logger.Warn("provider failed, trying next in chain",
			"alias", candidate.Alias,
			"provider", candidate.ProviderName,
			"chain_index", chainIndex,
			"error", attemptErr,
		)
	}

	if completionStream == nil {
		stream.Send(model.Response{
			Type:  model.ResponseError,
			Error: "all providers in fallback chain failed",
		})
		return
	}
	defer completionStream.Close()

	// Stream response chunks to the client. When a continuation is
	// active, accumulate the assistant's response content so we can
	// store the full conversation after successful completion.
	var finalUsage *model.Usage
	var finalModel string
	var responseContent strings.Builder
	for completionStream.Next() {
		chunk := completionStream.Response()

		// Accumulate content for continuation storage. Delta
		// messages carry incremental fragments; done messages
		// carry the full text for non-streaming requests.
		if request.ContinuationID != "" {
			responseContent.WriteString(chunk.Content)
		}

		if chunk.Type == model.ResponseDone {
			finalUsage = chunk.Usage
			finalModel = chunk.Model

			// Enrich the done message with cost information
			// before forwarding to the client.
			cost := calculateCost(chunk.Usage, resolution.Pricing)
			chunk.CostMicrodollars = cost

			// Echo the continuation_id so the agent can use it
			// in subsequent requests.
			chunk.ContinuationID = request.ContinuationID

			// Set the Degraded flag if the agent opted in and a
			// fallback was used.
			if degraded && request.ReportDegraded {
				chunk.Degraded = true
			}
		}

		if err := stream.Send(chunk); err != nil {
			ms.logger.Debug("stream send failed (client disconnected)",
				"subject", token.Subject,
				"error", err,
			)
			return
		}
	}

	// Check for stream errors from the provider.
	if err := completionStream.Err(); err != nil {
		stream.Send(model.Response{
			Type:  model.ResponseError,
			Error: fmt.Sprintf("stream error: %v", err),
		})
		return
	}

	// Store the continuation if this was a multi-turn request. The
	// full conversation (history + new messages + assistant response)
	// is saved so the next request in this continuation has the
	// complete context.
	if request.ContinuationID != "" {
		assistantMessage := model.Message{
			Role:    "assistant",
			Content: responseContent.String(),
		}
		fullConversation := make([]model.Message, len(providerMessages)+1)
		copy(fullConversation, providerMessages)
		fullConversation[len(providerMessages)] = assistantMessage
		ms.continuations.Store(contKey, fullConversation)
	}

	// Record cost and emit telemetry for the completed request.
	cost := calculateCost(finalUsage, resolution.Pricing)
	ms.quotaTracker.Record(account.AccountName, cost)

	latency := ms.clock.Now().Sub(startTime)
	ms.emitUsageTelemetry(token, request.Model, resolution, account, finalUsage, finalModel, cost, latency, degraded, chain[0].ProviderName)
}

// resolveModel resolves a model alias (or "auto") to a Resolution.
func (ms *ModelService) resolveModel(alias string) (modelregistry.Resolution, error) {
	if alias == "auto" {
		return ms.registry.ResolveAuto(nil)
	}
	return ms.registry.Resolve(alias)
}

// resolveModelChain resolves a model alias to a fallback chain.
// For "auto" resolution, returns a single-element chain (auto doesn't
// support fallbacks — it already picks from all available models).
func (ms *ModelService) resolveModelChain(alias string) ([]modelregistry.Resolution, error) {
	if alias == "auto" {
		resolution, err := ms.registry.ResolveAuto(nil)
		if err != nil {
			return nil, err
		}
		return []modelregistry.Resolution{resolution}, nil
	}
	return ms.registry.ResolveChain(alias)
}

// isRetriableProviderError returns true if the error indicates a
// transient provider failure that should trigger a fallback attempt.
// Connection failures, server errors (5xx), and rate limits (429)
// are retriable. Client errors (4xx except 429) and context
// cancellation are not.
func isRetriableProviderError(err error) bool {
	if err == nil {
		return false
	}

	message := err.Error()

	// Connection-level failures are always retriable.
	if strings.Contains(message, "connection refused") ||
		strings.Contains(message, "no such host") ||
		strings.Contains(message, "i/o timeout") ||
		strings.Contains(message, "connection reset") ||
		strings.Contains(message, "EOF") {
		return true
	}

	// HTTP 5xx and 429 are retriable. Look for status codes in
	// error messages from the provider layer.
	if strings.Contains(message, "status 5") ||
		strings.Contains(message, "status 429") ||
		strings.Contains(message, "502") ||
		strings.Contains(message, "503") ||
		strings.Contains(message, "504") {
		return true
	}

	return false
}

// emitUsageTelemetry records a telemetry span for a completed model
// request. No-op when the telemetry emitter is nil. When degraded is
// true, the span includes fallback attributes so operators can see
// which agents are running on non-primary models.
func (ms *ModelService) emitUsageTelemetry(
	token *servicetoken.Token,
	requestedAlias string,
	resolution modelregistry.Resolution,
	account modelregistry.AccountSelection,
	usage *model.Usage,
	providerModel string,
	costMicrodollars int64,
	latency time.Duration,
	degraded bool,
	primaryProvider string,
) {
	if ms.telemetry == nil {
		return
	}

	var inputTokens, outputTokens int64
	if usage != nil {
		inputTokens = usage.InputTokens
		outputTokens = usage.OutputTokens
	}

	attributes := map[string]any{
		"project":           token.Project,
		"agent":             token.Subject.String(),
		"model_alias":       requestedAlias,
		"provider":          resolution.ProviderName,
		"provider_model":    providerModel,
		"account":           account.AccountName,
		"input_tokens":      inputTokens,
		"output_tokens":     outputTokens,
		"cost_microdollars": costMicrodollars,
	}

	if degraded {
		attributes["fallback"] = true
		attributes["primary_provider"] = primaryProvider
	}

	ms.telemetry.RecordSpan(telemetry.Span{
		TraceID:    telemetry.NewTraceID(),
		SpanID:     telemetry.NewSpanID(),
		Operation:  "model.complete",
		StartTime:  ms.clock.Now().Add(-latency).UnixNano(),
		Duration:   latency.Nanoseconds(),
		Status:     telemetry.SpanStatusOK,
		Attributes: attributes,
	})
}
