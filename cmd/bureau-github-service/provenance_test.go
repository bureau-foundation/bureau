// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/github"
	"github.com/bureau-foundation/bureau/lib/provenance"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// --- Test PEM generation ---

// Fixed timestamps for test certificate generation. Using constants
// avoids wall-clock dependency while providing a validity window wide
// enough for any test execution.
var ( //@nolint:realclock // These are fixed timestamps, not wall-clock usage.
	provenanceTestCertNotBefore = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	provenanceTestCertNotAfter  = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
)

func provenanceGenerateTestPEM(t *testing.T) (caPEM, rekorPEM []byte) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             provenanceTestCertNotBefore,
		NotAfter:              provenanceTestCertNotAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating CA cert: %v", err)
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	rekorKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating rekor key: %v", err)
	}
	rekorPublicDER, err := x509.MarshalPKIXPublicKey(&rekorKey.PublicKey)
	if err != nil {
		t.Fatalf("marshaling rekor public key: %v", err)
	}
	rekorPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: rekorPublicDER})

	return caPEM, rekorPEM
}

func provenanceValidRoots(t *testing.T) schema.ProvenanceRootsContent {
	t.Helper()
	caPEM, rekorPEM := provenanceGenerateTestPEM(t)
	return schema.ProvenanceRootsContent{
		Roots: map[string]schema.ProvenanceTrustRoot{
			"test": {
				FulcioRootPEM:     string(caPEM),
				RekorPublicKeyPEM: string(rekorPEM),
			},
		},
	}
}

func provenanceValidPolicy() schema.ProvenancePolicyContent {
	return schema.ProvenancePolicyContent{
		TrustedIdentities: []schema.TrustedIdentity{
			{
				Name:           "ci",
				Roots:          "test",
				Issuer:         "https://test.example.com",
				SubjectPattern: "*",
			},
		},
		Enforcement: map[string]schema.EnforcementLevel{
			"forge_artifacts": schema.EnforcementRequire,
		},
	}
}

func provenanceNewTestVerifier(t *testing.T, enforcement schema.EnforcementLevel) *provenance.Verifier {
	t.Helper()
	roots := provenanceValidRoots(t)
	policy := schema.ProvenancePolicyContent{
		TrustedIdentities: []schema.TrustedIdentity{
			{
				Name:           "ci",
				Roots:          "test",
				Issuer:         "https://test.example.com",
				SubjectPattern: "*",
			},
		},
		Enforcement: map[string]schema.EnforcementLevel{
			"forge_artifacts": enforcement,
		},
	}
	verifier, err := provenance.NewVerifier(roots, policy)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return verifier
}

// --- Mock Matrix state server for provenance tests ---

// provenanceMockState provides a minimal Matrix state server for
// syncProvenance tests. Responds to GET state event requests.
type provenanceMockState struct {
	events map[string]json.RawMessage // "eventType\x00stateKey" → JSON
}

func newProvenanceMockState() *provenanceMockState {
	return &provenanceMockState{
		events: make(map[string]json.RawMessage),
	}
}

func (m *provenanceMockState) setStateEvent(eventType ref.EventType, stateKey string, content any) {
	data, _ := json.Marshal(content)
	key := string(eventType) + "\x00" + stateKey
	m.events[key] = data
}

func (m *provenanceMockState) deleteStateEvent(eventType ref.EventType, stateKey string) {
	key := string(eventType) + "\x00" + stateKey
	delete(m.events, key)
}

