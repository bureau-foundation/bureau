// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package servicetoken

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/codec"
)

// RevocationEntry identifies a single token to revoke and the time at
// which the blacklist entry can be cleaned up (the token's natural TTL
// expiry). Services use ExpiresAt for auto-cleanup — once the token
// would have expired anyway, keeping it blacklisted is unnecessary.
type RevocationEntry struct {
	// TokenID is the unique identifier of the token to revoke.
	TokenID string `cbor:"1,keyasint"`

	// ExpiresAt is the token's natural expiry (Unix seconds). The
	// service removes the blacklist entry after this time.
	ExpiresAt int64 `cbor:"2,keyasint"`
}

// RevocationRequest is the payload of a signed revocation message from
// the daemon to a service. The daemon signs the CBOR-encoded request
// with its Ed25519 private key; the service verifies using the same
// public key it uses for token verification.
type RevocationRequest struct {
	// Entries lists the tokens to revoke.
	Entries []RevocationEntry `cbor:"1,keyasint"`

	// IssuedAt is a Unix timestamp (seconds) of when the daemon
	// created this revocation request.
	IssuedAt int64 `cbor:"2,keyasint"`
}

// Errors returned by VerifyRevocation.
var (
	ErrRevocationTooShort  = errors.New("servicetoken: revocation data too short for signature")
	ErrRevocationBadSig    = errors.New("servicetoken: invalid revocation signature")
	ErrRevocationNoEntries = errors.New("servicetoken: revocation request has no entries")
)

// SignRevocation signs a revocation request with the daemon's private
// key. The wire format mirrors token signing: CBOR payload followed by
// a 64-byte Ed25519 signature.
func SignRevocation(privateKey ed25519.PrivateKey, request *RevocationRequest) ([]byte, error) {
	payload, err := codec.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("servicetoken: encoding revocation request: %w", err)
	}

	signature := ed25519.Sign(privateKey, payload)

	result := make([]byte, len(payload)+signatureSize)
	copy(result, payload)
	copy(result[len(payload):], signature)

	return result, nil
}

// VerifyRevocation verifies the Ed25519 signature on a signed
// revocation request and decodes the payload. Returns an error if the
// signature is invalid, the data is too short, or the payload contains
// no entries.
func VerifyRevocation(publicKey ed25519.PublicKey, data []byte) (*RevocationRequest, error) {
	if len(data) <= signatureSize {
		return nil, ErrRevocationTooShort
	}

	splitPoint := len(data) - signatureSize
	payload := data[:splitPoint]
	signature := data[splitPoint:]

	if !ed25519.Verify(publicKey, payload, signature) {
		return nil, ErrRevocationBadSig
	}

	var request RevocationRequest
	if err := codec.Unmarshal(payload, &request); err != nil {
		return nil, fmt.Errorf("servicetoken: decoding revocation request: %w", err)
	}

	if len(request.Entries) == 0 {
		return nil, ErrRevocationNoEntries
	}

	return &request, nil
}
