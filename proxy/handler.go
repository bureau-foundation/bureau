// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// Maximum request body size (64KB is plenty for CLI args)
const maxRequestBodySize = 64 * 1024

// IdentityInfo holds the agent's identity information, set by the server
// after loading credentials. Returned by the GET /v1/identity endpoint.
type IdentityInfo struct {
	// UserID is the agent's full Matrix user ID (e.g., "@iree/amdgpu/pm:bureau.local").
	UserID string `json:"user_id"`

	// ServerName is the Matrix server name (e.g., "bureau.local").
	ServerName string `json:"server_name,omitempty"`

	// ObserveSocket is the path to the proxy's observation socket.
	// Agents dial this socket to observe other principals. The proxy
	// injects credentials and forwards to the daemon. Empty when
	// observation is not configured.
	ObserveSocket string `json:"observe_socket,omitempty"`
}

// Handler handles HTTP requests to the proxy server.
type Handler struct {
	services     map[string]Service      // CLI services (JSON-RPC style)
	httpServices map[string]*HTTPService // HTTP proxy services
	identity     *IdentityInfo           // agent identity, nil if not set
	matrixPolicy *schema.MatrixPolicy    // Matrix access policy, nil = default-deny

	// serviceDirectory is the cached service directory pushed by the
	// daemon. Agents query this via GET /v1/services to discover what
	// services are available. The daemon pushes updates via the admin
	// socket whenever the directory changes.
	serviceDirectory []ServiceDirectoryEntry

	// serviceVisibility is a list of glob patterns that control which
	// services this agent can discover. Patterns match against service
	// localparts using Bureau's hierarchical glob syntax. An empty list
	// means the agent sees no services (default-deny). Set by the
	// daemon via the admin API when the sandbox starts.
	serviceVisibility []string

	mu     sync.RWMutex
	logger *slog.Logger
}

// ServiceDirectoryEntry describes a service in the Bureau service directory.
// The daemon maintains the authoritative directory from Matrix state events
// and pushes it to each proxy. Agents query the proxy to discover services
// without needing direct access to the Matrix service room.
type ServiceDirectoryEntry struct {
	// Localpart is the service identifier (the Matrix state key),
	// e.g., "service/stt/whisper".
	Localpart string `json:"localpart"`

	// Principal is the full Matrix user ID of the service provider,
	// e.g., "@service/stt/whisper:bureau.local".
	Principal string `json:"principal"`

	// Machine is the full Matrix user ID of the machine running this
	// service instance, e.g., "@machine/cloud-gpu-1:bureau.local".
	Machine string `json:"machine"`

	// Protocol is the wire protocol spoken by the service
	// (e.g., "http", "grpc", "raw-frames").
	Protocol string `json:"protocol"`

	// Description is a human-readable description of the service.
	Description string `json:"description,omitempty"`

	// Capabilities lists what this service instance supports
	// (e.g., ["streaming", "speaker-diarization"]).
	Capabilities []string `json:"capabilities,omitempty"`

	// Metadata holds service-specific key-value pairs (e.g., supported
	// languages, model version, max batch size).
	Metadata map[string]any `json:"metadata,omitempty"`
}

// NewHandler creates a new request handler.
func NewHandler(logger *slog.Logger) *Handler {
	return &Handler{
		services:     make(map[string]Service),
		httpServices: make(map[string]*HTTPService),
		logger:       logger,
	}
}

// RegisterService registers a CLI service that can be proxied.
func (h *Handler) RegisterService(name string, service Service) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.services[name] = service
	h.logger.Info("registered cli service", "name", name)
}

// RegisterHTTPService registers an HTTP proxy service.
func (h *Handler) RegisterHTTPService(name string, service *HTTPService) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.httpServices[name] = service
	h.logger.Info("registered http service", "name", name, "upstream", service.upstream.String())
}

// UnregisterHTTPService removes an HTTP proxy service by name.
// Returns true if the service existed and was removed, false if it wasn't found.
func (h *Handler) UnregisterHTTPService(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, existed := h.httpServices[name]
	if existed {
		delete(h.httpServices, name)
		h.logger.Info("unregistered http service", "name", name)
	}
	return existed
}