func (m *provenanceMockState) handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		// Parse: /_matrix/client/v3/rooms/{roomId}/state/{eventType}/{stateKey}
		// Path prefix is always /_matrix/client/v3/rooms/{roomId}/state/
		path := request.URL.Path
		const prefix = "/_matrix/client/v3/rooms/"
		if len(path) < len(prefix) {
			writer.WriteHeader(http.StatusNotFound)
			json.NewEncoder(writer).Encode(map[string]string{
				"errcode": "M_NOT_FOUND",
				"error":   "unknown path",
			})
			return
		}

		// Find the /state/ part.
		stateIndex := -1
		for i := len(prefix); i < len(path); i++ {
			if path[i:] == "/state" || (len(path) > i+6 && path[i:i+7] == "/state/") {
				stateIndex = i + 7
				break
			}
		}
		if stateIndex < 0 || stateIndex >= len(path) {
			writer.WriteHeader(http.StatusNotFound)
			json.NewEncoder(writer).Encode(map[string]string{
				"errcode": "M_NOT_FOUND",
				"error":   "not a state request",
			})
			return
		}

		remaining := path[stateIndex:]
		// Split into eventType and stateKey.
		eventType := remaining
		stateKey := ""
		for i, char := range remaining {
			if char == '/' {
				eventType = remaining[:i]
				stateKey = remaining[i+1:]
				break
			}
		}

		key := eventType + "\x00" + stateKey
		data, found := m.events[key]
		if !found {
			writer.WriteHeader(http.StatusNotFound)
			json.NewEncoder(writer).Encode(map[string]string{
				"errcode": "M_NOT_FOUND",
				"error":   "state event not found",
			})
			return
		}

		writer.WriteHeader(http.StatusOK)
		writer.Write(data)
	})
}

func newProvenanceTestService(t *testing.T, stateServer *httptest.Server) *GitHubService {
	t.Helper()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: stateServer.URL,
	})
	if err != nil {
		t.Fatalf("creating messaging client: %v", err)
	}

	session, err := client.SessionFromToken(
		ref.MustParseUserID("@test:test.local"), "test-token")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	return &GitHubService{
		session:     session,
		fleetRoomID: ref.MustParseRoomID("!fleet:test.local"),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// --- syncProvenance tests ---

func TestSyncProvenance_ConfiguresVerifier(t *testing.T) {
	t.Parallel()

	state := newProvenanceMockState()
	roots := provenanceValidRoots(t)
	policy := provenanceValidPolicy()
	state.setStateEvent(schema.EventTypeProvenanceRoots, "", roots)
	state.setStateEvent(schema.EventTypeProvenancePolicy, "", policy)

	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)

	gs := newProvenanceTestService(t, server)
	if err := gs.syncProvenance(context.Background()); err != nil {
		t.Fatalf("syncProvenance: %v", err)
	}

	if gs.provenanceVerifier.Load() == nil {
		t.Fatal("expected verifier after sync with valid roots and policy")
	}
}

func TestSyncProvenance_ClearsVerifierWhenBothAbsent(t *testing.T) {
	t.Parallel()

	state := newProvenanceMockState()
	// No roots or policy published.

	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)

	gs := newProvenanceTestService(t, server)
	// Pre-set a verifier to verify it gets cleared.
	gs.provenanceVerifier.Store(provenanceNewTestVerifier(t, schema.EnforcementRequire))

	if err := gs.syncProvenance(context.Background()); err != nil {
		t.Fatalf("syncProvenance: %v", err)
	}

	if gs.provenanceVerifier.Load() != nil {
		t.Fatal("verifier should be nil when both roots and policy are absent")
	}
}

func TestSyncProvenance_RetainsVerifierOnTransientError(t *testing.T) {
	t.Parallel()

	// First, configure a working verifier.
	state := newProvenanceMockState()
	roots := provenanceValidRoots(t)
	policy := provenanceValidPolicy()
	state.setStateEvent(schema.EventTypeProvenanceRoots, "", roots)
	state.setStateEvent(schema.EventTypeProvenancePolicy, "", policy)

	goodServer := httptest.NewServer(state.handler())
	t.Cleanup(goodServer.Close)

	gs := newProvenanceTestService(t, goodServer)
	if err := gs.syncProvenance(context.Background()); err != nil {
		t.Fatalf("syncProvenance: %v", err)
	}
	if gs.provenanceVerifier.Load() == nil {
		t.Fatal("expected verifier after initial sync")
	}
	initialVerifier := gs.provenanceVerifier.Load()

	// Point the service at a server that returns 500 for everything.
	errorServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(writer).Encode(map[string]string{
			"errcode": "M_UNKNOWN",
			"error":   "internal server error",
		})
	}))
	t.Cleanup(errorServer.Close)

	errorClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: errorServer.URL,
	})
	if err != nil {
		t.Fatalf("creating error client: %v", err)
	}
	errorSession, err := errorClient.SessionFromToken(
		ref.MustParseUserID("@test:test.local"), "test-token")
	if err != nil {
		t.Fatalf("creating error session: %v", err)
	}
	t.Cleanup(func() { errorSession.Close() })
	gs.session = errorSession

	// Sync with errors: verifier must be retained, and error must
	// be returned so the caller can fail-closed at startup.
	if err := gs.syncProvenance(context.Background()); err == nil {
		t.Fatal("expected error from syncProvenance on transient error")
	}
	if gs.provenanceVerifier.Load() == nil {
		t.Fatal("verifier was cleared on transient error — verification bypass")
	}
	if gs.provenanceVerifier.Load() != initialVerifier {
		t.Error("verifier was replaced on transient error — should retain previous")
	}
}

