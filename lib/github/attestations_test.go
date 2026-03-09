// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetAttestations_WithBundles(t *testing.T) {
	attestationResponse := AttestationResponse{
		Attestations: []AttestationEntry{
			{
				Bundle:       json.RawMessage(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`),
				RepositoryID: 12345,
			},
			{
				Bundle:       json.RawMessage(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`),
				RepositoryID: 12345,
			},
		},
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		expectedPath := "/repos/owner/repo/attestations/sha256:abcdef0123456789"
		if request.URL.Path != expectedPath {
			t.Errorf("path = %q, want %q", request.URL.Path, expectedPath)
		}
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(attestationResponse)
	}))
	defer server.Close()

	client := newTestClient(t, server)
	result, err := client.GetAttestations(context.Background(), "owner", "repo", "sha256:abcdef0123456789")
	if err != nil {
		t.Fatalf("GetAttestations: %v", err)
	}

	if len(result.Attestations) != 2 {
		t.Fatalf("len(Attestations) = %d, want 2", len(result.Attestations))
	}
	if result.Attestations[0].RepositoryID != 12345 {
		t.Errorf("RepositoryID = %d, want 12345", result.Attestations[0].RepositoryID)
	}
	// Verify the bundle JSON was preserved as raw message.
	if string(result.Attestations[0].Bundle) != `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}` {
		t.Errorf("Bundle = %s, unexpected", string(result.Attestations[0].Bundle))
	}
}

func TestGetAttestations_Empty(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(AttestationResponse{
			Attestations: []AttestationEntry{},
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	result, err := client.GetAttestations(context.Background(), "owner", "repo", "sha256:0000000000000000")
	if err != nil {
		t.Fatalf("GetAttestations: %v", err)
	}

	if len(result.Attestations) != 0 {
		t.Errorf("len(Attestations) = %d, want 0", len(result.Attestations))
	}
}

func TestGetAttestations_NotFound(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		json.NewEncoder(writer).Encode(map[string]string{
			"message": "Not Found",
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.GetAttestations(context.Background(), "owner", "repo", "sha256:0000000000000000")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestFormatSubjectDigest(t *testing.T) {
	digest := []byte{0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89}
	result := FormatSubjectDigest("sha256", digest)
	expected := "sha256:abcdef0123456789"
	if result != expected {
		t.Errorf("FormatSubjectDigest = %q, want %q", result, expected)
	}
}

func TestFormatSubjectDigest_SHA512(t *testing.T) {
	digest := []byte{0xff, 0x00, 0xaa, 0x55}
	result := FormatSubjectDigest("sha512", digest)
	expected := "sha512:ff00aa55"
	if result != expected {
		t.Errorf("FormatSubjectDigest = %q, want %q", result, expected)
	}
}
