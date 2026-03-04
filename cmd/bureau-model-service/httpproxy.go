// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/modelregistry"
	"github.com/bureau-foundation/bureau/lib/netutil"
	"github.com/bureau-foundation/bureau/lib/schema/model"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// httpProxy handles HTTP requests from standard AI SDKs (OpenAI,
// Anthropic) and forwards them to upstream providers with credential
// injection and usage tracking.
//
// The proxy is transparent: it forwards requests as-is without parsing
// the body (except to extract the model field for alias routing). The
// path prefix determines the wire protocol; the model alias determines
// the upstream provider within that protocol family.
//
// Request flow:
//  1. Extract provider prefix from URL path (e.g., /openai/v1/... → "openai")
//  2. Authenticate the Bureau service token from the request auth header
//  3. Parse the model field from the request body for alias resolution
//  4. Resolve the alias to a concrete provider and account
//  5. Inject the real API credential and forward to the upstream
//  6. Stream the response back, extracting usage for cost tracking
type httpProxy struct {
	modelService *ModelService
	auth         *service.AuthConfig
	logger       *slog.Logger
	client       *http.Client // direct HTTP for unauthenticated providers
	proxyClient  *http.Client // routes through proxy socket for credential injection
}

// newHTTPProxy creates an HTTP proxy backed by the model service's
// registry. Outbound requests route through the Bureau proxy socket
// (when configured) for credential injection. The auth config is
// shared with the CBOR socket server — both use the same token
// verification.
func newHTTPProxy(modelService *ModelService, auth *service.AuthConfig) *httpProxy {
	directTransport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
	}

	directClient := &http.Client{
		Timeout:   0,
		Transport: directTransport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	proxyClient := directClient
	if modelService.proxySocketPath != "" {
		socketPath := modelService.proxySocketPath
		proxyTransport := &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  false,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		}
		proxyClient = &http.Client{
			Timeout:   0,
			Transport: proxyTransport,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	return &httpProxy{
		modelService: modelService,
		auth:         auth,
		logger:       modelService.logger,
		client:       directClient,
		proxyClient:  proxyClient,
	}
}

// ServeHTTP dispatches incoming requests by path prefix.
func (proxy *httpProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	startTime := proxy.modelService.clock.Now()
	traceID := telemetry.NewTraceID()
	spanID := telemetry.NewSpanID()

	// Extract provider prefix: /openai/v1/chat/completions → ("openai", "/v1/chat/completions").
	providerName, remainingPath, err := extractProviderPrefix(request.URL.Path)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	// Look up the provider in the registry.
	providerConfig, ok := proxy.modelService.registry.GetProvider(providerName)
	if !ok {
		http.Error(writer, fmt.Sprintf("unknown provider %q", providerName), http.StatusNotFound)
		return
	}

	// Authenticate the Bureau service token.
	token, err := proxy.extractAndVerifyToken(request, providerConfig)
	if err != nil {
		proxy.logger.Debug("http proxy auth failed",
			"provider", providerName,
			"error", err,
		)
		http.Error(writer, "authentication failed", http.StatusUnauthorized)
		return
	}

	project := token.Project
	if project == "" {
		http.Error(writer, "service token missing project identity", http.StatusForbidden)
		return
	}

	// Read the request body so we can extract the model field for alias
	// resolution and then replay it for the upstream request.
	requestBody, err := io.ReadAll(io.LimitReader(request.Body, 10*1024*1024)) // 10 MB limit
	if err != nil {
		http.Error(writer, "failed to read request body", http.StatusBadRequest)
		return
	}

	// Extract model alias from the request body. If the body has a
	// "model" field, resolve it. If not (e.g., non-completion endpoints
	// like /v1/models), skip alias resolution and route to the provider
	// named by the path prefix.
	resolvedProvider := providerName
	var resolution *modelregistry.Resolution
	modelName := extractModelField(requestBody)
	if modelName != "" {
		resolved, resolveError := proxy.modelService.resolveModel(modelName)
		if resolveError != nil {
			http.Error(writer, fmt.Sprintf("model resolution failed: %v", resolveError), http.StatusBadRequest)
			return
		}

		// The resolved provider must match the path prefix's protocol.
		// The path prefix says "I speak this wire format," so the
		// resolved provider must accept that format. For now, verify
		// the provider exists and use it.
		resolvedProvider = resolved.ProviderName
		resolution = &resolved

		resolvedProviderConfig, resolvedOK := proxy.modelService.registry.GetProvider(resolvedProvider)
		if !resolvedOK {
			http.Error(writer, fmt.Sprintf("resolved provider %q not found in registry", resolvedProvider), http.StatusBadGateway)
			return
		}
		providerConfig = resolvedProviderConfig
	}

	// Authorize the request. Requests with a model field get a
	// target-scoped grant check (action + model alias). Requests
	// without a model field (e.g., GET /v1/models) get an action-
	// only check. The HTTP proxy uses model/complete as the action
	// for all forwarded requests since it proxies inference calls.
	if resolution != nil {
		if err := requireModelGrant(token, model.ActionComplete, resolution.Alias); err != nil {
			http.Error(writer, err.Error(), http.StatusForbidden)
			return
		}
	} else {
		if err := requireActionGrant(token, model.ActionList); err != nil {
			http.Error(writer, err.Error(), http.StatusForbidden)
			return
		}
	}

	// Select the account for this (project, provider) pair.
	account, err := proxy.modelService.registry.SelectAccount(project, resolvedProvider)
	if err != nil {
		http.Error(writer, fmt.Sprintf("no account for project %q, provider %q", project, resolvedProvider), http.StatusForbidden)
		return
	}

	// Check quota before forwarding.
	if err := proxy.modelService.quotaTracker.Check(account.AccountName, account.Quota); err != nil {
		var quotaError *modelregistry.QuotaExceededError
		if errors.As(err, &quotaError) {
			http.Error(writer, fmt.Sprintf("quota exceeded: %s limit for account %q (resets %s)",
				quotaError.Window, quotaError.AccountName,
				quotaError.ResetsAt.Format(time.RFC3339)), http.StatusTooManyRequests)
			return
		}
		http.Error(writer, fmt.Sprintf("quota check failed: %v", err), http.StatusInternalServerError)
		return
	}

	// Build the upstream URL. When a proxy socket is configured,
	// route through the proxy's /http/<provider>/ prefix — the proxy
	// handles credential injection. Otherwise fall through to the
	// provider's endpoint directly (dev/test without proxy).
	var upstreamURL string
	if proxy.modelService.proxySocketPath != "" && providerConfig.AuthMethod != model.AuthMethodNone {
		upstreamURL = "http://localhost/http/" + resolvedProvider + remainingPath
		if request.URL.RawQuery != "" {
			upstreamURL += "?" + request.URL.RawQuery
		}
	} else {
		var buildError error
		upstreamURL, buildError = buildUpstreamURL(providerConfig.Endpoint, remainingPath, request.URL.RawQuery)
		if buildError != nil {
			http.Error(writer, fmt.Sprintf("invalid upstream URL: %v", buildError), http.StatusBadGateway)
			return
		}
	}

	// Create the upstream request.
	upstreamRequest, err := http.NewRequestWithContext(request.Context(), request.Method, upstreamURL, bytes.NewReader(requestBody))
	if err != nil {
		http.Error(writer, "failed to create upstream request", http.StatusInternalServerError)
		return
	}

	// Copy headers from the original request, skipping hop-by-hop and
	// auth headers (the proxy injects the real credential).
	for key, values := range request.Header {
		if isHopByHopHeader(key) {
			continue
		}
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "x-api-key") {
			continue
		}
		for _, value := range values {
			upstreamRequest.Header.Add(key, value)
		}
	}

	// Inject traceparent for distributed tracing.
	upstreamRequest.Header.Set("Traceparent", formatTraceparent(traceID, spanID))

	proxy.logger.Info("http proxy request",
		"provider", resolvedProvider,
		"method", request.Method,
		"path", remainingPath,
		"upstream", upstreamURL,
		"subject", token.Subject,
		"trace_id", traceID.String(),
	)

	// Select the HTTP client: proxy-routed for authenticated providers,
	// direct for unauthenticated (local) providers.
	httpClient := proxy.client
	if providerConfig.AuthMethod != model.AuthMethodNone {
		httpClient = proxy.proxyClient
	}

	// Forward the request.
	response, err := httpClient.Do(upstreamRequest)
	if err != nil {
		proxy.logger.Error("upstream request failed",
			"provider", resolvedProvider,
			"error", err,
		)
		http.Error(writer, fmt.Sprintf("upstream request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()

	// Copy response headers.
	for key, values := range response.Header {
		if isHopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}

	// Determine if this is an SSE stream.
	isSSE := strings.Contains(response.Header.Get("Content-Type"), "text/event-stream")

	var extractedUsage *usageData
	if isSSE {
		extractedUsage = proxy.streamSSEResponse(writer, response, startTime, resolvedProvider, token)
	} else {
		extractedUsage = proxy.forwardResponse(writer, response, startTime, resolvedProvider)
	}

	// Record cost and emit telemetry.
	duration := proxy.modelService.clock.Now().Sub(startTime)
	proxy.recordUsage(token, resolvedProvider, resolution, account, extractedUsage, duration, traceID, spanID)
}

// usageData holds token counts extracted from a provider response.
type usageData struct {
	InputTokens  int64
	OutputTokens int64
	Model        string
}

// forwardResponse handles non-streaming HTTP responses. Buffers the
// body to extract usage, then forwards to the client.
func (proxy *httpProxy) forwardResponse(writer http.ResponseWriter, response *http.Response, startTime time.Time, providerName string) *usageData {
	// Read the full response for usage extraction. Limit to 100 MB
	// to prevent unbounded memory growth on pathological responses.
	body, err := io.ReadAll(io.LimitReader(response.Body, 100*1024*1024))
	if err != nil && !netutil.IsExpectedCloseError(err) {
		proxy.logger.Warn("error reading upstream response",
			"provider", providerName,
			"error", err,
		)
	}

	writer.WriteHeader(response.StatusCode)
	if _, writeErr := writer.Write(body); writeErr != nil && !netutil.IsExpectedCloseError(writeErr) {
		proxy.logger.Debug("client write failed",
			"provider", providerName,
			"error", writeErr,
		)
	}

	duration := proxy.modelService.clock.Now().Sub(startTime)
	proxy.logger.Info("http proxy complete",
		"provider", providerName,
		"status", response.StatusCode,
		"bytes", len(body),
		"duration", duration,
	)

	// Extract usage from JSON response body.
	return extractUsageFromJSON(body)
}

// streamSSEResponse handles streaming SSE responses. Forwards each
// chunk to the client with per-read flushing while scanning for
// usage data in SSE events.
func (proxy *httpProxy) streamSSEResponse(writer http.ResponseWriter, response *http.Response, startTime time.Time, providerName string, token *servicetoken.Token) *usageData {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		proxy.logger.Error("streaming not supported by response writer")
		http.Error(writer, "streaming not supported", http.StatusInternalServerError)
		return nil
	}

	// Set SSE headers.
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(response.StatusCode)
	flusher.Flush()

	// Stream the response through a usage scanner. The scanner
	// inspects each chunk for SSE data lines containing usage
	// information. The response bytes flow through to the client
	// unchanged.
	scanner := &usageScanner{}
	buffer := make([]byte, 4096)
	var totalBytes int64

	for {
		bytesRead, readErr := response.Body.Read(buffer)
		if bytesRead > 0 {
			// Feed the scanner for usage extraction.
			scanner.Write(buffer[:bytesRead])

			// Forward to client.
			written, writeErr := writer.Write(buffer[:bytesRead])
			if writeErr != nil {
				proxy.logger.Debug("client disconnected during SSE stream",
					"provider", providerName,
					"subject", token.Subject,
					"bytes_sent", totalBytes,
				)
				return scanner.usage()
			}
			totalBytes += int64(written)
			flusher.Flush()
		}
		if readErr != nil {
			if readErr != io.EOF {
				proxy.logger.Warn("upstream error during SSE stream",
					"provider", providerName,
					"error", readErr,
					"bytes_sent", totalBytes,
				)
			}
			break
		}
	}

	duration := proxy.modelService.clock.Now().Sub(startTime)
	proxy.logger.Info("http proxy SSE complete",
		"provider", providerName,
		"status", response.StatusCode,
		"bytes", totalBytes,
		"duration", duration,
	)

	return scanner.usage()
}

// recordUsage records cost and emits telemetry for a completed HTTP
// proxy request.
func (proxy *httpProxy) recordUsage(
	token *servicetoken.Token,
	providerName string,
	resolution *modelregistry.Resolution,
	account modelregistry.AccountSelection,
	usage *usageData,
	latency time.Duration,
	traceID telemetry.TraceID,
	spanID telemetry.SpanID,
) {
	if usage == nil {
		return
	}

	// Find pricing for cost calculation. If we resolved an alias,
	// use its pricing. Otherwise we don't have pricing info and
	// can only record the raw token counts.
	var cost int64
	if resolution != nil {
		modelUsage := &model.Usage{
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
		}
		cost = calculateCost(modelUsage, resolution.Pricing)
		proxy.modelService.quotaTracker.Record(account.AccountName, cost)
	}

	if proxy.modelService.telemetry == nil {
		return
	}

	attributes := map[string]any{
		"project":           token.Project,
		"agent":             token.Subject.String(),
		"provider":          providerName,
		"input_tokens":      usage.InputTokens,
		"output_tokens":     usage.OutputTokens,
		"cost_microdollars": cost,
		"interface":         "http",
	}
	if usage.Model != "" {
		attributes["provider_model"] = usage.Model
	}
	if resolution != nil {
		attributes["model_alias"] = resolution.Alias
	}

	proxy.modelService.telemetry.RecordSpan(telemetry.Span{
		TraceID:    traceID,
		SpanID:     spanID,
		Operation:  "model.http_proxy",
		StartTime:  proxy.modelService.clock.Now().Add(-latency).UnixNano(),
		Duration:   latency.Nanoseconds(),
		Status:     telemetry.SpanStatusOK,
		Attributes: attributes,
	})
}

// --- Token extraction and verification ---

// extractAndVerifyToken extracts the Bureau service token from the
// HTTP request's auth header, base64-decodes it, and verifies
// signature, expiry, audience, and blacklist.
//
// The token is transmitted as base64 in the header because service
// tokens are binary (CBOR payload + Ed25519 signature). The daemon
// writes the base64-encoded token to the agent's token file; the
// agent sets it as the SDK API key; the SDK sends it in the auth
// header; we decode and verify it here.
func (proxy *httpProxy) extractAndVerifyToken(request *http.Request, _ model.ModelProviderContent) (*servicetoken.Token, error) {
	headerValue := request.Header.Get("Authorization")
	if headerValue == "" {
		return nil, fmt.Errorf("missing Authorization header")
	}

	// Strip "Bearer " prefix (Authorization header convention).
	tokenString := strings.TrimPrefix(headerValue, "Bearer ")
	tokenString = strings.TrimPrefix(tokenString, "bearer ")

	// Base64-decode the token.
	tokenBytes, err := base64.StdEncoding.DecodeString(tokenString)
	if err != nil {
		// Try URL-safe base64 as a fallback.
		tokenBytes, err = base64.URLEncoding.DecodeString(tokenString)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 token: %w", err)
		}
	}

	// Verify signature, expiry, audience.
	token, err := servicetoken.VerifyForServiceAt(
		proxy.auth.PublicKey,
		tokenBytes,
		proxy.auth.Audience,
		proxy.auth.Clock.Now(),
	)
	if err != nil {
		return nil, err
	}

	// Check blacklist.
	if proxy.auth.Blacklist.IsRevoked(token.ID) {
		return nil, fmt.Errorf("token revoked")
	}

	return token, nil
}

// --- Path parsing and URL construction ---

// extractProviderPrefix splits the request path into provider name
// and remaining path. The first path segment is the provider name:
//
//	/openai/v1/chat/completions → ("openai", "/v1/chat/completions")
//	/anthropic/v1/messages      → ("anthropic", "/v1/messages")
func extractProviderPrefix(path string) (string, string, error) {
	// Trim leading slash, split on first slash.
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return "", "", fmt.Errorf("empty request path")
	}

	providerName, remaining, hasRemaining := strings.Cut(trimmed, "/")
	if providerName == "" {
		return "", "", fmt.Errorf("empty provider prefix in path %q", path)
	}

	if !hasRemaining {
		return providerName, "/", nil
	}
	return providerName, "/" + remaining, nil
}

