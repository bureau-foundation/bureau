// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/bureau-foundation/bureau/lib/netutil"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/secret"
)

// ClientConfig holds configuration for creating a Client.
type ClientConfig struct {
	// HomeserverURL is the base URL of the Matrix homeserver (e.g., "http://localhost:6167").
	HomeserverURL string
	// HTTPClient is used for all requests. If nil, http.DefaultClient is used.
	HTTPClient *http.Client
	// Logger is used for structured logging. If nil, slog.Default() is used.
	Logger *slog.Logger
}

// Client is an unauthenticated Matrix client.
// It holds the homeserver URL and HTTP transport, shared across Sessions.
type Client struct {
	baseURL    string
	httpClient *http.Client
	logger     *slog.Logger
}

// NewClient creates a new unauthenticated Matrix client.
func NewClient(config ClientConfig) (*Client, error) {
	if config.HomeserverURL == "" {
		return nil, fmt.Errorf("messaging: HomeserverURL is required")
	}

	// Validate the URL structure. We store the string form (with trailing
	// slash stripped) and build request URLs by direct concatenation. This
	// avoids double-encoding issues with Go's url.URL.String(), which
	// re-encodes Path even when RawPath is set if it doesn't consider
	// RawPath a valid encoding of Path.
	if _, err := url.Parse(config.HomeserverURL); err != nil {
		return nil, fmt.Errorf("messaging: invalid HomeserverURL %q: %w", config.HomeserverURL, err)
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Client{
		baseURL:    strings.TrimRight(config.HomeserverURL, "/"),
		httpClient: httpClient,
		logger:     logger,
	}, nil
}

// CloseIdleConnections closes idle HTTP connections in the underlying
// transport's connection pool. Call this after a network disruption to
// force subsequent requests to establish fresh TCP connections instead
// of reusing a poisoned pooled connection.
func (c *Client) CloseIdleConnections() {
	c.httpClient.CloseIdleConnections()
}

// ServerVersions returns the Matrix protocol versions and unstable features
// supported by the homeserver. This is an unauthenticated endpoint — useful
// for checking whether the homeserver is reachable and what it supports.
func (c *Client) ServerVersions(ctx context.Context) (*ServerVersionsResponse, error) {
	body, err := c.doRequest(ctx, http.MethodGet, "/_matrix/client/versions", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("messaging: server versions failed: %w", err)
	}

	var response ServerVersionsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("messaging: failed to parse versions response: %w", err)
	}
	return &response, nil
}

// Register creates a new account using token-authenticated registration (MSC3231).
// Returns a DirectSession for the newly created account.
//
// The registration flow uses the User-Interactive Authentication API (UIAA):
//   - First request returns 401 with available flows.
//   - Second request includes the auth stage with the registration token.
func (c *Client) Register(ctx context.Context, request RegisterRequest) (*DirectSession, error) {
	if request.Username == "" {
		return nil, fmt.Errorf("messaging: username is required for registration")
	}
	if request.Password == nil {
		return nil, fmt.Errorf("messaging: password is required for registration")
	}

	// Matrix registration uses the UIAA flow. First attempt without auth
	// to get the session ID, then complete with the registration token.
	//
	// Password is converted to string at the JSON serialization boundary.
	// The heap copy is short-lived — it exists only during the HTTP call.
	firstAttempt := map[string]any{
		"username": request.Username,
		"password": request.Password.String(),
	}
	body, err := c.doRequest(ctx, http.MethodPost, "/_matrix/client/v3/register", nil, firstAttempt)
	if err == nil {
		// Registration succeeded without UIAA (unlikely but possible if server
		// has no auth requirements).
		var authResponse AuthResponse
		if parseErr := json.Unmarshal(body, &authResponse); parseErr != nil {
			return nil, fmt.Errorf("messaging: failed to parse register response: %w", parseErr)
		}
		return c.sessionFromAuth(&authResponse)
	}

	// Expect 401 with UIAA session.
	if !isUnauthorizedUIAA(err) {
		return nil, fmt.Errorf("messaging: registration failed: %w", err)
	}

	// Extract the session ID from the 401 response.
	// The body is returned alongside the error by doRequest.
	sessionID, err := extractUIAASession(body)
	if err != nil {
		return nil, err
	}

	// Complete registration with the token auth stage.
	auth := map[string]any{
		"type":    "m.login.registration_token",
		"token":   request.RegistrationToken.String(),
		"session": sessionID,
	}
	completeRequest := map[string]any{
		"username": request.Username,
		"password": request.Password.String(),
		"auth":     auth,
	}
	body, err = c.doRequest(ctx, http.MethodPost, "/_matrix/client/v3/register", nil, completeRequest)
	if err != nil {
		return nil, fmt.Errorf("messaging: registration failed: %w", err)
	}

	var authResponse AuthResponse
	if err := json.Unmarshal(body, &authResponse); err != nil {
		return nil, fmt.Errorf("messaging: failed to parse register response: %w", err)
	}

	c.logger.Info("registered matrix account",
		"user_id", authResponse.UserID,
		"device_id", authResponse.DeviceID,
	)

	return c.sessionFromAuth(&authResponse)
}

