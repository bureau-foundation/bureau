// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"testing"

	"github.com/bureau-foundation/bureau/messaging"
)

func TestICEConfigFromTURN_Nil(t *testing.T) {
	config := ICEConfigFromTURN(nil)
	if len(config.Servers) != 0 {
		t.Errorf("expected no ICE servers for nil TURN, got %d", len(config.Servers))
	}
}

func TestICEConfigFromTURN_EmptyURIs(t *testing.T) {
	config := ICEConfigFromTURN(&messaging.TURNCredentialsResponse{
		Username: "user",
		Password: "pass",
		URIs:     []string{},
		TTL:      86400,
	})
	if len(config.Servers) != 0 {
		t.Errorf("expected no ICE servers for empty URIs, got %d", len(config.Servers))
	}
}

func TestICEConfigFromTURN_WithCredentials(t *testing.T) {
	config := ICEConfigFromTURN(&messaging.TURNCredentialsResponse{
		Username: "1234:user",
		Password: "secret",
		URIs:     []string{"turn:turn.bureau.local:3478?transport=udp", "turn:turn.bureau.local:3478?transport=tcp"},
		TTL:      86400,
	})
	if len(config.Servers) != 1 {
		t.Fatalf("expected 1 ICE server entry, got %d", len(config.Servers))
	}
	server := config.Servers[0]
	if len(server.URLs) != 2 {
		t.Errorf("expected 2 URLs, got %d", len(server.URLs))
	}
	if server.Username != "1234:user" {
		t.Errorf("username = %q, want %q", server.Username, "1234:user")
	}
	if server.Credential != "secret" {
		t.Errorf("credential = %v, want %q", server.Credential, "secret")
	}
}
