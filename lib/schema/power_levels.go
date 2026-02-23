// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
)

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

// UserLevel returns the power level for a Matrix user ID string. If the user
// has an explicit entry in the Users map, that value is returned. Otherwise
// falls back to UsersDefault. If UsersDefault is also nil (not set), returns 0
// per the Matrix spec default.
func (powerLevels *PowerLevels) UserLevel(userID string) int {
	if powerLevels.Users != nil {
		if level, ok := powerLevels.Users[userID]; ok {
			return level
		}
	}
	if powerLevels.UsersDefault != nil {
		return *powerLevels.UsersDefault
	}
	return 0
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

// StateSession is the subset of the Matrix client-server API needed for
// state event read-modify-write operations. Satisfied implicitly by
// messaging.Session, messaging.DirectSession, and proxyclient.ProxySession.
type StateSession interface {
	GetStateEvent(ctx context.Context, roomID ref.RoomID, eventType ref.EventType, stateKey string) (json.RawMessage, error)
	SendStateEvent(ctx context.Context, roomID ref.RoomID, eventType ref.EventType, stateKey string, content any) (ref.EventID, error)
}

// PowerLevelGrants specifies user and event type power level changes to
// apply in a single read-modify-write operation. Either or both maps may
// be non-empty; nil maps are skipped.
type PowerLevelGrants struct {
	Users  map[ref.UserID]int
	Events map[ref.EventType]int
}

// GrantPowerLevels reads the current m.room.power_levels state event from
// a room, applies all user and event type grants, and writes the updated
// event back. One GET + one PUT regardless of how many grants are included.
//
// This is the canonical way to modify power levels in an existing room.
// For setting power levels at room creation time, use PowerLevelContentOverride
// in the CreateRoomRequest instead.
func GrantPowerLevels(ctx context.Context, session StateSession, roomID ref.RoomID, grants PowerLevelGrants) error {
	content, err := session.GetStateEvent(ctx, roomID, MatrixEventTypePowerLevels, "")
	if err != nil {
		return fmt.Errorf("reading power levels for %s: %w", roomID, err)
	}

	var powerLevels PowerLevels
	if err := json.Unmarshal(content, &powerLevels); err != nil {
		return fmt.Errorf("parsing power levels for %s: %w", roomID, err)
	}

	for userID, level := range grants.Users {
		powerLevels.SetUserLevel(userID, level)
	}
	for eventType, level := range grants.Events {
		powerLevels.SetEventLevel(eventType, level)
	}

	if _, err := session.SendStateEvent(ctx, roomID, MatrixEventTypePowerLevels, "", powerLevels); err != nil {
		return fmt.Errorf("writing power levels for %s: %w", roomID, err)
	}

	return nil
}
