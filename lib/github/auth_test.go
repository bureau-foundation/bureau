// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
)

// testRSAPrivateKeyPEM is a 2048-bit RSA private key for testing.
// Generated once at init time — do not use outside tests.
var testRSAPrivateKeyPEM = generateTestKey()

func generateTestKey() []byte {
	// Generate a real RSA key. 2048 bits is the minimum GitHub
	// accepts, but for tests it only needs to sign correctly.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("generating test RSA key: " + err.Error())
	}

	derBytes := x509.MarshalPKCS1PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: derBytes})
}

func TestGenerateJWT_Structure(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))

	auth, err := newAppAuth(12345, 67890, testRSAPrivateKeyPEM, fakeClock)
	if err != nil {
		t.Fatalf("newAppAuth: %v", err)
	}

	jwt, err := auth.generateJWT()
	if err != nil {
		t.Fatalf("generateJWT: %v", err)
	}

	// JWT has three dot-separated parts.
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}

	// Verify header.
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decoding header: %v", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatalf("parsing header: %v", err)
	}
	if header.Alg != "RS256" {
		t.Errorf("header.alg = %q, want RS256", header.Alg)
	}
	if header.Typ != "JWT" {
		t.Errorf("header.typ = %q, want JWT", header.Typ)
	}

	// Verify claims.
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding claims: %v", err)
	}
	var claims struct {
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
		Issuer    string `json:"iss"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("parsing claims: %v", err)
	}

	expectedIAT := time.Date(2026, 3, 1, 11, 59, 0, 0, time.UTC).Unix()
	expectedEXP := time.Date(2026, 3, 1, 12, 10, 0, 0, time.UTC).Unix()

	if claims.IssuedAt != expectedIAT {
		t.Errorf("iat = %d, want %d", claims.IssuedAt, expectedIAT)
	}
	if claims.ExpiresAt != expectedEXP {
		t.Errorf("exp = %d, want %d", claims.ExpiresAt, expectedEXP)
	}
	if claims.Issuer != "12345" {
		t.Errorf("iss = %q, want %q", claims.Issuer, "12345")
	}

	// Verify signature with public key.
	signingInput := parts[0] + "." + parts[1]
	signatureBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decoding signature: %v", err)
	}

	block, _ := pem.Decode(testRSAPrivateKeyPEM)
	privateKey, _ := x509.ParsePKCS1PrivateKey(block.Bytes)

	err = rsa.VerifyPKCS1v15(&privateKey.PublicKey, crypto.SHA256, sha256Hash([]byte(signingInput)), signatureBytes)
	if err != nil {
		t.Errorf("signature verification failed: %v", err)
	}
}

func TestAppAuth_TokenRotation(t *testing.T) {
	fakeClock := clock.Fake(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))

	requestCount := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount++
		// Verify the request is a token exchange.
		if !strings.Contains(request.URL.Path, "/app/installations/67890/access_tokens") {
			t.Errorf("unexpected path: %s", request.URL.Path)
			http.Error(writer, "not found", 404)
			return
		}

		// Verify JWT is in the Authorization header.
		authHeader := request.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ey") {
			t.Errorf("expected JWT in Authorization header, got %q", authHeader)
		}

		expiresAt := fakeClock.Now().Add(1 * time.Hour)
		writer.WriteHeader(http.StatusCreated)
		json.NewEncoder(writer).Encode(map[string]any{
			"token":      fmt.Sprintf("ghs_test_token_%d", requestCount),
			"expires_at": expiresAt.Format(time.RFC3339),
		})
	}))
	defer server.Close()

	auth, err := newAppAuth(12345, 67890, testRSAPrivateKeyPEM, fakeClock)
	if err != nil {
		t.Fatalf("newAppAuth: %v", err)
	}
	auth.httpClient = server.Client()
	auth.baseURL = server.URL

	ctx := context.Background()

	// First call should trigger token exchange.
	header1, err := auth.AuthorizationHeader(ctx)
	if err != nil {
		t.Fatalf("first AuthorizationHeader: %v", err)
	}
	if header1 != "Bearer ghs_test_token_1" {
		t.Errorf("first token = %q, want %q", header1, "Bearer ghs_test_token_1")
	}
	if requestCount != 1 {
		t.Errorf("expected 1 token exchange, got %d", requestCount)
	}

	// Second call within the token's validity should use cached token.
	header2, err := auth.AuthorizationHeader(ctx)
	if err != nil {
		t.Fatalf("second AuthorizationHeader: %v", err)
	}
	if header2 != "Bearer ghs_test_token_1" {
		t.Errorf("second token = %q, want cached %q", header2, "Bearer ghs_test_token_1")
	}
	if requestCount != 1 {
		t.Errorf("expected still 1 token exchange, got %d", requestCount)
	}

	// Advance clock past the rotation margin (1 hour - 5 min = 55 min).
	fakeClock.Advance(56 * time.Minute)

	// Third call should trigger rotation.
	header3, err := auth.AuthorizationHeader(ctx)
	if err != nil {
		t.Fatalf("third AuthorizationHeader: %v", err)
	}
	if header3 != "Bearer ghs_test_token_2" {
		t.Errorf("third token = %q, want %q", header3, "Bearer ghs_test_token_2")
	}
	if requestCount != 2 {
		t.Errorf("expected 2 token exchanges, got %d", requestCount)
	}
}

func TestTokenAuth(t *testing.T) {
	auth := newTokenAuth("ghp_test123")
	header, err := auth.AuthorizationHeader(context.Background())
	if err != nil {
		t.Fatalf("AuthorizationHeader: %v", err)
	}
	if header != "Bearer ghp_test123" {
		t.Errorf("got %q, want %q", header, "Bearer ghp_test123")
	}
}

func TestNewAppAuth_InvalidPEM(t *testing.T) {
	_, err := newAppAuth(1, 1, []byte("not a pem"), clock.Real())
	if err == nil {
		t.Error("expected error for invalid PEM")
	}
}

func TestNewAppAuth_PKCS8Key(t *testing.T) {
	// Convert the PKCS1 test key to PKCS8 format.
	block, _ := pem.Decode(testRSAPrivateKeyPEM)
	pkcs1Key, _ := x509.ParsePKCS1PrivateKey(block.Bytes)
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(pkcs1Key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	pkcs8PEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes})

	auth, err := newAppAuth(1, 1, pkcs8PEM, clock.Real())
	if err != nil {
		t.Fatalf("newAppAuth with PKCS8: %v", err)
	}

	// Verify it can generate a JWT.
	_, err = auth.generateJWT()
	if err != nil {
		t.Errorf("generateJWT with PKCS8 key: %v", err)
	}
}

// sha256Hash computes SHA-256 for signature verification in tests.
func sha256Hash(data []byte) []byte {
	hash := sha256.Sum256(data)
	return hash[:]
}