// ListHTTPServices returns the names of all registered HTTP proxy services.
func (h *Handler) ListHTTPServices() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	names := make([]string, 0, len(h.httpServices))
	for name := range h.httpServices {
		names = append(names, name)
	}
	return names
}

// SetIdentity sets the agent's identity information. This is called by the
// server after loading credentials. Once set, the GET /v1/identity endpoint
// returns this information to the agent.
func (h *Handler) SetIdentity(identity IdentityInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.identity = &identity
	h.logger.Info("agent identity configured", "user_id", identity.UserID)
}

// SetMatrixPolicy configures the Matrix access policy for this agent. When
// nil, the proxy uses default-deny: all self-service membership operations
// (join, invite, room creation) are blocked. The agent can only interact
// with rooms the admin explicitly placed it in.
func (h *Handler) SetMatrixPolicy(policy *schema.MatrixPolicy) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.matrixPolicy = policy
	if policy != nil {
		h.logger.Info("matrix policy configured",
			"allow_join", policy.AllowJoin,
			"allow_invite", policy.AllowInvite,
			"allow_room_create", policy.AllowRoomCreate,
		)
	} else {
		h.logger.Info("matrix policy configured (default-deny)")
	}
}

// HandleHealth handles health check requests.
func (h *Handler) HandleHealth(w http.ResponseWriter, r *http.Request) {
	h.writeJSON(w, map[string]string{"status": "ok"})
}

// HandleIdentity returns the agent's Matrix identity. Agents use this to
// discover their own user ID without holding credentials directly.
func (h *Handler) HandleIdentity(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	identity := h.identity
	h.mu.RUnlock()

	if identity == nil {
		h.sendError(w, http.StatusServiceUnavailable, "identity not configured")
		return
	}

	h.writeJSON(w, identity)
}

// HandleProxy handles proxy requests.
func (h *Handler) HandleProxy(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	// Limit request body size to prevent DoS
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	// Parse request
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Check if it was a size limit error
		if strings.Contains(err.Error(), "http: request body too large") {
			h.sendError(w, http.StatusRequestEntityTooLarge, "request body too large (max %d bytes)", maxRequestBodySize)
			return
		}
		h.sendError(w, http.StatusBadRequest, "invalid request: %v", err)
		return
	}

	// Validate request
	if req.Service == "" {
		h.sendError(w, http.StatusBadRequest, "service is required")
		return
	}

	// Look up service
	h.mu.RLock()
	service, ok := h.services[req.Service]
	h.mu.RUnlock()

	if !ok {
		h.sendError(w, http.StatusNotFound, "unknown service: %s", req.Service)
		return
	}

	// Log the request
	h.logger.Info("proxy request",
		"service", req.Service,
		"args", req.Args,
		"stream", req.Stream,
	)

	// Handle streaming vs buffered execution
	if req.Stream {
		h.handleStreamingRequest(w, r, req, service, startTime)
	} else {
		h.handleBufferedRequest(w, r, req, service, startTime)
	}
}

// handleBufferedRequest executes the command and returns buffered output.
func (h *Handler) handleBufferedRequest(w http.ResponseWriter, r *http.Request, req Request, service Service, startTime time.Time) {
	result, err := service.Execute(r.Context(), req.Args, req.Input)
	if err != nil {
		h.logger.Warn("execution failed",
			"service", req.Service,
			"args", req.Args,
			"error", err,
			"duration", time.Since(startTime),
		)
		// Return 403 for blocked commands, 500 for other errors
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "request blocked") {
			status = http.StatusForbidden
		} else if strings.Contains(err.Error(), "missing credentials") {
			status = http.StatusServiceUnavailable
		}
		h.sendError(w, status, "%v", err)
		return
	}

	h.logger.Info("proxy complete",
		"service", req.Service,
		"args", req.Args,
		"exit_code", result.ExitCode,
		"duration", time.Since(startTime),
	)

	h.writeJSON(w, Response{
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
	})
}

