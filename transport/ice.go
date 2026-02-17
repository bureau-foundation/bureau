// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"github.com/pion/webrtc/v4"

	"github.com/bureau-foundation/bureau/messaging"
)

// ICEConfig holds ICE server configuration for WebRTC PeerConnections.
// The daemon refreshes this periodically from the Matrix TURN credential
// endpoint to keep HMAC credentials valid.
type ICEConfig struct {
	// Servers is the list of ICE servers (STUN + TURN) to use during
	// candidate gathering. Order matters: pion tries them in sequence.
	Servers []webrtc.ICEServer

	// InterfaceFilter restricts which network interfaces are used for
	// ICE candidate gathering. Only interfaces for which the function
	// returns true are considered. This field is required — callers
	// must choose explicitly.
	//
	// Without filtering, pion generates host candidates for every
	// network interface and tests them all during connectivity checks.
	// On a machine with Docker bridges, veth pairs, etc. (52+
	// interfaces is common), this turns into O(interfaces × 200ms)
	// of failed connectivity checks before the correct candidate pair
	// succeeds.
	//
	// Use LoopbackInterfaceFilter for tests. Use
	// ExcludeVirtualInterfaceFilter for production daemons.
	InterfaceFilter func(interfaceName string) (keep bool)
}

// LoopbackInterfaceFilter restricts ICE candidate gathering to the
// loopback interface only. Use this in tests where both peers run
// in-process and only loopback candidates can succeed.
func LoopbackInterfaceFilter(interfaceName string) bool {
	return interfaceName == "lo"
}

// ExcludeVirtualInterfaceFilter keeps all interfaces except Docker
// bridges (br-*) and container veth pairs (veth*). These interfaces
// cannot route between independent machines and would only produce
// candidate pairs that fail during connectivity checks.
func ExcludeVirtualInterfaceFilter(interfaceName string) bool {
	if len(interfaceName) >= 4 && interfaceName[:4] == "veth" {
		return false
	}
	if len(interfaceName) >= 3 && interfaceName[:3] == "br-" {
		return false
	}
	return true
}

// ICEConfigFromTURN converts a messaging.TURNCredentialsResponse into an
// ICEConfig suitable for pion/webrtc. When turn is nil (homeserver has no
// TURN configured), returns a config with only host candidates (no STUN,
// no TURN) — sufficient for same-machine and same-LAN testing.
//
// interfaceFilter is required and controls which network interfaces are
// used for ICE candidate gathering.
func ICEConfigFromTURN(turn *messaging.TURNCredentialsResponse, interfaceFilter func(string) bool) ICEConfig {
	if turn == nil || len(turn.URIs) == 0 {
		return ICEConfig{InterfaceFilter: interfaceFilter}
	}
	return ICEConfig{
		Servers: []webrtc.ICEServer{
			{
				URLs:       turn.URIs,
				Username:   turn.Username,
				Credential: turn.Password,
			},
		},
		InterfaceFilter: interfaceFilter,
	}
}