// Login authenticates with username and password, returning a DirectSession.
// The password Buffer is read but not closed — the caller retains ownership.
func (c *Client) Login(ctx context.Context, username string, password *secret.Buffer) (*DirectSession, error) {
	if username == "" {
		return nil, fmt.Errorf("messaging: username is required for login")
	}
	if password == nil {
		return nil, fmt.Errorf("messaging: password is required for login")
	}

	// Password is converted to string at the JSON serialization boundary.
	loginRequest := LoginRequest{
		Type:                     "m.login.password",
		User:                     username,
		Password:                 password.String(),
		InitialDeviceDisplayName: "bureau",
	}

	body, err := c.doRequest(ctx, http.MethodPost, "/_matrix/client/v3/login", nil, loginRequest)
	if err != nil {
		return nil, fmt.Errorf("messaging: login failed: %w", err)
	}

	var authResponse AuthResponse
	if err := json.Unmarshal(body, &authResponse); err != nil {
		return nil, fmt.Errorf("messaging: failed to parse login response: %w", err)
	}

	c.logger.Info("logged in to matrix",
		"user_id", authResponse.UserID,
		"device_id", authResponse.DeviceID,
	)

	return c.sessionFromAuth(&authResponse)
}

// ProxySession creates a DirectSession that relies on an external proxy for
// credential injection. No access token is stored — the proxy intercepts
// outgoing requests and adds the Authorization header. This is the
// connection path for code running inside a Bureau sandbox, where
// credentials are never exposed to the agent.
//
// userID must be the fully-qualified Matrix user ID (e.g., "@alice:bureau.local"),
// typically obtained from the proxy's /v1/identity endpoint.
func (c *Client) ProxySession(userID ref.UserID) *DirectSession {
	return &DirectSession{
		client: c,
		userID: userID,
	}
}

// SessionFromToken creates a DirectSession from an existing access token string.
// The token is moved into mmap-backed memory (locked against swap, excluded
// from core dumps). The original string remains on the heap briefly — it will
// be collected by the GC, but the mmap buffer is the durable copy.
//
// This does NOT validate the token — the first API call will fail if invalid.
// userID must be the fully-qualified Matrix user ID (e.g., "@alice:bureau.local").
//
// The caller must call Close on the returned DirectSession when done.
func (c *Client) SessionFromToken(userID ref.UserID, accessToken string) (*DirectSession, error) {
	tokenBuffer, err := secret.NewFromBytes([]byte(accessToken))
	if err != nil {
		return nil, fmt.Errorf("messaging: protecting access token: %w", err)
	}
	return &DirectSession{
		client:      c,
		accessToken: tokenBuffer,
		userID:      userID,
	}, nil
}

func (c *Client) sessionFromAuth(auth *AuthResponse) (*DirectSession, error) {
	tokenBuffer, err := secret.NewFromBytes([]byte(auth.AccessToken))
	if err != nil {
		return nil, fmt.Errorf("messaging: protecting access token: %w", err)
	}
	return &DirectSession{
		client:      c,
		accessToken: tokenBuffer,
		userID:      auth.UserID,
		deviceID:    auth.DeviceID,
	}, nil
}

