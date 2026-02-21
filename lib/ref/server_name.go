// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref

import "fmt"

// ServerName is a validated Matrix server name (e.g., "bureau.local",
// "matrix.example.com:8448").
//
// Server names identify Matrix homeservers. They appear after the colon
// in user IDs (@localpart:server) and room aliases (#localpart:server).
// Bureau constructs server names from configuration, CLI flags, and
// Matrix API responses; they are validated at the boundary and passed
// through as typed values.
//
// ServerName is an immutable value type. The zero value is not valid;
// use IsZero to check.
type ServerName struct {
	name string
}

// ParseServerName validates and wraps a raw Matrix server name string.
// Returns an error if the string is empty or contains invalid characters
// (control characters, Matrix sigils).
func ParseServerName(raw string) (ServerName, error) {
	if err := validateServer(raw); err != nil {
		return ServerName{}, err
	}
	return ServerName{name: raw}, nil
}

// MustParseServerName is like ParseServerName but panics on error. Use
// in tests and static initialization where the input is known-valid.
func MustParseServerName(raw string) ServerName {
	s, err := ParseServerName(raw)
	if err != nil {
		panic(fmt.Sprintf("ref.MustParseServerName(%q): %v", raw, err))
	}
	return s
}

// newServerName wraps a server name string that has already been
// validated through parsing or validateServer. Package-private: external
// callers must use ParseServerName.
func newServerName(name string) ServerName {
	return ServerName{name: name}
}

// String returns the server name string (e.g., "bureau.local").
func (s ServerName) String() string { return s.name }

// IsZero reports whether the ServerName is the zero value (uninitialized).
func (s ServerName) IsZero() bool { return s.name == "" }

// MarshalText implements encoding.TextMarshaler for JSON and other
// text-based serialization formats.
func (s ServerName) MarshalText() ([]byte, error) {
	if s.name == "" {
		return []byte{}, nil
	}
	return []byte(s.name), nil
}

// UnmarshalText implements encoding.TextUnmarshaler for JSON and other
// text-based serialization formats. Validates the server name.
// An empty input produces the zero value (unset server name).
func (s *ServerName) UnmarshalText(data []byte) error {
	if len(data) == 0 {
		*s = ServerName{}
		return nil
	}
	parsed, err := ParseServerName(string(data))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}
