// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema/agent"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/messaging"
)

// registerActions registers socket protocol actions on the server.
func (agentService *AgentService) registerActions(server *service.SocketServer) {
	// Unauthenticated health check.
	server.Handle("status", agentService.handleStatus)

	// Authenticated session actions.
	server.HandleAuth("get-session", agentService.withReadLock(agentService.handleGetSession))
	server.HandleAuth("start-session", agentService.withWriteLock(agentService.handleStartSession))
	server.HandleAuth("end-session", agentService.withWriteLock(agentService.handleEndSession))
	server.HandleAuth("store-session-log", agentService.withWriteLock(agentService.handleStoreSessionLog))

	// Authenticated context actions.
	server.HandleAuth("set-context", agentService.withWriteLock(agentService.handleSetContext))
	server.HandleAuth("get-context", agentService.withReadLock(agentService.handleGetContext))
	server.HandleAuth("delete-context", agentService.withWriteLock(agentService.handleDeleteContext))
	server.HandleAuth("list-context", agentService.withReadLock(agentService.handleListContext))

	// Authenticated context commit actions.
	server.HandleAuth("checkpoint-context", agentService.withWriteLock(agentService.handleCheckpointContext))
	server.HandleAuth("materialize-context", agentService.withReadLock(agentService.handleMaterializeContext))
	server.HandleAuth("show-context-commit", agentService.withReadLock(agentService.handleShowContextCommit))
	server.HandleAuth("history-context", agentService.withReadLock(agentService.handleHistoryContext))
	server.HandleAuth("update-context-metadata", agentService.withWriteLock(agentService.handleUpdateContextMetadata))
	server.HandleAuth("resolve-context", agentService.withWriteLock(agentService.handleResolveContext))

	// Authenticated metrics actions.
	server.HandleAuth("get-metrics", agentService.withReadLock(agentService.handleGetMetrics))
}

// --- Locking wrappers ---

// withReadLock wraps an AuthActionFunc with a read lock acquisition.
func (agentService *AgentService) withReadLock(handler service.AuthActionFunc) service.AuthActionFunc {
	return func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		agentService.mutex.RLock()
		defer agentService.mutex.RUnlock()
		return handler(ctx, token, raw)
	}
}

// withWriteLock wraps an AuthActionFunc with a write lock acquisition.
func (agentService *AgentService) withWriteLock(handler service.AuthActionFunc) service.AuthActionFunc {
	return func(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
		agentService.mutex.Lock()
		defer agentService.mutex.Unlock()
		return handler(ctx, token, raw)
	}
}

// --- Status handler (unauthenticated) ---

type statusResponse struct {
	Status    string `cbor:"status"`
	Principal string `cbor:"principal"`
	Machine   string `cbor:"machine"`
	UptimeMS  int64  `cbor:"uptime_ms"`
}

func (agentService *AgentService) handleStatus(_ context.Context, _ []byte) (any, error) {
	return statusResponse{
		Status:    "running",
		Principal: agentService.principalName,
		Machine:   agentService.machineName,
		UptimeMS:  agentService.clock.Now().Sub(agentService.startedAt).Milliseconds(),
	}, nil
}

// --- Read authorization helper ---

// principalReadRequest is the common wire format for read actions that
// target a principal's state. Both get-session and get-metrics use this
// structure.
type principalReadRequest struct {
	Action         string `cbor:"action"`
	PrincipalLocal string `cbor:"principal_local"`
}

// authorizeRead checks that the caller is authorized to read the target
// principal's data. Self-reads are always allowed. Cross-principal reads
// require an agent/read grant with the target as a grant target.
func authorizeRead(token *servicetoken.Token, principalLocal string) error {
	if principalLocal != token.Subject.Localpart() {
		if !servicetoken.GrantsAllow(token.Grants, "agent/read", principalLocal) {
			return fmt.Errorf("access denied: no agent/read grant for %s", principalLocal)
		}
	}
	return nil
}

// resolvePrincipalForRead unmarshals a principalReadRequest, resolves the
// target principal (defaulting to the caller), and enforces the agent/read
// authorization check for cross-principal access. Use this for handlers
// whose request contains only an optional principal_local field.
func resolvePrincipalForRead(token *servicetoken.Token, raw []byte) (string, error) {
	var request principalReadRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return "", fmt.Errorf("invalid request: %w", err)
	}

	principalLocal := request.PrincipalLocal
	if principalLocal == "" {
		principalLocal = token.Subject.Localpart()
	}

	if err := authorizeRead(token, principalLocal); err != nil {
		return "", err
	}

	return principalLocal, nil
}

// --- Session handlers ---

// getSessionResponse is the wire format for the "get-session" response.
type getSessionResponse struct {
	// Session is the current session state from Matrix. Nil if no
	// agent session state event exists for this principal.
	Session *agent.AgentSessionContent `cbor:"session,omitempty"`
}

func (agentService *AgentService) handleGetSession(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	principalLocal, err := resolvePrincipalForRead(token, raw)
	if err != nil {
		return nil, err
	}

	content, err := agentService.readSessionState(ctx, principalLocal)
	if err != nil {
		return nil, fmt.Errorf("reading session state: %w", err)
	}

	return getSessionResponse{Session: content}, nil
}

