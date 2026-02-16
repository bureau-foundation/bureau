// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package servicetoken

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/codec"
)

// UpstreamConfig is the payload of a signed upstream configuration
// message from the daemon to a service. The daemon signs the
// CBOR-encoded config with its Ed25519 private key; the service
// verifies using the same public key it uses for token verification.
//
// An empty UpstreamSocket means "no upstream" — the service should
// clear any previously configured upstream and serve only from its
// local store.
type UpstreamConfig struct {
	// UpstreamSocket is the Unix socket path for the upstream
	// (shared cache) service. Empty means no upstream.
	UpstreamSocket string `cbor:"1,keyasint"`

	// IssuedAt is a Unix timestamp (seconds) of when the daemon
	// created this configuration.
	IssuedAt int64 `cbor:"2,keyasint"`

	// ServiceToken is a daemon-minted service token that the
	// artifact service includes when connecting to the upstream.
	// For local upstream connections (same machine), this token is
	// signed by the local daemon and verifiable by the upstream
	// service. For remote upstream connections (via tunnel), this
	// is nil — the tunnel handler on the remote machine injects a
	// token signed by that machine's daemon.
	ServiceToken []byte `cbor:"3,keyasint,omitempty"`
}

// Errors returned by VerifyUpstreamConfig.
var (
	ErrUpstreamConfigTooShort = errors.New("servicetoken: upstream config data too short for signature")
	ErrUpstreamConfigBadSig   = errors.New("servicetoken: invalid upstream config signature")
)

// SignUpstreamConfig signs an upstream configuration with the daemon's
// private key. The wire format mirrors token signing: CBOR payload
// followed by a 64-byte Ed25519 signature.
func SignUpstreamConfig(privateKey ed25519.PrivateKey, config *UpstreamConfig) ([]byte, error) {
	payload, err := codec.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("servicetoken: encoding upstream config: %w", err)
	}

	signature := ed25519.Sign(privateKey, payload)

	result := make([]byte, len(payload)+signatureSize)
	copy(result, payload)
	copy(result[len(payload):], signature)

	return result, nil
}

// VerifyUpstreamConfig verifies the Ed25519 signature on a signed
// upstream configuration and decodes the payload. Returns an error if
// the signature is invalid or the data is too short.
func VerifyUpstreamConfig(publicKey ed25519.PublicKey, data []byte) (*UpstreamConfig, error) {
	if len(data) <= signatureSize {
		return nil, ErrUpstreamConfigTooShort
	}

	splitPoint := len(data) - signatureSize
	payload := data[:splitPoint]
	signature := data[splitPoint:]

	if !ed25519.Verify(publicKey, payload, signature) {
		return nil, ErrUpstreamConfigBadSig
	}

	var config UpstreamConfig
	if err := codec.Unmarshal(payload, &config); err != nil {
		return nil, fmt.Errorf("servicetoken: decoding upstream config: %w", err)
	}

	return &config, nil
}
