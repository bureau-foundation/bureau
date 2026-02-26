// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agentdriver

import (
	"context"
	"fmt"
	"math"

	"github.com/bureau-foundation/bureau/lib/schema/agent"
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
func (c *AgentServiceClient) GetSession(ctx context.Context, principalLocal string) (*agent.AgentSessionContent, error) {
	fields := make(map[string]any, 1)
	if principalLocal != "" {
		fields["principal_local"] = principalLocal
	}

	var response struct {
		Session *agent.AgentSessionContent `cbor:"session"`
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

// StoreSessionLogResponse holds the artifact ref returned after the
// agent service stores the session log in the artifact service.
type StoreSessionLogResponse struct {
	Ref string `cbor:"ref"`
}

// StoreSessionLog uploads raw session log bytes through the agent
// service, which stores them in the artifact service and returns
// the content-addressed ref. The session must be active — the agent
// service validates the session ID to prevent orphaned artifacts.
func (c *AgentServiceClient) StoreSessionLog(ctx context.Context, sessionID string, data []byte) (*StoreSessionLogResponse, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("data is required")
	}
	var response StoreSessionLogResponse
	if err := c.client.Call(ctx, "store-session-log", map[string]any{
		"session_id": sessionID,
		"data":       data,
	}, &response); err != nil {
		return nil, err
	}
	return &response, nil
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
func (c *AgentServiceClient) GetContext(ctx context.Context, key, principalLocal string) (*agent.ContextEntry, error) {
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
		Entry *agent.ContextEntry `cbor:"entry"`
	}
	if err := c.client.Call(ctx, "get-context", fields, &response); err != nil {
		return nil, err
	}
	return response.Entry, nil
}

// ListContext returns all context entries matching the given prefix.
// If prefix is empty, all entries are returned. If principalLocal is
// empty, the server defaults to the caller.
func (c *AgentServiceClient) ListContext(ctx context.Context, prefix, principalLocal string) (map[string]agent.ContextEntry, error) {
	fields := make(map[string]any, 2)
	if prefix != "" {
		fields["prefix"] = prefix
	}
	if principalLocal != "" {
		fields["principal_local"] = principalLocal
	}

	var response struct {
		Entries map[string]agent.ContextEntry `cbor:"entries"`
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

// --- Context commit actions ---

// CheckpointContextRequest holds the parameters for creating a context
// commit. The request may include inline Data (CBOR checkpoint bytes)
// for the agent service to store as an artifact, or a pre-stored
// ArtifactRef. When Data is provided, the agent service stores it in
// the artifact service and fills in the ArtifactRef. When ArtifactRef
// is provided directly, the caller has already stored the artifact
// (write-through ordering). Exactly one of Data or ArtifactRef must
// be set.
//
// Server-side fields (Principal, Machine, CreatedAt) are derived from
// the service token and clock. The caller does not supply these.
type CheckpointContextRequest struct {
	// Parent is the ctx-* ID of the parent commit. Empty for root
	// commits (start of a new conversation).
	Parent string

	// CommitType describes the artifact's role: "delta" (new entries
	// since parent), "compaction" (summary replacing prefix chain),
	// or "snapshot" (full materialized context). Required.
	CommitType string

	// Data is the raw checkpoint bytes (e.g., CBOR-encoded events)
	// for the agent service to store as an artifact. When provided,
	// the agent service stores the data in the artifact service and
	// derives ArtifactRef from the resulting BLAKE3 hash. Mutually
	// exclusive with ArtifactRef.
	Data []byte

	// ArtifactRef is the BLAKE3 content-addressed ref of a
	// pre-stored artifact. Mutually exclusive with Data.
	ArtifactRef string

	// Format is the serialization format of the artifact (e.g.,
	// "bureau-agent-v1", "events-v1"). Required.
	Format string

	// Template is the agent template identifier. Optional.
	Template string

	// SessionID identifies the session that produced this commit.
	SessionID string

	// Checkpoint describes what triggered the commit: "turn_boundary",
	// "tool_call", "compaction", "session_end", "explicit". Required.
	Checkpoint string

	// TicketID is the ticket being worked when this commit was created.
	TicketID string

	// ThreadID is the Matrix event ID of the thread this commit is
	// part of.
	ThreadID string

	// Summary is an optional human-readable description.
	Summary string

	// MessageCount is the number of messages in the delta artifact.
	MessageCount int

	// TokenCount is the approximate token count of the delta content.
	TokenCount int64
}

// CheckpointContextResponse is the response from a checkpoint-context
// call. Contains the deterministic ctx-* identifier for the new commit.
type CheckpointContextResponse struct {
	ID string `cbor:"id"`
}

// CheckpointContext creates a new context commit in the agent's
// understanding chain. Returns the deterministic ctx-* identifier
// for the new commit.
//
// When request.Data is set, the agent service stores it as an artifact
// and derives ArtifactRef. When request.ArtifactRef is set, the
// caller has already stored the artifact. Exactly one must be provided.
func (c *AgentServiceClient) CheckpointContext(ctx context.Context, request CheckpointContextRequest) (*CheckpointContextResponse, error) {
	if request.CommitType == "" {
		return nil, fmt.Errorf("commit_type is required")
	}
	hasData := len(request.Data) > 0
	hasRef := request.ArtifactRef != ""
	if !hasData && !hasRef {
		return nil, fmt.Errorf("exactly one of data or artifact_ref is required")
	}
	if hasData && hasRef {
		return nil, fmt.Errorf("data and artifact_ref are mutually exclusive")
	}
	if request.Format == "" {
		return nil, fmt.Errorf("format is required")
	}
	if request.Checkpoint == "" {
		return nil, fmt.Errorf("checkpoint is required")
	}

	fields := map[string]any{
		"commit_type": request.CommitType,
		"format":      request.Format,
		"checkpoint":  request.Checkpoint,
	}
	if hasData {
		fields["data"] = request.Data
	}
	if hasRef {
		fields["artifact_ref"] = request.ArtifactRef
	}
	if request.Parent != "" {
		fields["parent"] = request.Parent
	}
	if request.Template != "" {
		fields["template"] = request.Template
	}
	if request.SessionID != "" {
		fields["session_id"] = request.SessionID
	}
	if request.TicketID != "" {
		fields["ticket_id"] = request.TicketID
	}
	if request.ThreadID != "" {
		fields["thread_id"] = request.ThreadID
	}
	if request.Summary != "" {
		fields["summary"] = request.Summary
	}
	if request.MessageCount != 0 {
		fields["message_count"] = request.MessageCount
	}
	if request.TokenCount != 0 {
		fields["token_count"] = request.TokenCount
	}

	var response CheckpointContextResponse
	if err := c.client.Call(ctx, "checkpoint-context", fields, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// --- Metrics actions ---

// GetMetrics returns the aggregated metrics for a principal. If
// principalLocal is empty, the server defaults to the caller. Returns
// nil if no metrics exist.
func (c *AgentServiceClient) GetMetrics(ctx context.Context, principalLocal string) (*agent.AgentMetricsContent, error) {
	fields := make(map[string]any, 1)
	if principalLocal != "" {
		fields["principal_local"] = principalLocal
	}

	var response struct {
		Metrics *agent.AgentMetricsContent `cbor:"metrics"`
	}
	if err := c.client.Call(ctx, "get-metrics", fields, &response); err != nil {
		return nil, err
	}
	return response.Metrics, nil
}