// startSessionRequest is the wire format for the "start-session" action.
type startSessionRequest struct {
	Action    string `cbor:"action"`
	SessionID string `cbor:"session_id"`
}

func (agentService *AgentService) handleStartSession(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	var request startSessionRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.SessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	principalLocal := token.Subject.Localpart()

	// Read current state, apply mutation, write back.
	current, err := agentService.readSessionState(ctx, principalLocal)
	if err != nil {
		return nil, fmt.Errorf("reading current session state: %w", err)
	}

	if current == nil {
		current = &agent.AgentSessionContent{Version: agent.AgentSessionVersion}
	}

	if err := current.CanModify(); err != nil {
		return nil, err
	}

	if current.ActiveSessionID != "" {
		return nil, fmt.Errorf("session %s is already active; end it before starting a new one", current.ActiveSessionID)
	}

	current.ActiveSessionID = request.SessionID
	current.ActiveSessionStartedAt = agentService.clock.Now().UTC().Format("2006-01-02T15:04:05Z")

	if _, err := agentService.session.SendStateEvent(
		ctx, agentService.configRoomID,
		agent.EventTypeAgentSession, principalLocal, current,
	); err != nil {
		return nil, fmt.Errorf("writing session state: %w", err)
	}

	agentService.logger.Info("session started",
		"principal", principalLocal,
		"session_id", request.SessionID,
	)

	return nil, nil
}

// endSessionRequest is the wire format for the "end-session" action.
type endSessionRequest struct {
	Action    string `cbor:"action"`
	SessionID string `cbor:"session_id"`

	// SessionLogArtifactRef is the artifact ref for the session's
	// JSONL log, written to the artifact service before this call.
	// Empty if session logging was disabled.
	SessionLogArtifactRef string `cbor:"session_log_artifact_ref,omitempty"`

	// LatestContextCommitID is the ctx-* identifier of the most
	// recent context checkpoint from this session. Stored in the
	// session state event so subsequent sessions can chain from it.
	LatestContextCommitID string `cbor:"latest_context_commit_id,omitempty"`

	// Metrics from the completed session, to be added to the
	// principal's aggregate totals.
	InputTokens      int64   `cbor:"input_tokens"`
	OutputTokens     int64   `cbor:"output_tokens"`
	CacheReadTokens  int64   `cbor:"cache_read_tokens"`
	CacheWriteTokens int64   `cbor:"cache_write_tokens"`
	CostUSD          float64 `cbor:"cost_usd"`
	ToolCalls        int64   `cbor:"tool_calls"`
	Turns            int64   `cbor:"turns"`
	Errors           int64   `cbor:"errors"`
	DurationSeconds  int64   `cbor:"duration_seconds"`
}

func (agentService *AgentService) handleEndSession(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	var request endSessionRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.SessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	principalLocal := token.Subject.Localpart()

	// Update session state.
	sessionContent, err := agentService.readSessionState(ctx, principalLocal)
	if err != nil {
		return nil, fmt.Errorf("reading current session state: %w", err)
	}

	if sessionContent == nil {
		return nil, fmt.Errorf("no session state exists for %s", principalLocal)
	}

	if err := sessionContent.CanModify(); err != nil {
		return nil, err
	}

	if sessionContent.ActiveSessionID != request.SessionID {
		return nil, fmt.Errorf(
			"session mismatch: active session is %q, but end-session was called for %q",
			sessionContent.ActiveSessionID, request.SessionID,
		)
	}

	// Transition: active → latest, clear active.
	sessionContent.ActiveSessionID = ""
	sessionContent.ActiveSessionStartedAt = ""
	sessionContent.LatestSessionID = request.SessionID
	if request.SessionLogArtifactRef != "" {
		sessionContent.LatestSessionArtifactRef = request.SessionLogArtifactRef
	}
	if request.LatestContextCommitID != "" {
		sessionContent.LatestContextCommitID = request.LatestContextCommitID
	}

	if _, err := agentService.session.SendStateEvent(
		ctx, agentService.configRoomID,
		agent.EventTypeAgentSession, principalLocal, sessionContent,
	); err != nil {
		return nil, fmt.Errorf("writing session state: %w", err)
	}

	// Update aggregated metrics.
	metricsContent, err := agentService.readMetricsState(ctx, principalLocal)
	if err != nil {
		return nil, fmt.Errorf("reading current metrics state: %w", err)
	}

	if metricsContent == nil {
		metricsContent = &agent.AgentMetricsContent{Version: agent.AgentMetricsVersion}
	}

	if err := metricsContent.CanModify(); err != nil {
		return nil, err
	}

	// Idempotency: skip if this session's metrics were already applied.
	if metricsContent.LastSessionID == request.SessionID {
		agentService.logger.Info("session metrics already applied, skipping",
			"principal", principalLocal,
			"session_id", request.SessionID,
		)
	} else {
		metricsContent.TotalInputTokens += request.InputTokens
		metricsContent.TotalOutputTokens += request.OutputTokens
		metricsContent.TotalCacheReadTokens += request.CacheReadTokens
		metricsContent.TotalCacheWriteTokens += request.CacheWriteTokens
		metricsContent.TotalCostMilliUSD += int64(request.CostUSD * 1000)
		metricsContent.TotalToolCalls += request.ToolCalls
		metricsContent.TotalTurns += request.Turns
		metricsContent.TotalErrors += request.Errors
		metricsContent.TotalSessionCount++
		metricsContent.TotalDurationSeconds += request.DurationSeconds
		metricsContent.LastSessionID = request.SessionID
		metricsContent.LastUpdatedAt = agentService.clock.Now().UTC().Format("2006-01-02T15:04:05Z")

		if _, err := agentService.session.SendStateEvent(
			ctx, agentService.configRoomID,
			agent.EventTypeAgentMetrics, principalLocal, metricsContent,
		); err != nil {
			return nil, fmt.Errorf("writing metrics state: %w", err)
		}
	}

	agentService.logger.Info("session ended",
		"principal", principalLocal,
		"session_id", request.SessionID,
		"input_tokens", request.InputTokens,
		"output_tokens", request.OutputTokens,
	)

	return nil, nil
}

