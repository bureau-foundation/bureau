// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package servicetoken

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/codec"
)

// PushTargetsConfig is the payload of a signed push targets
// configuration message from the daemon to a service. The daemon
// signs the CBOR-encoded config with its Ed25519 private key; the
// service verifies using the same public key it uses for token
// verification.
//
// Each entry in Targets maps a machine localpart (e.g.,
// "machine/gpu-server-1") to a PushTarget describing how to reach
// that machine's artifact service. The artifact service uses this
// directory when processing store requests with PushTargets or the
// "replicate" cache policy.
type PushTargetsConfig struct {
	// Targets maps machine localparts to push targets. An empty
	// map means "no push targets available" — store requests with
	// PushTargets will report errors for all requested targets.
	Targets map[string]PushTarget `cbor:"1,keyasint"`

	// IssuedAt is a Unix timestamp (seconds) of when the daemon
	// created this configuration.
	IssuedAt int64 `cbor:"2,keyasint"`
}

// PushTarget describes how to reach a specific machine's artifact
// service for push-through replication.
type PushTarget struct {
	// SocketPath is the Unix socket path for the target machine's
	// artifact service. For local services (same machine), this is
	// the service's direct socket. For remote services, this is a
	// daemon-managed tunnel socket that bridges to the remote
	// service via the transport layer.
	SocketPath string `cbor:"1,keyasint"`

	// Token is a daemon-minted service token for authenticating
	// store requests on the target. For local services, this is
	// signed by the local daemon. For remote services (tunnel
	// connections), this is nil — the tunnel handler on the remote
	// machine injects a token signed by that machine's daemon.
	Token []byte `cbor:"2,keyasint,omitempty"`
}

// Errors returned by VerifyPushTargetsConfig.
var (
	ErrPushTargetsConfigTooShort = errors.New("servicetoken: push targets config data too short for signature")
	ErrPushTargetsConfigBadSig   = errors.New("servicetoken: invalid push targets config signature")
)

// SignPushTargetsConfig signs a push targets configuration with the
// daemon's private key. The wire format mirrors token signing: CBOR
// payload followed by a 64-byte Ed25519 signature.
func SignPushTargetsConfig(privateKey ed25519.PrivateKey, config *PushTargetsConfig) ([]byte, error) {
	payload, err := codec.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("servicetoken: encoding push targets config: %w", err)
	}

	signature := ed25519.Sign(privateKey, payload)

	result := make([]byte, len(payload)+signatureSize)
	copy(result, payload)
	copy(result[len(payload):], signature)

	return result, nil
}

// VerifyPushTargetsConfig verifies the Ed25519 signature on a signed
// push targets configuration and decodes the payload. Returns an
// error if the signature is invalid or the data is too short.
func VerifyPushTargetsConfig(publicKey ed25519.PublicKey, data []byte) (*PushTargetsConfig, error) {
	if len(data) <= signatureSize {
		return nil, ErrPushTargetsConfigTooShort
	}

	splitPoint := len(data) - signatureSize
	payload := data[:splitPoint]
	signature := data[splitPoint:]

	if !ed25519.Verify(publicKey, payload, signature) {
		return nil, ErrPushTargetsConfigBadSig
	}

	var config PushTargetsConfig
	if err := codec.Unmarshal(payload, &config); err != nil {
		return nil, fmt.Errorf("servicetoken: decoding push targets config: %w", err)
	}

	return &config, nil
}
