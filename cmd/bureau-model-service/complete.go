// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
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

	// Resolve the model alias to a concrete provider and model.
	resolution, err := ms.resolveModel(request.Model)
	if err != nil {
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

	// Select the account for this (project, provider) pair.
	account, err := ms.registry.SelectAccount(project, resolution.ProviderName)
	if err != nil {
		stream.SendError(fmt.Sprintf("no account for project %q, provider %q", project, resolution.ProviderName))
		return
	}

	// Check quota before forwarding the request.
	if err := ms.quotaTracker.Check(account.AccountName, account.Quota); err != nil {
		var quotaError *modelregistry.QuotaExceededError
		if errors.As(err, &quotaError) {
			stream.SendError(fmt.Sprintf("quota exceeded: %s limit for account %q (resets %s)",
				quotaError.Window, quotaError.AccountName,
				quotaError.ResetsAt.Format(time.RFC3339)))
			return
		}
		stream.SendError(fmt.Sprintf("quota check failed: %v", err))
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

	// Look up the API credential for this account.
	credential := ms.lookupCredential(resolution, account)

	// Get or create the provider HTTP client.
	provider := ms.getOrCreateProvider(resolution.ProviderName, resolution.Provider)

	// Track active requests for background scheduling. Immediate and
	// batch requests count as "active" so background requests wait
	// for them to complete. Background requests are NOT tracked — they
	// should not block other background requests.
	if policy != model.LatencyBackground {
		ms.latencyRouter.RecordActiveStart(resolution.ProviderName)
		defer ms.latencyRouter.RecordActiveEnd(resolution.ProviderName)
	}

	// Apply latency gating. For batch policy, blocks until enough
	// concurrent requests accumulate (or a timer fires) so the
	// inference engine sees concurrent requests it can batch
	// internally. For background, blocks until the provider is idle.
	// For immediate, returns nil without blocking.
	//
	// Gating BEFORE the ack is correct: the client's OpenStream
	// blocks until we ack, and batch/background clients expect
	// latency. Context cancellation (client disconnect) triggers
	// the pre-ack error path.
	if err := ms.latencyRouter.GateComplete(
		ctx, policy,
		resolution.ProviderName, resolution.ProviderModel,
		resolution.Provider.MaxBatchSize,
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

	// Forward the request to the provider. When a continuation is
	// active, providerMessages includes the prepended history.
	completionStream, err := provider.Complete(ctx, &modelprovider.CompleteRequest{
		Model:      resolution.ProviderModel,
		Messages:   providerMessages,
		Stream:     request.Stream,
		Credential: credential,
	})
	if err != nil {
		stream.Send(model.Response{
			Type:  model.ResponseError,
			Error: fmt.Sprintf("provider error: %v", err),
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
	ms.emitUsageTelemetry(token, request.Model, resolution, account, finalUsage, finalModel, cost, latency)
}

// resolveModel resolves a model alias (or "auto") to a Resolution.
func (ms *ModelService) resolveModel(alias string) (modelregistry.Resolution, error) {
	if alias == "auto" {
		return ms.registry.ResolveAuto(nil)
	}
	return ms.registry.Resolve(alias)
}

// lookupCredential returns the API key for the given account. Returns
// an empty string if no credential is needed (local providers) or if
// the credential is missing (which will cause a provider-side auth
// failure with a clear error).
func (ms *ModelService) lookupCredential(resolution modelregistry.Resolution, account modelregistry.AccountSelection) string {
	if resolution.Provider.AuthMethod == model.AuthMethodNone {
		return ""
	}
	if account.CredentialRef == "" {
		return ""
	}
	return ms.credentials.Get(account.CredentialRef)
}

// emitUsageTelemetry records a telemetry span for a completed model
// request. No-op when the telemetry emitter is nil.
func (ms *ModelService) emitUsageTelemetry(
	token *servicetoken.Token,
	requestedAlias string,
	resolution modelregistry.Resolution,
	account modelregistry.AccountSelection,
	usage *model.Usage,
	providerModel string,
	costMicrodollars int64,
	latency time.Duration,
) {
	if ms.telemetry == nil {
		return
	}

	var inputTokens, outputTokens int64
	if usage != nil {
		inputTokens = usage.InputTokens
		outputTokens = usage.OutputTokens
	}

	ms.telemetry.RecordSpan(telemetry.Span{
		TraceID:   telemetry.NewTraceID(),
		SpanID:    telemetry.NewSpanID(),
		Operation: "model.complete",
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
			"output_tokens":     outputTokens,
			"cost_microdollars": costMicrodollars,
		},
	})
}