// --- Session log handler ---

// storeSessionLogRequest is the wire format for "store-session-log".
type storeSessionLogRequest struct {
	Action    string `cbor:"action"`
	SessionID string `cbor:"session_id"`
	Data      []byte `cbor:"data"`
}

// storeSessionLogResponse is returned after the session log is stored.
type storeSessionLogResponse struct {
	Ref string `cbor:"ref"`
}

// handleStoreSessionLog stores the agent's JSONL session log as an
// artifact and returns the content-addressed ref. The caller passes
// this ref to end-session so it is recorded in the session state
// event. The session must be active — this prevents orphaned
// artifacts from agents that crashed before calling end-session.
func (agentService *AgentService) handleStoreSessionLog(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	var request storeSessionLogRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.SessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	if len(request.Data) == 0 {
		return nil, fmt.Errorf("data is required")
	}

	principalLocal := token.Subject.Localpart()

	// Verify the caller has an active session matching the request.
	sessionContent, err := agentService.readSessionState(ctx, principalLocal)
	if err != nil {
		return nil, fmt.Errorf("reading session state: %w", err)
	}
	if sessionContent == nil || sessionContent.ActiveSessionID != request.SessionID {
		activeID := ""
		if sessionContent != nil {
			activeID = sessionContent.ActiveSessionID
		}
		return nil, fmt.Errorf(
			"session mismatch: active session is %q, but store-session-log was called for %q",
			activeID, request.SessionID,
		)
	}

	header := &artifactstore.StoreHeader{
		Action:      "store",
		ContentType: "application/jsonl",
		Size:        int64(len(request.Data)),
		Data:        request.Data,
		Labels:      []string{"session-log"},
		CachePolicy: "ephemeral",
		TTL:         "30d",
	}
	storeResponse, err := agentService.artifactClient.Store(ctx, header, nil)
	if err != nil {
		return nil, fmt.Errorf("storing session log: %w", err)
	}
	artifactRef := storeResponse.Ref

	agentService.logger.Info("session log stored",
		"principal", principalLocal,
		"session_id", request.SessionID,
		"ref", artifactRef,
		"size", len(request.Data),
	)

	return storeSessionLogResponse{Ref: artifactRef}, nil
}

// --- Metrics handler ---

// getMetricsResponse is the wire format for the "get-metrics" response.
type getMetricsResponse struct {
	Metrics *agent.AgentMetricsContent `cbor:"metrics,omitempty"`
}

func (agentService *AgentService) handleGetMetrics(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	principalLocal, err := resolvePrincipalForRead(token, raw)
	if err != nil {
		return nil, err
	}

	content, err := agentService.readMetricsState(ctx, principalLocal)
	if err != nil {
		return nil, fmt.Errorf("reading metrics state: %w", err)
	}

	return getMetricsResponse{Metrics: content}, nil
}

// --- Context handlers ---

// setContextRequest is the wire format for the "set-context" action.
// The caller must have already stored the content in the artifact service
// and obtained an artifact ref before calling this. The agent service
// only records the ref and metadata in the Matrix state event (write-
// through ordering: artifact exists before ref is recorded).
type setContextRequest struct {
	Action      string `cbor:"action"`
	Key         string `cbor:"key"`
	ArtifactRef string `cbor:"artifact_ref"`
	Size        int64  `cbor:"size"`
	ContentType string `cbor:"content_type"`

	// Optional metadata for conversation context entries.
	SessionID    string `cbor:"session_id,omitempty"`
	MessageCount int    `cbor:"message_count,omitempty"`
	TokenCount   int64  `cbor:"token_count,omitempty"`
}

func (agentService *AgentService) handleSetContext(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	var request setContextRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.Key == "" {
		return nil, fmt.Errorf("key is required")
	}
	if request.ArtifactRef == "" {
		return nil, fmt.Errorf("artifact_ref is required")
	}
	if request.ContentType == "" {
		return nil, fmt.Errorf("content_type is required")
	}

	principalLocal := token.Subject.Localpart()

	current, err := agentService.readContextState(ctx, principalLocal)
	if err != nil {
		return nil, fmt.Errorf("reading current context state: %w", err)
	}

	if current == nil {
		current = &agent.AgentContextContent{Version: agent.AgentContextVersion}
	}

	if err := current.CanModify(); err != nil {
		return nil, err
	}

	if current.Entries == nil {
		current.Entries = make(map[string]agent.ContextEntry)
	}

	current.Entries[request.Key] = agent.ContextEntry{
		ArtifactRef:  request.ArtifactRef,
		Size:         request.Size,
		ContentType:  request.ContentType,
		ModifiedAt:   agentService.clock.Now().UTC().Format("2006-01-02T15:04:05Z"),
		SessionID:    request.SessionID,
		MessageCount: request.MessageCount,
		TokenCount:   request.TokenCount,
	}

	if _, err := agentService.session.SendStateEvent(
		ctx, agentService.configRoomID,
		agent.EventTypeAgentContext, principalLocal, current,
	); err != nil {
		return nil, fmt.Errorf("writing context state: %w", err)
	}

	agentService.logger.Info("context entry set",
		"principal", principalLocal,
		"key", request.Key,
		"artifact_ref", request.ArtifactRef,
	)

	return nil, nil
}

