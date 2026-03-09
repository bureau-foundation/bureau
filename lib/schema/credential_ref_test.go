// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

func TestParseCredentialRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		input        string
		wantRoom     string
		wantStateKey string
		wantServer   string
	}{
		{
			name:         "fleet credential",
			input:        "bureau/fleet/prod:nix-builder",
			wantRoom:     "bureau/fleet/prod",
			wantStateKey: "nix-builder",
		},
		{
			name:         "federated credential",
			input:        "iree/fleet/prod@other.example:model-cache",
			wantRoom:     "iree/fleet/prod",
			wantStateKey: "model-cache",
			wantServer:   "other.example",
		},
		{
			name:         "federated with port",
			input:        "bureau/fleet/prod@other.example:8448:deploy-key",
			wantRoom:     "bureau/fleet/prod",
			wantStateKey: "deploy-key",
			wantServer:   "other.example:8448",
		},
		{
			name:         "single-segment room",
			input:        "fleet:backup-key",
			wantRoom:     "fleet",
			wantStateKey: "backup-key",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			credentialRef, err := ParseCredentialRef(test.input)
			if err != nil {
				t.Fatalf("ParseCredentialRef(%q) error: %v", test.input, err)
			}
			if credentialRef.Room != test.wantRoom {
				t.Errorf("Room = %q, want %q", credentialRef.Room, test.wantRoom)
			}
			if credentialRef.StateKey != test.wantStateKey {
				t.Errorf("StateKey = %q, want %q", credentialRef.StateKey, test.wantStateKey)
			}
			if credentialRef.Server != test.wantServer {
				t.Errorf("Server = %q, want %q", credentialRef.Server, test.wantServer)
			}
		})
	}
}

func TestParseCredentialRefErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{"empty string", ""},
		{"no colon", "bureau/fleet/prod"},
		{"empty state key", "bureau/fleet/prod:"},
		{"empty room", ":nix-builder"},
		{"empty room with server", "@server:nix-builder"},
		{"empty server after at", "bureau/fleet/prod@:nix-builder"},

		// Path traversal in state key (via ValidatePathSegment).
		{"dotdot state key", "bureau/fleet/prod:.."},
		{"dot state key", "bureau/fleet/prod:."},
		{"hidden state key", "bureau/fleet/prod:.secret"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseCredentialRef(test.input)
			if err == nil {
				t.Errorf("ParseCredentialRef(%q) should have returned an error", test.input)
			}
		})
	}
}

func TestCredentialRefString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  CredentialRef
		want string
	}{
		{
			name: "local",
			ref:  CredentialRef{Room: "bureau/fleet/prod", StateKey: "nix-builder"},
			want: "bureau/fleet/prod:nix-builder",
		},
		{
			name: "federated",
			ref:  CredentialRef{Room: "iree/fleet/prod", StateKey: "deploy-key", Server: "other.example"},
			want: "iree/fleet/prod@other.example:deploy-key",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := test.ref.String()
			if got != test.want {
				t.Errorf("String() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCredentialRefRoundTrip(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"bureau/fleet/prod:nix-builder",
		"iree/fleet/prod@other.example:model-cache",
		"bureau/fleet/prod@other.example:8448:deploy-key",
	}

	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			credentialRef, err := ParseCredentialRef(input)
			if err != nil {
				t.Fatalf("ParseCredentialRef(%q) error: %v", input, err)
			}
			roundTripped := credentialRef.String()
			if roundTripped != input {
				t.Errorf("round-trip failed: %q -> %q", input, roundTripped)
			}
		})
	}
}

func TestCredentialRefRoomAlias(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		ref           CredentialRef
		defaultServer string
		want          string
	}{
		{
			name:          "local uses default server",
			ref:           CredentialRef{Room: "bureau/fleet/prod", StateKey: "nix-builder"},
			defaultServer: "bureau.local",
			want:          "#bureau/fleet/prod:bureau.local",
		},
		{
			name:          "federated uses explicit server",
			ref:           CredentialRef{Room: "iree/fleet/prod", StateKey: "key", Server: "other.example"},
			defaultServer: "bureau.local",
			want:          "#iree/fleet/prod:other.example",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := test.ref.RoomAlias(ref.MustParseServerName(test.defaultServer)).String()
			if got != test.want {
				t.Errorf("RoomAlias(%q) = %q, want %q", test.defaultServer, got, test.want)
			}
		})
	}
}
