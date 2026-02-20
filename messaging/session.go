// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package messaging

import (
	"context"
	"encoding/json"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// Session is the interface for Matrix operations that sandboxed and
// CLI code can perform. Two implementations exist:
//
//   - *DirectSession: direct homeserver connection using an access token. Used by
//     the daemon, operator CLI commands, and service binaries.
//   - proxyclient.ProxySession: delegates to the proxy's /v1/matrix/*
//     endpoints. Used by sandboxed agents and pipeline executors.
//
// Operator-only methods (AccessToken, DeviceID, ChangePassword,
// DeactivateUser, ResetUserPassword, LogoutAll, UploadMedia,
// TURNCredentials, KickUser, LeaveRoom) are not part of this interface.
// Code that needs them should type-assert to *DirectSession.
type Session interface {
	// UserID returns the fully-qualified Matrix user ID
	// (e.g., "@agent/test:bureau.local").
	UserID() string

	// Close releases any resources held by the session. Idempotent.
	Close() error

	// WhoAmI validates the session and returns the user ID.
	WhoAmI(ctx context.Context) (string, error)

	// ResolveAlias resolves a room alias to a room ID.
	ResolveAlias(ctx context.Context, alias string) (ref.RoomID, error)

	// GetStateEvent fetches a specific state event's content from a room.
	// Returns the raw JSON content for the caller to unmarshal.
	GetStateEvent(ctx context.Context, roomID ref.RoomID, eventType, stateKey string) (json.RawMessage, error)

	// GetRoomState fetches all current state events from a room.
	GetRoomState(ctx context.Context, roomID ref.RoomID) ([]Event, error)

	// SendStateEvent sends a state event to a room. Returns the event ID.
	SendStateEvent(ctx context.Context, roomID ref.RoomID, eventType, stateKey string, content any) (string, error)

	// SendEvent sends an event of any type to a room. Returns the event ID.
	SendEvent(ctx context.Context, roomID ref.RoomID, eventType string, content any) (string, error)

	// SendMessage sends a message to a room. Returns the event ID.
	SendMessage(ctx context.Context, roomID ref.RoomID, content MessageContent) (string, error)

	// CreateRoom creates a new Matrix room.
	CreateRoom(ctx context.Context, request CreateRoomRequest) (*CreateRoomResponse, error)

	// InviteUser invites a user to a room.
	InviteUser(ctx context.Context, roomID ref.RoomID, userID string) error

	// JoinRoom joins a room by room ID. Returns the room ID. To join
	// by alias, resolve with ResolveAlias first.
	JoinRoom(ctx context.Context, roomID ref.RoomID) (ref.RoomID, error)

	// JoinedRooms returns the list of room IDs the user has joined.
	JoinedRooms(ctx context.Context) ([]ref.RoomID, error)

	// GetRoomMembers returns the members of a room.
	GetRoomMembers(ctx context.Context, roomID ref.RoomID) ([]RoomMember, error)

	// GetDisplayName fetches a user's display name.
	GetDisplayName(ctx context.Context, userID string) (string, error)

	// RoomMessages fetches paginated messages from a room.
	RoomMessages(ctx context.Context, roomID ref.RoomID, options RoomMessagesOptions) (*RoomMessagesResponse, error)

	// ThreadMessages fetches messages in a thread.
	ThreadMessages(ctx context.Context, roomID ref.RoomID, threadRootID string, options ThreadMessagesOptions) (*ThreadMessagesResponse, error)

	// Sync performs an incremental sync with the homeserver.
	Sync(ctx context.Context, options SyncOptions) (*SyncResponse, error)
}

// Compile-time check: *DirectSession implements Session.
var _ Session = (*DirectSession)(nil)