// getContextRequest is the wire format for the "get-context" action.
type getContextRequest struct {
	Action         string `cbor:"action"`
	PrincipalLocal string `cbor:"principal_local"`
	Key            string `cbor:"key"`
}

// getContextResponse is the wire format for the "get-context" response.
type getContextResponse struct {
	Entry *agent.ContextEntry `cbor:"entry,omitempty"`
}

func (agentService *AgentService) handleGetContext(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	var request getContextRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.Key == "" {
		return nil, fmt.Errorf("key is required")
	}

	principalLocal := request.PrincipalLocal
	if principalLocal == "" {
		principalLocal = token.Subject.Localpart()
	}

	if err := authorizeRead(token, principalLocal); err != nil {
		return nil, err
	}

	content, err := agentService.readContextState(ctx, principalLocal)
	if err != nil {
		return nil, fmt.Errorf("reading context state: %w", err)
	}

	if content == nil || content.Entries == nil {
		return getContextResponse{}, nil
	}

	entry, exists := content.Entries[request.Key]
	if !exists {
		return getContextResponse{}, nil
	}

	return getContextResponse{Entry: &entry}, nil
}

// deleteContextRequest is the wire format for the "delete-context" action.
type deleteContextRequest struct {
	Action string `cbor:"action"`
	Key    string `cbor:"key"`
}

func (agentService *AgentService) handleDeleteContext(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	var request deleteContextRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.Key == "" {
		return nil, fmt.Errorf("key is required")
	}

	principalLocal := token.Subject.Localpart()

	current, err := agentService.readContextState(ctx, principalLocal)
	if err != nil {
		return nil, fmt.Errorf("reading current context state: %w", err)
	}

	if current == nil || current.Entries == nil {
		return nil, fmt.Errorf("no context entry %q exists for %s", request.Key, principalLocal)
	}

	if err := current.CanModify(); err != nil {
		return nil, err
	}

	if _, exists := current.Entries[request.Key]; !exists {
		return nil, fmt.Errorf("no context entry %q exists for %s", request.Key, principalLocal)
	}

	delete(current.Entries, request.Key)

	// If the map is empty after deletion, nil it out so the state
	// event omits the field.
	if len(current.Entries) == 0 {
		current.Entries = nil
	}

	if _, err := agentService.session.SendStateEvent(
		ctx, agentService.configRoomID,
		agent.EventTypeAgentContext, principalLocal, current,
	); err != nil {
		return nil, fmt.Errorf("writing context state: %w", err)
	}

	agentService.logger.Info("context entry deleted",
		"principal", principalLocal,
		"key", request.Key,
	)

	return nil, nil
}

// listContextRequest is the wire format for the "list-context" action.
type listContextRequest struct {
	Action         string `cbor:"action"`
	PrincipalLocal string `cbor:"principal_local"`
	Prefix         string `cbor:"prefix,omitempty"`
}

// listContextResponse is the wire format for the "list-context" response.
type listContextResponse struct {
	Entries map[string]agent.ContextEntry `cbor:"entries,omitempty"`
}

func (agentService *AgentService) handleListContext(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	var request listContextRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	principalLocal := request.PrincipalLocal
	if principalLocal == "" {
		principalLocal = token.Subject.Localpart()
	}

	if err := authorizeRead(token, principalLocal); err != nil {
		return nil, err
	}

	content, err := agentService.readContextState(ctx, principalLocal)
	if err != nil {
		return nil, fmt.Errorf("reading context state: %w", err)
	}

	if content == nil || content.Entries == nil {
		return listContextResponse{}, nil
	}

	// If no prefix filter, return all entries.
	if request.Prefix == "" {
		return listContextResponse{Entries: content.Entries}, nil
	}

	// Filter entries by key prefix.
	filtered := make(map[string]agent.ContextEntry)
	for key, entry := range content.Entries {
		if strings.HasPrefix(key, request.Prefix) {
			filtered[key] = entry
		}
	}

	if len(filtered) == 0 {
		return listContextResponse{}, nil
	}

	return listContextResponse{Entries: filtered}, nil
}

// --- Context commit handlers ---

