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
}

// ICEConfigFromTURN converts a messaging.TURNCredentialsResponse into an
// ICEConfig suitable for pion/webrtc. When turn is nil (homeserver has no
// TURN configured), returns a config with only host candidates (no STUN,
// no TURN) — sufficient for same-machine and same-LAN testing.
func ICEConfigFromTURN(turn *messaging.TURNCredentialsResponse) ICEConfig {
	if turn == nil || len(turn.URIs) == 0 {
		return ICEConfig{}
	}
	return ICEConfig{
		Servers: []webrtc.ICEServer{
			{
				URLs:       turn.URIs,
				Username:   turn.Username,
				Credential: turn.Password,
			},
		},
	}
}
