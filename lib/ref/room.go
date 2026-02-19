// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref

import (
	"fmt"
	"strings"
)

// RoomID is a validated Matrix room ID (e.g., "!abc123:bureau.local").
//
// Room IDs are server-assigned opaque identifiers returned by room
// resolution and creation operations. They always start with '!' and
// contain a ':' separating the opaque local part from the server name.
// Bureau code never constructs room IDs directly — they come from the
// Matrix homeserver via alias resolution, room creation, or /sync
// responses, and are parsed into this type at the boundary.
//
// RoomID is an immutable value type. The zero value is not valid;
// use IsZero to check.
type RoomID struct {
	id string
}

// ParseRoomID validates and wraps a raw Matrix room ID string.
// Returns an error if the string is empty, doesn't start with '!',
// or is missing the ':server' suffix.
func ParseRoomID(raw string) (RoomID, error) {
	if raw == "" {
		return RoomID{}, fmt.Errorf("empty room ID")
	}
	if raw[0] != '!' {
		return RoomID{}, fmt.Errorf("room ID must start with '!': %q", raw)
	}

	colonIndex := strings.IndexByte(raw[1:], ':')
	if colonIndex < 0 {
		return RoomID{}, fmt.Errorf("room ID missing ':server' suffix: %q", raw)
	}
	if colonIndex == 0 {
		return RoomID{}, fmt.Errorf("room ID has empty local part: %q", raw)
	}

	serverPart := raw[1+colonIndex+1:]
	if serverPart == "" {
		return RoomID{}, fmt.Errorf("room ID has empty server name: %q", raw)
	}

	return RoomID{id: raw}, nil
}

// String returns the full room ID string (e.g., "!abc123:bureau.local").
func (r RoomID) String() string { return r.id }

// IsZero reports whether the RoomID is the zero value (uninitialized).
func (r RoomID) IsZero() bool { return r.id == "" }
