// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// registerActions registers socket protocol actions on the server.
func (agentService *AgentService) registerActions(server *service.SocketServer) {
	// Unauthenticated health check.
	server.Handle("status", agentService.handleStatus)

	// Authenticated session actions.
	server.HandleAuth("get-session", agentService.withReadLock(agentService.handleGetSession))
	server.HandleAuth("start-session", agentService.withWriteLock(agentService.handleStartSession))
	server.HandleAuth("end-session", agentService.withWriteLock(agentService.handleEndSession))

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

// resolvePrincipalForRead unmarshals a principalReadRequest, resolves the
// target principal (defaulting to the caller), and enforces the agent/read
// authorization check for cross-principal access.
func resolvePrincipalForRead(token *servicetoken.Token, raw []byte) (string, error) {
	var request principalReadRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return "", fmt.Errorf("invalid request: %w", err)
	}

	principalLocal := request.PrincipalLocal
	if principalLocal == "" {
		principalLocal = token.Subject
	}

	// Callers can read their own data. Reading another principal's
	// data requires an agent/read grant with that principal as target.
	if principalLocal != token.Subject {
		if !servicetoken.GrantsAllow(token.Grants, "agent/read", principalLocal) {
			return "", fmt.Errorf("access denied: no agent/read grant for %s", principalLocal)
		}
	}

	return principalLocal, nil
}

// --- Session handlers ---

// getSessionResponse is the wire format for the "get-session" response.
type getSessionResponse struct {
	// Session is the current session state from Matrix. Nil if no
	// agent session state event exists for this principal.
	Session *schema.AgentSessionContent `cbor:"session,omitempty"`
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

	principalLocal := token.Subject

	// Read current state, apply mutation, write back.
	current, err := agentService.readSessionState(ctx, principalLocal)
	if err != nil {
		return nil, fmt.Errorf("reading current session state: %w", err)
	}

	if current == nil {
		current = &schema.AgentSessionContent{Version: schema.AgentSessionVersion}
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
		schema.EventTypeAgentSession, principalLocal, current,
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

	principalLocal := token.Subject

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

	if _, err := agentService.session.SendStateEvent(
		ctx, agentService.configRoomID,
		schema.EventTypeAgentSession, principalLocal, sessionContent,
	); err != nil {
		return nil, fmt.Errorf("writing session state: %w", err)
	}

	// Update aggregated metrics.
	metricsContent, err := agentService.readMetricsState(ctx, principalLocal)
	if err != nil {
		return nil, fmt.Errorf("reading current metrics state: %w", err)
	}

	if metricsContent == nil {
		metricsContent = &schema.AgentMetricsContent{Version: schema.AgentMetricsVersion}
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
			schema.EventTypeAgentMetrics, principalLocal, metricsContent,
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

// --- Metrics handler ---

// getMetricsResponse is the wire format for the "get-metrics" response.
type getMetricsResponse struct {
	Metrics *schema.AgentMetricsContent `cbor:"metrics,omitempty"`
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

// --- Matrix state helpers ---

// readSessionState reads the m.bureau.agent_session state event for a
// principal from the config room. Returns nil if no event exists.
func (agentService *AgentService) readSessionState(ctx context.Context, principalLocal string) (*schema.AgentSessionContent, error) {
	raw, err := agentService.session.GetStateEvent(
		ctx, agentService.configRoomID,
		schema.EventTypeAgentSession, principalLocal,
	)
	if err != nil {
		// Matrix returns 404 for missing state events. Treat this as
		// "no session state" rather than an error.
		return nil, nil
	}

	var content schema.AgentSessionContent
	if unmarshalError := json.Unmarshal(raw, &content); unmarshalError != nil {
		return nil, fmt.Errorf("unmarshaling agent session content: %w", unmarshalError)
	}

	return &content, nil
}

// readMetricsState reads the m.bureau.agent_metrics state event for a
// principal from the config room. Returns nil if no event exists.
func (agentService *AgentService) readMetricsState(ctx context.Context, principalLocal string) (*schema.AgentMetricsContent, error) {
	raw, err := agentService.session.GetStateEvent(
		ctx, agentService.configRoomID,
		schema.EventTypeAgentMetrics, principalLocal,
	)
	if err != nil {
		return nil, nil
	}

	var content schema.AgentMetricsContent
	if unmarshalError := json.Unmarshal(raw, &content); unmarshalError != nil {
		return nil, fmt.Errorf("unmarshaling agent metrics content: %w", unmarshalError)
	}

	return &content, nil
}
