// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package provenance

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// generateTestPEM creates a self-signed X.509 certificate and ECDSA
// key pair, returning both as PEM-encoded strings. The certificate
// is suitable for testing parseTrustRoot but will not pass real
// Sigstore verification (it's not a Fulcio CA).
func generateTestPEM(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-fulcio-root"},
		NotBefore:             time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certBlock := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	pubKeyDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	keyBlock := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubKeyDER})

	return string(certBlock), string(keyBlock)
}

func TestParseTrustRoot(t *testing.T) {
	certPEM, keyPEM := generateTestPEM(t)

	t.Run("valid PEM", func(t *testing.T) {
		root, err := parseTrustRoot(schema.ProvenanceTrustRoot{
			FulcioRootPEM:     certPEM,
			RekorPublicKeyPEM: keyPEM,
		})
		if err != nil {
			t.Fatalf("parseTrustRoot: %v", err)
		}
		if root == nil {
			t.Fatal("parseTrustRoot returned nil without error")
		}

		if len(root.fulcioRoots) != 1 {
			t.Fatalf("fulcioRoots count = %d, want 1", len(root.fulcioRoots))
		}

		if root.rekorKey == nil {
			t.Fatal("rekorKey is nil")
		}

		if len(root.rekorKeyID) != 32 {
			t.Fatalf("rekorKeyID length = %d, want 32 (SHA-256)", len(root.rekorKeyID))
		}
	})

	t.Run("invalid Fulcio PEM", func(t *testing.T) {
		_, err := parseTrustRoot(schema.ProvenanceTrustRoot{
			FulcioRootPEM:     "not-a-pem",
			RekorPublicKeyPEM: keyPEM,
		})
		if err == nil {
			t.Fatal("expected error for invalid Fulcio PEM")
		}
	})

	t.Run("invalid Rekor PEM", func(t *testing.T) {
		_, err := parseTrustRoot(schema.ProvenanceTrustRoot{
			FulcioRootPEM:     certPEM,
			RekorPublicKeyPEM: "not-a-pem",
		})
		if err == nil {
			t.Fatal("expected error for invalid Rekor PEM")
		}
	})

	t.Run("corrupted certificate DER", func(t *testing.T) {
		badCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not-valid-der")})
		_, err := parseTrustRoot(schema.ProvenanceTrustRoot{
			FulcioRootPEM:     string(badCert),
			RekorPublicKeyPEM: keyPEM,
		})
		if err == nil {
			t.Fatal("expected error for corrupted certificate DER")
		}
	})

	t.Run("corrupted public key DER", func(t *testing.T) {
		badKey := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("not-valid-der")})
		_, err := parseTrustRoot(schema.ProvenanceTrustRoot{
			FulcioRootPEM:     certPEM,
			RekorPublicKeyPEM: string(badKey),
		})
		if err == nil {
			t.Fatal("expected error for corrupted public key DER")
		}
	})
}

func TestNewVerifier(t *testing.T) {
	certPEM, keyPEM := generateTestPEM(t)

	validRoots := schema.ProvenanceRootsContent{
		Roots: map[string]schema.ProvenanceTrustRoot{
			"test": {
				FulcioRootPEM:     certPEM,
				RekorPublicKeyPEM: keyPEM,
			},
		},
	}

	t.Run("valid construction", func(t *testing.T) {
		policy := schema.ProvenancePolicyContent{
			TrustedIdentities: []schema.TrustedIdentity{
				{
					Name:           "test-ci",
					Roots:          "test",
					Issuer:         "https://token.actions.githubusercontent.com",
					SubjectPattern: "repo:test-org/test-repo:*",
				},
			},
			Enforcement: map[string]schema.EnforcementLevel{
				"nix_store_paths": schema.EnforcementRequire,
			},
		}

		verifier, err := NewVerifier(validRoots, policy)
		if err != nil {
			t.Fatalf("NewVerifier: %v", err)
		}
		if verifier == nil {
			t.Fatal("NewVerifier returned nil without error")
		}
	})

	t.Run("dangling root reference", func(t *testing.T) {
		policy := schema.ProvenancePolicyContent{
			TrustedIdentities: []schema.TrustedIdentity{
				{
					Name:           "broken",
					Roots:          "nonexistent",
					Issuer:         "https://example.com",
					SubjectPattern: "*",
				},
			},
		}

		_, err := NewVerifier(validRoots, policy)
		if err == nil {
			t.Fatal("expected error for dangling root reference")
		}
	})

	t.Run("empty roots", func(t *testing.T) {
		_, err := NewVerifier(schema.ProvenanceRootsContent{}, schema.ProvenancePolicyContent{})
		if err == nil {
			t.Fatal("expected error for empty roots")
		}
	})

	t.Run("invalid PEM in root", func(t *testing.T) {
		badRoots := schema.ProvenanceRootsContent{
			Roots: map[string]schema.ProvenanceTrustRoot{
				"bad": {
					FulcioRootPEM:     "not-pem",
					RekorPublicKeyPEM: keyPEM,
				},
			},
		}
		_, err := NewVerifier(badRoots, schema.ProvenancePolicyContent{})
		if err == nil {
			t.Fatal("expected error for invalid PEM")
		}
	})
}

func TestEnforcement(t *testing.T) {
	certPEM, keyPEM := generateTestPEM(t)

	verifier, err := NewVerifier(
		schema.ProvenanceRootsContent{
			Roots: map[string]schema.ProvenanceTrustRoot{
				"test": {FulcioRootPEM: certPEM, RekorPublicKeyPEM: keyPEM},
			},
		},
		schema.ProvenancePolicyContent{
			Enforcement: map[string]schema.EnforcementLevel{
				"nix_store_paths": schema.EnforcementRequire,
				"artifacts":       schema.EnforcementWarn,
				"models":          schema.EnforcementLog,
			},
		},
	)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	tests := []struct {
		category string
		want     schema.EnforcementLevel
	}{
		{"nix_store_paths", schema.EnforcementRequire},
		{"artifacts", schema.EnforcementWarn},
		{"models", schema.EnforcementLog},
		{"forge_artifacts", schema.EnforcementLog}, // missing → default to log
		{"templates", schema.EnforcementLog},       // missing → default to log
		{"unknown_future_category", schema.EnforcementLog},
	}

	for _, test := range tests {
		got := verifier.Enforcement(test.category)
		if got != test.want {
			t.Errorf("Enforcement(%q) = %q, want %q", test.category, got, test.want)
		}
	}
}