// buildUpstreamURL constructs the full upstream URL by joining the
// provider endpoint with the remaining request path. Uses string
// concatenation (not url.URL.String()) to avoid double-encoding
// percent-encoded segments — the same approach as proxy/http_service.go.
func buildUpstreamURL(endpoint, remainingPath, rawQuery string) (string, error) {
	// Validate the endpoint parses as a URL.
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint %q: %w", endpoint, err)
	}

	// Join endpoint path and remaining path with a single slash.
	base := strings.TrimRight(parsed.String(), "/")
	result := base + remainingPath

	if rawQuery != "" {
		result += "?" + rawQuery
	}
	return result, nil
}

// --- Model field extraction ---

// extractModelField extracts the "model" field from a JSON request
// body. Returns empty string if the body is not JSON, has no model
// field, or the model field is not a string. This is intentionally
// lenient — not all endpoints have a model field (e.g., /v1/models).
func extractModelField(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	var partial struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &partial); err != nil {
		return ""
	}
	return partial.Model
}

// --- Usage extraction ---

// extractUsageFromJSON extracts token usage from a JSON response body.
// Handles both OpenAI format (prompt_tokens/completion_tokens) and
// Anthropic format (input_tokens/output_tokens). Returns nil if no
// usage data is found.
func extractUsageFromJSON(body []byte) *usageData {
	if len(body) == 0 {
		return nil
	}

	var response struct {
		Model string `json:"model"`
		Usage *struct {
			// OpenAI format
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			// Anthropic format
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil
	}
	if response.Usage == nil {
		return nil
	}

	usage := &usageData{Model: response.Model}

	// OpenAI and Anthropic use different field names. Take whichever
	// is non-zero, preferring the more specific (non-zero) value.
	if response.Usage.PromptTokens > 0 {
		usage.InputTokens = response.Usage.PromptTokens
	} else {
		usage.InputTokens = response.Usage.InputTokens
	}

	if response.Usage.CompletionTokens > 0 {
		usage.OutputTokens = response.Usage.CompletionTokens
	} else {
		usage.OutputTokens = response.Usage.OutputTokens
	}

	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		return nil
	}
	return usage
}

