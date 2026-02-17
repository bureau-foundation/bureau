// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"math"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
)

// Default sandbox paths for the agent service socket and token.
const (
	DefaultAgentServiceSocketPath = "/run/bureau/service/agent.sock"
	DefaultAgentServiceTokenPath  = "/run/bureau/service/token/agent.token"
)

// AgentServiceClient provides typed access to the agent service over
// its Unix domain socket. Each method maps to a single socket action.
// The client handles CBOR encoding/decoding and token authentication
// via the underlying service.ServiceClient.
type AgentServiceClient struct {
	client *service.ServiceClient
}

// NewAgentServiceClient creates a client that authenticates using the
// token at tokenPath. Returns an error if the socket path is empty or
// the token cannot be read.
func NewAgentServiceClient(socketPath, tokenPath string) (*AgentServiceClient, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("agent service socket path is required")
	}
	client, err := service.NewServiceClient(socketPath, tokenPath)
	if err != nil {
		return nil, fmt.Errorf("creating agent service client: %w", err)
	}
	return &AgentServiceClient{client: client}, nil
}

// NewAgentServiceClientFromToken creates a client with pre-loaded token
// bytes, useful in tests or when the caller manages token lifecycle.
func NewAgentServiceClientFromToken(socketPath string, tokenBytes []byte) *AgentServiceClient {
	return &AgentServiceClient{
		client: service.NewServiceClientFromToken(socketPath, tokenBytes),
	}
}

// --- Session actions ---

// GetSession returns the current session state for a principal.
// If principalLocal is empty, the server defaults to the caller's
// identity. Returns nil session if no session state exists.
func (c *AgentServiceClient) GetSession(ctx context.Context, principalLocal string) (*schema.AgentSessionContent, error) {
	fields := make(map[string]any, 1)
	if principalLocal != "" {
		fields["principal_local"] = principalLocal
	}

	var response struct {
		Session *schema.AgentSessionContent `cbor:"session"`
	}
	if err := c.client.Call(ctx, "get-session", fields, &response); err != nil {
		return nil, err
	}
	return response.Session, nil
}

// StartSession begins a new session for the caller. The sessionID must
// be unique. Returns an error if a session is already active.
func (c *AgentServiceClient) StartSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("session_id is required")
	}
	return c.client.Call(ctx, "start-session", map[string]any{
		"session_id": sessionID,
	}, nil)
}

// EndSessionRequest holds the parameters for ending a session.
type EndSessionRequest struct {
	SessionID             string
	SessionLogArtifactRef string
	InputTokens           int64
	OutputTokens          int64
	CacheReadTokens       int64
	CacheWriteTokens      int64
	CostUSD               float64
	ToolCalls             int64
	Turns                 int64
	Errors                int64
	DurationSeconds       int64
}

// EndSessionRequestFromSummary constructs an EndSessionRequest from a
// SessionSummary and session metadata. This is the standard conversion
// used at the end of Run() to report session results.
func EndSessionRequestFromSummary(sessionID string, summary SessionSummary, sessionLogArtifactRef string) EndSessionRequest {
	return EndSessionRequest{
		SessionID:             sessionID,
		SessionLogArtifactRef: sessionLogArtifactRef,
		InputTokens:           summary.InputTokens,
		OutputTokens:          summary.OutputTokens,
		CacheReadTokens:       summary.CacheReadTokens,
		CacheWriteTokens:      summary.CacheWriteTokens,
		CostUSD:               summary.CostUSD,
		ToolCalls:             summary.ToolCallCount,
		Turns:                 summary.TurnCount,
		Errors:                summary.ErrorCount,
		DurationSeconds:       int64(math.Round(summary.Duration.Seconds())),
	}
}