func TestVerifyRejectsInvalidBundle(t *testing.T) {
	certPEM, keyPEM := generateTestPEM(t)

	verifier, err := NewVerifier(
		schema.ProvenanceRootsContent{
			Roots: map[string]schema.ProvenanceTrustRoot{
				"test": {FulcioRootPEM: certPEM, RekorPublicKeyPEM: keyPEM},
			},
		},
		schema.ProvenancePolicyContent{
			TrustedIdentities: []schema.TrustedIdentity{
				{
					Name:           "test",
					Roots:          "test",
					Issuer:         "https://example.com",
					SubjectPattern: "*",
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	result := verifier.Verify([]byte("not-valid-json"), "sha256", []byte{0x01, 0x02})
	if result.Status != StatusRejected {
		t.Errorf("Status = %v, want StatusRejected", result.Status)
	}
	if result.Error == nil {
		t.Error("Error is nil, want parse error")
	}
}

func TestGlobMatching(t *testing.T) {
	tests := []struct {
		pattern string
		value   string
		want    bool
	}{
		// Exact matches.
		{"hello", "hello", true},
		{"hello", "world", false},

		// Star matches any sequence including path separators.
		{"repo:test-org/*:ref:*", "repo:test-org/my-repo:ref:refs/heads/main", true},
		{"repo:test-org/*:ref:*", "repo:other-org/my-repo:ref:refs/heads/main", false},
		{"*@*.iam.gserviceaccount.com", "sa@project.iam.gserviceaccount.com", true},
		{"*@*.iam.gserviceaccount.com", "sa@example.com", false},

		// Star crosses path separators (unlike path.Match).
		{"*/ci.yaml@*", "https://github.com/org/repo/.github/workflows/ci.yaml@refs/heads/main", true},

		// Question mark matches single character.
		{"v?.0", "v1.0", true},
		{"v?.0", "v10.0", false},

		// Regex metacharacters are escaped.
		{"file.txt", "filextxt", false},
		{"(test)", "(test)", true},
		{"a+b", "a+b", true},
		{"a+b", "aab", false},

		// Empty pattern matches empty string only.
		{"", "", true},
		{"", "something", false},
	}

	for _, test := range tests {
		got := matchGlob(test.pattern, test.value)
		if got != test.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", test.pattern, test.value, got, test.want)
		}
	}
}

func TestMatchIdentity(t *testing.T) {
	identity := schema.TrustedIdentity{
		Name:            "bureau-ci",
		Roots:           "sigstore_public",
		Issuer:          "https://token.actions.githubusercontent.com",
		SubjectPattern:  "repo:bureau-foundation/bureau:ref:*",
		WorkflowPattern: "*/.github/workflows/ci.yaml@*",
	}

	t.Run("full match", func(t *testing.T) {
		if !matchIdentity(identity,
			"https://token.actions.githubusercontent.com",
			"repo:bureau-foundation/bureau:ref:refs/heads/main",
			"https://github.com/bureau-foundation/bureau/.github/workflows/ci.yaml@refs/heads/main",
		) {
			t.Error("expected match")
		}
	})

	t.Run("wrong issuer", func(t *testing.T) {
		if matchIdentity(identity,
			"https://accounts.google.com",
			"repo:bureau-foundation/bureau:ref:refs/heads/main",
			"https://github.com/bureau-foundation/bureau/.github/workflows/ci.yaml@refs/heads/main",
		) {
			t.Error("expected no match with wrong issuer")
		}
	})

	t.Run("wrong subject", func(t *testing.T) {
		if matchIdentity(identity,
			"https://token.actions.githubusercontent.com",
			"repo:evil-org/evil-repo:ref:refs/heads/main",
			"https://github.com/bureau-foundation/bureau/.github/workflows/ci.yaml@refs/heads/main",
		) {
			t.Error("expected no match with wrong subject")
		}
	})

	t.Run("wrong workflow", func(t *testing.T) {
		if matchIdentity(identity,
			"https://token.actions.githubusercontent.com",
			"repo:bureau-foundation/bureau:ref:refs/heads/main",
			"https://github.com/bureau-foundation/bureau/.github/workflows/evil.yaml@refs/heads/main",
		) {
			t.Error("expected no match with wrong workflow")
		}
	})

	t.Run("no workflow pattern matches any", func(t *testing.T) {
		noWorkflow := schema.TrustedIdentity{
			Name:           "no-workflow",
			Roots:          "test",
			Issuer:         "https://accounts.google.com",
			SubjectPattern: "sa@*",
		}
		if !matchIdentity(noWorkflow,
			"https://accounts.google.com",
			"sa@project.iam.gserviceaccount.com",
			"anything-here",
		) {
			t.Error("expected match when WorkflowPattern is empty")
		}
	})
}

func TestStatusString(t *testing.T) {
	tests := []struct {
		status Status
		want   string
	}{
		{StatusVerified, "verified"},
		{StatusUnverified, "unverified"},
		{StatusRejected, "rejected"},
		{Status(99), "Status(99)"},
	}

	for _, test := range tests {
		got := test.status.String()
		if got != test.want {
			t.Errorf("Status(%d).String() = %q, want %q", int(test.status), got, test.want)
		}
	}
}

// ---- End-to-end verification tests using real Sigstore fixtures ------

// readTestFixture reads a file from testdata/ relative to the test.
func readTestFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func TestVerifyMessageSignatureBundle(t *testing.T) {
	// The othername.sigstore.json fixture is a v0.3 bundle with
	// messageSignature content, signed against the scaffolding trust
	// root. Artifact digest is SHA-256.
	bundleData := readTestFixture(t, "othername.sigstore.json")
	trustedRootData := readTestFixture(t, "trusted-root-scaffolding.json")

	// Parse the trusted root from JSON fixture format.
	root, err := parseTrustedRootFile(trustedRootData)
	if err != nil {
		t.Fatalf("parseTrustedRootFile: %v", err)
	}

	// Parse the bundle to extract the expected digest.
	bundle, err := parseBundle(bundleData)
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}

	t.Run("certificate chain", func(t *testing.T) {
		if err := verifyCertificateChain(bundle.leafCert, root); err != nil {
			t.Fatalf("verifyCertificateChain: %v", err)
		}
	})

	t.Run("signature", func(t *testing.T) {
		if err := verifySignature(bundle, bundle.digestValue); err != nil {
			t.Fatalf("verifySignature: %v", err)
		}
	})

	t.Run("tlog entry", func(t *testing.T) {
		if err := verifyTlogEntry(&bundle.tlogEntry, root.rekorKey, root.rekorKeyID, bundle); err != nil {
			t.Fatalf("verifyTlogEntry: %v", err)
		}
	})

	t.Run("OIDC claims", func(t *testing.T) {
		claims := extractFulcioClaims(bundle.leafCert)
		if claims.Issuer == "" {
			t.Error("Issuer is empty")
		}
		if claims.SubjectAlternativeName == "" {
			t.Error("SubjectAlternativeName is empty")
		}
		t.Logf("issuer=%q subject=%q", claims.Issuer, claims.SubjectAlternativeName)
	})

	t.Run("full verification via Verifier", func(t *testing.T) {
		// Build a Verifier with the fixture's trust root and a
		// policy that matches the fixture's OIDC identity.
		claims := extractFulcioClaims(bundle.leafCert)

		verifier, err := NewVerifier(
			schema.ProvenanceRootsContent{
				Roots: map[string]schema.ProvenanceTrustRoot{
					// We can't use PEM-based config here because
					// the fixture uses Sigstore trusted root JSON.
					// Instead, inject the parsed root directly.
				},
			},
			schema.ProvenancePolicyContent{},
		)
		// NewVerifier will fail because no roots. That's expected —
		// we test full verification by constructing the Verifier
		// manually.
		_ = verifier
		_ = err

		// Construct a Verifier with the pre-parsed root.
		verifier = &Verifier{
			roots: map[string]*rootSet{
				"scaffolding": {
					root: root,
					identities: []schema.TrustedIdentity{
						{
							Name:           "test-fixture",
							Roots:          "scaffolding",
							Issuer:         claims.Issuer,
							SubjectPattern: "*",
						},
					},
				},
			},
			policy: schema.ProvenancePolicyContent{},
		}

		result := verifier.Verify(bundleData, bundle.digestAlgorithm, bundle.digestValue)
		if result.Status != StatusVerified {
			t.Fatalf("Status = %v, want StatusVerified; error: %v", result.Status, result.Error)
		}
		if result.Identity != "test-fixture" {
			t.Errorf("Identity = %q, want %q", result.Identity, "test-fixture")
		}
		if result.Issuer != claims.Issuer {
			t.Errorf("Issuer = %q, want %q", result.Issuer, claims.Issuer)
		}
		if result.IntegratedTime == 0 {
			t.Error("IntegratedTime is 0, want nonzero tlog timestamp")
		}
		t.Logf("verified: identity=%q issuer=%q subject=%q time=%d",
			result.Identity, result.Issuer, result.Subject, result.IntegratedTime)
	})

	t.Run("wrong digest rejected", func(t *testing.T) {
		verifier := &Verifier{
			roots: map[string]*rootSet{
				"scaffolding": {
					root: root,
					identities: []schema.TrustedIdentity{
						{
							Name:           "test",
							Roots:          "scaffolding",
							Issuer:         "*",
							SubjectPattern: "*",
						},
					},
				},
			},
		}

		wrongDigest := make([]byte, len(bundle.digestValue))
		copy(wrongDigest, bundle.digestValue)
		wrongDigest[0] ^= 0xff // flip a byte

		result := verifier.Verify(bundleData, bundle.digestAlgorithm, wrongDigest)
		if result.Status != StatusRejected {
			t.Fatalf("Status = %v, want StatusRejected for wrong digest", result.Status)
		}
	})

	t.Run("wrong issuer rejected", func(t *testing.T) {
		verifier := &Verifier{
			roots: map[string]*rootSet{
				"scaffolding": {
					root: root,
					identities: []schema.TrustedIdentity{
						{
							Name:           "wrong-issuer",
							Roots:          "scaffolding",
							Issuer:         "https://totally-different-issuer.example.com",
							SubjectPattern: "*",
						},
					},
				},
			},
		}

		result := verifier.Verify(bundleData, bundle.digestAlgorithm, bundle.digestValue)
		if result.Status != StatusRejected {
			t.Fatalf("Status = %v, want StatusRejected for wrong issuer", result.Status)
		}
	})
}

func TestVerifyDSSEBundle(t *testing.T) {
	// The sigstore.js@2.0.0 fixture is a v0.1 bundle with DSSE
	// envelope content (in-toto SLSA provenance), signed against
	// the public-good trust root. Artifact digest is SHA-512.
	bundleData := readTestFixture(t, "sigstore.js@2.0.0-provenance.sigstore.json")
	trustedRootData := readTestFixture(t, "trusted-root-public-good.json")

	root, err := parseTrustedRootFile(trustedRootData)
	if err != nil {
		t.Fatalf("parseTrustedRootFile: %v", err)
	}

	bundle, err := parseBundle(bundleData)
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}

	if !bundle.isDSSE {
		t.Fatal("expected DSSE bundle")
	}
	if len(bundle.dsseSubjects) == 0 {
		t.Fatal("expected at least one in-toto subject")
	}

	t.Run("certificate chain", func(t *testing.T) {
		if err := verifyCertificateChain(bundle.leafCert, root); err != nil {
			t.Fatalf("verifyCertificateChain: %v", err)
		}
	})

	t.Run("signature", func(t *testing.T) {
		// For DSSE, the expectedDigest isn't used by verifySignature
		// (it computes PAE internally), but we pass it anyway.
		if err := verifySignature(bundle, nil); err != nil {
			t.Fatalf("verifySignature: %v", err)
		}
	})

	t.Run("tlog entry", func(t *testing.T) {
		if err := verifyTlogEntry(&bundle.tlogEntry, root.rekorKey, root.rekorKeyID, bundle); err != nil {
			t.Fatalf("verifyTlogEntry: %v", err)
		}
	})

	t.Run("OIDC claims", func(t *testing.T) {
		claims := extractFulcioClaims(bundle.leafCert)
		if claims.Issuer == "" {
			t.Error("Issuer is empty")
		}
		if claims.SubjectAlternativeName == "" {
			t.Error("SubjectAlternativeName is empty")
		}
		t.Logf("issuer=%q subject=%q", claims.Issuer, claims.SubjectAlternativeName)
	})

	t.Run("full verification via Verifier", func(t *testing.T) {
		claims := extractFulcioClaims(bundle.leafCert)

		// Extract the expected artifact digest from the in-toto subject.
		subject := bundle.dsseSubjects[0]
		var digestAlgorithm string
		var artifactDigest []byte
		for algorithm, hexDigest := range subject.Digest {
			digestAlgorithm = algorithm
			artifactDigest, err = hex.DecodeString(hexDigest)
			if err != nil {
				t.Fatalf("decode subject digest: %v", err)
			}
			break
		}

		verifier := &Verifier{
			roots: map[string]*rootSet{
				"public_good": {
					root: root,
					identities: []schema.TrustedIdentity{
						{
							Name:           "github-actions",
							Roots:          "public_good",
							Issuer:         claims.Issuer,
							SubjectPattern: "*",
						},
					},
				},
			},
			policy: schema.ProvenancePolicyContent{},
		}

		result := verifier.Verify(bundleData, digestAlgorithm, artifactDigest)
		if result.Status != StatusVerified {
			t.Fatalf("Status = %v, want StatusVerified; error: %v", result.Status, result.Error)
		}
		if result.Identity != "github-actions" {
			t.Errorf("Identity = %q, want %q", result.Identity, "github-actions")
		}
		t.Logf("verified: identity=%q issuer=%q subject=%q time=%d",
			result.Identity, result.Issuer, result.Subject, result.IntegratedTime)
	})
}

func TestParseTrustedRootJSON(t *testing.T) {
	t.Run("scaffolding", func(t *testing.T) {
		data := readTestFixture(t, "trusted-root-scaffolding.json")
		root, err := parseTrustedRootJSON(data)
		if err != nil {
			t.Fatalf("parseTrustedRootJSON: %v", err)
		}
		if len(root.fulcioRoots) == 0 {
			t.Fatal("no Fulcio roots parsed")
		}
		if root.rekorKey == nil {
			t.Fatal("no Rekor key parsed")
		}
		t.Logf("fulcio roots: %d, rekor key ID: %x", len(root.fulcioRoots), root.rekorKeyID)
	})

	t.Run("public good", func(t *testing.T) {
		data := readTestFixture(t, "trusted-root-public-good.json")
		root, err := parseTrustedRootJSON(data)
		if err != nil {
			t.Fatalf("parseTrustedRootJSON: %v", err)
		}
		if len(root.fulcioRoots) == 0 {
			t.Fatal("no Fulcio roots parsed")
		}
		if root.rekorKey == nil {
			t.Fatal("no Rekor key parsed")
		}
		t.Logf("fulcio roots: %d, rekor key ID: %x", len(root.fulcioRoots), root.rekorKeyID)
	})
}

func TestExtractFulcioClaims(t *testing.T) {
	// Use the real bundle fixtures to test claim extraction.
	t.Run("othername fixture", func(t *testing.T) {
		bundle, err := parseBundle(readTestFixture(t, "othername.sigstore.json"))
		if err != nil {
			t.Fatalf("parseBundle: %v", err)
		}

		claims := extractFulcioClaims(bundle.leafCert)
		if claims.Issuer == "" {
			t.Error("Issuer is empty")
		}
		if claims.SubjectAlternativeName == "" {
			t.Error("SubjectAlternativeName is empty")
		}
	})

	t.Run("sigstore.js fixture", func(t *testing.T) {
		bundle, err := parseBundle(readTestFixture(t, "sigstore.js@2.0.0-provenance.sigstore.json"))
		if err != nil {
			t.Fatalf("parseBundle: %v", err)
		}

		claims := extractFulcioClaims(bundle.leafCert)
		if claims.Issuer == "" {
			t.Error("Issuer is empty")
		}
		if claims.SubjectAlternativeName == "" {
			t.Error("SubjectAlternativeName is empty")
		}
		// GitHub Actions bundles should have build config URI.
		if claims.BuildConfigURI == "" {
			t.Error("BuildConfigURI is empty for GitHub Actions bundle")
		}
		t.Logf("issuer=%q subject=%q buildConfig=%q",
			claims.Issuer, claims.SubjectAlternativeName, claims.BuildConfigURI)
	})
}

func TestMerkleProofVerification(t *testing.T) {
	// Verify the Merkle inclusion proof from the messageSignature
	// fixture independently.
	bundle, err := parseBundle(readTestFixture(t, "othername.sigstore.json"))
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}

	if bundle.tlogEntry.inclusionProof == nil {
		t.Fatal("expected inclusion proof in v0.3 bundle")
	}

	proof := bundle.tlogEntry.inclusionProof
	leafHash := rfc6962LeafHash(bundle.tlogEntry.canonicalizedBody)

	if err := verifyInclusionProof(
		proof.logIndex, proof.treeSize,
		proof.rootHash, leafHash, proof.hashes,
	); err != nil {
		t.Fatalf("verifyInclusionProof: %v", err)
	}

	t.Run("tampered leaf rejected", func(t *testing.T) {
		tamperedBody := make([]byte, len(bundle.tlogEntry.canonicalizedBody))
		copy(tamperedBody, bundle.tlogEntry.canonicalizedBody)
		tamperedBody[0] ^= 0xff

		tamperedLeafHash := rfc6962LeafHash(tamperedBody)
		if err := verifyInclusionProof(
			proof.logIndex, proof.treeSize,
			proof.rootHash, tamperedLeafHash, proof.hashes,
		); err == nil {
			t.Fatal("expected error for tampered leaf")
		}
	})
}

