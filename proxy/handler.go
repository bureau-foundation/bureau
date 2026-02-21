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

	"github.com/bureau-foundation/bureau/lib/authorization"
	"github.com/bureau-foundation/bureau/lib/ref"
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

// Handler handles HTTP requests to the proxy server. Authorization is
// grant-based: the daemon pushes pre-resolved grants via the admin socket,
// and the handler checks them via authorization.GrantsAllow for both Matrix
// API gating and service directory filtering.
type Handler struct {
	services     map[string]Service      // CLI services (JSON-RPC style)
	httpServices map[string]*HTTPService // HTTP proxy services
	identity     *IdentityInfo           // agent identity, nil if not set
	grants       []schema.Grant          // pre-resolved grants from daemon, nil = default-deny

	// serviceDirectory is the cached service directory pushed by the
	// daemon. Agents query this via GET /v1/services to discover what
	// services are available. The daemon pushes updates via the admin
	// socket whenever the directory changes.
	serviceDirectory []ServiceDirectoryEntry

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
	Principal ref.UserID `json:"principal"`

	// Machine is the full Matrix user ID of the machine running this
	// service instance, e.g., "@machine/cloud-gpu-1:bureau.local".
	Machine ref.UserID `json:"machine"`

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

// SetGrants configures the pre-resolved authorization grants for this agent.
// When nil (the default), the proxy uses default-deny: all gated operations
// are blocked. When non-nil, the proxy checks grants via
// authorization.GrantsAllow for Matrix API gating and service discovery.
//
// The daemon resolves grants from the authorization index (merging machine
// defaults, room-level grants, and per-principal policy) and pushes them
// to the proxy via the admin socket.
func (h *Handler) SetGrants(grants []schema.Grant) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.grants = grants
	h.logger.Info("authorization grants configured", "grants", len(grants))
}

// HandleGrants returns the principal's authorization grants. The MCP
// server calls this on startup to determine which tools the agent is
// permitted to invoke.
func (h *Handler) HandleGrants(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	grants := h.grants
	h.mu.RUnlock()

	// Return empty array, not null, when there are no grants.
	if grants == nil {
		grants = []schema.Grant{}
	}

	h.writeJSON(w, grants)
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

	// The raw /http/matrix/ passthrough is for third-party Matrix client
	// libraries that need direct homeserver access. Sandboxed agents use
	// the structured /v1/matrix/* endpoints by default — the passthrough
	// requires an explicit grant.
	if serviceName == matrixServiceName {
		if !h.requireGrant(w, "matrix/raw-api") {
			return
		}
		// Additional Matrix-specific policy: block join/invite/create without
		// the appropriate grants.
		if blocked, reason := h.checkMatrixPolicy(r.Method, servicePath); blocked {
			http.Error(w, reason, http.StatusForbidden)
			return
		}
	}

	// Rewrite the URL path for the service handler
	r.URL.Path = servicePath

	// Delegate to the HTTP service
	service.ServeHTTP(w, r)
}

// requireGrant checks that the agent's pre-resolved grants allow the given
// action. If denied, writes a 403 JSON error response and returns false.
// If allowed, returns true without writing anything.
//
// Used by structured /v1/matrix/* endpoints that need grant enforcement
// (create-room, join, invite). Uses the same grant evaluation as
// checkMatrixPolicy but with a cleaner interface for handlers that know
// their action statically.
func (h *Handler) requireGrant(w http.ResponseWriter, action string) bool {
	h.mu.RLock()
	grants := h.grants
	h.mu.RUnlock()

	result := authorization.GrantsCheck(grants, action, ref.UserID{})
	if result.Allowed {
		return true
	}

	h.logger.Warn("authorization denied",
		"action", action,
	)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	json.NewEncoder(w).Encode(matrixErrorResponse{
		Error: fmt.Sprintf("authorization: no grant for action %q", action),
		Code:  "M_FORBIDDEN",
	})
	return false
}

