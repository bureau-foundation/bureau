// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref

import "fmt"

// DeviceID is a Matrix device identifier. Device IDs are opaque
// server-assigned strings with no internal structure — unlike UserID
// and RoomID, there is no localpart:server format to validate. The
// type exists to prevent accidental confusion with other string values
// (user IDs, room IDs, access tokens) at compile time.
type DeviceID struct {
	id string
}

// ParseDeviceID constructs a DeviceID from a raw string. Returns an
// error if the string is empty.
func ParseDeviceID(raw string) (DeviceID, error) {
	if raw == "" {
		return DeviceID{}, fmt.Errorf("device ID is empty")
	}
	return DeviceID{id: raw}, nil
}

// String returns the raw device ID string.
func (d DeviceID) String() string {
	return d.id
}

// IsZero reports whether the DeviceID is the zero value (empty).
func (d DeviceID) IsZero() bool {
	return d.id == ""
}

// MarshalText implements encoding.TextMarshaler. Returns an error if
// the DeviceID is zero, since serializing an empty device ID would
// produce ambiguous JSON.
func (d DeviceID) MarshalText() ([]byte, error) {
	if d.id == "" {
		return nil, fmt.Errorf("cannot marshal zero DeviceID")
	}
	return []byte(d.id), nil
}

// UnmarshalText implements encoding.TextUnmarshaler. An empty input
// produces the zero value (matching the omitempty JSON convention for
// optional device IDs).
func (d *DeviceID) UnmarshalText(data []byte) error {
	if len(data) == 0 {
		*d = DeviceID{}
		return nil
	}
	*d = DeviceID{id: string(data)}
	return nil
}