// usageScanner inspects SSE byte chunks for usage data. It assembles
// complete lines from the stream and parses "data:" lines that
// contain usage information. Designed to be called from a streaming
// loop where each Write corresponds to a read from the upstream.
type usageScanner struct {
	// partial holds an incomplete line from the previous Write.
	partial []byte

	// lastUsage holds the most recently extracted usage data.
	// For OpenAI, usage appears in a late chunk. For Anthropic,
	// input_tokens and output_tokens appear in separate events,
	// so we accumulate by taking the max of each field.
	lastUsage *usageData
}

// Write processes a chunk of SSE data, scanning for usage information.
// Always returns len(data), nil — usage extraction failures are
// silently ignored since they must not interrupt the response stream.
func (scanner *usageScanner) Write(data []byte) (int, error) {
	// Prepend any partial line from the previous chunk.
	var working []byte
	if len(scanner.partial) > 0 {
		working = make([]byte, len(scanner.partial)+len(data))
		copy(working, scanner.partial)
		copy(working[len(scanner.partial):], data)
		scanner.partial = scanner.partial[:0]
	} else {
		working = data
	}

	for len(working) > 0 {
		newlineIndex := bytes.IndexByte(working, '\n')
		if newlineIndex == -1 {
			// No complete line — save as partial for next chunk.
			scanner.partial = append(scanner.partial[:0], working...)
			break
		}

		line := working[:newlineIndex]
		working = working[newlineIndex+1:]

		// Strip carriage return.
		line = bytes.TrimRight(line, "\r")

		// Only inspect "data:" lines.
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}

		payload := bytes.TrimPrefix(line, []byte("data:"))
		payload = bytes.TrimPrefix(payload, []byte(" "))

		// Skip [DONE] sentinel and empty payloads.
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}

		scanner.tryExtractUsage(payload)
	}

	return len(data), nil
}