func TestCheckpointVerification(t *testing.T) {
	// Verify the checkpoint from the messageSignature fixture
	// independently.
	bundleData := readTestFixture(t, "othername.sigstore.json")
	trustedRootData := readTestFixture(t, "trusted-root-scaffolding.json")

	bundle, err := parseBundle(bundleData)
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}

	root, err := parseTrustedRootJSON(trustedRootData)
	if err != nil {
		t.Fatalf("parseTrustedRootJSON: %v", err)
	}

	if bundle.tlogEntry.inclusionProof == nil {
		t.Fatal("expected inclusion proof")
	}

	proof := bundle.tlogEntry.inclusionProof
	if err := verifyCheckpoint(
		proof.checkpoint, proof.rootHash, proof.treeSize,
		root.rekorKey, root.rekorKeyID,
	); err != nil {
		t.Fatalf("verifyCheckpoint: %v", err)
	}
}

// ---- Attack-shaped tests ----------------------------------------------
//
// These tests verify that specific attack vectors are rejected. Each test
// corresponds to a finding from the cross-validated security review.

func TestTlogBindingRejectsSubstitutedEntry(t *testing.T) {
	// Attack: take a valid bundle and replace its tlog entry's
	// canonicalizedBody with one from a different signing event.
	// Without tlog binding verification, the Merkle proof and
	// checkpoint would still verify (they prove the OTHER entry
	// exists in the log), but the entry wouldn't match THIS
	// bundle's signature/cert.
	bundleData := readTestFixture(t, "othername.sigstore.json")
	trustedRootData := readTestFixture(t, "trusted-root-scaffolding.json")

	root, err := parseTrustedRootFile(trustedRootData)
	if err != nil {
		t.Fatalf("parseTrustedRootFile: %v", err)
	}

	bundle, err := parseBundle(bundleData)
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}

	// Tamper the signature within the bundle so it no longer matches
	// the signature embedded in canonicalizedBody.
	originalSig := make([]byte, len(bundle.signature))
	copy(originalSig, bundle.signature)
	bundle.signature[0] ^= 0xff

	err = verifyTlogEntry(&bundle.tlogEntry, root.rekorKey, root.rekorKeyID, bundle)
	if err == nil {
		t.Fatal("expected rejection when bundle signature doesn't match tlog entry")
	}
	t.Logf("correctly rejected: %v", err)

	// Restore and tamper the leaf cert instead.
	copy(bundle.signature, originalSig)
	bundle.leafCert = &x509.Certificate{Raw: []byte("wrong-cert")}

	err = verifyTlogEntry(&bundle.tlogEntry, root.rekorKey, root.rekorKeyID, bundle)
	if err == nil {
		t.Fatal("expected rejection when bundle cert doesn't match tlog entry")
	}
	t.Logf("correctly rejected: %v", err)
}

