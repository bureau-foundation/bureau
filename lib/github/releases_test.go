// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetReleaseByTag_Success(t *testing.T) {
	release := Release{
		ID:      42,
		TagName: "v1.0.0",
		Name:    "Release 1.0.0",
		Assets: []ReleaseAsset{
			{ID: 100, Name: "binary.tar.gz", Size: 1024},
		},
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/owner/repo/releases/tags/v1.0.0" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(release)
	}))
	defer server.Close()

	client := newTestClient(t, server)
	result, err := client.GetReleaseByTag(context.Background(), "owner", "repo", "v1.0.0")
	if err != nil {
		t.Fatalf("GetReleaseByTag: %v", err)
	}

	if result.ID != 42 {
		t.Errorf("ID = %d, want 42", result.ID)
	}
	if result.TagName != "v1.0.0" {
		t.Errorf("TagName = %q, want %q", result.TagName, "v1.0.0")
	}
	if len(result.Assets) != 1 {
		t.Fatalf("len(Assets) = %d, want 1", len(result.Assets))
	}
	if result.Assets[0].Name != "binary.tar.gz" {
		t.Errorf("Assets[0].Name = %q, want %q", result.Assets[0].Name, "binary.tar.gz")
	}
}

func TestGetReleaseByTag_NotFound(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		json.NewEncoder(writer).Encode(map[string]string{
			"message": "Not Found",
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.GetReleaseByTag(context.Background(), "owner", "repo", "v0.0.0")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}

	if !IsNotFound(err) {
		t.Errorf("expected IsNotFound(err) to be true, got error: %v", err)
	}
}

func TestDownloadReleaseAsset_Success(t *testing.T) {
	expectedContent := "binary-content-bytes"

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/owner/repo/releases/assets/100" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}

		// Verify the Accept header is set for binary content.
		accept := request.Header.Get("Accept")
		if accept != "application/octet-stream" {
			t.Errorf("Accept = %q, want %q", accept, "application/octet-stream")
		}

		// Verify auth header is present.
		auth := request.Header.Get("Authorization")
		if auth == "" {
			t.Error("missing Authorization header")
		}

		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Write([]byte(expectedContent))
	}))
	defer server.Close()

	client := newTestClient(t, server)
	body, err := client.DownloadReleaseAsset(context.Background(), "owner", "repo", 100)
	if err != nil {
		t.Fatalf("DownloadReleaseAsset: %v", err)
	}
	defer body.Close()

	content, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if string(content) != expectedContent {
		t.Errorf("content = %q, want %q", string(content), expectedContent)
	}
}

func TestDownloadReleaseAsset_NotFound(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		json.NewEncoder(writer).Encode(map[string]string{
			"message": "Not Found",
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.DownloadReleaseAsset(context.Background(), "owner", "repo", 999)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}

	if !IsNotFound(err) {
		t.Errorf("expected IsNotFound(err) to be true, got error: %v", err)
	}
}

func TestDownloadReleaseAsset_RateLimited(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("X-RateLimit-Remaining", "0")
		writer.Header().Set("X-RateLimit-Reset", "9999999999")
		writer.WriteHeader(http.StatusForbidden)
		json.NewEncoder(writer).Encode(map[string]string{
			"message": "API rate limit exceeded",
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.DownloadReleaseAsset(context.Background(), "owner", "repo", 100)
	if err == nil {
		t.Fatal("expected error for rate-limited response")
	}
}
