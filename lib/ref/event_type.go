// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref

// EventType identifies a Matrix state or timeline event type. Bureau
// defines custom event types (m.bureau.*) and references standard Matrix
// event types (m.room.*, m.space.*). Constants live in lib/schema.
//
// EventType is a named string type, not a struct wrapper: event types
// are opaque identifiers that need no parsing or validation. The type
// exists purely for compile-time safety — preventing accidental use of
// a state key where an event type is expected (or vice versa).
type EventType string

// String returns the event type string (e.g., "m.bureau.template").
func (t EventType) String() string { return string(t) }