func TestTlogBindingRejectsSubstitutedDSSEEntry(t *testing.T) {
	// Same attack as above but for DSSE/intoto bundles, which have
	// the additional wrinkle of double-base64 signature encoding.
	bundleData := readTestFixture(t, "sigstore.js@2.0.0-provenance.sigstore.json")
	trustedRootData := readTestFixture(t, "trusted-root-public-good.json")

	root, err := parseTrustedRootFile(trustedRootData)
	if err != nil {
		t.Fatalf("parseTrustedRootFile: %v", err)
	}

	bundle, err := parseBundle(bundleData)
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}

	// Tamper the signature.
	bundle.signature[0] ^= 0xff

	err = verifyTlogEntry(&bundle.tlogEntry, root.rekorKey, root.rekorKeyID, bundle)
	if err == nil {
		t.Fatal("expected rejection when DSSE bundle signature doesn't match intoto tlog entry")
	}
	t.Logf("correctly rejected: %v", err)
}

func TestCertValidityWindowEnforced(t *testing.T) {
	// Attack: use a signing key from an expired Fulcio cert. The
	// cert was valid when issued but has since expired. Without
	// checking integratedTime against the cert validity period,
	// the verifier would accept signatures made after cert expiry.
	bundleData := readTestFixture(t, "othername.sigstore.json")
	trustedRootData := readTestFixture(t, "trusted-root-scaffolding.json")

	root, err := parseTrustedRootFile(trustedRootData)
	if err != nil {
		t.Fatalf("parseTrustedRootFile: %v", err)
	}

	bundle, err := parseBundle(bundleData)
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}

	t.Run("integratedTime before cert validity", func(t *testing.T) {
		saved := bundle.tlogEntry.integratedTime
		defer func() { bundle.tlogEntry.integratedTime = saved }()

		// Set integratedTime to well before the cert was issued.
		bundle.tlogEntry.integratedTime = bundle.leafCert.NotBefore.Add(-24 * time.Hour).Unix()

		err := verifyTlogEntry(&bundle.tlogEntry, root.rekorKey, root.rekorKeyID, bundle)
		if err == nil {
			t.Fatal("expected rejection for integratedTime before cert validity")
		}
		t.Logf("correctly rejected: %v", err)
	})

	t.Run("integratedTime after cert expiry", func(t *testing.T) {
		saved := bundle.tlogEntry.integratedTime
		defer func() { bundle.tlogEntry.integratedTime = saved }()

		// Set integratedTime to after the cert expired.
		bundle.tlogEntry.integratedTime = bundle.leafCert.NotAfter.Add(24 * time.Hour).Unix()

		err := verifyTlogEntry(&bundle.tlogEntry, root.rekorKey, root.rekorKeyID, bundle)
		if err == nil {
			t.Fatal("expected rejection for integratedTime after cert expiry")
		}
		t.Logf("correctly rejected: %v", err)
	})
}

func TestLogIDMismatchRejected(t *testing.T) {
	// Attack: present a tlog entry claiming to be from log A while
	// we're verifying against log B's key. The checkpoint signature
	// check would also catch this, but the LogID check is a faster
	// and more informative first line of defense.
	bundleData := readTestFixture(t, "othername.sigstore.json")
	trustedRootData := readTestFixture(t, "trusted-root-scaffolding.json")

	root, err := parseTrustedRootFile(trustedRootData)
	if err != nil {
		t.Fatalf("parseTrustedRootFile: %v", err)
	}

	bundle, err := parseBundle(bundleData)
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}

	// Replace the trusted root's key ID with a different value.
	wrongKeyID := make([]byte, len(root.rekorKeyID))
	copy(wrongKeyID, root.rekorKeyID)
	wrongKeyID[0] ^= 0xff

	err = verifyTlogEntry(&bundle.tlogEntry, root.rekorKey, wrongKeyID, bundle)
	if err == nil {
		t.Fatal("expected rejection for LogID mismatch")
	}
	if !strings.Contains(err.Error(), "LogID") {
		t.Errorf("error should mention LogID, got: %v", err)
	}
}

func TestFilterHandledCriticalExtensions(t *testing.T) {
	// Verify that unknown critical extensions survive filtering.
	// If all critical extensions were blanket-removed, a certificate
	// with a genuinely unknown critical constraint would be silently
	// accepted instead of being rejected by x509.Verify.

	unknownOID := asn1.ObjectIdentifier{1, 2, 3, 4, 5, 6, 7}
	fulcioOID := oidIssuerV2
	sanOID := oidSubjectAltName

	input := []asn1.ObjectIdentifier{unknownOID, fulcioOID, sanOID}
	remaining := filterHandledCriticalExtensions(input)

	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining OID, got %d: %v", len(remaining), remaining)
	}
	if !remaining[0].Equal(unknownOID) {
		t.Errorf("expected unknown OID %v to survive filtering, got %v", unknownOID, remaining[0])
	}
}

func TestFilterHandledCriticalExtensionsUnknownFulcioOID(t *testing.T) {
	// A future Fulcio extension OID (under the base prefix) should
	// still be filtered out, even if we haven't explicitly listed it.
	futureOID := append(append(asn1.ObjectIdentifier{}, fulcioOIDBase...), 99)
	input := []asn1.ObjectIdentifier{futureOID}

	remaining := filterHandledCriticalExtensions(input)
	if len(remaining) != 0 {
		t.Errorf("expected future Fulcio OID to be filtered, got %v", remaining)
	}
}