// checkpointContextRequest is the wire format for the "checkpoint-context"
// action. The request may include inline Data (e.g., CBOR-encoded events)
// for the agent service to store as an artifact, or a pre-stored
// ArtifactRef from a caller that already wrote to the artifact service.
// When Data is provided, the agent service stores it and derives
// ArtifactRef from the resulting BLAKE3 hash. Exactly one of Data or
// ArtifactRef must be set.
//
// Server-side fields (Principal, Machine, CreatedAt) are derived from
// the service token and clock. The caller does not supply these.
type checkpointContextRequest struct {
	Action       string `cbor:"action"`
	Parent       string `cbor:"parent,omitempty"`
	CommitType   string `cbor:"commit_type"`
	Data         []byte `cbor:"data,omitempty"`
	ArtifactRef  string `cbor:"artifact_ref,omitempty"`
	Format       string `cbor:"format"`
	Template     string `cbor:"template,omitempty"`
	SessionID    string `cbor:"session_id,omitempty"`
	Checkpoint   string `cbor:"checkpoint"`
	TicketID     string `cbor:"ticket_id,omitempty"`
	ThreadID     string `cbor:"thread_id,omitempty"`
	Summary      string `cbor:"summary,omitempty"`
	MessageCount int    `cbor:"message_count,omitempty"`
	TokenCount   int64  `cbor:"token_count,omitempty"`
}

// checkpointContextResponse is the wire format for the "checkpoint-context"
// response. Returns the deterministic ctx-* identifier for the new commit.
type checkpointContextResponse struct {
	ID string `cbor:"id"`
}

func (agentService *AgentService) handleCheckpointContext(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	var request checkpointContextRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	// Resolve artifact ref: either store inline data or use the
	// pre-stored ref. Exactly one must be provided.
	hasData := len(request.Data) > 0
	hasRef := request.ArtifactRef != ""
	if !hasData && !hasRef {
		return nil, fmt.Errorf("exactly one of data or artifact_ref is required")
	}
	if hasData && hasRef {
		return nil, fmt.Errorf("data and artifact_ref are mutually exclusive")
	}

	artifactRef := request.ArtifactRef
	if hasData {
		// Store the inline data as an artifact. The content type is
		// derived from the format — CBOR for structured formats,
		// application/octet-stream as fallback.
		contentType := contentTypeForFormat(request.Format)
		storedRef, storeErr := agentService.storeArtifact(ctx, request.Data, contentType, []string{"context-delta"})
		if storeErr != nil {
			return nil, fmt.Errorf("storing checkpoint data: %w", storeErr)
		}
		artifactRef = storedRef
	}

	createdAt := agentService.clock.Now().UTC().Format("2006-01-02T15:04:05Z")

	content := agent.ContextCommitContent{
		Version:      agent.ContextCommitVersion,
		Parent:       request.Parent,
		CommitType:   request.CommitType,
		ArtifactRef:  artifactRef,
		Format:       request.Format,
		Template:     request.Template,
		Principal:    token.Subject,
		Machine:      token.Machine.UserID(),
		SessionID:    request.SessionID,
		Checkpoint:   request.Checkpoint,
		TicketID:     request.TicketID,
		ThreadID:     request.ThreadID,
		Summary:      request.Summary,
		MessageCount: request.MessageCount,
		TokenCount:   request.TokenCount,
		CreatedAt:    createdAt,
	}

	if err := content.Validate(); err != nil {
		return nil, fmt.Errorf("invalid context commit: %w", err)
	}

	commitID := agent.GenerateContextCommitID(
		request.Parent, artifactRef, createdAt, request.Template,
	)

	if err := agentService.storeCommitMetadata(ctx, commitID, &content); err != nil {
		return nil, err
	}

	// Update the in-memory index so show-context-commit and
	// resolve-context see the commit immediately without waiting
	// for a subsequent CAS fetch.
	agentService.indexCommit(commitID, content)

	agentService.logger.Info("context commit created",
		"principal", token.Subject.Localpart(),
		"commit_id", commitID,
		"commit_type", request.CommitType,
		"artifact_ref", artifactRef,
		"checkpoint", request.Checkpoint,
		"data_size", len(request.Data),
	)

	return checkpointContextResponse{ID: commitID}, nil
}

// contentTypeForFormat returns the MIME content type for a checkpoint
// format. Used when storing inline data as an artifact.
func contentTypeForFormat(format string) string {
	switch format {
	case "events-v1", "bureau-agent-v1":
		return "application/cbor"
	case "claude-code-v1":
		return "application/jsonl"
	default:
		return "application/octet-stream"
	}
}

// --- Context materialization handler ---

// materializeContextRequest is the wire format for the
// "materialize-context" action. Reconstructs the full conversation
// from a context commit chain by fetching deltas from the artifact
// service and concatenating them using format-specific rules.
type materializeContextRequest struct {
	Action         string `cbor:"action"`
	CommitID       string `cbor:"commit_id"`
	OutputFormat   string `cbor:"output_format,omitempty"`
	StopStrategy   string `cbor:"stop_strategy,omitempty"`
	IncludeContent bool   `cbor:"include_content,omitempty"`
}

// materializeContextResponse is the wire format for the
// "materialize-context" response. Contains the artifact ref for the
// concatenated conversation and aggregate statistics from the chain.
// When IncludeContent was requested, Content and ContentType are
// populated with the raw materialized bytes.
type materializeContextResponse struct {
	ArtifactRef  string `cbor:"artifact_ref"`
	Format       string `cbor:"format"`
	MessageCount int    `cbor:"message_count"`
	TokenCount   int64  `cbor:"token_count"`
	CommitCount  int    `cbor:"commit_count"`
	Content      []byte `cbor:"content,omitempty"`
	ContentType  string `cbor:"content_type,omitempty"`
}

