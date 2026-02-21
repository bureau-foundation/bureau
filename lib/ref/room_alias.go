// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref

import "fmt"

// RoomAlias is a validated Matrix room alias (e.g., "#bureau/system:bureau.local").
//
// Room aliases are human-readable names that resolve to opaque RoomIDs.
// They always start with '#' and contain a ':' separating the localpart
// from the server name. Bureau constructs room aliases from validated
// Namespace, Fleet, and entity references; external aliases arrive from
// Matrix API responses and are parsed at the boundary.
//
// RoomAlias is an immutable value type. The zero value is not valid;
// use IsZero to check.
type RoomAlias struct {
	alias string
}

// ParseRoomAlias validates and wraps a raw Matrix room alias string.
// Returns an error if the string is empty, doesn't start with '#',
// or is missing the ':server' suffix.
func ParseRoomAlias(raw string) (RoomAlias, error) {
	_, _, err := parseRoomAlias(raw)
	if err != nil {
		return RoomAlias{}, err
	}
	return RoomAlias{alias: raw}, nil
}

// MustParseRoomAlias is like ParseRoomAlias but panics on error. Use in
// tests and static initialization where the input is known-valid.
func MustParseRoomAlias(raw string) RoomAlias {
	a, err := ParseRoomAlias(raw)
	if err != nil {
		panic(fmt.Sprintf("ref.MustParseRoomAlias(%q): %v", raw, err))
	}
	return a
}

// newRoomAlias constructs a RoomAlias from a known-valid localpart and server.
// Used internally by Namespace, Fleet, and entity constructors where the
// components have already been validated.
func newRoomAlias(localpart, server string) RoomAlias {
	return RoomAlias{alias: "#" + localpart + ":" + server}
}

// String returns the full room alias string (e.g., "#bureau/system:bureau.local").
func (a RoomAlias) String() string { return a.alias }

// IsZero reports whether the RoomAlias is the zero value (uninitialized).
func (a RoomAlias) IsZero() bool { return a.alias == "" }

// Localpart returns the alias localpart without the '#' prefix or ':server' suffix.
func (a RoomAlias) Localpart() string {
	if a.alias == "" {
		return ""
	}
	// Safe: validated at construction to contain '#' prefix and ':server'.
	localpart, _, _ := parseRoomAlias(a.alias)
	return localpart
}

// Server returns the server name from the alias.
func (a RoomAlias) Server() string {
	if a.alias == "" {
		return ""
	}
	_, server, _ := parseRoomAlias(a.alias)
	return server
}

// MarshalText implements encoding.TextMarshaler for JSON and other
// text-based serialization formats.
func (a RoomAlias) MarshalText() ([]byte, error) {
	if a.alias == "" {
		return []byte{}, nil
	}
	return []byte(a.alias), nil
}

// UnmarshalText implements encoding.TextUnmarshaler for JSON and other
// text-based serialization formats. Validates the room alias format.
// An empty input produces the zero value (unset room alias).
func (a *RoomAlias) UnmarshalText(data []byte) error {
	if len(data) == 0 {
		*a = RoomAlias{}
		return nil
	}
	parsed, err := ParseRoomAlias(string(data))
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}
