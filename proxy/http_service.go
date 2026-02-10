// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPService proxies HTTP requests to an upstream API with credential injection.
// Used for SDK-based API access (OpenAI, Anthropic, etc.) where agents need
// API keys but shouldn't see them directly.
type HTTPService struct {
	name          string
	upstream      *url.URL
	injectHeaders map[string]string // header name -> credential name
	stripHeaders  []string          // headers to remove from incoming requests
	filter        Filter            // optional path filtering
	credential    CredentialSource
	logger        *slog.Logger
	client        *http.Client
}

// HTTPServiceConfig holds configuration for creating an HTTPService.
type HTTPServiceConfig struct {
	// Name is the service name used in routing and logging.
	Name string

	// Upstream is the target URL (e.g., "https://api.openai.com").
	// For Unix socket upstreams, set UpstreamUnix instead and leave this
	// empty — the upstream URL will default to "http://localhost".
	Upstream string

	// UpstreamUnix is the path to a Unix socket to use as the upstream.
	// When set, all HTTP requests are sent over this socket. The Upstream
	// field (or "http://localhost" if empty) is used only for path
	// construction and the Host header.
	//
	// This enables local service routing: the daemon configures a proxy
	// to forward requests to another principal's proxy socket.
	UpstreamUnix string

	// InjectHeaders maps header names to credential names.
	// Example: {"Authorization": "openai-bearer"} injects the "openai-bearer"
	// credential as the Authorization header.
	InjectHeaders map[string]string

	// StripHeaders lists headers to remove from incoming requests.
	StripHeaders []string

	// Filter validates request paths before forwarding.
	// If nil, all paths are allowed.
	Filter Filter

	// Credential provides credential values.
	Credential CredentialSource

	// Logger for request logging.
	Logger *slog.Logger
}

// NewHTTPService creates a new HTTP proxy service.
func NewHTTPService(config HTTPServiceConfig) (*HTTPService, error) {
	if config.Name == "" {
		return nil, fmt.Errorf("service name is required")
	}
	if config.Upstream == "" && config.UpstreamUnix == "" {
		return nil, fmt.Errorf("upstream URL or upstream unix socket is required")
	}

	// For Unix socket upstreams, default the URL to http://localhost so path
	// construction works. All actual I/O goes through the socket.
	upstreamURL := config.Upstream
	if upstreamURL == "" {
		upstreamURL = "http://localhost"
	}

	upstream, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream URL: %w", err)
	}

	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Create HTTP client with reasonable timeouts.
	// No timeout on response body reading - SSE streams can be long-lived.
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
	}

	// For Unix socket upstreams, override the dialer to connect to the socket
	// regardless of the hostname in the URL.
	if config.UpstreamUnix != "" {
		socketPath := config.UpstreamUnix
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}
	}

	client := &http.Client{
		Timeout:   0, // No overall timeout - SSE streams are long-lived
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &HTTPService{
		name:          config.Name,
		upstream:      upstream,
		injectHeaders: config.InjectHeaders,
		stripHeaders:  config.StripHeaders,
		filter:        config.Filter,
		credential:    config.Credential,
		logger:        logger,
		client:        client,
	}, nil
}

// Name returns the service name.
func (s *HTTPService) Name() string {
	return s.name
}