// handleStreamingRequest executes the command and streams output as newline-delimited JSON.
func (h *Handler) handleStreamingRequest(w http.ResponseWriter, r *http.Request, req Request, service Service, startTime time.Time) {
	// Check if service supports streaming
	streamingService, ok := service.(StreamingService)
	if !ok {
		// Fall back to buffered mode if service doesn't support streaming
		h.logger.Info("service does not support streaming, falling back to buffered",
			"service", req.Service,
		)
		h.handleBufferedRequest(w, r, req, service, startTime)
		return
	}

	// Set up streaming response
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Create writers that emit JSON chunks.
	// Use a shared mutex to prevent race conditions since os/exec pumps
	// stdout and stderr in separate goroutines.
	var mu sync.Mutex
	encoder := json.NewEncoder(w)
	stdoutWriter := &chunkWriter{mu: &mu, encoder: encoder, chunkType: "stdout", flusher: flusher}
	stderrWriter := &chunkWriter{mu: &mu, encoder: encoder, chunkType: "stderr", flusher: flusher}

	// Execute with streaming
	exitCode, err := streamingService.ExecuteStream(r.Context(), req.Args, req.Input, stdoutWriter, stderrWriter)

	// Lock for final writes to ensure they don't race with any trailing output
	mu.Lock()
	defer mu.Unlock()

	if err != nil {
		h.logger.Warn("streaming execution failed",
			"service", req.Service,
			"args", req.Args,
			"error", err,
			"duration", time.Since(startTime),
		)
		// Send error as final chunk
		if encodeErr := encoder.Encode(StreamChunk{Type: "error", Data: err.Error()}); encodeErr != nil {
			h.logger.Warn("writing streaming error chunk", "error", encodeErr, "service", req.Service)
		}
		flusher.Flush()
		return
	}

	// Send exit code as final chunk
	if err := encoder.Encode(StreamChunk{Type: "exit", Code: exitCode}); err != nil {
		h.logger.Warn("writing streaming exit chunk", "error", err, "service", req.Service)
	}
	flusher.Flush()

	h.logger.Info("proxy streaming complete",
		"service", req.Service,
		"args", req.Args,
		"exit_code", exitCode,
		"duration", time.Since(startTime),
	)
}

// chunkWriter wraps output and emits JSON chunks for streaming.
// It uses a mutex to serialize writes since stdout and stderr are pumped
// by separate goroutines in os/exec.
type chunkWriter struct {
	mu        *sync.Mutex // shared between stdout/stderr writers
	encoder   *json.Encoder
	chunkType string
	flusher   http.Flusher
}

// Write implements io.Writer by emitting JSON chunks.
func (w *chunkWriter) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	err = w.encoder.Encode(StreamChunk{Type: w.chunkType, Data: string(p)})
	if err != nil {
		return 0, err
	}
	w.flusher.Flush()
	return len(p), nil
}

func (h *Handler) sendError(w http.ResponseWriter, status int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(Response{
		ExitCode: -1,
		Error:    fmt.Sprintf(format, args...),
	}); err != nil {
		h.logger.Warn("writing JSON error response", "error", err, "status", status)
	}
}

// writeJSON encodes value as JSON into w, setting the Content-Type header.
// If encoding fails (typically because the client disconnected), the error
// is logged — the caller cannot send a corrective response to a dead client.
func (h *Handler) writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		h.logger.Warn("writing JSON response", "error", err)
	}
}

