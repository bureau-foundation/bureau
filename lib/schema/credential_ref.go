// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// separate types for compile-time safety. The struct shape and parse/format
// patterns are identical by design — the shared logic (parseRef, formatRef,
// refRoomAlias) is already extracted into template_ref.go.
//
//nolint:dupl // CredentialRef, PipelineRef, and TemplateRef are deliberately
package schema

import (
	"github.com/bureau-foundation/bureau/lib/ref"
)

// CredentialRef is a parsed credential reference identifying an
// m.bureau.credentials state event in a specific room. The wire format
// is the same as template and pipeline references:
//
//	<room-alias-localpart>[@<server>]:<state-key>
//
// Examples:
//
//	bureau/fleet/prod:nix-builder
//	iree/fleet/prod@other.example:model-cache
//	${FLEET_ROOM}:nix-builder   (before variable substitution)
//
// The room reference identifies the Matrix room containing the
// credential event. The state key identifies which credential bundle
// within that room. Room membership is the trust boundary: the daemon
// must be a member of the referenced room to read the credential.
type CredentialRef struct {
	// Room is the room alias localpart (e.g., "bureau/fleet/prod").
	Room string

	// StateKey is the credential state key (e.g., "nix-builder").
	StateKey string

	// Server is the optional homeserver for federated references.
	// Empty means use the local homeserver.
	Server string
}

// ParseCredentialRef parses a credential reference string. The format
// is "<room-alias-localpart>[@<server>]:<state-key>". Uses the same
// parsing logic as template and pipeline references.
func ParseCredentialRef(reference string) (CredentialRef, error) {
	room, stateKey, server, err := parseRef(reference, "credential")
	if err != nil {
		return CredentialRef{}, err
	}
	return CredentialRef{
		Room:     room,
		StateKey: stateKey,
		Server:   server,
	}, nil
}

// String returns the canonical wire-format representation.
func (credentialRef CredentialRef) String() string {
	return formatRef(credentialRef.Room, credentialRef.StateKey, credentialRef.Server)
}

// RoomAlias returns the full Matrix room alias for this credential
// reference, using defaultServer when the reference doesn't specify one.
func (credentialRef CredentialRef) RoomAlias(defaultServer ref.ServerName) ref.RoomAlias {
	return refRoomAlias(credentialRef.Room, credentialRef.Server, defaultServer)
}
