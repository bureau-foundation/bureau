// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package messaging

// RegisterRequest holds parameters for registering a new Matrix account.
type RegisterRequest struct {
	Username          string `json:"username"`
	Password          string `json:"password"`
	RegistrationToken string `json:"-"` // used in the auth flow, not sent as a top-level field
}

// AuthResponse is returned by Register and Login.
type AuthResponse struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
	DeviceID    string `json:"device_id"`
}

// CreateRoomRequest holds parameters for creating a Matrix room.
type CreateRoomRequest struct {
	Name        string            `json:"name,omitempty"`
	Topic       string            `json:"topic,omitempty"`
	Alias       string            `json:"room_alias_name,omitempty"` // local alias without # or :server
	RoomVersion string            `json:"room_version,omitempty"`    // e.g. "11"; empty uses server default
	Visibility  string            `json:"visibility,omitempty"`      // "public" or "private"
	Preset      string            `json:"preset,omitempty"`          // "private_chat", "public_chat", "trusted_private_chat"
	Invite      []string          `json:"invite,omitempty"`
	CreationContent        map[string]any `json:"creation_content,omitempty"`          // e.g. {"type": "m.space"} for spaces
	InitialState           []StateEvent   `json:"initial_state,omitempty"`
	PowerLevelContentOverride map[string]any `json:"power_level_content_override,omitempty"` // override default power levels
}

// CreateRoomResponse is returned by CreateRoom.
type CreateRoomResponse struct {
	RoomID string `json:"room_id"`
}

// StateEvent represents a Matrix state event for room creation or state setting.
type StateEvent struct {
	Type     string `json:"type"`
	StateKey string `json:"state_key"`
	Content  any    `json:"content"`
}

// MessageContent is the content body of a Matrix message event (m.room.message).
// Threads are first-class: set RelatesTo to send messages within a thread.
type MessageContent struct {
	MsgType   string     `json:"msgtype"`
	Body      string     `json:"body"`
	RelatesTo *RelatesTo `json:"m.relates_to,omitempty"`
}

// RelatesTo expresses relationships between events.
// For threads, RelType is "m.thread" and EventID is the thread root.
type RelatesTo struct {
	RelType       string     `json:"rel_type"`
	EventID       string     `json:"event_id"`
	IsFallingBack bool       `json:"is_falling_back,omitempty"`
	InReplyTo     *InReplyTo `json:"m.in_reply_to,omitempty"`
}

// InReplyTo references a specific event being replied to within a thread.
type InReplyTo struct {
	EventID string `json:"event_id"`
}

// NewTextMessage creates a plain text message with no thread context.
func NewTextMessage(body string) MessageContent {
	return MessageContent{
		MsgType: "m.text",
		Body:    body,
	}
}

// NewThreadReply creates a message that replies within an existing thread.
// threadRootID is the event ID of the thread's root message.
func NewThreadReply(threadRootID, body string) MessageContent {
	return MessageContent{
		MsgType: "m.text",
		Body:    body,
		RelatesTo: &RelatesTo{
			RelType:       "m.thread",
			EventID:       threadRootID,
			IsFallingBack: true,
			InReplyTo: &InReplyTo{
				EventID: threadRootID,
			},
		},
	}
}

// Event represents a Matrix event from the server.
type Event struct {
	EventID        string         `json:"event_id"`
	Type           string         `json:"type"`
	Sender         string         `json:"sender"`
	OriginServerTS int64          `json:"origin_server_ts"`
	Content        map[string]any `json:"content"`
	RoomID         string         `json:"room_id,omitempty"`
	StateKey       *string        `json:"state_key,omitempty"`
	Unsigned       *EventUnsigned `json:"unsigned,omitempty"`
}

// EventUnsigned holds optional unsigned data attached to events.
type EventUnsigned struct {
	Age             int64  `json:"age,omitempty"`
	TransactionID   string `json:"transaction_id,omitempty"`
}

// RoomMessagesOptions controls pagination for room message fetching.
type RoomMessagesOptions struct {
	From      string // pagination token; empty means "from now"
	Direction string // "b" (backward/older) or "f" (forward/newer)
	Limit     int    // max events to return; 0 uses server default
}

// RoomMessagesResponse is returned by RoomMessages.
type RoomMessagesResponse struct {
	Start string  `json:"start"`
	End   string  `json:"end"`
	Chunk []Event `json:"chunk"`
}

// ThreadMessagesOptions controls pagination for thread message fetching.
type ThreadMessagesOptions struct {
	From  string // pagination token
	Limit int    // max events to return; 0 uses server default
}

// ThreadMessagesResponse is returned by ThreadMessages.
type ThreadMessagesResponse struct {
	Chunk     []Event `json:"chunk"`
	NextBatch string  `json:"next_batch,omitempty"`
}

// SyncOptions controls the behavior of the /sync endpoint.
type SyncOptions struct {
	Since      string // next_batch token from previous sync; empty for initial sync
	Timeout    int    // long-poll timeout in milliseconds; 0 for immediate return
	SetTimeout bool   // if true, send the timeout parameter (needed to distinguish "not set" from "0")
	Filter     string // filter ID or inline JSON filter
}

// SyncResponse is the top-level response from /sync.
type SyncResponse struct {
	NextBatch string        `json:"next_batch"`
	Rooms     RoomsSection  `json:"rooms"`
}

// RoomsSection contains per-room sync data grouped by membership state.
type RoomsSection struct {
	Join   map[string]JoinedRoom `json:"join,omitempty"`
	Invite map[string]InvitedRoom `json:"invite,omitempty"`
	Leave  map[string]LeftRoom    `json:"leave,omitempty"`
}

// JoinedRoom contains sync data for a room the user has joined.
type JoinedRoom struct {
	Timeline TimelineSection `json:"timeline"`
	State    StateSection    `json:"state"`
}

// InvitedRoom contains sync data for a room the user was invited to.
type InvitedRoom struct {
	InviteState StateSection `json:"invite_state"`
}

// LeftRoom contains sync data for a room the user has left.
type LeftRoom struct {
	Timeline TimelineSection `json:"timeline"`
	State    StateSection    `json:"state"`
}

// TimelineSection contains timeline events from a sync response.
type TimelineSection struct {
	Events    []Event `json:"events"`
	PrevBatch string  `json:"prev_batch"`
	Limited   bool    `json:"limited"`
}

// StateSection contains state events from a sync response.
type StateSection struct {
	Events []Event `json:"events"`
}

// InviteRequest holds the user ID to invite to a room.
type InviteRequest struct {
	UserID string `json:"user_id"`
}

// SendEventResponse is returned by SendMessage, SendEvent, and SendStateEvent.
type SendEventResponse struct {
	EventID string `json:"event_id"`
}

// WhoAmIResponse is returned by WhoAmI.
type WhoAmIResponse struct {
	UserID   string `json:"user_id"`
	DeviceID string `json:"device_id,omitempty"`
}

// ResolveAliasResponse is returned by ResolveAlias.
type ResolveAliasResponse struct {
	RoomID  string   `json:"room_id"`
	Servers []string `json:"servers"`
}

// UploadResponse is returned by UploadMedia.
type UploadResponse struct {
	ContentURI string `json:"content_uri"`
}

// LoginRequest is the request body for password login.
type LoginRequest struct {
	Type     string `json:"type"`
	User     string `json:"user"`
	Password string `json:"password"`
	DeviceID string `json:"device_id,omitempty"`
	InitialDeviceDisplayName string `json:"initial_device_display_name,omitempty"`
}