func TestSyncProvenance_RetainsVerifierOnMalformedRoots(t *testing.T) {
	t.Parallel()

	// Start with valid config.
	state := newProvenanceMockState()
	roots := provenanceValidRoots(t)
	policy := provenanceValidPolicy()
	state.setStateEvent(schema.EventTypeProvenanceRoots, "", roots)
	state.setStateEvent(schema.EventTypeProvenancePolicy, "", policy)

	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)

	gs := newProvenanceTestService(t, server)
	if err := gs.syncProvenance(context.Background()); err != nil {
		t.Fatalf("syncProvenance: %v", err)
	}
	if gs.provenanceVerifier.Load() == nil {
		t.Fatal("expected verifier after initial sync")
	}

	// Publish empty roots (NewVerifier will fail).
	state.setStateEvent(schema.EventTypeProvenanceRoots, "",
		schema.ProvenanceRootsContent{Roots: map[string]schema.ProvenanceTrustRoot{}})

	if err := gs.syncProvenance(context.Background()); err != nil {
		t.Fatalf("syncProvenance: %v", err)
	}
	if gs.provenanceVerifier.Load() == nil {
		t.Fatal("verifier was cleared when malformed roots were published — attack: publish empty roots to disable verification")
	}
}

func TestSyncProvenance_RetainsVerifierOnPolicyDeleted(t *testing.T) {
	t.Parallel()

	// Start with valid config.
	state := newProvenanceMockState()
	roots := provenanceValidRoots(t)
	policy := provenanceValidPolicy()
	state.setStateEvent(schema.EventTypeProvenanceRoots, "", roots)
	state.setStateEvent(schema.EventTypeProvenancePolicy, "", policy)

	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)

	gs := newProvenanceTestService(t, server)
	if err := gs.syncProvenance(context.Background()); err != nil {
		t.Fatalf("syncProvenance: %v", err)
	}
	if gs.provenanceVerifier.Load() == nil {
		t.Fatal("expected verifier after initial sync")
	}

	// Delete only policy, keeping roots. This simulates an attacker
	// deleting the policy event.
	state.deleteStateEvent(schema.EventTypeProvenancePolicy, "")

	if err := gs.syncProvenance(context.Background()); err != nil {
		t.Fatalf("syncProvenance: %v", err)
	}
	if gs.provenanceVerifier.Load() == nil {
		t.Fatal("verifier was cleared when only policy was deleted — attack: delete policy to disable verification")
	}
}

func TestSyncProvenance_RetainsVerifierOnRootsDeleted(t *testing.T) {
	t.Parallel()

	// Start with valid config.
	state := newProvenanceMockState()
	roots := provenanceValidRoots(t)
	policy := provenanceValidPolicy()
	state.setStateEvent(schema.EventTypeProvenanceRoots, "", roots)
	state.setStateEvent(schema.EventTypeProvenancePolicy, "", policy)

	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)

	gs := newProvenanceTestService(t, server)
	if err := gs.syncProvenance(context.Background()); err != nil {
		t.Fatalf("syncProvenance: %v", err)
	}
	if gs.provenanceVerifier.Load() == nil {
		t.Fatal("expected verifier after initial sync")
	}

	// Delete only roots, keeping policy.
	state.deleteStateEvent(schema.EventTypeProvenanceRoots, "")

	if err := gs.syncProvenance(context.Background()); err != nil {
		t.Fatalf("syncProvenance: %v", err)
	}
	if gs.provenanceVerifier.Load() == nil {
		t.Fatal("verifier was cleared when only roots were deleted — attack: delete roots to disable verification")
	}
}

