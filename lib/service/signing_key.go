// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// ResolveSystemRoom resolves the namespace's system room alias and joins
// it. Called once at startup by services that need to fetch state from
// the system room (e.g., token signing keys).
func ResolveSystemRoom(ctx context.Context, session *messaging.DirectSession, namespace ref.Namespace) (ref.RoomID, error) {
	return ResolveRoom(ctx, session, namespace.SystemRoomAlias())
}

// LoadTokenSigningKey fetches the daemon's Ed25519 token signing
// public key from the system room. The daemon publishes the key as an
// m.bureau.token_signing_key state event with the machine's fleet-scoped
// localpart (e.g., "bureau/fleet/prod/machine/workstation") as the state key.
//
// Returns the decoded public key suitable for use in AuthConfig. Fails
// if the state event doesn't exist, the public key field is empty, or
// the hex decoding produces a key of the wrong length.
func LoadTokenSigningKey(ctx context.Context, session *messaging.DirectSession, systemRoomID ref.RoomID, machine ref.Machine) (ed25519.PublicKey, error) {
	stateKey := machine.Localpart()
	raw, err := session.GetStateEvent(ctx, systemRoomID.String(), schema.EventTypeTokenSigningKey, stateKey)
	if err != nil {
		return nil, fmt.Errorf("fetching token signing key for %s from %s: %w", stateKey, systemRoomID, err)
	}

	return parseTokenSigningKey(raw, stateKey)
}

// parseTokenSigningKey decodes a TokenSigningKeyContent JSON payload
// into an Ed25519 public key. Validates that the key is present,
// valid hex, and the correct length.
func parseTokenSigningKey(raw json.RawMessage, machineLocalpart string) (ed25519.PublicKey, error) {
	var content schema.TokenSigningKeyContent
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil, fmt.Errorf("parsing token signing key event: %w", err)
	}

	if content.PublicKey == "" {
		return nil, fmt.Errorf("token signing key event for %s has empty public_key field", machineLocalpart)
	}

	keyBytes, err := hex.DecodeString(content.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("hex-decoding token signing key for %s: %w", machineLocalpart, err)
	}

	if len(keyBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("token signing key for %s has wrong length: got %d bytes, want %d", machineLocalpart, len(keyBytes), ed25519.PublicKeySize)
	}

	return ed25519.PublicKey(keyBytes), nil
}