// checkMatrixPolicy checks whether a raw Matrix passthrough request is allowed
// by the agent's grants. Returns (true, reason) if the request should be
// blocked, (false, "") if allowed. Only used for /http/matrix/ passthrough —
// the structured /v1/matrix/* endpoints have their own grant checks.
//
// The gated endpoints are:
//   - POST /_matrix/client/v3/join/{roomIdOrAlias}   — matrix/join
//   - POST /_matrix/client/v3/rooms/{roomId}/join     — matrix/join
//   - POST /_matrix/client/v3/rooms/{roomId}/invite   — matrix/invite
//   - POST /_matrix/client/v3/createRoom              — matrix/create-room
//
// Everything else (messages, state, sync, room history) is allowed — the
// homeserver enforces room membership and power levels for those operations.
func (h *Handler) checkMatrixPolicy(method, path string) (blocked bool, reason string) {
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

	// Determine the authorization action from the Matrix API endpoint.
	action := h.matrixAPIAction(apiPath)
	if action == "" {
		// Unrecognized endpoint — allow through, homeserver handles authz.
		return false, ""
	}

	h.mu.RLock()
	grants := h.grants
	h.mu.RUnlock()

	// Matrix operations are self-service (empty target) — the principal
	// is acting on infrastructure, not on another principal.
	result := authorization.GrantsCheck(grants, action, ref.UserID{})
	if result.Allowed {
		return false, ""
	}
	h.logger.Warn("authorization denied matrix request",
		"action", action,
		"method", method,
		"path", path,
	)
	return true, fmt.Sprintf("authorization: no grant for action %q", action)
}

// matrixAPIAction maps a Matrix client API path (after the /v3/ prefix)
// to a Bureau authorization action. Returns empty string for endpoints
// that are not gated by Bureau authorization.
func (h *Handler) matrixAPIAction(apiPath string) string {
	// POST /_matrix/client/v3/createRoom
	if apiPath == "createRoom" {
		return "matrix/create-room"
	}

	// POST /_matrix/client/v3/join/{roomIdOrAlias}
	if strings.HasPrefix(apiPath, "join/") {
		return "matrix/join"
	}

	// POST /_matrix/client/v3/rooms/{roomId}/join
	// POST /_matrix/client/v3/rooms/{roomId}/invite
	if strings.HasPrefix(apiPath, "rooms/") {
		parts := strings.Split(apiPath, "/")
		if len(parts) >= 3 {
			switch parts[len(parts)-1] {
			case "join":
				return "matrix/join"
			case "invite":
				return "matrix/invite"
			}
		}
	}

	return ""
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
//  1. Authorization: each entry is checked against the agent's grants for
//     the service/discover action. Default-deny: no matching grant means
//     the service is not visible.
//  2. Query parameters: further filter by protocol and/or capability.
//
// Optional query parameters:
//   - capability: only entries with this capability
//   - protocol: only entries with this wire protocol
func (h *Handler) HandleServiceDirectory(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	directory := h.serviceDirectory
	grants := h.grants
	h.mu.RUnlock()

	if directory == nil {
		directory = []ServiceDirectoryEntry{}
	}

	// Stage 1: authorization filtering. Each service entry is checked
	// against the agent's grants for the service/discover action.
	// Default-deny: if no grants match, the service is not visible.
	//
	// Service discovery uses localpart-level matching (GrantsAllowServiceType)
	// because ServiceVisibility patterns match service TYPE localparts
	// (e.g., "service/stt/*" matches "service/stt/test"), not full user IDs.
	visible := make([]ServiceDirectoryEntry, 0, len(directory))
	for _, entry := range directory {
		if authorization.GrantsAllowServiceType(grants, "service/discover", entry.Localpart) {
			visible = append(visible, entry)
		}
	}
	directory = visible

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

// HandleAdminSetAuthorization replaces the pre-resolved authorization grants
// for this proxy. The daemon pushes grants via PUT /v1/admin/authorization
// when the sandbox starts or when the principal's authorization changes at
// runtime (config change, temporal grant arrival/expiry).
func (h *Handler) HandleAdminSetAuthorization(w http.ResponseWriter, r *http.Request) {
	var grants []schema.Grant
	if err := json.NewDecoder(r.Body).Decode(&grants); err != nil {
		h.sendError(w, http.StatusBadRequest, "invalid authorization payload: %v", err)
		return
	}

	h.SetGrants(grants)

	h.writeJSON(w, map[string]any{
		"status": "ok",
		"grants": len(grants),
	})
}