func (agentService *AgentService) handleMaterializeContext(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	var request materializeContextRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.CommitID == "" {
		return nil, fmt.Errorf("commit_id is required")
	}

	stopStrategy := request.StopStrategy
	if stopStrategy == "" {
		stopStrategy = stopStrategyCompaction
	}

	// Walk the commit chain from tip to stop point.
	chain, err := agentService.walkChain(ctx, request.CommitID, stopStrategy)
	if err != nil {
		return nil, fmt.Errorf("walking context chain from %s: %w", request.CommitID, err)
	}

	// Validate format consistency across the chain. All commits must
	// share the same format — cross-format translation is not yet
	// implemented.
	chainFormat := chain[0].Content.Format
	for _, entry := range chain[1:] {
		if entry.Content.Format != chainFormat {
			return nil, fmt.Errorf(
				"format mismatch in chain: commit %s has format %q but chain root has %q; "+
					"cross-format translation is not yet supported",
				entry.ID, entry.Content.Format, chainFormat,
			)
		}
	}

	// If the caller requested a specific output format, verify it
	// matches the chain's native format.
	if request.OutputFormat != "" && request.OutputFormat != chainFormat {
		return nil, fmt.Errorf(
			"requested output format %q but chain is in %q; "+
				"cross-format translation is not yet supported",
			request.OutputFormat, chainFormat,
		)
	}

	// Fetch artifacts and concatenate using format-specific rules.
	content, contentType, err := agentService.concatenateDeltas(ctx, chain)
	if err != nil {
		return nil, fmt.Errorf("concatenating deltas: %w", err)
	}

	// Store the materialized result as an artifact.
	artifactRef, err := agentService.storeArtifact(ctx, content, contentType, []string{"context-materialization"})
	if err != nil {
		return nil, err
	}

	// Aggregate statistics from the chain.
	var totalMessageCount int
	var totalTokenCount int64
	for _, entry := range chain {
		totalMessageCount += entry.Content.MessageCount
		totalTokenCount += entry.Content.TokenCount
	}

	agentService.logger.Info("context materialized",
		"principal", token.Subject.Localpart(),
		"tip_commit", request.CommitID,
		"stop_strategy", stopStrategy,
		"format", chainFormat,
		"commit_count", len(chain),
		"message_count", totalMessageCount,
		"token_count", totalTokenCount,
		"artifact_ref", artifactRef,
	)

	response := materializeContextResponse{
		ArtifactRef:  artifactRef,
		Format:       chainFormat,
		MessageCount: totalMessageCount,
		TokenCount:   totalTokenCount,
		CommitCount:  len(chain),
	}

	if request.IncludeContent {
		response.Content = content
		response.ContentType = contentType
	}

	return response, nil
}

// --- Matrix state helpers ---

// readSessionState reads the m.bureau.agent_session state event for a
// principal from the config room. Returns nil if no event exists.
func (agentService *AgentService) readSessionState(ctx context.Context, principalLocal string) (*agent.AgentSessionContent, error) {
	content, err := messaging.GetState[agent.AgentSessionContent](ctx, agentService.session, agentService.configRoomID, agent.EventTypeAgentSession, principalLocal)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading session state for %s: %w", principalLocal, err)
	}
	return &content, nil
}

// readContextState reads the m.bureau.agent_context state event for a
// principal from the config room. Returns nil if no event exists.
func (agentService *AgentService) readContextState(ctx context.Context, principalLocal string) (*agent.AgentContextContent, error) {
	content, err := messaging.GetState[agent.AgentContextContent](ctx, agentService.session, agentService.configRoomID, agent.EventTypeAgentContext, principalLocal)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading context state for %s: %w", principalLocal, err)
	}
	return &content, nil
}

// readMetricsState reads the m.bureau.agent_metrics state event for a
// principal from the config room. Returns nil if no event exists.
func (agentService *AgentService) readMetricsState(ctx context.Context, principalLocal string) (*agent.AgentMetricsContent, error) {
	content, err := messaging.GetState[agent.AgentMetricsContent](ctx, agentService.session, agentService.configRoomID, agent.EventTypeAgentMetrics, principalLocal)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading metrics state for %s: %w", principalLocal, err)
	}
	return &content, nil
}

// fetchCommitMetadata reads a context commit's metadata from the
// artifact store by resolving its tag. Each context commit is stored
// as a CBOR artifact tagged as "ctx/<commitID>". Returns an error for
// missing commits — a missing commit in a chain is a broken chain,
// never an expected "nothing here yet" case.
func (agentService *AgentService) fetchCommitMetadata(ctx context.Context, commitID string) (*agent.ContextCommitContent, error) {
	tagName := "ctx/" + commitID

	resolved, err := agentService.artifactClient.Resolve(ctx, tagName)
	if err != nil {
		return nil, fmt.Errorf("context commit %q not found: %w", commitID, err)
	}

	result, err := agentService.artifactClient.Fetch(ctx, resolved.Ref)
	if err != nil {
		return nil, fmt.Errorf("fetching context commit %q (ref %s): %w", commitID, resolved.Ref, err)
	}
	defer result.Content.Close()

	data, err := io.ReadAll(result.Content)
	if err != nil {
		return nil, fmt.Errorf("reading context commit %q content: %w", commitID, err)
	}

	var content agent.ContextCommitContent
	if err := codec.Unmarshal(data, &content); err != nil {
		return nil, fmt.Errorf("decoding context commit %q: %w", commitID, err)
	}

	return &content, nil
}

