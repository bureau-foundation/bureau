// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package servicetoken

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/principal"
)

// signatureSize is the fixed size of an Ed25519 signature.
const signatureSize = ed25519.SignatureSize // 64 bytes

// Grant is the authorization grant embedded in a service token. It
// contains only the fields needed for service-side authorization
// checks — no audit metadata (GrantedBy, GrantedAt).
//
// This type is wire-compatible with schema.Grant's CBOR encoding for
// the overlapping fields. The daemon converts from schema.Grant when
// minting tokens, stripping audit fields.
type Grant struct {
	// Actions is a list of action patterns (glob syntax).
	Actions []string `cbor:"1,keyasint"`

	// Targets is a list of localpart patterns (glob syntax).
	Targets []string `cbor:"2,keyasint,omitempty"`
}

// Token is the CBOR-encoded payload of a service identity token.
type Token struct {
	// Subject is the principal's localpart (e.g., "iree/amdgpu/pm").
	Subject string `cbor:"1,keyasint"`

	// Machine is the machine name where the principal is running.
	Machine string `cbor:"2,keyasint"`

	// Audience is the service role this token is scoped to (e.g.,
	// "ticket", "artifact"). A token for the ticket service cannot
	// be used against the artifact service.
	Audience string `cbor:"3,keyasint"`

	// Grants are the pre-resolved grants relevant to this service.
	// The daemon filters the principal's full grant set to include
	// only actions matching the service's namespace.
	Grants []Grant `cbor:"4,keyasint,omitempty"`

	// ID is a unique token identifier (hex string). Used for
	// emergency revocation via the Blacklist.
	ID string `cbor:"5,keyasint"`

	// IssuedAt is a Unix timestamp (seconds) of when the daemon
	// minted this token.
	IssuedAt int64 `cbor:"6,keyasint"`

	// ExpiresAt is a Unix timestamp (seconds) after which this
	// token is no longer valid.
	ExpiresAt int64 `cbor:"7,keyasint"`
}

// Errors returned by Verify and related functions.
var (
	ErrTokenTooShort    = errors.New("servicetoken: token too short for signature")
	ErrInvalidSignature = errors.New("servicetoken: invalid Ed25519 signature")
	ErrTokenExpired     = errors.New("servicetoken: token has expired")
	ErrAudienceMismatch = errors.New("servicetoken: audience does not match")
	ErrTokenRevoked     = errors.New("servicetoken: token has been revoked")
)

// Mint signs a Token with the daemon's private key and returns the raw
// wire-format bytes: CBOR-encoded payload followed by the 64-byte
// Ed25519 signature.
func Mint(privateKey ed25519.PrivateKey, token *Token) ([]byte, error) {
	payload, err := codec.Marshal(token)
	if err != nil {
		return nil, fmt.Errorf("servicetoken: encoding token payload: %w", err)
	}

	signature := ed25519.Sign(privateKey, payload)

	// Concatenate payload and signature.
	result := make([]byte, len(payload)+signatureSize)
	copy(result, payload)
	copy(result[len(payload):], signature)

	return result, nil
}

// Verify splits the raw token bytes, verifies the Ed25519 signature,
// CBOR-decodes the payload, and checks expiry. Returns the decoded
// Token on success.
//
// The caller should additionally check the Audience field against the
// expected service role and consult the Blacklist for revoked token IDs.
func Verify(publicKey ed25519.PublicKey, tokenBytes []byte) (*Token, error) {
	return VerifyAt(publicKey, tokenBytes, time.Now())
}

// VerifyAt is like Verify but accepts an explicit time for expiry
// checks. This supports deterministic testing.
func VerifyAt(publicKey ed25519.PublicKey, tokenBytes []byte, now time.Time) (*Token, error) {
	if len(tokenBytes) <= signatureSize {
		return nil, ErrTokenTooShort
	}

	splitPoint := len(tokenBytes) - signatureSize
	payload := tokenBytes[:splitPoint]
	signature := tokenBytes[splitPoint:]

	if !ed25519.Verify(publicKey, payload, signature) {
		return nil, ErrInvalidSignature
	}

	var token Token
	if err := codec.Unmarshal(payload, &token); err != nil {
		return nil, fmt.Errorf("servicetoken: decoding token payload: %w", err)
	}

	if now.Unix() >= token.ExpiresAt {
		return nil, ErrTokenExpired
	}

	return &token, nil
}

// VerifyForService combines Verify with an audience check. This is the
// standard verification path for services: verify signature, check
// expiry, and confirm the token is scoped to this service.
func VerifyForService(publicKey ed25519.PublicKey, tokenBytes []byte, expectedAudience string) (*Token, error) {
	return VerifyForServiceAt(publicKey, tokenBytes, expectedAudience, time.Now())
}

// VerifyForServiceAt is like VerifyForService but accepts an explicit time.
func VerifyForServiceAt(publicKey ed25519.PublicKey, tokenBytes []byte, expectedAudience string, now time.Time) (*Token, error) {
	token, err := VerifyAt(publicKey, tokenBytes, now)
	if err != nil {
		return nil, err
	}

	if token.Audience != expectedAudience {
		return nil, fmt.Errorf("%w: got %q, want %q", ErrAudienceMismatch, token.Audience, expectedAudience)
	}

	return token, nil
}

// GrantsAllow checks whether the token's embedded grants authorize a
// specific action on a specific target. Uses the same glob matching
// semantics as the authorization package.
//
// For self-service actions (empty target), only the action patterns
// are checked. For cross-principal actions, both action and target
// patterns must match.
func GrantsAllow(grants []Grant, action, target string) bool {
	for _, grant := range grants {
		if !principal.MatchAnyPattern(grant.Actions, action) {
			continue
		}
		if target == "" {
			return true
		}
		if len(grant.Targets) == 0 {
			continue
		}
		if principal.MatchAnyPattern(grant.Targets, target) {
			return true
		}
	}
	return false
}
