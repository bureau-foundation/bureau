// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// ResolveSystemRoom resolves the #bureau/system room alias and joins
// it. Returns the room ID. Called once at startup by services that
// need to fetch state from the system room (e.g., token signing keys).
func ResolveSystemRoom(ctx context.Context, session *messaging.Session, serverName string) (string, error) {
	alias := principal.RoomAlias(schema.RoomAliasSystem, serverName)

	roomID, err := session.ResolveAlias(ctx, alias)
	if err != nil {
		return "", fmt.Errorf("resolving system room alias %q: %w", alias, err)
	}

	if _, err := session.JoinRoom(ctx, roomID); err != nil {
		return "", fmt.Errorf("joining system room %s: %w", roomID, err)
	}

	return roomID, nil
}

// LoadTokenSigningKey fetches the daemon's Ed25519 token signing
// public key from the #bureau/system room. The daemon publishes the
// key as an m.bureau.token_signing_key state event with the machine
// localpart as the state key.
//
// Returns the decoded public key suitable for use in AuthConfig. Fails
// if the state event doesn't exist, the public key field is empty, or
// the hex decoding produces a key of the wrong length.
func LoadTokenSigningKey(ctx context.Context, session *messaging.Session, systemRoomID, machineLocalpart string) (ed25519.PublicKey, error) {
	raw, err := session.GetStateEvent(ctx, systemRoomID, schema.EventTypeTokenSigningKey, machineLocalpart)
	if err != nil {
		return nil, fmt.Errorf("fetching token signing key for %s from %s: %w", machineLocalpart, systemRoomID, err)
	}

	return parseTokenSigningKey(raw, machineLocalpart)
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