func TestSyncProvenance_RetainsVerifierOnDanglingRootRef(t *testing.T) {
	t.Parallel()

	// Start with valid config.
	state := newProvenanceMockState()
	roots := provenanceValidRoots(t)
	policy := provenanceValidPolicy()
	state.setStateEvent(schema.EventTypeProvenanceRoots, "", roots)
	state.setStateEvent(schema.EventTypeProvenancePolicy, "", policy)

	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)

	gs := newProvenanceTestService(t, server)
	if err := gs.syncProvenance(context.Background()); err != nil {
		t.Fatalf("syncProvenance: %v", err)
	}
	if gs.provenanceVerifier.Load() == nil {
		t.Fatal("expected verifier after initial sync")
	}

	// Publish policy with a dangling root set reference.
	state.setStateEvent(schema.EventTypeProvenancePolicy, "",
		schema.ProvenancePolicyContent{
			TrustedIdentities: []schema.TrustedIdentity{
				{
					Name:           "ci",
					Roots:          "nonexistent_root_set",
					Issuer:         "https://test.example.com",
					SubjectPattern: "*",
				},
			},
			Enforcement: map[string]schema.EnforcementLevel{
				"forge_artifacts": schema.EnforcementRequire,
			},
		})

	if err := gs.syncProvenance(context.Background()); err != nil {
		t.Fatalf("syncProvenance: %v", err)
	}
	if gs.provenanceVerifier.Load() == nil {
		t.Fatal("verifier was cleared when dangling root ref was published — attack: publish policy with nonexistent root set")
	}
}

func TestSyncProvenance_UnknownEnforcementRetainsPrevious(t *testing.T) {
	t.Parallel()

	// Start with valid config.
	state := newProvenanceMockState()
	roots := provenanceValidRoots(t)
	policy := provenanceValidPolicy()
	state.setStateEvent(schema.EventTypeProvenanceRoots, "", roots)
	state.setStateEvent(schema.EventTypeProvenancePolicy, "", policy)

	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)

	gs := newProvenanceTestService(t, server)
	if err := gs.syncProvenance(context.Background()); err != nil {
		t.Fatalf("syncProvenance: %v", err)
	}
	if gs.provenanceVerifier.Load() == nil {
		t.Fatal("expected verifier after initial sync")
	}

	// Publish policy with unknown enforcement level.
	state.setStateEvent(schema.EventTypeProvenancePolicy, "",
		schema.ProvenancePolicyContent{
			TrustedIdentities: []schema.TrustedIdentity{
				{
					Name:           "ci",
					Roots:          "test",
					Issuer:         "https://test.example.com",
					SubjectPattern: "*",
				},
			},
			Enforcement: map[string]schema.EnforcementLevel{
				"forge_artifacts": "attacker_controlled_value",
			},
		})

	if err := gs.syncProvenance(context.Background()); err != nil {
		t.Fatalf("syncProvenance: %v", err)
	}
	if gs.provenanceVerifier.Load() == nil {
		t.Fatal("verifier was cleared when unknown enforcement level was published — attack: downgrade enforcement to skip verification")
	}
}

func TestSyncProvenance_TransientErrorAtStartupBlocksBoot(t *testing.T) {
	t.Parallel()

	// Simulate a homeserver that is completely unreachable at
	// startup. This is the fail-open scenario: without the error
	// return from syncProvenance, the service would start with
	// a nil verifier and silently bypass "require" enforcement.
	errorServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(writer).Encode(map[string]string{
			"errcode": "M_UNKNOWN",
			"error":   "internal server error",
		})
	}))
	t.Cleanup(errorServer.Close)

	gs := newProvenanceTestService(t, errorServer)

	// No previous verifier (fresh service start).
	err := gs.syncProvenance(context.Background())
	if err == nil {
		t.Fatal("syncProvenance must return error on transient failure — service would start with nil verifier and bypass enforcement")
	}
	if gs.provenanceVerifier.Load() != nil {
		t.Fatal("verifier should remain nil (no previous state to retain)")
	}
}