// buildSANWithOtherName constructs a SAN extension value containing a
// single otherName entry with the given type-id OID and UTF8String value.
//
// The ASN.1 structure is:
//
//	SEQUENCE {
//	    [0] CONSTRUCTED {          -- otherName GeneralName
//	        OBJECT IDENTIFIER,    -- type-id
//	        [0] EXPLICIT {        -- value wrapper
//	            UTF8String        -- the identity string
//	        }
//	    }
//	}
func buildSANWithOtherName(typeOID asn1.ObjectIdentifier, value string) []byte {
	utf8String, _ := asn1.Marshal(value)
	explicitValue := asn1.RawValue{
		Class:      asn1.ClassContextSpecific,
		Tag:        0,
		IsCompound: true,
		Bytes:      utf8String,
	}
	explicitValueDER, _ := asn1.Marshal(explicitValue)

	oidDER, _ := asn1.Marshal(typeOID)
	otherNameContent := slices.Concat(oidDER, explicitValueDER)

	otherName := asn1.RawValue{
		Class:      asn1.ClassContextSpecific,
		Tag:        0,
		IsCompound: true,
		Bytes:      otherNameContent,
	}
	otherNameDER, _ := asn1.Marshal(otherName)

	// Wrap in SEQUENCE (the SAN extension value is a SEQUENCE of
	// GeneralName entries).
	sanSequence, _ := asn1.Marshal(asn1.RawValue{
		Class:      asn1.ClassUniversal,
		Tag:        asn1.TagSequence,
		IsCompound: true,
		Bytes:      otherNameDER,
	})
	return sanSequence
}

func TestExtractOtherNameSANIgnoresNonFulcioOID(t *testing.T) {
	// Attack: A certificate with an otherName SAN using a non-Fulcio
	// OID should not have its value extracted. Without the OID check,
	// an attacker could get a cert with an arbitrary otherName type
	// (e.g., a hardware serial number) and have it treated as an
	// identity SAN.
	sanValue := buildSANWithOtherName(
		asn1.ObjectIdentifier{1, 2, 3, 4, 5}, // non-Fulcio OID
		"attacker@evil.com",
	)

	cert := &x509.Certificate{
		Extensions: []pkix.Extension{
			{Id: oidSubjectAltName, Value: sanValue},
		},
	}

	result := extractOtherNameSAN(cert)
	if result != "" {
		t.Errorf("expected empty string for non-Fulcio otherName OID, got %q", result)
	}
}

func TestExtractOtherNameSANAcceptsFulcioOID(t *testing.T) {
	// Verify that a Fulcio otherName OID IS extracted.
	sanValue := buildSANWithOtherName(oidFulcioOtherName, "legit-user")

	cert := &x509.Certificate{
		Extensions: []pkix.Extension{
			{Id: oidSubjectAltName, Value: sanValue},
		},
	}

	result := extractOtherNameSAN(cert)
	if result != "legit-user" {
		t.Errorf("extractOtherNameSAN = %q, want %q", result, "legit-user")
	}
}

func TestMatchesDigestCaseInsensitive(t *testing.T) {
	// The in-toto spec doesn't mandate hex case. Different tools
	// produce different casing. Without case-insensitive comparison,
	// valid digests could be rejected based on capitalization alone.
	digest := []byte{0xAB, 0xCD, 0xEF, 0x01, 0x23}
	lowerHex := "abcdef0123"
	upperHex := "ABCDEF0123"
	mixedHex := "AbCdEf0123"

	tests := []struct {
		name      string
		hexDigest string
	}{
		{"lowercase", lowerHex},
		{"uppercase", upperHex},
		{"mixed case", mixedHex},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			subject := inTotoSubject{
				Digest: map[string]string{"sha256": test.hexDigest},
			}
			if !subject.matchesDigest("sha256", digest) {
				t.Errorf("matchesDigest should accept %q hex for digest %x", test.hexDigest, digest)
			}
		})
	}
}

func TestFetchBundleRejectsPathTraversal(t *testing.T) {
	// Attack: a malicious store path basename containing path
	// traversal sequences could request unintended URLs.
	tests := []struct {
		name     string
		basename string
	}{
		{"path traversal", "../../../etc/passwd"},
		{"query injection", "legitimate?evil=true"},
		{"fragment injection", "legitimate#fragment"},
		{"empty", ""},
		{"slashes", "some/nested/path"},
		{"missing hash prefix", "not-a-valid-nix-basename"},
		{"too short hash", "abc-name"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := FetchBundle(&http.Client{}, "https://cache.example.com", test.basename)
			if err == nil {
				t.Errorf("expected rejection for basename %q", test.basename)
			}
			if !strings.Contains(err.Error(), "invalid store path basename") {
				t.Errorf("expected validation error, got: %v", err)
			}
		})
	}

	t.Run("valid basename format accepted", func(t *testing.T) {
		if validStorePathBasename.MatchString("abcdefghijklmnopqrstuvwxyz012345-bureau-env") != true {
			t.Error("expected valid Nix store path basename to pass regex")
		}
	})

	t.Run("valid basename with special chars", func(t *testing.T) {
		if validStorePathBasename.MatchString("abcdefghijklmnopqrstuvwxyz012345-bureau_env.1.2.3") != true {
			t.Error("expected basename with dots and underscores to pass regex")
		}
	})
}

// ---- Round 2 attack-shaped tests --------------------------------------

func TestDigestAlgorithmAllowlist(t *testing.T) {
	// Attack: an attacker crafts a bundle with a weak hash algorithm
	// (e.g., MD5 or SHA1) and a valid signature over that weak digest.
	// If the verifier accepts arbitrary algorithms, digest collision
	// attacks become feasible.

	tests := []struct {
		algorithm string
		wantErr   bool
	}{
		{"SHA2_256", false},
		{"SHA2_384", false},
		{"SHA2_512", false},
		{"sha256", false},
		{"sha384", false},
		{"sha512", false},
		{"MD5", true},
		{"SHA1", true},
		{"sha1", true},
		{"md5", true},
		{"SHA3_256", true}, // not supported in our verification
		{"BLAKE2b", true},
		{"", true},
	}

	for _, test := range tests {
		_, err := normalizeHashAlgorithm(test.algorithm)
		if test.wantErr && err == nil {
			t.Errorf("normalizeHashAlgorithm(%q) = nil error, want rejection", test.algorithm)
		}
		if !test.wantErr && err != nil {
			t.Errorf("normalizeHashAlgorithm(%q) = %v, want success", test.algorithm, err)
		}
	}
}

func TestIssuerV2TakesPriorityOverV1(t *testing.T) {
	// Attack: a certificate contains both V1 and V2 issuer extensions
	// with different values. The V2 extension (DER-encoded UTF8String)
	// must always take priority. An attacker who can influence the V1
	// value should not be able to override the V2 issuer.

	v2Issuer := "https://legitimate-issuer.example.com"
	v1Issuer := "https://attacker-controlled-issuer.example.com"

	// DER-encode the V2 issuer as a UTF8String.
	v2Value, err := asn1.Marshal(v2Issuer)
	if err != nil {
		t.Fatalf("marshal V2 issuer: %v", err)
	}

	cert := &x509.Certificate{
		Extensions: []pkix.Extension{
			// V1 extension: raw string bytes.
			{Id: oidIssuerV1, Value: []byte(v1Issuer)},
			// V2 extension: DER-encoded UTF8String.
			{Id: oidIssuerV2, Value: v2Value},
		},
	}

	claims := extractFulcioClaims(cert)
	if claims.Issuer != v2Issuer {
		t.Errorf("Issuer = %q, want V2 value %q", claims.Issuer, v2Issuer)
	}
}

func TestIssuerV1FallbackWhenNoV2(t *testing.T) {
	// Verify that V1 issuer is used when V2 is absent.
	v1Issuer := "https://token.actions.githubusercontent.com"

	cert := &x509.Certificate{
		Extensions: []pkix.Extension{
			{Id: oidIssuerV1, Value: []byte(v1Issuer)},
		},
	}

	claims := extractFulcioClaims(cert)
	if claims.Issuer != v1Issuer {
		t.Errorf("Issuer = %q, want V1 fallback %q", claims.Issuer, v1Issuer)
	}
}

func TestSETRejectsTamperedIntegratedTime(t *testing.T) {
	// SET verification prevents an attacker from modifying
	// integratedTime in the bundle JSON to make an expired-cert
	// signature appear valid. Even a 1-second shift invalidates the
	// SET because the canonical payload includes integratedTime.
	bundleData := readTestFixture(t, "othername.sigstore.json")
	trustedRootData := readTestFixture(t, "trusted-root-scaffolding.json")

	root, err := parseTrustedRootFile(trustedRootData)
	if err != nil {
		t.Fatalf("parseTrustedRootFile: %v", err)
	}

	bundle, err := parseBundle(bundleData)
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}

	bundle.tlogEntry.integratedTime = bundle.tlogEntry.integratedTime + 1

	err = verifyTlogEntry(&bundle.tlogEntry, root.rekorKey, root.rekorKeyID, bundle)
	if err == nil {
		t.Fatal("tampered integratedTime was accepted — SET verification should have rejected it")
	}
	if !strings.Contains(err.Error(), "SET signature verification failed") {
		t.Fatalf("expected SET signature failure, got: %v", err)
	}
}