// ServeHTTP handles proxied HTTP requests.
// This implements http.Handler for direct use with HTTP routers.
func (s *HTTPService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	// Check path filter if configured
	if s.filter != nil {
		// Use method + path for filtering (e.g., "POST /v1/chat/completions")
		filterInput := []string{r.Method, r.URL.Path}
		if err := s.filter.Check(filterInput); err != nil {
			s.logger.Warn("http request blocked",
				"service", s.name,
				"method", r.Method,
				"path", r.URL.Path,
				"error", err,
			)
			http.Error(w, fmt.Sprintf("request blocked: %v", err), http.StatusForbidden)
			return
		}
	}

	// Validate credentials are available before making request
	var missingCredentials []string
	for _, credName := range s.injectHeaders {
		if s.credential == nil || s.credential.Get(credName) == nil {
			missingCredentials = append(missingCredentials, credName)
		}
	}
	if len(missingCredentials) > 0 {
		s.logger.Error("missing credentials for http service",
			"service", s.name,
			"missing", missingCredentials,
		)
		http.Error(w, fmt.Sprintf("missing credentials: %v", missingCredentials), http.StatusServiceUnavailable)
		return
	}

	// Build upstream URL
	upstreamURL := *s.upstream
	upstreamURL.Path = singleJoiningSlash(s.upstream.Path, r.URL.Path)
	upstreamURL.RawQuery = r.URL.RawQuery

	// Create upstream request
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), r.Body)
	if err != nil {
		s.logger.Error("failed to create upstream request",
			"service", s.name,
			"error", err,
		)
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	// Copy headers from original request
	for key, values := range r.Header {
		// Skip hop-by-hop headers
		if isHopByHopHeader(key) {
			continue
		}
		// Skip headers we're going to strip
		if s.shouldStripHeader(key) {
			continue
		}
		for _, value := range values {
			upstreamReq.Header.Add(key, value)
		}
	}

	// Inject credential headers
	for headerName, credName := range s.injectHeaders {
		value := s.credential.Get(credName)
		upstreamReq.Header.Set(headerName, value.String())
	}

	// Set standard proxy headers
	if clientIP := r.RemoteAddr; clientIP != "" {
		// Don't expose internal IPs - just note that it's proxied
		upstreamReq.Header.Set("X-Forwarded-For", "bureau-proxy")
	}

	s.logger.Info("http proxy request",
		"service", s.name,
		"method", r.Method,
		"path", r.URL.Path,
		"upstream", upstreamURL.String(),
	)

	// Make the upstream request
	resp, err := s.client.Do(upstreamReq)
	if err != nil {
		s.logger.Error("upstream request failed",
			"service", s.name,
			"error", err,
			"duration", time.Since(startTime),
		)
		http.Error(w, fmt.Sprintf("upstream request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for key, values := range resp.Header {
		if isHopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// Check if this is an SSE response
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	if isSSE {
		// For SSE, we need to flush after each chunk
		s.streamSSE(w, resp, startTime)
	} else {
		// For regular responses, just copy
		w.WriteHeader(resp.StatusCode)
		bytesCopied, _ := io.Copy(w, resp.Body)

		s.logger.Info("http proxy complete",
			"service", s.name,
			"method", r.Method,
			"path", r.URL.Path,
			"status", resp.StatusCode,
			"bytes", bytesCopied,
			"duration", time.Since(startTime),
		)
	}
}

// streamSSE handles Server-Sent Events streaming responses.
// It reads from the upstream and flushes each chunk immediately to the client.
func (s *HTTPService) streamSSE(w http.ResponseWriter, resp *http.Response, startTime time.Time) {
	// Ensure we can flush
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.logger.Error("streaming not supported by response writer")
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Set headers for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	// Stream the response
	// Use a small buffer to minimize latency while still being efficient
	buffer := make([]byte, 4096)
	var totalBytes int64

	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			written, writeErr := w.Write(buffer[:n])
			if writeErr != nil {
				s.logger.Warn("client disconnected during SSE stream",
					"service", s.name,
					"bytes_sent", totalBytes,
					"duration", time.Since(startTime),
				)
				return
			}
			totalBytes += int64(written)
			flusher.Flush()
		}
		if err != nil {
			if err != io.EOF {
				s.logger.Warn("upstream error during SSE stream",
					"service", s.name,
					"error", err,
					"bytes_sent", totalBytes,
					"duration", time.Since(startTime),
				)
			}
			break
		}
	}

	s.logger.Info("http proxy SSE complete",
		"service", s.name,
		"status", resp.StatusCode,
		"bytes", totalBytes,
		"duration", time.Since(startTime),
	)
}

// shouldStripHeader checks if a header should be stripped from incoming requests.
func (s *HTTPService) shouldStripHeader(name string) bool {
	nameLower := strings.ToLower(name)
	for _, strip := range s.stripHeaders {
		if strings.ToLower(strip) == nameLower {
			return true
		}
	}
	return false
}

// isHopByHopHeader returns true for headers that should not be forwarded.
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

// singleJoiningSlash joins two URL paths with a single slash.
func singleJoiningSlash(a, b string) string {
	aSlash := strings.HasSuffix(a, "/")
	bSlash := strings.HasPrefix(b, "/")
	switch {
	case aSlash && bSlash:
		return a + b[1:]
	case !aSlash && !bSlash:
		return a + "/" + b
	}
	return a + b
}
