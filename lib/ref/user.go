// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref

import "fmt"

// UserID is a validated Matrix user ID (e.g., "@admin:bureau.local").
//
// A Matrix user ID always starts with '@' and contains a ':'
// separating the localpart from the server name. This type validates
// the structural format but does NOT enforce Bureau-specific localpart
// rules — it accepts any valid Matrix user ID, including non-Bureau
// accounts like "@admin:server" or "@alice:example.com".
//
// For Bureau entities with fleet-scoped naming, use Entity (which
// provides UserID() returning this type). Use ref.UserID directly
// when the code must accept any Matrix user ID, such as Session
// interface methods that work with both Bureau and non-Bureau accounts.
//
// UserID is an immutable value type. The zero value is not valid;
// use IsZero to check.
type UserID struct {
	id string
}

// ParseUserID validates and wraps a raw Matrix user ID string.
// Returns an error if the string is empty, doesn't start with '@',
// has an empty localpart, or is missing the ':server' suffix.
func ParseUserID(raw string) (UserID, error) {
	_, _, err := parseMatrixID(raw)
	if err != nil {
		return UserID{}, err
	}
	return UserID{id: raw}, nil
}

// String returns the full user ID string (e.g., "@admin:bureau.local").
func (u UserID) String() string { return u.id }

// IsZero reports whether the UserID is the zero value (uninitialized).
func (u UserID) IsZero() bool { return u.id == "" }

// Localpart returns the localpart portion of the user ID (without the
// '@' prefix or ':server' suffix). Panics if called on a zero-value
// UserID.
func (u UserID) Localpart() string {
	if u.id == "" {
		panic("UserID.Localpart called on zero value")
	}
	localpart, _, err := parseMatrixID(u.id)
	if err != nil {
		// UserID was validated at construction — this is unreachable.
		panic(fmt.Sprintf("UserID.Localpart: internal error parsing %q: %v", u.id, err))
	}
	return localpart
}

// Server returns the server portion of the user ID (after the ':').
// Panics if called on a zero-value UserID.
func (u UserID) Server() string {
	if u.id == "" {
		panic("UserID.Server called on zero value")
	}
	_, server, err := parseMatrixID(u.id)
	if err != nil {
		// UserID was validated at construction — this is unreachable.
		panic(fmt.Sprintf("UserID.Server: internal error parsing %q: %v", u.id, err))
	}
	return server
}

// MarshalText implements encoding.TextMarshaler for JSON and other
// text-based serialization formats.
func (u UserID) MarshalText() ([]byte, error) {
	if u.id == "" {
		return []byte{}, nil
	}
	return []byte(u.id), nil
}

// UnmarshalText implements encoding.TextUnmarshaler for JSON and other
// text-based serialization formats. Validates the user ID format.
// An empty input produces the zero value (unset user ID).
func (u *UserID) UnmarshalText(data []byte) error {
	if len(data) == 0 {
		*u = UserID{}
		return nil
	}
	parsed, err := ParseUserID(string(data))
	if err != nil {
		return err
	}
	*u = parsed
	return nil
}