func TestSETRejectsTamperedLogIndex(t *testing.T) {
	// The SET also covers logIndex. An attacker modifying logIndex
	// (e.g., to confuse audit tools about entry ordering) must be
	// caught by SET verification.
	bundleData := readTestFixture(t, "othername.sigstore.json")
	trustedRootData := readTestFixture(t, "trusted-root-scaffolding.json")

	root, err := parseTrustedRootFile(trustedRootData)
	if err != nil {
		t.Fatalf("parseTrustedRootFile: %v", err)
	}

	bundle, err := parseBundle(bundleData)
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}

	bundle.tlogEntry.logIndex = bundle.tlogEntry.logIndex + 1

	err = verifyTlogEntry(&bundle.tlogEntry, root.rekorKey, root.rekorKeyID, bundle)
	if err == nil {
		t.Fatal("tampered logIndex was accepted — SET verification should have rejected it")
	}
	if !strings.Contains(err.Error(), "SET signature verification failed") {
		t.Fatalf("expected SET signature failure, got: %v", err)
	}
}

func TestSETRejectsTamperedCanonicalizedBody(t *testing.T) {
	// The SET covers the canonicalizedBody (as a base64 string).
	// Tampering with it should fail SET verification even before
	// the tlog binding check catches the mismatch.
	bundleData := readTestFixture(t, "othername.sigstore.json")
	trustedRootData := readTestFixture(t, "trusted-root-scaffolding.json")

	root, err := parseTrustedRootFile(trustedRootData)
	if err != nil {
		t.Fatalf("parseTrustedRootFile: %v", err)
	}

	bundle, err := parseBundle(bundleData)
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}

	// Replace the base64 body with a different base64 string.
	bundle.tlogEntry.canonicalizedBodyB64 = "dGFtcGVyZWQ="

	err = verifyTlogEntry(&bundle.tlogEntry, root.rekorKey, root.rekorKeyID, bundle)
	if err == nil {
		t.Fatal("tampered canonicalizedBody was accepted")
	}
	if !strings.Contains(err.Error(), "SET signature verification failed") {
		t.Fatalf("expected SET signature failure, got: %v", err)
	}
}

func TestSETRejectsMissingSET(t *testing.T) {
	// A bundle without a SET must be rejected. An attacker stripping
	// the SET would remove the only authentication of integratedTime.
	bundleData := readTestFixture(t, "othername.sigstore.json")
	trustedRootData := readTestFixture(t, "trusted-root-scaffolding.json")

	root, err := parseTrustedRootFile(trustedRootData)
	if err != nil {
		t.Fatalf("parseTrustedRootFile: %v", err)
	}

	bundle, err := parseBundle(bundleData)
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}

	// Strip the SET.
	bundle.tlogEntry.signedEntryTimestamp = nil

	err = verifyTlogEntry(&bundle.tlogEntry, root.rekorKey, root.rekorKeyID, bundle)
	if err == nil {
		t.Fatal("missing SET was accepted")
	}
	if !strings.Contains(err.Error(), "no signed entry timestamp") {
		t.Fatalf("expected missing SET error, got: %v", err)
	}
}

func TestSETVerifiesWithBothFixtures(t *testing.T) {
	// Verify SET passes with both fixture types (messageSignature
	// and DSSE/intoto) to confirm our canonical JSON reconstruction
	// matches what both the scaffolding and public-good Rekor
	// instances signed.
	tests := []struct {
		name        string
		bundle      string
		trustedRoot string
	}{
		{
			name:        "scaffolding/messageSignature",
			bundle:      "othername.sigstore.json",
			trustedRoot: "trusted-root-scaffolding.json",
		},
		{
			name:        "public-good/DSSE",
			bundle:      "sigstore.js@2.0.0-provenance.sigstore.json",
			trustedRoot: "trusted-root-public-good.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundleData := readTestFixture(t, tt.bundle)
			trustedRootData := readTestFixture(t, tt.trustedRoot)

			root, err := parseTrustedRootFile(trustedRootData)
			if err != nil {
				t.Fatalf("parseTrustedRootFile: %v", err)
			}

			bundle, err := parseBundle(bundleData)
			if err != nil {
				t.Fatalf("parseBundle: %v", err)
			}

			// Verify SET directly (not through full verifyTlogEntry,
			// to isolate SET verification from other checks).
			err = verifySignedEntryTimestamp(&bundle.tlogEntry, root.rekorKey)
			if err != nil {
				t.Fatalf("SET verification failed: %v", err)
			}
		})
	}
}

func TestTrustedRootSeparatesIntermediatesFromRoots(t *testing.T) {
	// The public-good trusted root has both a root CA (self-signed)
	// and an intermediate CA. parseTrustedRootJSON must separate
	// them: intermediates must not be trust anchors, because that
	// would bypass path length and name constraints from the root.
	trustedRootData := readTestFixture(t, "trusted-root-public-good.json")

	root, err := parseTrustedRootFile(trustedRootData)
	if err != nil {
		t.Fatalf("parseTrustedRootFile: %v", err)
	}

	// The public-good root has 1 intermediate (sigstore-intermediate)
	// and 1 root (sigstore). The root cert appears in two CA entries
	// but should be deduplicated or at least all present copies
	// should be in fulcioRoots, not fulcioIntermediates.
	if len(root.fulcioRoots) == 0 {
		t.Fatal("no root CAs found")
	}
	for _, rootCert := range root.fulcioRoots {
		if rootCert.Subject.String() != rootCert.Issuer.String() {
			t.Errorf("non-self-signed cert in roots: subject=%q issuer=%q",
				rootCert.Subject, rootCert.Issuer)
		}
	}
	for _, intermediate := range root.fulcioIntermediates {
		if intermediate.Subject.String() == intermediate.Issuer.String() {
			t.Errorf("self-signed cert in intermediates: subject=%q",
				intermediate.Subject)
		}
	}

	t.Logf("roots=%d intermediates=%d", len(root.fulcioRoots), len(root.fulcioIntermediates))

	// Verify that the DSSE fixture still validates correctly with
	// the separated pools. The leaf cert chains through the
	// intermediate to the root.
	bundleData := readTestFixture(t, "sigstore.js@2.0.0-provenance.sigstore.json")
	bundle, err := parseBundle(bundleData)
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}

	if err := verifyCertificateChain(bundle.leafCert, root); err != nil {
		t.Fatalf("cert chain with separated intermediates: %v", err)
	}
}

func TestIntermediateOnlyDoesNotVerify(t *testing.T) {
	// If we have only intermediates (no roots), verifyCertificateChain
	// must reject because there is no trust anchor to chain to.
	bundleData := readTestFixture(t, "sigstore.js@2.0.0-provenance.sigstore.json")
	trustedRootData := readTestFixture(t, "trusted-root-public-good.json")

	fullRoot, err := parseTrustedRootFile(trustedRootData)
	if err != nil {
		t.Fatalf("parseTrustedRootFile: %v", err)
	}

	bundle, err := parseBundle(bundleData)
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}

	// Create a parsedRoot with only intermediates as roots (the
	// old vulnerable behavior) — but with no actual roots.
	if len(fullRoot.fulcioIntermediates) == 0 {
		t.Skip("no intermediates in public-good root to test with")
	}

	badRoot := &parsedRoot{
		fulcioRoots:         nil, // no trust anchors
		fulcioIntermediates: fullRoot.fulcioIntermediates,
	}

	err = verifyCertificateChain(bundle.leafCert, badRoot)
	if err == nil {
		t.Fatal("cert chain verified with no root CAs — intermediates should not be trust anchors")
	}
}