// EndSession ends the active session, recording metrics and an optional
// session log artifact ref. Returns an error if no matching session is
// active.
func (c *AgentServiceClient) EndSession(ctx context.Context, request EndSessionRequest) error {
	if request.SessionID == "" {
		return fmt.Errorf("session_id is required")
	}
	fields := map[string]any{
		"session_id":         request.SessionID,
		"input_tokens":       request.InputTokens,
		"output_tokens":      request.OutputTokens,
		"cache_read_tokens":  request.CacheReadTokens,
		"cache_write_tokens": request.CacheWriteTokens,
		"cost_usd":           request.CostUSD,
		"tool_calls":         request.ToolCalls,
		"turns":              request.Turns,
		"errors":             request.Errors,
		"duration_seconds":   request.DurationSeconds,
	}
	if request.SessionLogArtifactRef != "" {
		fields["session_log_artifact_ref"] = request.SessionLogArtifactRef
	}
	return c.client.Call(ctx, "end-session", fields, nil)
}

// --- Context actions ---

// SetContextRequest holds the parameters for setting a context entry.
type SetContextRequest struct {
	Key          string
	ArtifactRef  string
	Size         int64
	ContentType  string
	SessionID    string
	MessageCount int
	TokenCount   int64
}

// SetContext creates or updates a context entry for the caller. The
// artifact ref should point to content already stored in the artifact
// service (write-through ordering).
func (c *AgentServiceClient) SetContext(ctx context.Context, request SetContextRequest) error {
	if request.Key == "" {
		return fmt.Errorf("key is required")
	}
	if request.ArtifactRef == "" {
		return fmt.Errorf("artifact_ref is required")
	}
	if request.ContentType == "" {
		return fmt.Errorf("content_type is required")
	}
	fields := map[string]any{
		"key":          request.Key,
		"artifact_ref": request.ArtifactRef,
		"size":         request.Size,
		"content_type": request.ContentType,
	}
	if request.SessionID != "" {
		fields["session_id"] = request.SessionID
	}
	if request.MessageCount != 0 {
		fields["message_count"] = request.MessageCount
	}
	if request.TokenCount != 0 {
		fields["token_count"] = request.TokenCount
	}
	return c.client.Call(ctx, "set-context", fields, nil)
}

// GetContext returns a single context entry by key. If principalLocal
// is empty, the server defaults to the caller. Returns nil if the key
// does not exist.
func (c *AgentServiceClient) GetContext(ctx context.Context, key, principalLocal string) (*schema.ContextEntry, error) {
	if key == "" {
		return nil, fmt.Errorf("key is required")
	}
	fields := map[string]any{
		"key": key,
	}
	if principalLocal != "" {
		fields["principal_local"] = principalLocal
	}

	var response struct {
		Entry *schema.ContextEntry `cbor:"entry"`
	}
	if err := c.client.Call(ctx, "get-context", fields, &response); err != nil {
		return nil, err
	}
	return response.Entry, nil
}

// ListContext returns all context entries matching the given prefix.
// If prefix is empty, all entries are returned. If principalLocal is
// empty, the server defaults to the caller.
func (c *AgentServiceClient) ListContext(ctx context.Context, prefix, principalLocal string) (map[string]schema.ContextEntry, error) {
	fields := make(map[string]any, 2)
	if prefix != "" {
		fields["prefix"] = prefix
	}
	if principalLocal != "" {
		fields["principal_local"] = principalLocal
	}

	var response struct {
		Entries map[string]schema.ContextEntry `cbor:"entries"`
	}
	if err := c.client.Call(ctx, "list-context", fields, &response); err != nil {
		return nil, err
	}
	return response.Entries, nil
}

// DeleteContext removes a context entry by key for the caller.
func (c *AgentServiceClient) DeleteContext(ctx context.Context, key string) error {
	if key == "" {
		return fmt.Errorf("key is required")
	}
	return c.client.Call(ctx, "delete-context", map[string]any{
		"key": key,
	}, nil)
}

// --- Metrics actions ---

// GetMetrics returns the aggregated metrics for a principal. If
// principalLocal is empty, the server defaults to the caller. Returns
// nil if no metrics exist.
func (c *AgentServiceClient) GetMetrics(ctx context.Context, principalLocal string) (*schema.AgentMetricsContent, error) {
	fields := make(map[string]any, 1)
	if principalLocal != "" {
		fields["principal_local"] = principalLocal
	}

	var response struct {
		Metrics *schema.AgentMetricsContent `cbor:"metrics"`
	}
	if err := c.client.Call(ctx, "get-metrics", fields, &response); err != nil {
		return nil, err
	}
	return response.Metrics, nil
}
