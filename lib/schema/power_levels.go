// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import "github.com/bureau-foundation/bureau/lib/ref"

// PowerLevels is a typed representation of the Matrix m.room.power_levels
// state event content. It supports typed read-modify-write operations:
// unmarshal the raw JSON from GetStateEvent, modify with SetUserLevel or
// SetEventLevel, then send the struct back with SendStateEvent.
//
// Pointer-to-int fields distinguish "not set" (nil, omitted from JSON) from
// "explicitly set to 0" (pointer to 0). This preserves server defaults for
// fields the caller doesn't touch.
type PowerLevels struct {
	Users         map[string]int `json:"users,omitempty"`
	UsersDefault  *int           `json:"users_default,omitempty"`
	Events        map[string]int `json:"events,omitempty"`
	EventsDefault *int           `json:"events_default,omitempty"`
	StateDefault  *int           `json:"state_default,omitempty"`
	Invite        *int           `json:"invite,omitempty"`
	Ban           *int           `json:"ban,omitempty"`
	Kick          *int           `json:"kick,omitempty"`
	Redact        *int           `json:"redact,omitempty"`
	Notifications map[string]int `json:"notifications,omitempty"`
}

// SetUserLevel sets the power level for a Matrix user ID. Initializes the
// Users map if nil.
func (powerLevels *PowerLevels) SetUserLevel(userID ref.UserID, level int) {
	if powerLevels.Users == nil {
		powerLevels.Users = make(map[string]int)
	}
	powerLevels.Users[userID.String()] = level
}

// SetEventLevel sets the required power level for sending a given event type.
// Initializes the Events map if nil.
func (powerLevels *PowerLevels) SetEventLevel(eventType ref.EventType, level int) {
	if powerLevels.Events == nil {
		powerLevels.Events = make(map[string]int)
	}
	powerLevels.Events[string(eventType)] = level
}