func TestIsSelfSignedRejectsSameDNIntermediate(t *testing.T) {
	// A CA intermediate issued with the same Subject DN as its root
	// must not be classified as self-signed. The old heuristic
	// (Issuer.String() == Subject.String()) would misclassify this
	// as a root. The cryptographic check catches it because the
	// intermediate's signature was made by the root's key, not its
	// own.
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate root key: %v", err)
	}

	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "same-dn-ca"},
		NotBefore:             time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatalf("create root cert: %v", err)
	}
	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatalf("parse root cert: %v", err)
	}

	// Create an intermediate with the SAME Subject DN, signed by the root.
	interKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate intermediate key: %v", err)
	}

	interTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "same-dn-ca"}, // Same DN!
		NotBefore:             time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	interDER, err := x509.CreateCertificate(rand.Reader, interTemplate, rootCert, &interKey.PublicKey, rootKey)
	if err != nil {
		t.Fatalf("create intermediate cert: %v", err)
	}
	interCert, err := x509.ParseCertificate(interDER)
	if err != nil {
		t.Fatalf("parse intermediate cert: %v", err)
	}

	// Verify the DNs match (confirming the attack scenario).
	if interCert.Issuer.String() != interCert.Subject.String() {
		t.Fatalf("test setup: Issuer=%q != Subject=%q", interCert.Issuer, interCert.Subject)
	}

	// The root must be classified as self-signed.
	if !isSelfSigned(rootCert) {
		t.Error("root cert not classified as self-signed")
	}

	// The intermediate must NOT be classified as self-signed,
	// despite having the same Issuer and Subject DN.
	if isSelfSigned(interCert) {
		t.Error("same-DN intermediate was incorrectly classified as self-signed")
	}
}

// TestSyntheticEndToEndVerification constructs a complete Sigstore
// bundle from scratch — CA, leaf cert, signature, tlog entry, Merkle
// tree, checkpoint, SET — and verifies it through the full Verifier
// pipeline. This proves our implementation matches the Sigstore
// specification in the general case, not just for our two test
// fixtures from specific Rekor instances.
func TestSyntheticEndToEndVerification(t *testing.T) {
	// ---- Step 1: Generate CA and Rekor keys ----

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}

	rekorKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate Rekor key: %v", err)
	}

	signingKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}

	// ---- Step 2: Create CA certificate ----

	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-fulcio-root"},
		NotBefore:             time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}

	// ---- Step 3: Issue Fulcio-like leaf certificate ----

	// Encode OIDC claims as V2 DER-encoded UTF8Strings.
	issuerValue := "https://token.actions.githubusercontent.com"
	issuerDER, _ := asn1.Marshal(issuerValue)

	buildConfigURI := "https://github.com/test-org/test-repo/.github/workflows/release.yml@refs/heads/main"
	buildConfigDER, _ := asn1.Marshal(buildConfigURI)

	subjectURI, _ := url.Parse("https://github.com/test-org/test-repo/.github/workflows/release.yml@refs/heads/main")

	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{},
		NotBefore:    time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(2026, 6, 15, 12, 10, 0, 0, time.UTC), // 10-minute Fulcio cert
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{subjectURI},
		ExtraExtensions: []pkix.Extension{
			{Id: oidIssuerV2, Value: issuerDER, Critical: true},
			{Id: oidBuildConfigURI, Value: buildConfigDER, Critical: true},
		},
	}

	leafCertDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &signingKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}

	// ---- Step 4: Sign an artifact ----

	artifactDigest := sha256.Sum256([]byte("synthetic-test-artifact-content"))

	signature, err := ecdsa.SignASN1(rand.Reader, signingKey, artifactDigest[:])
	if err != nil {
		t.Fatalf("sign artifact: %v", err)
	}

	// ---- Step 5: Construct Rekor tlog entry body ----

	// Build a hashedrekord entry body matching the Rekor format.
	// The Rekor hashedrekord format stores the certificate as
	// base64(PEM(DER)), not base64(DER) directly.
	leafCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafCertDER})

	entryBody := map[string]interface{}{
		"apiVersion": "0.0.1",
		"kind":       "hashedrekord",
		"spec": map[string]interface{}{
			"signature": map[string]interface{}{
				"content": base64.StdEncoding.EncodeToString(signature),
				"publicKey": map[string]interface{}{
					"content": base64.StdEncoding.EncodeToString(leafCertPEM),
				},
			},
			"data": map[string]interface{}{
				"hash": map[string]interface{}{
					"algorithm": "sha256",
					"value":     hex.EncodeToString(artifactDigest[:]),
				},
			},
		},
	}

	entryBodyJSON, err := json.Marshal(entryBody)
	if err != nil {
		t.Fatalf("marshal entry body: %v", err)
	}
	canonicalizedBodyB64 := base64.StdEncoding.EncodeToString(entryBodyJSON)

	// ---- Step 6: Build Merkle tree (single leaf) ----

	// RFC 6962 leaf hash: SHA-256(0x00 || leaf data)
	leafHasher := sha256.New()
	leafHasher.Write([]byte{0x00})
	leafHasher.Write(entryBodyJSON)
	leafHash := leafHasher.Sum(nil)

	// For a single-leaf tree, the root hash equals the leaf hash.
	rootHash := leafHash
	treeSize := int64(1)
	logIndex := int64(0)

	// ---- Step 7: Sign checkpoint ----

	rekorKeyDER, err := x509.MarshalPKIXPublicKey(&rekorKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal Rekor public key: %v", err)
	}
	rekorKeyIDFull := sha256.Sum256(rekorKeyDER)
	rekorKeyID := rekorKeyIDFull[:]
	checkpointKeyID := rekorKeyIDFull[:4]

	originLine := "test-rekor-instance - 1234567890"
	checkpointBody := fmt.Sprintf("%s\n%d\n%s\n",
		originLine,
		treeSize,
		base64.StdEncoding.EncodeToString(rootHash),
	)

	checkpointDigest := sha256.Sum256([]byte(checkpointBody))
	checkpointSig, err := ecdsa.SignASN1(rand.Reader, rekorKey, checkpointDigest[:])
	if err != nil {
		t.Fatalf("sign checkpoint: %v", err)
	}

	// Build C2SP signed note format.
	sigPayload := slices.Concat(checkpointKeyID, checkpointSig)
	checkpoint := checkpointBody + "\n\u2014 test-rekor-instance " +
		base64.StdEncoding.EncodeToString(sigPayload) + "\n"

	// ---- Step 8: Generate SET ----

	// Must be within the leaf cert's validity window:
	// 2026-06-15 12:00:00 to 2026-06-15 12:10:00 UTC
	integratedTime := time.Date(2026, 6, 15, 12, 5, 0, 0, time.UTC).Unix()

	setPayloadObj := setPayload{
		Body:           canonicalizedBodyB64,
		IntegratedTime: integratedTime,
		LogID:          hex.EncodeToString(rekorKeyID),
		LogIndex:       logIndex,
	}
	setPayloadJSON, err := json.Marshal(setPayloadObj)
	if err != nil {
		t.Fatalf("marshal SET payload: %v", err)
	}
	setDigest := sha256.Sum256(setPayloadJSON)
	setSignature, err := ecdsa.SignASN1(rand.Reader, rekorKey, setDigest[:])
	if err != nil {
		t.Fatalf("sign SET: %v", err)
	}

	// ---- Step 9: Assemble bundle JSON ----

	bundle := rawBundle{
		MediaType: "application/vnd.dev.sigstore.bundle.v0.3+json",
		VerificationMaterial: rawVerificationMaterial{
			Certificate: &rawCertificate{
				RawBytes: base64.StdEncoding.EncodeToString(leafCertDER),
			},
			TlogEntries: []rawTlogEntry{
				{
					LogIndex: fmt.Sprintf("%d", logIndex),
					LogID: rawLogID{
						KeyID: base64.StdEncoding.EncodeToString(rekorKeyID),
					},
					KindVersion: rawKindVersion{
						Kind:    "hashedrekord",
						Version: "0.0.1",
					},
					IntegratedTime: fmt.Sprintf("%d", integratedTime),
					InclusionPromise: &rawInclusionPromise{
						SignedEntryTimestamp: base64.StdEncoding.EncodeToString(setSignature),
					},
					InclusionProof: &rawInclusionProof{
						LogIndex: fmt.Sprintf("%d", logIndex),
						RootHash: base64.StdEncoding.EncodeToString(rootHash),
						TreeSize: fmt.Sprintf("%d", treeSize),
						Hashes:   []string{}, // single-leaf tree has no proof hashes
						Checkpoint: rawCheckpoint{
							Envelope: checkpoint,
						},
					},
					CanonicalizedBody: canonicalizedBodyB64,
				},
			},
		},
		MessageSignature: &rawMessageSignature{
			MessageDigest: rawMessageDigest{
				Algorithm: "SHA2_256",
				Digest:    base64.StdEncoding.EncodeToString(artifactDigest[:]),
			},
			Signature: base64.StdEncoding.EncodeToString(signature),
		},
	}

	bundleJSON, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}

	// ---- Step 10: Build trust root and policy ----

	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})
	rekorKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: rekorKeyDER})

	roots := schema.ProvenanceRootsContent{
		Roots: map[string]schema.ProvenanceTrustRoot{
			"synthetic": {
				FulcioRootPEM:     string(caCertPEM),
				RekorPublicKeyPEM: string(rekorKeyPEM),
			},
		},
	}

	policy := schema.ProvenancePolicyContent{
		Enforcement: map[string]schema.EnforcementLevel{
			"nix-store-path": schema.EnforcementRequire,
		},
		TrustedIdentities: []schema.TrustedIdentity{
			{
				Name:           "github-actions-test",
				Issuer:         issuerValue,
				SubjectPattern: "https://github.com/test-org/*",
				Roots:          "synthetic",
			},
		},
	}

	// ---- Step 11: Verify ----

	verifier, err := NewVerifier(roots, policy)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	result := verifier.Verify(bundleJSON, "sha256", artifactDigest[:])
	if result.Status != StatusVerified {
		t.Fatalf("verification failed: status=%s error=%v", result.Status, result.Error)
	}

	if result.Identity != "github-actions-test" {
		t.Errorf("identity = %q, want %q", result.Identity, "github-actions-test")
	}
	if result.Issuer != issuerValue {
		t.Errorf("issuer = %q, want %q", result.Issuer, issuerValue)
	}
	if result.IntegratedTime != integratedTime {
		t.Errorf("integratedTime = %d, want %d", result.IntegratedTime, integratedTime)
	}

	t.Logf("synthetic bundle verified: identity=%q issuer=%q subject=%q integratedTime=%d",
		result.Identity, result.Issuer, result.Subject, result.IntegratedTime)

	// ---- Step 12: Verify rejection of wrong digest ----

	wrongDigest := sha256.Sum256([]byte("wrong-content"))
	wrongResult := verifier.Verify(bundleJSON, "sha256", wrongDigest[:])
	if wrongResult.Status != StatusRejected {
		t.Fatal("wrong digest was not rejected")
	}

	// ---- Step 13: Verify rejection of tampered integratedTime ----

	var tamperedBundle rawBundle
	if err := json.Unmarshal(bundleJSON, &tamperedBundle); err != nil {
		t.Fatalf("unmarshal for tampering: %v", err)
	}
	tamperedBundle.VerificationMaterial.TlogEntries[0].IntegratedTime = fmt.Sprintf("%d", integratedTime+1)
	tamperedJSON, _ := json.Marshal(tamperedBundle)
	tamperedResult := verifier.Verify(tamperedJSON, "sha256", artifactDigest[:])
	if tamperedResult.Status != StatusRejected {
		t.Fatal("tampered integratedTime was not rejected")
	}
	if !strings.Contains(tamperedResult.Error.Error(), "SET signature verification failed") {
		t.Fatalf("expected SET failure for tampered timestamp, got: %v", tamperedResult.Error)
	}
}