func TestSyncProvenance_NilVerifierNeverConfigured(t *testing.T) {
	t.Parallel()

	state := newProvenanceMockState()
	// No state events.

	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)

	gs := newProvenanceTestService(t, server)
	if err := gs.syncProvenance(context.Background()); err != nil {
		t.Fatalf("syncProvenance: %v", err)
	}

	if gs.provenanceVerifier.Load() != nil {
		t.Fatal("verifier should be nil when no provenance config exists")
	}
}

// --- verifyArtifactProvenance tests ---

func TestVerifyArtifactProvenance_NoVerifier(t *testing.T) {
	t.Parallel()

	gs := &GitHubService{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	result, allowed := gs.verifyArtifactProvenance(
		context.Background(), nil, "owner", "repo", []byte("digest"), "test-label")
	if !allowed {
		t.Fatal("download should be allowed when no verifier configured")
	}
	if result != nil {
		t.Fatal("result should be nil when no verifier configured")
	}
}

func TestVerifyArtifactProvenance_NoAttestationsRequire(t *testing.T) {
	t.Parallel()

	// GitHub server returns empty attestations.
	mock := newMockGitHub(t)
	mock.routes["GET /repos/owner/repo/attestations/sha256:"+
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"] = mockRoute{
		Status: http.StatusOK,
		Body:   github.AttestationResponse{Attestations: []github.AttestationEntry{}},
	}

	githubClient := newTestGitHubClient(t, mock)
	gs := &GitHubService{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	gs.provenanceVerifier.Store(provenanceNewTestVerifier(t, schema.EnforcementRequire))

	digest := computeSHA256([]byte(""))
	result, allowed := gs.verifyArtifactProvenance(
		context.Background(), githubClient, "owner", "repo", digest, "test-label")
	if allowed {
		t.Fatal("download should be rejected when require + no attestations")
	}
	if result == nil {
		t.Fatal("expected result with unverified status")
	}
	if result.Status != "unverified" {
		t.Errorf("status = %q, want %q", result.Status, "unverified")
	}
}

func TestVerifyArtifactProvenance_NoAttestationsWarn(t *testing.T) {
	t.Parallel()

	mock := newMockGitHub(t)
	mock.routes["GET /repos/owner/repo/attestations/sha256:"+
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"] = mockRoute{
		Status: http.StatusOK,
		Body:   github.AttestationResponse{Attestations: []github.AttestationEntry{}},
	}

	githubClient := newTestGitHubClient(t, mock)
	gs := &GitHubService{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	gs.provenanceVerifier.Store(provenanceNewTestVerifier(t, schema.EnforcementWarn))

	digest := computeSHA256([]byte(""))
	result, allowed := gs.verifyArtifactProvenance(
		context.Background(), githubClient, "owner", "repo", digest, "test-label")
	if !allowed {
		t.Fatal("download should be allowed when warn + no attestations")
	}
	if result == nil {
		t.Fatal("expected result with unverified status")
	}
	if result.Status != "unverified" {
		t.Errorf("status = %q, want %q", result.Status, "unverified")
	}
	if result.Enforcement != "warn" {
		t.Errorf("enforcement = %q, want %q", result.Enforcement, "warn")
	}
}

func TestVerifyArtifactProvenance_NoAttestationsLog(t *testing.T) {
	t.Parallel()

	mock := newMockGitHub(t)
	mock.routes["GET /repos/owner/repo/attestations/sha256:"+
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"] = mockRoute{
		Status: http.StatusOK,
		Body:   github.AttestationResponse{Attestations: []github.AttestationEntry{}},
	}

	githubClient := newTestGitHubClient(t, mock)
	gs := &GitHubService{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	gs.provenanceVerifier.Store(provenanceNewTestVerifier(t, schema.EnforcementLog))

	digest := computeSHA256([]byte(""))
	result, allowed := gs.verifyArtifactProvenance(
		context.Background(), githubClient, "owner", "repo", digest, "test-label")
	if !allowed {
		t.Fatal("download should be allowed when log + no attestations")
	}
	if result.Status != "unverified" {
		t.Errorf("status = %q, want %q", result.Status, "unverified")
	}
}

func TestVerifyArtifactProvenance_AttestationFetchErrorRequire(t *testing.T) {
	t.Parallel()

	// No route for attestations — 404 from mock.
	mock := newMockGitHub(t)
	githubClient := newTestGitHubClient(t, mock)

	gs := &GitHubService{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	gs.provenanceVerifier.Store(provenanceNewTestVerifier(t, schema.EnforcementRequire))

	digest := computeSHA256([]byte("test"))
	result, allowed := gs.verifyArtifactProvenance(
		context.Background(), githubClient, "owner", "repo", digest, "test-label")
	if allowed {
		t.Fatal("download should be rejected when require + attestation fetch error")
	}
	if result == nil {
		t.Fatal("expected result on error")
	}
	if result.Status != "unverified" {
		t.Errorf("status = %q, want %q", result.Status, "unverified")
	}
}

func TestVerifyArtifactProvenance_RejectedBundleRequire(t *testing.T) {
	t.Parallel()

	// Return a bundle that won't verify (wrong format for Sigstore).
	mock := newMockGitHub(t)
	digest := computeSHA256([]byte("artifact-content"))
	digestHex := github.FormatSubjectDigest("sha256", digest)
	mock.routes["GET /repos/owner/repo/attestations/"+digestHex] = mockRoute{
		Status: http.StatusOK,
		Body: github.AttestationResponse{
			Attestations: []github.AttestationEntry{
				{
					Bundle:       json.RawMessage(`{"mediaType":"invalid"}`),
					RepositoryID: 12345,
				},
			},
		},
	}

	githubClient := newTestGitHubClient(t, mock)
	gs := &GitHubService{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	gs.provenanceVerifier.Store(provenanceNewTestVerifier(t, schema.EnforcementRequire))

	result, allowed := gs.verifyArtifactProvenance(
		context.Background(), githubClient, "owner", "repo", digest, "test-label")
	if allowed {
		t.Fatal("download should be rejected when require + all bundles rejected")
	}
	if result == nil {
		t.Fatal("expected result on rejection")
	}
	if result.Status != "rejected" {
		t.Errorf("status = %q, want %q", result.Status, "rejected")
	}
}

func TestVerifyArtifactProvenance_RejectedBundleWarnAllowed(t *testing.T) {
	t.Parallel()

	mock := newMockGitHub(t)
	digest := computeSHA256([]byte("artifact-content"))
	digestHex := github.FormatSubjectDigest("sha256", digest)
	mock.routes["GET /repos/owner/repo/attestations/"+digestHex] = mockRoute{
		Status: http.StatusOK,
		Body: github.AttestationResponse{
			Attestations: []github.AttestationEntry{
				{
					Bundle:       json.RawMessage(`{"mediaType":"invalid"}`),
					RepositoryID: 12345,
				},
			},
		},
	}

	githubClient := newTestGitHubClient(t, mock)
	gs := &GitHubService{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	gs.provenanceVerifier.Store(provenanceNewTestVerifier(t, schema.EnforcementWarn))

	result, allowed := gs.verifyArtifactProvenance(
		context.Background(), githubClient, "owner", "repo", digest, "test-label")
	if !allowed {
		t.Fatal("download should be allowed when warn + bundle rejected")
	}
	if result.Status != "rejected" {
		t.Errorf("status = %q, want %q", result.Status, "rejected")
	}
	if result.Enforcement != "warn" {
		t.Errorf("enforcement = %q, want %q", result.Enforcement, "warn")
	}
}

// newTestGitHubClient creates a GitHub client backed by the mock server.
func newTestGitHubClient(t *testing.T, mock *mockGitHub) *github.Client {
	t.Helper()
	client, err := github.NewClient(github.Config{
		BaseURL:    mock.server.URL,
		Token:      "test-token",
		HTTPClient: mock.server.Client(),
		Clock:      clock.Fake(outboundClockEpoch),
	})
	if err != nil {
		t.Fatalf("creating GitHub client: %v", err)
	}
	return client
}
