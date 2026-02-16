// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import "strings"

// TicketRef is a parsed ticket reference. A ticket reference is either
// a bare ticket ID (room context must be provided separately) or a
// room-qualified reference like "iree/general/tkt-a3f9" (same-server)
// or "iree/general:other.server/tkt-a3f9" (cross-server).
//
// The format mirrors GitHub's issue reference convention:
//   - #123           → bare (this repo)
//   - org/repo#123   → qualified (another repo)
//
// For Bureau:
//   - tkt-a3f9                           → bare (room from context)
//   - iree/general/tkt-a3f9             → same-server room alias localpart
//   - iree/general:other.server/tkt-a3f9 → cross-server full alias
//
// Parsing is unambiguous because ticket IDs never contain "/".
type TicketRef struct {
	// RoomLocalpart is the room alias localpart (e.g., "iree/general").
	// Empty for bare references.
	RoomLocalpart string

	// ServerName is the Matrix server name (e.g., "other.server").
	// Empty for same-server references and bare references.
	ServerName string

	// Ticket is the bare ticket ID (e.g., "tkt-a3f9").
	Ticket string
}

// ParseTicketRef parses a ticket reference string into its components.
// Returns the room alias localpart (if any), the server name (if
// cross-server), and the bare ticket ID.
//
// Examples:
//
//	"tkt-a3f9"                            → ("", "", "tkt-a3f9")
//	"iree/general/tkt-a3f9"              → ("iree/general", "", "tkt-a3f9")
//	"iree/general:other.server/tkt-a3f9" → ("iree/general", "other.server", "tkt-a3f9")
func ParseTicketRef(ref string) TicketRef {
	lastSlash := strings.LastIndex(ref, "/")
	if lastSlash < 0 {
		// Bare reference: no room qualifier.
		return TicketRef{Ticket: ref}
	}

	ticketID := ref[lastSlash+1:]
	room := ref[:lastSlash]

	// Use the first ":" — room alias localparts never contain ":"
	// but server names can (e.g., "matrix.example.com:8448").
	if colon := strings.Index(room, ":"); colon >= 0 {
		// Cross-server: "localpart:server"
		return TicketRef{
			RoomLocalpart: room[:colon],
			ServerName:    room[colon+1:],
			Ticket:        ticketID,
		}
	}

	// Same-server: "localpart" only.
	return TicketRef{
		RoomLocalpart: room,
		Ticket:        ticketID,
	}
}

// IsBare returns true if this reference has no room qualifier and
// requires external room context.
func (r TicketRef) IsBare() bool {
	return r.RoomLocalpart == ""
}

// FormatTicketRef formats a room-qualified ticket reference. If
// serverName is empty, produces a same-server reference. If
// roomLocalpart is also empty, returns the bare ticket ID.
func FormatTicketRef(roomLocalpart, serverName, ticketID string) string {
	if roomLocalpart == "" {
		return ticketID
	}
	if serverName != "" {
		return roomLocalpart + ":" + serverName + "/" + ticketID
	}
	return roomLocalpart + "/" + ticketID
}