func TestProofHashLengthValidation(t *testing.T) {
	// Defense in depth: malformed proof hashes (wrong length) should
	// be rejected explicitly rather than producing a confusing root
	// hash mismatch error.
	rootHash := make([]byte, 32)
	leafHash := make([]byte, 32)

	_, err := rand.Read(rootHash)
	if err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	_, err = rand.Read(leafHash)
	if err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	// A proof with a truncated hash should fail with a specific error.
	truncatedHash := make([]byte, 16) // 16 bytes instead of 32
	err = verifyInclusionProof(0, 2, rootHash, leafHash, [][]byte{truncatedHash})
	if err == nil {
		t.Fatal("expected rejection for truncated proof hash")
	}
	if !strings.Contains(err.Error(), "proof[0] length") {
		t.Errorf("expected hash length error, got: %v", err)
	}
}

func TestGlobNewlineInjection(t *testing.T) {
	// Go's regexp `.*` does NOT match newlines by default. This is
	// a security feature: certificate SANs with embedded newlines
	// are rejected by glob patterns even when using `*`. This
	// prevents newline-injection attacks where an attacker appends
	// `\nevil-data` to a legitimate-looking SAN value.
	tests := []struct {
		pattern string
		value   string
		want    bool
	}{
		// Embedded newline: .* does not cross newlines, so the
		// anchored regex ^repo:org/.*$ fails.
		{"repo:org/*", "repo:org/repo\nrepo:evil/repo", false},
		// Trailing newline on exact match also fails.
		{"repo:org/specific", "repo:org/specific\n", false},
		// Null byte: .* does match null bytes.
		{"repo:org/*", "repo:org/re\x00po", true},
		// Clean values still work.
		{"repo:org/*", "repo:org/repo", true},
	}

	for _, test := range tests {
		got := matchGlob(test.pattern, test.value)
		if got != test.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", test.pattern, test.value, got, test.want)
		}
	}
}

func TestCheckpointOriginCrossCheck(t *testing.T) {
	// The checkpoint's origin line (first line of the note body)
	// must match the signer name from the signature line. This
	// prevents an attacker from grafting a valid checkpoint
	// signature onto a note body from a different log.

	t.Run("mismatched origin rejected", func(t *testing.T) {
		// Construct a checkpoint where the origin line says
		// "evil-log" but the signer name says "rekor".
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

		// Compute the key ID so the checkpoint passes the key
		// ID check and reaches the origin cross-check.
		checkpointKeyID := computeCheckpointKeyID(&key.PublicKey)

		// Build a signed note with the correct key ID prefix
		// but mismatched origin/signer names.
		fakeSig := slices.Concat(checkpointKeyID, make([]byte, 64))
		fakeCheckpoint := "evil-log - 12345\n10\n" +
			base64.StdEncoding.EncodeToString(make([]byte, 32)) +
			"\n\n\u2014 rekor " + base64.StdEncoding.EncodeToString(fakeSig)

		// The rekorKeyID passed to verifyCheckpoint is the full
		// 32-byte SHA-256; the checkpoint only carries the first
		// 4 bytes. Pass a keyID whose first 4 bytes match.
		fullKeyID := make([]byte, 32)
		copy(fullKeyID, checkpointKeyID)

		err := verifyCheckpoint(fakeCheckpoint, make([]byte, 32), 10, &key.PublicKey, fullKeyID)
		if err == nil {
			t.Fatal("checkpoint with mismatched origin was accepted")
		}
		if !strings.Contains(err.Error(), "does not match signer") {
			t.Fatalf("expected origin mismatch error, got: %v", err)
		}
	})

	t.Run("matching origin accepted in real fixtures", func(t *testing.T) {
		// Verify that real fixtures pass the origin cross-check
		// by extracting and comparing the origin and signer names
		// directly.
		for _, fixture := range []string{
			"othername.sigstore.json",
			"sigstore.js@2.0.0-provenance.sigstore.json",
		} {
			bundleData := readTestFixture(t, fixture)
			var raw struct {
				VerificationMaterial struct {
					TlogEntries []struct {
						InclusionProof struct {
							Checkpoint struct {
								Envelope string `json:"envelope"`
							} `json:"checkpoint"`
						} `json:"inclusionProof"`
					} `json:"tlogEntries"`
				} `json:"verificationMaterial"`
			}
			if err := json.Unmarshal(bundleData, &raw); err != nil {
				t.Fatalf("unmarshal %s: %v", fixture, err)
			}

			envelope := raw.VerificationMaterial.TlogEntries[0].InclusionProof.Checkpoint.Envelope
			noteText, signerName, _, _, err := parseSignedNote(envelope)
			if err != nil {
				t.Fatalf("parseSignedNote(%s): %v", fixture, err)
			}

			origin, _, _, err := parseCheckpointBody(noteText)
			if err != nil {
				t.Fatalf("parseCheckpointBody(%s): %v", fixture, err)
			}

			originName := origin
			if dashIndex := strings.Index(origin, " - "); dashIndex >= 0 {
				originName = origin[:dashIndex]
			}

			if signerName != originName {
				t.Errorf("%s: origin %q != signer %q", fixture, originName, signerName)
			} else {
				t.Logf("%s: origin=%q signer=%q (match)", fixture, originName, signerName)
			}
		}
	})
}
