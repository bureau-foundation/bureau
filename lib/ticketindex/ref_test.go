// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketindex

import "testing"

func TestParseTicketRef(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantLocalpart string
		wantServer    string
		wantTicket    string
		wantBare      bool
	}{
		{
			name:       "bare_ticket_id",
			input:      "tkt-a3f9",
			wantTicket: "tkt-a3f9",
			wantBare:   true,
		},
		{
			name:       "bare_with_long_hash",
			input:      "iree-b2c4f9a1",
			wantTicket: "iree-b2c4f9a1",
			wantBare:   true,
		},
		{
			name:          "same_server_simple_room",
			input:         "tickets/tkt-a3f9",
			wantLocalpart: "tickets",
			wantTicket:    "tkt-a3f9",
			wantBare:      false,
		},
		{
			name:          "same_server_nested_room",
			input:         "iree/general/tkt-a3f9",
			wantLocalpart: "iree/general",
			wantTicket:    "tkt-a3f9",
			wantBare:      false,
		},
		{
			name:          "same_server_deep_path",
			input:         "bureau/project/amdgpu/iree-0f2e",
			wantLocalpart: "bureau/project/amdgpu",
			wantTicket:    "iree-0f2e",
			wantBare:      false,
		},
		{
			name:          "cross_server",
			input:         "iree/general:other.server/tkt-a3f9",
			wantLocalpart: "iree/general",
			wantServer:    "other.server",
			wantTicket:    "tkt-a3f9",
			wantBare:      false,
		},
		{
			name:          "cross_server_simple_room",
			input:         "tickets:remote.host/tkt-1234",
			wantLocalpart: "tickets",
			wantServer:    "remote.host",
			wantTicket:    "tkt-1234",
			wantBare:      false,
		},
		{
			name:          "cross_server_with_port",
			input:         "iree/general:matrix.example.com:8448/tkt-cafe",
			wantLocalpart: "iree/general",
			wantServer:    "matrix.example.com:8448",
			wantTicket:    "tkt-cafe",
			wantBare:      false,
		},
		{
			name:          "cross_server_simple_with_port",
			input:         "tickets:host:9999/tkt-0001",
			wantLocalpart: "tickets",
			wantServer:    "host:9999",
			wantTicket:    "tkt-0001",
			wantBare:      false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ref := ParseTicketRef(test.input)
			if ref.RoomLocalpart != test.wantLocalpart {
				t.Errorf("RoomLocalpart = %q, want %q", ref.RoomLocalpart, test.wantLocalpart)
			}
			if ref.ServerName != test.wantServer {
				t.Errorf("ServerName = %q, want %q", ref.ServerName, test.wantServer)
			}
			if ref.Ticket != test.wantTicket {
				t.Errorf("Ticket = %q, want %q", ref.Ticket, test.wantTicket)
			}
			if ref.IsBare() != test.wantBare {
				t.Errorf("IsBare() = %v, want %v", ref.IsBare(), test.wantBare)
			}
		})
	}
}

func TestFormatTicketRef(t *testing.T) {
	tests := []struct {
		name          string
		roomLocalpart string
		serverName    string
		ticketID      string
		want          string
	}{
		{
			name:     "bare",
			ticketID: "tkt-a3f9",
			want:     "tkt-a3f9",
		},
		{
			name:          "same_server",
			roomLocalpart: "iree/general",
			ticketID:      "tkt-a3f9",
			want:          "iree/general/tkt-a3f9",
		},
		{
			name:          "cross_server",
			roomLocalpart: "iree/general",
			serverName:    "other.server",
			ticketID:      "tkt-a3f9",
			want:          "iree/general:other.server/tkt-a3f9",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := FormatTicketRef(test.roomLocalpart, test.serverName, test.ticketID)
			if got != test.want {
				t.Errorf("FormatTicketRef(%q, %q, %q) = %q, want %q",
					test.roomLocalpart, test.serverName, test.ticketID, got, test.want)
			}
		})
	}
}

func TestParseFormatRoundTrip(t *testing.T) {
	refs := []string{
		"tkt-a3f9",
		"iree/general/tkt-a3f9",
		"iree/general:other.server/tkt-a3f9",
		"iree/general:matrix.example.com:8448/tkt-cafe",
		"bureau/project/amdgpu/iree-0f2e",
	}

	for _, original := range refs {
		ref := ParseTicketRef(original)
		formatted := FormatTicketRef(ref.RoomLocalpart, ref.ServerName, ref.Ticket)
		if formatted != original {
			t.Errorf("round-trip failed: %q → %+v → %q", original, ref, formatted)
		}
	}
}