// HandleHTTPProxy handles HTTP proxy requests.
// Routes requests like /http/openai/v1/chat/completions to the appropriate HTTP service.
func (h *Handler) HandleHTTPProxy(w http.ResponseWriter, r *http.Request) {
	// Extract service name from path: /http/{service}/...
	// The path should be /http/openai/v1/chat/completions -> service=openai, path=/v1/chat/completions
	path := r.URL.Path
	if !strings.HasPrefix(path, "/http/") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	// Remove /http/ prefix
	remaining := strings.TrimPrefix(path, "/http/")

	// Split into service name and remaining path
	parts := strings.SplitN(remaining, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "service name required in path", http.StatusBadRequest)
		return
	}

	serviceName := parts[0]
	var servicePath string
	if len(parts) > 1 {
		servicePath = "/" + parts[1]
	} else {
		servicePath = "/"
	}

	// Look up HTTP service
	h.mu.RLock()
	service, ok := h.httpServices[serviceName]
	h.mu.RUnlock()

	if !ok {
		http.Error(w, fmt.Sprintf("unknown http service: %s", serviceName), http.StatusNotFound)
		return
	}

	// Enforce Matrix access policy on Matrix API requests. The check uses
	// the rewritten servicePath (what the upstream sees), not the original
	// request path, so it works regardless of how the service is named.
	if strings.HasPrefix(servicePath, "/_matrix/") {
		if blocked, reason := h.checkMatrixPolicy(r.Method, servicePath); blocked {
			h.logger.Warn("matrix policy blocked request",
				"method", r.Method,
				"path", servicePath,
				"reason", reason,
			)
			http.Error(w, reason, http.StatusForbidden)
			return
		}
	}

	// Rewrite the URL path for the service handler
	r.URL.Path = servicePath

	// Delegate to the HTTP service
	service.ServeHTTP(w, r)
}

// checkMatrixPolicy checks whether a Matrix API request is allowed by the
// agent's policy. Returns (true, reason) if the request should be blocked,
// (false, "") if allowed.
//
// The gated endpoints are:
//   - POST /_matrix/client/v3/join/{roomIdOrAlias}   — AllowJoin
//   - POST /_matrix/client/v3/rooms/{roomId}/join     — AllowJoin
//   - POST /_matrix/client/v3/rooms/{roomId}/invite   — AllowInvite
//   - POST /_matrix/client/v3/createRoom              — AllowRoomCreate
//
// Everything else (messages, state, sync, room history) is allowed — the
// homeserver enforces room membership and power levels for those operations.
func (h *Handler) checkMatrixPolicy(method, path string) (blocked bool, reason string) {
	h.mu.RLock()
	policy := h.matrixPolicy
	h.mu.RUnlock()

	// Only POST requests are gated — reads always pass through to the
	// homeserver which enforces membership.
	if method != http.MethodPost {
		return false, ""
	}

	const clientPrefix = "/_matrix/client/v3/"
	if !strings.HasPrefix(path, clientPrefix) {
		return false, ""
	}
	apiPath := strings.TrimPrefix(path, clientPrefix)

	// POST /_matrix/client/v3/createRoom
	if apiPath == "createRoom" {
		if policy != nil && policy.AllowRoomCreate {
			return false, ""
		}
		return true, "matrix policy: room creation is not allowed for this agent"
	}

	// POST /_matrix/client/v3/join/{roomIdOrAlias}
	if strings.HasPrefix(apiPath, "join/") {
		if policy != nil && policy.AllowJoin {
			return false, ""
		}
		return true, "matrix policy: joining rooms is not allowed for this agent"
	}

	// POST /_matrix/client/v3/rooms/{roomId}/join
	// POST /_matrix/client/v3/rooms/{roomId}/invite
	if strings.HasPrefix(apiPath, "rooms/") {
		// Extract the action after rooms/{roomId}/...
		// Room IDs contain colons and may be URL-encoded, but the action
		// segment is always the last path component.
		parts := strings.Split(apiPath, "/")
		if len(parts) >= 3 {
			action := parts[len(parts)-1]
			switch action {
			case "join":
				if policy != nil && policy.AllowJoin {
					return false, ""
				}
				return true, "matrix policy: joining rooms is not allowed for this agent"
			case "invite":
				if policy != nil && policy.AllowInvite {
					return false, ""
				}
				return true, "matrix policy: inviting users is not allowed for this agent"
			}
		}
	}

	// All other endpoints are allowed — the homeserver handles authz.
	return false, ""
}