// storeCommitMetadata serializes a ContextCommitContent to CBOR and
// stores it as an artifact with a mutable tag. The tag name is
// "ctx/<commitID>", using optimistic mode (last writer wins) so that
// metadata updates (Summary changes) overwrite the previous version.
func (agentService *AgentService) storeCommitMetadata(ctx context.Context, commitID string, content *agent.ContextCommitContent) error {
	cborBytes, err := codec.Marshal(content)
	if err != nil {
		return fmt.Errorf("encoding context commit %s: %w", commitID, err)
	}

	header := &artifactstore.StoreHeader{
		Action:      "store",
		ContentType: "application/cbor",
		Size:        int64(len(cborBytes)),
		Data:        cborBytes,
		Labels:      []string{"context-commit"},
		Tag:         "ctx/" + commitID,
	}

	if _, err := agentService.artifactClient.Store(ctx, header, nil); err != nil {
		return fmt.Errorf("storing context commit metadata %s: %w", commitID, err)
	}

	return nil
}

// getCommit reads a context commit by its ctx-* ID, checking the
// in-memory index first and falling back to the artifact store for
// commits not in the index. The index is populated write-through by
// handleCheckpointContext; commits created before the service started
// (or by other instances) are fetched from CAS on demand.
func (agentService *AgentService) getCommit(ctx context.Context, commitID string) (*agent.ContextCommitContent, error) {
	if content, exists := agentService.commitIndex[commitID]; exists {
		return &content, nil
	}
	return agentService.fetchCommitMetadata(ctx, commitID)
}

// --- Show context commit handler ---

// showContextCommitRequest is the wire format for the
// "show-context-commit" action.
type showContextCommitRequest struct {
	Action   string `cbor:"action"`
	CommitID string `cbor:"commit_id"`
}

// showContextCommitResponse is the wire format for the
// "show-context-commit" response.
type showContextCommitResponse struct {
	ID     string                      `cbor:"id"`
	Commit *agent.ContextCommitContent `cbor:"commit"`
}

func (agentService *AgentService) handleShowContextCommit(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	var request showContextCommitRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.CommitID == "" {
		return nil, fmt.Errorf("commit_id is required")
	}

	content, err := agentService.getCommit(ctx, request.CommitID)
	if err != nil {
		return nil, err
	}

	// Authorize: the caller must be the commit's principal or hold
	// an agent/read grant for that principal.
	if err := authorizeRead(token, content.Principal.Localpart()); err != nil {
		return nil, err
	}

	return showContextCommitResponse{ID: request.CommitID, Commit: content}, nil
}

// --- History context handler ---

// historyContextRequest is the wire format for the "history-context"
// action. Walks the parent chain from a commit back toward the root,
// returning metadata for each commit in the chain.
type historyContextRequest struct {
	Action   string `cbor:"action"`
	CommitID string `cbor:"commit_id"`
	Depth    int    `cbor:"depth,omitempty"`
}

// historyContextEntry pairs a commit ID with its deserialized content,
// used in the history-context response.
type historyContextEntry struct {
	ID      string                     `cbor:"id"`
	Content agent.ContextCommitContent `cbor:"content"`
}

// historyContextResponse is the wire format for the "history-context"
// response. Commits are ordered from the requested tip commit back
// toward root.
type historyContextResponse struct {
	Commits []historyContextEntry `cbor:"commits"`
}

func (agentService *AgentService) handleHistoryContext(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	var request historyContextRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.CommitID == "" {
		return nil, fmt.Errorf("commit_id is required")
	}

	// Read the tip commit first to authorize.
	tipContent, err := agentService.getCommit(ctx, request.CommitID)
	if err != nil {
		return nil, err
	}

	if err := authorizeRead(token, tipContent.Principal.Localpart()); err != nil {
		return nil, err
	}

	// Walk the parent chain from tip toward root.
	var commits []historyContextEntry
	currentID := request.CommitID
	currentContent := tipContent

	for {
		commits = append(commits, historyContextEntry{
			ID:      currentID,
			Content: *currentContent,
		})

		if request.Depth > 0 && len(commits) >= request.Depth {
			break
		}

		if currentContent.Parent == "" {
			break
		}

		if len(commits) >= maxChainDepth {
			return nil, fmt.Errorf(
				"chain depth exceeded %d commits from %q: possible cycle or corrupt chain",
				maxChainDepth, request.CommitID,
			)
		}

		nextContent, err := agentService.getCommit(ctx, currentContent.Parent)
		if err != nil {
			return nil, fmt.Errorf("reading parent commit %q: %w", currentContent.Parent, err)
		}
		currentID = currentContent.Parent
		currentContent = nextContent
	}

	return historyContextResponse{Commits: commits}, nil
}

// --- Update context metadata handler ---

// updateContextMetadataRequest is the wire format for the
// "update-context-metadata" action. Updates mutable metadata fields
// on an existing context commit. The delta artifact is immutable;
// only metadata (currently just summary) can be changed after creation.
type updateContextMetadataRequest struct {
	Action   string `cbor:"action"`
	CommitID string `cbor:"commit_id"`
	Summary  string `cbor:"summary"`
}

