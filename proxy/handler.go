// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Maximum request body size (64KB is plenty for CLI args)
const maxRequestBodySize = 64 * 1024

// Handler handles HTTP requests to the proxy server.
type Handler struct {
	services     map[string]Service      // CLI services (JSON-RPC style)
	httpServices map[string]*HTTPService // HTTP proxy services
	mu           sync.RWMutex
	logger       *slog.Logger
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

// HandleHealth handles health check requests.
func (h *Handler) HandleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Response{
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
		encoder.Encode(StreamChunk{Type: "error", Data: err.Error()})
		flusher.Flush()
		return
	}

	// Send exit code as final chunk
	encoder.Encode(StreamChunk{Type: "exit", Code: exitCode})
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
	json.NewEncoder(w).Encode(Response{
		ExitCode: -1,
		Error:    fmt.Sprintf(format, args...),
	})
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

	// Rewrite the URL path for the service handler
	r.URL.Path = servicePath

	// Delegate to the HTTP service
	service.ServeHTTP(w, r)
}