// AdminServiceRequest is the JSON body for PUT /v1/admin/services/{name}.
type AdminServiceRequest struct {
	// UpstreamURL is the upstream HTTP(S) URL. At least one of UpstreamURL
	// or UpstreamUnix is required. When both are provided, UpstreamURL
	// provides the scheme, host, and path prefix for URL construction
	// (e.g., "http://localhost/http/stt-whisper"), and UpstreamUnix
	// provides the physical transport (the actual Unix socket to connect
	// to). This is used for cross-principal routing: the path prefix
	// encodes where the service lives on the provider's proxy, and the
	// socket provides the connection to that proxy.
	UpstreamURL string `json:"upstream_url,omitempty"`

	// UpstreamUnix is the path to a Unix socket upstream. Used for local
	// service routing where the upstream is another principal's proxy socket.
	// Can be combined with UpstreamURL (see above).
	UpstreamUnix string `json:"upstream_unix,omitempty"`

	// InjectHeaders maps header names to credential names for injection.
	InjectHeaders map[string]string `json:"inject_headers,omitempty"`

	// StripHeaders lists headers to remove from incoming requests.
	StripHeaders []string `json:"strip_headers,omitempty"`
}

// AdminServiceInfo is returned by GET /v1/admin/services and
// GET /v1/admin/services/{name}.
type AdminServiceInfo struct {
	Name     string `json:"name"`
	Upstream string `json:"upstream"`
}

// HandleAdminListServices returns the list of registered HTTP services.
func (h *Handler) HandleAdminListServices(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	services := make([]AdminServiceInfo, 0, len(h.httpServices))
	for name, service := range h.httpServices {
		services = append(services, AdminServiceInfo{
			Name:     name,
			Upstream: service.upstream.String(),
		})
	}
	h.mu.RUnlock()

	h.writeJSON(w, services)
}

// HandleAdminRegisterService creates or replaces an HTTP proxy service.
// The service name is extracted from the URL path.
func (h *Handler) HandleAdminRegisterService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		h.sendError(w, http.StatusBadRequest, "service name is required")
		return
	}

	var req AdminServiceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.sendError(w, http.StatusBadRequest, "invalid request body: %v", err)
		return
	}

	if req.UpstreamURL == "" && req.UpstreamUnix == "" {
		h.sendError(w, http.StatusBadRequest, "upstream_url or upstream_unix is required")
		return
	}

	service, err := NewHTTPService(HTTPServiceConfig{
		Name:          name,
		Upstream:      req.UpstreamURL,
		UpstreamUnix:  req.UpstreamUnix,
		InjectHeaders: req.InjectHeaders,
		StripHeaders:  req.StripHeaders,
		Logger:        h.logger,
	})
	if err != nil {
		h.sendError(w, http.StatusBadRequest, "failed to create service: %v", err)
		return
	}

	h.RegisterHTTPService(name, service)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(AdminServiceInfo{
		Name:     name,
		Upstream: service.upstream.String(),
	}); err != nil {
		h.logger.Warn("writing JSON response", "error", err)
	}
}

// HandleAdminUnregisterService removes an HTTP proxy service.
// The service name is extracted from the URL path.
func (h *Handler) HandleAdminUnregisterService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		h.sendError(w, http.StatusBadRequest, "service name is required")
		return
	}

	if !h.UnregisterHTTPService(name) {
		h.sendError(w, http.StatusNotFound, "unknown service: %s", name)
		return
	}

	h.writeJSON(w, map[string]string{"status": "removed", "name": name})
}

// SetServiceDirectory replaces the cached service directory. Called by the
// daemon (via the admin API) whenever the service directory changes.
func (h *Handler) SetServiceDirectory(entries []ServiceDirectoryEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.serviceDirectory = entries
	h.logger.Info("service directory updated", "services", len(entries))
}

