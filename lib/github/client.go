// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/netutil"
)

// githubAPIVersion is the GitHub REST API version header. Pinning the
// version ensures consistent behavior as GitHub evolves the API.
const githubAPIVersion = "2022-11-28"

// defaultBaseURL is the base URL for the public GitHub API.
const defaultBaseURL = "https://api.github.com"

// Config holds configuration for creating a GitHub API Client.
//
// Exactly one authentication mode must be configured:
//   - App authentication: set AppID, PrivateKey, and InstallationID
//   - Token authentication: set Token
type Config struct {
	// BaseURL is the root URL for API requests. Defaults to
	// "https://api.github.com". Must use HTTPS.
	BaseURL string

	// AppID is the GitHub App's numeric ID. Required for App auth.
	AppID int64

	// PrivateKey is the PEM-encoded RSA private key for the GitHub App.
	// Required for App auth.
	PrivateKey []byte

	// InstallationID is the GitHub App installation's numeric ID.
	// Required for App auth.
	InstallationID int64

	// Token is a personal access token or fine-grained token. Required
	// for token auth. Mutually exclusive with App auth fields.
	Token string

	// HTTPClient is used for all HTTP requests. Defaults to
	// http.DefaultClient.
	HTTPClient *http.Client

	// Clock provides time operations. Defaults to clock.Real().
	// Inject clock.Fake() in tests for deterministic behavior.
	Clock clock.Clock

	// Logger is used for structured logging. Defaults to slog.Default().
	Logger *slog.Logger
}

// Client is a typed GitHub REST API client with automatic authentication,
// rate limiting, pagination, ETag caching, and structured error handling.
type Client struct {
	baseURL    string
	httpClient *http.Client
	auth       authenticator
	rateLimit  *rateLimitTracker
	etagCache  *etagCache
	clock      clock.Clock
	logger     *slog.Logger
}

// NewClient creates a GitHub API client from the given configuration.
// Returns an error if the configuration is invalid (bad auth config,
// non-HTTPS URL, unparseable private key).
func NewClient(config Config) (*Client, error) {
	// Resolve defaults.
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	// Enforce HTTPS.
	if !strings.HasPrefix(baseURL, "https://") {
		return nil, fmt.Errorf("github: API client requires HTTPS (got %q)", baseURL)
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	clk := config.Clock
	if clk == nil {
		clk = clock.Real()
	}

	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Validate auth configuration: exactly one mode.
	hasApp := config.AppID != 0 || len(config.PrivateKey) > 0 || config.InstallationID != 0
	hasToken := config.Token != ""

	if hasApp && hasToken {
		return nil, fmt.Errorf("github: cannot configure both App auth and token auth")
	}
	if !hasApp && !hasToken {
		return nil, fmt.Errorf("github: no authentication configured (set AppID+PrivateKey+InstallationID or Token)")
	}

	var auth authenticator
	if hasApp {
		// Validate all App fields are present.
		if config.AppID == 0 {
			return nil, fmt.Errorf("github: AppID is required for App auth")
		}
		if len(config.PrivateKey) == 0 {
			return nil, fmt.Errorf("github: PrivateKey is required for App auth")
		}
		if config.InstallationID == 0 {
			return nil, fmt.Errorf("github: InstallationID is required for App auth")
		}

		appAuth, err := newAppAuth(config.AppID, config.InstallationID, config.PrivateKey, clk)
		if err != nil {
			return nil, err
		}
		// Wire the HTTP transport for token exchange requests.
		appAuth.httpClient = httpClient
		appAuth.baseURL = baseURL
		auth = appAuth
	} else {
		auth = newTokenAuth(config.Token)
	}

	return &Client{
		baseURL:    baseURL,
		httpClient: httpClient,
		auth:       auth,
		rateLimit:  newRateLimitTracker(clk),
		etagCache:  newETagCache(),
		clock:      clk,
		logger:     logger,
	}, nil
}

// do executes an authenticated GitHub API request. Handles rate limit
// waiting, authentication, ETag caching, and error parsing. The path
// should be relative to the base URL (e.g., "/repos/owner/repo/issues").
//
// For GET requests, ETag caching is applied. For non-GET requests, the
// body is JSON-encoded from the provided value (pass nil for no body).
//
// Returns the parsed response body as raw bytes. On non-2xx responses,
// returns an *APIError.
func (client *Client) do(ctx context.Context, method, path string, requestBody any) ([]byte, http.Header, error) {
	return client.doWithRetry(ctx, method, path, requestBody, false)
}

// doWithRetry is the internal implementation of do with a retry flag
// to prevent infinite recursion on persistent rate limiting.
func (client *Client) doWithRetry(ctx context.Context, method, path string, requestBody any, isRetry bool) ([]byte, http.Header, error) {
	url := client.baseURL + path
	response, err := client.doRaw(ctx, method, url, requestBody)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()

	// Rate limit tracker is already updated by doRaw.

	// Handle 304 Not Modified — return cached body.
	if response.StatusCode == http.StatusNotModified {
		cached := client.etagCache.body(url)
		if cached != nil {
			return cached, response.Header, nil
		}
		// Cache miss on 304 — should not happen, but fall through to
		// read the (empty) response body rather than failing silently.
	}

	// Read response body.
	body, err := netutil.ReadResponse(response.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("github: reading response body: %w", err)
	}

	// Handle non-2xx responses.
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Check for rate limit — attempt one retry after backoff.
		// Only retry once to avoid infinite loops on persistent rate limiting.
		if !isRetry && (response.StatusCode == 429 || (response.StatusCode == 403 && isRateLimitMessage(string(body)))) {
			retryDuration := client.rateLimit.retryAfter(response.Header)
			if retryDuration > 0 {
				client.logger.Info("rate limited, backing off",
					"duration", retryDuration,
					"method", method,
					"path", path,
				)

				select {
				case <-client.clock.After(retryDuration):
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				}

				return client.doWithRetry(ctx, method, path, requestBody, true)
			}
		}

		return nil, nil, parseAPIErrorFromBody(response.StatusCode, body)
	}

	// Cache ETag for GET responses.
	if method == http.MethodGet {
		if etag := response.Header.Get("ETag"); etag != "" {
			client.etagCache.put(url, etag, body)
		}
	}

	return body, response.Header, nil
}

