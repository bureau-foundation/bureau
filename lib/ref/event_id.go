// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref

import "fmt"

// EventID is a validated Matrix event ID (e.g., "$abc123xyz").
//
// Event IDs are server-assigned identifiers for timeline events. In
// room version 4+ they are "$base64hash" (no ":server" suffix). In
// older room versions the format is "$something:server". Bureau treats
// event IDs as opaque — the only validation is that they start with '$'
// and contain at least one character after the prefix.
//
// EventID is an immutable value type. The zero value is not valid;
// use IsZero to check.
type EventID struct {
	id string
}

// ParseEventID validates and wraps a raw Matrix event ID string.
// Returns an error if the string is empty, doesn't start with '$',
// or has nothing after the '$' prefix.
func ParseEventID(raw string) (EventID, error) {
	if raw == "" {
		return EventID{}, fmt.Errorf("empty event ID")
	}
	if raw[0] != '$' {
		return EventID{}, fmt.Errorf("event ID must start with '$': %q", raw)
	}
	if len(raw) < 2 {
		return EventID{}, fmt.Errorf("event ID has no content after '$': %q", raw)
	}
	return EventID{id: raw}, nil
}

// MustParseEventID is like ParseEventID but panics on error. Use in
// tests and static initialization where the input is known-valid.
func MustParseEventID(raw string) EventID {
	e, err := ParseEventID(raw)
	if err != nil {
		panic(fmt.Sprintf("ref.MustParseEventID(%q): %v", raw, err))
	}
	return e
}

// String returns the full event ID string (e.g., "$abc123xyz").
func (e EventID) String() string { return e.id }

// IsZero reports whether the EventID is the zero value (uninitialized).
func (e EventID) IsZero() bool { return e.id == "" }

// MarshalText implements encoding.TextMarshaler for JSON and other
// text-based serialization formats.
func (e EventID) MarshalText() ([]byte, error) {
	if e.id == "" {
		return nil, nil
	}
	return []byte(e.id), nil
}

// UnmarshalText implements encoding.TextUnmarshaler for JSON and other
// text-based serialization formats. Validates the event ID format.
// An empty input produces the zero value (unset event ID).
func (e *EventID) UnmarshalText(data []byte) error {
	if len(data) == 0 {
		*e = EventID{}
		return nil
	}
	parsed, err := ParseEventID(string(data))
	if err != nil {
		return err
	}
	*e = parsed
	return nil
}