// HandleServiceDirectory returns the service directory to the agent. Agents
// query GET /v1/services to discover what services exist across the Bureau
// deployment. The daemon pushes the authoritative directory to each proxy;
// the proxy serves it as a read-only catalog.
//
// The directory is filtered in two stages:
//  1. Visibility: only services matching the agent's ServiceVisibility
//     patterns are included. An empty pattern list means nothing is visible.
//  2. Query parameters: further filter by protocol and/or capability.
//
// Optional query parameters:
//   - capability: only entries with this capability
//   - protocol: only entries with this wire protocol
func (h *Handler) HandleServiceDirectory(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	directory := h.serviceDirectory
	visibility := h.serviceVisibility
	h.mu.RUnlock()

	if directory == nil {
		directory = []ServiceDirectoryEntry{}
	}

	// Stage 1: visibility filtering. If no patterns are configured,
	// the agent sees nothing (default-deny).
	if len(visibility) > 0 {
		visible := make([]ServiceDirectoryEntry, 0, len(directory))
		for _, entry := range directory {
			if principal.MatchAnyPattern(visibility, entry.Localpart) {
				visible = append(visible, entry)
			}
		}
		directory = visible
	} else if len(directory) > 0 {
		// Empty visibility = default-deny: no services visible.
		directory = []ServiceDirectoryEntry{}
	}

	// Stage 2: query parameter filtering.
	capability := r.URL.Query().Get("capability")
	protocol := r.URL.Query().Get("protocol")

	if capability != "" || protocol != "" {
		filtered := make([]ServiceDirectoryEntry, 0, len(directory))
		for _, entry := range directory {
			if protocol != "" && entry.Protocol != protocol {
				continue
			}
			if capability != "" && !slices.Contains(entry.Capabilities, capability) {
				continue
			}
			filtered = append(filtered, entry)
		}
		directory = filtered
	}

	h.writeJSON(w, directory)
}

// HandleAdminSetDirectory replaces the full service directory. The daemon
// pushes the complete directory via PUT /v1/admin/directory whenever the
// service state in Matrix changes.
func (h *Handler) HandleAdminSetDirectory(w http.ResponseWriter, r *http.Request) {
	var entries []ServiceDirectoryEntry
	if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
		h.sendError(w, http.StatusBadRequest, "invalid directory payload: %v", err)
		return
	}

	h.SetServiceDirectory(entries)

	h.writeJSON(w, map[string]any{
		"status":   "ok",
		"services": len(entries),
	})
}

// SetServiceVisibility configures which services this agent can discover.
// Patterns use Bureau's hierarchical glob syntax (principal.MatchPattern):
// "service/stt/*", "service/**", "**", etc. An empty list means the agent
// cannot see any services. Called by the daemon via the admin API when the
// sandbox starts or when the policy changes.
func (h *Handler) SetServiceVisibility(patterns []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.serviceVisibility = patterns
	h.logger.Info("service visibility configured", "patterns", patterns)
}

// HandleAdminSetVisibility sets the service visibility patterns. The daemon
// pushes these via PUT /v1/admin/visibility when configuring the proxy.
func (h *Handler) HandleAdminSetVisibility(w http.ResponseWriter, r *http.Request) {
	var patterns []string
	if err := json.NewDecoder(r.Body).Decode(&patterns); err != nil {
		h.sendError(w, http.StatusBadRequest, "invalid visibility payload: %v", err)
		return
	}

	h.SetServiceVisibility(patterns)

	h.writeJSON(w, map[string]any{
		"status":   "ok",
		"patterns": len(patterns),
	})
}

// HandleAdminSetMatrixPolicy updates the Matrix access policy for this proxy.
// The daemon pushes policy changes via PUT /v1/admin/policy when the
// PrincipalAssignment's MatrixPolicy is modified at runtime.
func (h *Handler) HandleAdminSetMatrixPolicy(w http.ResponseWriter, r *http.Request) {
	var policy *schema.MatrixPolicy
	if err := json.NewDecoder(r.Body).Decode(&policy); err != nil {
		h.sendError(w, http.StatusBadRequest, "invalid policy payload: %v", err)
		return
	}

	h.SetMatrixPolicy(policy)

	allowJoin := policy != nil && policy.AllowJoin
	allowInvite := policy != nil && policy.AllowInvite
	allowRoomCreate := policy != nil && policy.AllowRoomCreate

	h.writeJSON(w, map[string]any{
		"status":            "ok",
		"allow_join":        allowJoin,
		"allow_invite":      allowInvite,
		"allow_room_create": allowRoomCreate,
	})
}
