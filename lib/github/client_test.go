// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
)

// newTestClient creates a Client backed by the given httptest.Server.
// Uses token auth for simplicity.
func newTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := NewClient(Config{
		BaseURL:    server.URL,
		Token:      "test-token",
		HTTPClient: server.Client(),
		Clock:      clock.Real(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func TestNewClient_HTTPSEnforcement(t *testing.T) {
	_, err := NewClient(Config{
		BaseURL: "http://api.github.com",
		Token:   "test",
	})
	if err == nil {
		t.Fatal("expected error for HTTP URL")
	}
	if got := err.Error(); got != `github: API client requires HTTPS (got "http://api.github.com")` {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestNewClient_MutuallyExclusiveAuth(t *testing.T) {
	_, err := NewClient(Config{
		BaseURL:        "https://api.github.com",
		Token:          "test",
		AppID:          1,
		PrivateKey:     testRSAPrivateKeyPEM,
		InstallationID: 1,
	})
	if err == nil {
		t.Fatal("expected error for both auth modes")
	}
}

func TestNewClient_NoAuth(t *testing.T) {
	_, err := NewClient(Config{
		BaseURL: "https://api.github.com",
	})
	if err == nil {
		t.Fatal("expected error for no auth")
	}
}

func TestNewClient_PartialAppAuth(t *testing.T) {
	_, err := NewClient(Config{
		BaseURL: "https://api.github.com",
		AppID:   1,
		// Missing PrivateKey and InstallationID.
	})
	if err == nil {
		t.Fatal("expected error for partial App auth")
	}
}

func TestClient_AuthHeaderInjection(t *testing.T) {
	var receivedAuth string
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedAuth = request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(`{"number":1,"title":"Test"}`))
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.GetIssue(context.Background(), "owner", "repo", 1)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}

	if receivedAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", receivedAuth, "Bearer test-token")
	}
}

func TestClient_GitHubHeaders(t *testing.T) {
	var receivedAccept, receivedVersion string
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedAccept = request.Header.Get("Accept")
		receivedVersion = request.Header.Get("X-GitHub-Api-Version")
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(`{"number":1}`))
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.GetIssue(context.Background(), "owner", "repo", 1)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}

	if receivedAccept != "application/vnd.github+json" {
		t.Errorf("Accept = %q, want %q", receivedAccept, "application/vnd.github+json")
	}
	if receivedVersion != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q, want %q", receivedVersion, "2022-11-28")
	}
}

func TestClient_RateLimitBackoff(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	requestCount := 0
	resetTime := fakeClock.Now().Add(30 * time.Second)

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount++
		if requestCount == 1 {
			// First request: rate limited.
			writer.Header().Set("X-RateLimit-Remaining", "0")
			writer.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetTime.Unix(), 10))
			writer.Header().Set("Retry-After", "30")
			writer.WriteHeader(http.StatusForbidden)
			json.NewEncoder(writer).Encode(map[string]string{
				"message": "API rate limit exceeded",
			})
			return
		}
		// Second request: success.
		writer.Header().Set("X-RateLimit-Remaining", "4999")
		writer.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetTime.Add(1*time.Hour).Unix(), 10))
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(`{"number":42,"title":"Test Issue"}`))
	}))
	defer server.Close()

	client, err := NewClient(Config{
		BaseURL:    server.URL,
		Token:      "test-token",
		HTTPClient: server.Client(),
		Clock:      fakeClock,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Start the request in a goroutine since it will block on rate limit.
	done := make(chan error, 1)
	var issue *Issue
	go func() {
		var requestErr error
		issue, requestErr = client.GetIssue(context.Background(), "owner", "repo", 42)
		done <- requestErr
	}()

	// Wait for the goroutine to register a timer (the rate limit backoff
	// calls clock.After), then advance past the retry-after duration.
	fakeClock.WaitForTimers(1)
	fakeClock.Advance(31 * time.Second)

	if err := <-done; err != nil {
		t.Fatalf("GetIssue: %v", err)
	}

	if requestCount != 2 {
		t.Errorf("expected 2 requests (rate limited + retry), got %d", requestCount)
	}
	if issue == nil || issue.Number != 42 {
		t.Errorf("expected issue #42, got %+v", issue)
	}
}

func TestClient_ETagCaching(t *testing.T) {
	requestCount := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount++
		ifNoneMatch := request.Header.Get("If-None-Match")

		if ifNoneMatch == `"etag-123"` {
			// Second request with matching ETag: return 304.
			writer.WriteHeader(http.StatusNotModified)
			return
		}

		// First request: return data with ETag.
		writer.Header().Set("ETag", `"etag-123"`)
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(`{"number":1,"title":"Cached Issue"}`))
	}))
	defer server.Close()

	client := newTestClient(t, server)
	ctx := context.Background()

	// First request — should get the full response.
	issue1, err := client.GetIssue(ctx, "owner", "repo", 1)
	if err != nil {
		t.Fatalf("first GetIssue: %v", err)
	}
	if issue1.Title != "Cached Issue" {
		t.Errorf("first issue title = %q, want %q", issue1.Title, "Cached Issue")
	}

	// Second request — should get 304 and use cached response.
	issue2, err := client.GetIssue(ctx, "owner", "repo", 1)
	if err != nil {
		t.Fatalf("second GetIssue: %v", err)
	}
	if issue2.Title != "Cached Issue" {
		t.Errorf("second issue title = %q, want %q", issue2.Title, "Cached Issue")
	}

	if requestCount != 2 {
		t.Errorf("expected 2 HTTP requests, got %d", requestCount)
	}
}

func TestClient_ErrorParsing(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
		json.NewEncoder(writer).Encode(map[string]any{
			"message":           "Not Found",
			"documentation_url": "https://docs.github.com/rest",
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.GetIssue(context.Background(), "owner", "repo", 999)
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !IsNotFound(err) {
		t.Errorf("expected IsNotFound, got: %v", err)
	}
}

func TestClient_ValidationError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(writer).Encode(map[string]any{
			"message": "Validation Failed",
			"errors": []map[string]string{
				{"resource": "Issue", "code": "missing_field", "field": "title"},
			},
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.CreateIssue(context.Background(), "owner", "repo", CreateIssueRequest{})
	if err == nil {
		t.Fatal("expected error for 422")
	}
	if !IsValidationFailed(err) {
		t.Errorf("expected IsValidationFailed, got: %v", err)
	}
}