func (agentService *AgentService) handleUpdateContextMetadata(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	var request updateContextMetadataRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.CommitID == "" {
		return nil, fmt.Errorf("commit_id is required")
	}

	content, err := agentService.getCommit(ctx, request.CommitID)
	if err != nil {
		return nil, err
	}

	// Authorize write: the caller must be the commit's principal or
	// hold an agent/write grant (e.g., batch summarization service).
	principalLocal := content.Principal.Localpart()
	if principalLocal != token.Subject.Localpart() {
		if !servicetoken.GrantsAllow(token.Grants, "agent/write", principalLocal) {
			return nil, fmt.Errorf("access denied: no agent/write grant for %s", principalLocal)
		}
	}

	if err := content.CanModify(); err != nil {
		return nil, err
	}

	content.Summary = request.Summary

	if err := agentService.storeCommitMetadata(ctx, request.CommitID, content); err != nil {
		return nil, err
	}

	// Update the in-memory index.
	agentService.commitIndex[request.CommitID] = *content

	agentService.logger.Info("context commit metadata updated",
		"commit_id", request.CommitID,
		"summary_length", len(request.Summary),
	)

	return nil, nil
}

// --- Resolve context handler ---

// resolveContextRequest is the wire format for the "resolve-context"
// action. Finds the nearest context commit at or before the given
// timestamp for a principal.
type resolveContextRequest struct {
	Action         string `cbor:"action"`
	PrincipalLocal string `cbor:"principal_local,omitempty"`
	Timestamp      string `cbor:"timestamp"`
}

// resolveContextResponse is the wire format for the "resolve-context"
// response. CommitID is empty if no commit exists at or before the
// requested timestamp.
type resolveContextResponse struct {
	CommitID string `cbor:"commit_id,omitempty"`
}

func (agentService *AgentService) handleResolveContext(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	var request resolveContextRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.Timestamp == "" {
		return nil, fmt.Errorf("timestamp is required")
	}

	principalLocal := request.PrincipalLocal
	if principalLocal == "" {
		principalLocal = token.Subject.Localpart()
	}

	if err := authorizeRead(token, principalLocal); err != nil {
		return nil, err
	}

	// Lazy-load timelines from the artifact store on first access
	// after service restart. Write-through from checkpoint-context
	// keeps timelines current during normal operation; this handles
	// commits created before this process started.
	if !agentService.timelinesLoaded {
		if err := agentService.loadAllTimelines(ctx); err != nil {
			return nil, fmt.Errorf("loading timelines from artifact store: %w", err)
		}
	}

	timeline := agentService.principalTimelines[principalLocal]
	if len(timeline) == 0 {
		return resolveContextResponse{}, nil
	}

	// Binary search: find the rightmost entry where CreatedAt <=
	// timestamp. sort.Search finds the first index where the
	// predicate is true — here, the first index where CreatedAt >
	// timestamp. The entry we want is one before that.
	position := sort.Search(len(timeline), func(i int) bool {
		return timeline[i].CreatedAt > request.Timestamp
	})

	if position == 0 {
		// All entries are after the requested timestamp.
		return resolveContextResponse{}, nil
	}

	return resolveContextResponse{CommitID: timeline[position-1].CommitID}, nil
}

// loadAllTimelines scans all context commit tags in the artifact store
// and rebuilds the in-memory commit index and principal timelines. This
// is called lazily on the first resolve-context request after a service
// restart. Subsequent calls are skipped because timelinesLoaded is true.
//
// Must be called under the write lock.
func (agentService *AgentService) loadAllTimelines(ctx context.Context) error {
	tagsResponse, err := agentService.artifactClient.Tags(ctx, "ctx/")
	if err != nil {
		return fmt.Errorf("listing context commit tags: %w", err)
	}

	loaded := 0
	for _, tag := range tagsResponse.Tags {
		// Extract commit ID from tag name "ctx/<commitID>".
		commitID := strings.TrimPrefix(tag.Name, "ctx/")
		if commitID == tag.Name {
			// Tag didn't have the expected prefix — skip.
			continue
		}

		// Skip commits already in the index (populated write-through
		// by checkpoint-context during this process's lifetime).
		if _, exists := agentService.commitIndex[commitID]; exists {
			continue
		}

		result, err := agentService.artifactClient.Fetch(ctx, tag.Ref)
		if err != nil {
			agentService.logger.Warn("failed to fetch context commit during timeline load",
				"commit_id", commitID,
				"ref", tag.Ref,
				"error", err,
			)
			continue
		}

		data, err := io.ReadAll(result.Content)
		result.Content.Close()
		if err != nil {
			agentService.logger.Warn("failed to read context commit during timeline load",
				"commit_id", commitID,
				"error", err,
			)
			continue
		}

		var content agent.ContextCommitContent
		if err := codec.Unmarshal(data, &content); err != nil {
			agentService.logger.Warn("failed to decode context commit during timeline load",
				"commit_id", commitID,
				"error", err,
			)
			continue
		}

		agentService.indexCommit(commitID, content)
		loaded++
	}

	agentService.timelinesLoaded = true
	agentService.logger.Info("loaded context commit timelines from artifact store",
		"loaded", loaded,
		"total_indexed", len(agentService.commitIndex),
	)

	return nil
}