// doRequest performs an HTTP request to the homeserver and returns the response body.
// On 2xx, returns the body. On 4xx/5xx, returns a *MatrixError.
// accessToken may be nil for unauthenticated endpoints.
// query may be nil for endpoints without query parameters.
func (c *Client) doRequest(ctx context.Context, method, path string, accessToken *secret.Buffer, requestBody any, query ...url.Values) ([]byte, error) {
	requestURL := c.baseURL + path
	if len(query) > 0 && query[0] != nil {
		requestURL += "?" + query[0].Encode()
	}

	var bodyReader io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return nil, fmt.Errorf("messaging: failed to encode request body: %w", err)
		}
		bodyReader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, requestURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("messaging: failed to create request: %w", err)
	}

	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if accessToken != nil {
		request.Header.Set("Authorization", "Bearer "+accessToken.String())
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("messaging: request to %s %s failed: %w", method, path, err)
	}
	defer response.Body.Close()

	responseBody, err := netutil.ReadResponse(response.Body)
	if err != nil {
		return nil, fmt.Errorf("messaging: failed to read response body: %w", err)
	}

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return responseBody, nil
	}

	// All Matrix error responses use the same JSON shape.
	var matrixErr MatrixError
	if jsonErr := json.Unmarshal(responseBody, &matrixErr); jsonErr != nil {
		// Server returned non-JSON error. This should not happen with a
		// spec-compliant server, but fail loud with the raw body.
		return nil, fmt.Errorf("messaging: unexpected %d response from %s %s: %s",
			response.StatusCode, method, path, string(responseBody))
	}
	matrixErr.StatusCode = response.StatusCode

	return responseBody, &matrixErr
}

// doRequestRaw performs an HTTP request with a raw body (for media upload).
func (c *Client) doRequestRaw(ctx context.Context, method, path string, accessToken *secret.Buffer, contentType string, body io.Reader) ([]byte, error) {
	requestURL := c.baseURL + path

	request, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return nil, fmt.Errorf("messaging: failed to create request: %w", err)
	}

	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if accessToken != nil {
		request.Header.Set("Authorization", "Bearer "+accessToken.String())
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("messaging: request to %s %s failed: %w", method, path, err)
	}
	defer response.Body.Close()

	responseBody, err := netutil.ReadResponse(response.Body)
	if err != nil {
		return nil, fmt.Errorf("messaging: failed to read response body: %w", err)
	}

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return responseBody, nil
	}

	var matrixErr MatrixError
	if jsonErr := json.Unmarshal(responseBody, &matrixErr); jsonErr != nil {
		return nil, fmt.Errorf("messaging: unexpected %d response from %s %s: %s",
			response.StatusCode, method, path, string(responseBody))
	}
	matrixErr.StatusCode = response.StatusCode

	return nil, &matrixErr
}

// isUnauthorizedUIAA checks if an error is a 401 from the UIAA flow.
// This is the expected response when registration requires authentication stages.
func isUnauthorizedUIAA(err error) bool {
	var matrixErr *MatrixError
	if err == nil {
		return false
	}
	switch e := err.(type) { //nolint:errorlint // direct type assertion, no wrapping
	case *MatrixError:
		matrixErr = e
	default:
		return false
	}
	return matrixErr.StatusCode == http.StatusUnauthorized
}

// extractUIAASession extracts the session ID from a UIAA 401 response.
// The 401 response body is returned alongside the error by doRequest.
func extractUIAASession(body []byte) (string, error) {
	// The UIAA 401 response has a "session" field at the top level.
	var uiaaResponse struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(body, &uiaaResponse); err != nil {
		return "", fmt.Errorf("messaging: failed to parse UIAA response: %w", err)
	}
	if uiaaResponse.Session == "" {
		return "", fmt.Errorf("messaging: UIAA response missing session ID")
	}
	return uiaaResponse.Session, nil
}