// usageFields holds the token count fields found in a provider
// response. Supports both OpenAI naming (prompt_tokens) and
// Anthropic naming (input_tokens).
type usageFields struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
}

// tryExtractUsage attempts to parse usage data from a single SSE data
// payload. Updates lastUsage if usage fields are found.
//
// Handles two layout patterns:
//   - OpenAI: {"model":"...", "usage": {"prompt_tokens":N, ...}}
//   - Anthropic message_start: {"message": {"model":"...", "usage": {"input_tokens":N, ...}}}
//   - Anthropic message_delta: {"usage": {"output_tokens":N}}
func (scanner *usageScanner) tryExtractUsage(payload []byte) {
	// Quick check: skip payloads that don't mention "usage".
	if !bytes.Contains(payload, []byte(`"usage"`)) {
		return
	}

	// Try the common flat layout first (OpenAI, Anthropic message_delta).
	var flat struct {
		Model string       `json:"model"`
		Usage *usageFields `json:"usage"`
	}
	if err := json.Unmarshal(payload, &flat); err != nil {
		return
	}

	if flat.Usage != nil {
		scanner.accumulateUsage(flat.Model, flat.Usage)
		return
	}

	// Try the Anthropic message_start layout where usage is nested
	// under a "message" wrapper.
	var nested struct {
		Message *struct {
			Model string       `json:"model"`
			Usage *usageFields `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(payload, &nested); err != nil {
		return
	}
	if nested.Message != nil && nested.Message.Usage != nil {
		scanner.accumulateUsage(nested.Message.Model, nested.Message.Usage)
	}
}

// accumulateUsage merges a set of usage fields into the scanner's
// running totals. Takes the maximum of each field across all events
// because Anthropic splits input and output tokens across separate
// SSE events (message_start and message_delta).
func (scanner *usageScanner) accumulateUsage(eventModel string, fields *usageFields) {
	if scanner.lastUsage == nil {
		scanner.lastUsage = &usageData{}
	}

	if eventModel != "" {
		scanner.lastUsage.Model = eventModel
	}

	inputTokens := fields.PromptTokens
	if fields.InputTokens > inputTokens {
		inputTokens = fields.InputTokens
	}
	if inputTokens > scanner.lastUsage.InputTokens {
		scanner.lastUsage.InputTokens = inputTokens
	}

	outputTokens := fields.CompletionTokens
	if fields.OutputTokens > outputTokens {
		outputTokens = fields.OutputTokens
	}
	if outputTokens > scanner.lastUsage.OutputTokens {
		scanner.lastUsage.OutputTokens = outputTokens
	}
}

// usage returns the accumulated usage data, or nil if no usage was
// found in the stream.
func (scanner *usageScanner) usage() *usageData {
	if scanner.lastUsage != nil && scanner.lastUsage.InputTokens == 0 && scanner.lastUsage.OutputTokens == 0 {
		return nil
	}
	return scanner.lastUsage
}

// --- Shared helpers ---

// hopByHopHeaders are HTTP headers that must not be forwarded through
// a proxy per RFC 2616 section 13.5.1.
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

func isHopByHopHeader(name string) bool {
	return hopByHopHeaders[strings.ToLower(name)]
}

// formatTraceparent builds a W3C Trace Context traceparent header.
func formatTraceparent(traceID telemetry.TraceID, spanID telemetry.SpanID) string {
	return fmt.Sprintf("00-%s-%s-01", traceID.String(), spanID.String())
}
