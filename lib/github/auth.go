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
	"strconv"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/netutil"
)

// authenticator provides Authorization header values for GitHub API
// requests. Implementations handle token lifecycle (static for PATs,
// auto-rotating for App installation tokens).
type authenticator interface {
	// AuthorizationHeader returns a valid Authorization header value
	// (e.g., "Bearer ghp_xxx"). For App auth, this may trigger token
	// rotation if the current token is near expiry.
	AuthorizationHeader(ctx context.Context) (string, error)
}

// tokenRotationMargin is how far before expiry we rotate the
// installation token. GitHub tokens have a 1-hour TTL; rotating
// 5 minutes early avoids races where a token expires mid-request.
const tokenRotationMargin = 5 * time.Minute

// --- Token (PAT) authentication ---

// tokenAuth is a static Bearer token authenticator for personal access
// tokens and fine-grained tokens.
type tokenAuth struct {
	header string
}

func newTokenAuth(token string) *tokenAuth {
	return &tokenAuth{header: "Bearer " + token}
}

func (auth *tokenAuth) AuthorizationHeader(_ context.Context) (string, error) {
	return auth.header, nil
}

// --- GitHub App authentication ---

// appAuth authenticates as a GitHub App installation. Generates RS256
// JWTs from the App's private key, exchanges them for short-lived
// installation access tokens, and auto-rotates before expiry.
type appAuth struct {
	appID          int64
	installationID int64
	privateKey     *rsa.PrivateKey
	clock          clock.Clock

	// httpClient and baseURL are used for the token exchange request.
	// These are set by the Client after construction, since the auth
	// and client have a circular dependency (client needs auth for
	// headers, auth needs client's HTTP transport for token exchange).
	httpClient *http.Client
	baseURL    string

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func newAppAuth(appID, installationID int64, privateKeyPEM []byte, clock clock.Clock) (*appAuth, error) {
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, fmt.Errorf("github: failed to decode PEM block from private key")
	}

	privateKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// Try PKCS8 format as fallback — GitHub documents PKCS1 but
		// some key generation tools produce PKCS8.
		keyInterface, pkcs8Err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if pkcs8Err != nil {
			return nil, fmt.Errorf("github: parsing private key: %w (also tried PKCS8: %v)", err, pkcs8Err)
		}
		var ok bool
		privateKey, ok = keyInterface.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("github: private key is not RSA")
		}
	}

	return &appAuth{
		appID:          appID,
		installationID: installationID,
		privateKey:     privateKey,
		clock:          clock,
	}, nil
}

func (auth *appAuth) AuthorizationHeader(ctx context.Context) (string, error) {
	auth.mu.Lock()
	defer auth.mu.Unlock()

	// Return cached token if it's still valid with margin.
	if auth.token != "" && auth.clock.Now().Before(auth.expiresAt.Add(-tokenRotationMargin)) {
		return "Bearer " + auth.token, nil
	}

	// Generate a new JWT and exchange for an installation token.
	token, expiresAt, err := auth.rotate(ctx)
	if err != nil {
		return "", err
	}

	auth.token = token
	auth.expiresAt = expiresAt
	return "Bearer " + token, nil
}

// rotate generates a JWT and exchanges it for a fresh installation token.
// Must be called with auth.mu held.
func (auth *appAuth) rotate(ctx context.Context) (string, time.Time, error) {
	jwt, err := auth.generateJWT()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github: generating JWT: %w", err)
	}

	url := auth.baseURL + "/app/installations/" + strconv.FormatInt(auth.installationID, 10) + "/access_tokens"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github: creating token exchange request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+jwt)
	request.Header.Set("Accept", "application/vnd.github+json")

	response, err := auth.httpClient.Do(request)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github: token exchange request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusCreated {
		body, _ := netutil.ReadResponse(response.Body)
		return "", time.Time{}, fmt.Errorf("github: token exchange returned HTTP %d: %s", response.StatusCode, body)
	}

	var result struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", time.Time{}, fmt.Errorf("github: decoding token exchange response: %w", err)
	}

	if result.Token == "" {
		return "", time.Time{}, fmt.Errorf("github: token exchange returned empty token")
	}

	return result.Token, result.ExpiresAt, nil
}

// generateJWT creates an RS256-signed JWT for GitHub App authentication.
// The JWT has a 10-minute expiry and is used solely to exchange for
// installation tokens. Implementation uses stdlib crypto — no external
// JWT library needed for this constrained use case.
func (auth *appAuth) generateJWT() (string, error) {
	now := auth.clock.Now()

	// Header: always RS256.
	header := base64URLEncode([]byte(`{"alg":"RS256","typ":"JWT"}`))

	// Claims: iss (App ID), iat (issued at, 60s in past for clock skew),
	// exp (10 minutes from now).
	claims := struct {
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
		Issuer    string `json:"iss"`
	}{
		IssuedAt:  now.Add(-60 * time.Second).Unix(),
		ExpiresAt: now.Add(10 * time.Minute).Unix(),
		Issuer:    strconv.FormatInt(auth.appID, 10),
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshaling claims: %w", err)
	}
	payload := base64URLEncode(claimsJSON)

	// Sign: RSASSA-PKCS1-v1_5 with SHA-256.
	signingInput := header + "." + payload
	hash := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, auth.privateKey, crypto.SHA256, hash[:])
	if err != nil {
		return "", fmt.Errorf("signing JWT: %w", err)
	}

	return signingInput + "." + base64URLEncode(signature), nil
}

// base64URLEncode encodes data as base64url without padding, per RFC 7515.
func base64URLEncode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}