// doRaw executes an HTTP request with authentication and rate limit
// waiting, but without response parsing. Returns the raw *http.Response.
// The caller is responsible for closing the response body.
//
// This is used by both do() (for standard requests) and PageIterator
// (which needs access to the Link header before parsing the body).
func (client *Client) doRaw(ctx context.Context, method, url string, requestBody any) (*http.Response, error) {
	// Preemptive rate limit check.
	if err := client.rateLimit.wait(ctx); err != nil {
		return nil, err
	}

	// Build the request body.
	var bodyReader io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return nil, fmt.Errorf("github: encoding request body: %w", err)
		}
		bodyReader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("github: creating request: %w", err)
	}

	// Authentication.
	authHeader, err := client.auth.AuthorizationHeader(ctx)
	if err != nil {
		return nil, fmt.Errorf("github: authentication: %w", err)
	}
	request.Header.Set("Authorization", authHeader)

	// Standard GitHub headers.
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	// ETag for conditional GET requests.
	if method == http.MethodGet {
		if etag := client.etagCache.get(url); etag != "" {
			request.Header.Set("If-None-Match", etag)
		}
	}

	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, url, err)
	}

	// Update rate limit tracker from every response.
	client.rateLimit.update(response.Header)

	return response, nil
}

// get is a convenience method for GET requests that return a single JSON
// object. Decodes the response into result.
func (client *Client) get(ctx context.Context, path string, result any) error {
	body, _, err := client.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, result)
}

// post is a convenience method for POST requests that return a JSON
// object. Decodes the response into result.
func (client *Client) post(ctx context.Context, path string, requestBody any, result any) error {
	body, _, err := client.do(ctx, http.MethodPost, path, requestBody)
	if err != nil {
		return err
	}
	if result != nil {
		return json.Unmarshal(body, result)
	}
	return nil
}

// patch is a convenience method for PATCH requests that return a JSON
// object. Decodes the response into result.
func (client *Client) patch(ctx context.Context, path string, requestBody any, result any) error {
	body, _, err := client.do(ctx, http.MethodPatch, path, requestBody)
	if err != nil {
		return err
	}
	if result != nil {
		return json.Unmarshal(body, result)
	}
	return nil
}

// delete is a convenience method for DELETE requests.
func (client *Client) delete(ctx context.Context, path string) error {
	_, _, err := client.do(ctx, http.MethodDelete, path, nil)
	return err
}

// listOptions is implemented by option structs for paginated list
// endpoints. Provides the common query parameters (state, sort, per_page)
// that most GitHub list endpoints accept.
type listOptions interface {
	queryParams() string
}

// list creates a PageIterator for a paginated GET endpoint.
func list[T any](client *Client, path string) *PageIterator[T] {
	return &PageIterator[T]{
		client:  client,
		nextURL: client.baseURL + path,
	}
}

// buildListPath constructs a path with query parameters from list options.
// The basePath should not include a trailing "?" — this function appends
// the query string only if there are parameters.
func buildListPath(basePath string, options listOptions) string {
	query := options.queryParams()
	if query == "" {
		return basePath
	}
	return basePath + "?" + query
}

// parseAPIError reads a GitHub API error from an HTTP response.
func parseAPIError(response *http.Response) *APIError {
	body, _ := netutil.ReadResponse(response.Body)
	return parseAPIErrorFromBody(response.StatusCode, body)
}

// parseAPIErrorFromBody parses a GitHub API error from a status code
// and response body.
func parseAPIErrorFromBody(statusCode int, body []byte) *APIError {
	apiError := &APIError{StatusCode: statusCode}

	var wireError struct {
		Message          string            `json:"message"`
		DocumentationURL string            `json:"documentation_url"`
		Errors           []ValidationError `json:"errors"`
	}
	if json.Unmarshal(body, &wireError) == nil && wireError.Message != "" {
		apiError.Message = wireError.Message
		apiError.DocumentationURL = wireError.DocumentationURL
		apiError.Errors = wireError.Errors
	} else {
		apiError.Message = string(body)
	}

	return apiError
}
