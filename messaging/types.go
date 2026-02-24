// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package messaging

import (
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/secret"
)

// RegisterRequest holds parameters for registering a new Matrix account.
// Password and RegistrationToken are stored in mmap-backed buffers (locked
// against swap, excluded from core dumps). The caller retains ownership of
// the buffers — Register reads from them but does not close them.
type RegisterRequest struct {
	Username          string
	Password          *secret.Buffer
	RegistrationToken *secret.Buffer
}

// AuthResponse is returned by Register and Login.
type AuthResponse struct {
	UserID      ref.UserID `json:"user_id"`
	AccessToken string     `json:"access_token"`
	DeviceID    string     `json:"device_id"`
}

// CreateRoomRequest holds parameters for creating a Matrix room.
type CreateRoomRequest struct {
	Name                      string         `json:"name,omitempty"`
	Topic                     string         `json:"topic,omitempty"`
	Alias                     string         `json:"room_alias_name,omitempty"` // local alias without # or :server
	RoomVersion               string         `json:"room_version,omitempty"`    // e.g. "11"; empty uses server default
	Visibility                string         `json:"visibility,omitempty"`      // "public" or "private"
	Preset                    string         `json:"preset,omitempty"`          // "private_chat", "public_chat", "trusted_private_chat"
	Invite                    []string       `json:"invite,omitempty"`
	CreationContent           map[string]any `json:"creation_content,omitempty"` // e.g. {"type": "m.space"} for spaces
	InitialState              []StateEvent   `json:"initial_state,omitempty"`
	PowerLevelContentOverride map[string]any `json:"power_level_content_override,omitempty"` // override default power levels
}

// CreateRoomResponse is returned by CreateRoom.
type CreateRoomResponse struct {
	RoomID ref.RoomID `json:"room_id"`
}

// StateEvent represents a Matrix state event for room creation or state setting.
type StateEvent struct {
	Type     string `json:"type"`
	StateKey string `json:"state_key"`
	Content  any    `json:"content"`
}

// MessageContent is the content body of a Matrix message event (m.room.message).
// Threads are first-class: set RelatesTo to send messages within a thread.
//
// Target scopes a message to a specific agent. Machine-originated messages
// (daemon, test harness, services) always set Target so that only the
// intended agent's message pump injects the prompt. Human-originated
// messages use Mentions (or body @-patterns) for chat-model routing:
// agents mentioned receive the message, no mentions means broadcast.
type MessageContent struct {
	MsgType   string     `json:"msgtype"`
	Body      string     `json:"body"`
	Target    string     `json:"target,omitempty"`
	Mentions  *Mentions  `json:"m.mentions,omitempty"`
	RelatesTo *RelatesTo `json:"m.relates_to,omitempty"`
}

// Mentions identifies users referenced in a message. Follows the Matrix
// spec m.mentions format: a list of fully-qualified Matrix user IDs
// (e.g., "@agent/worker:bureau.local") that the message is addressed to.
type Mentions struct {
	UserIDs []string `json:"user_ids,omitempty"`
}

// RelatesTo expresses relationships between events.
// For threads, RelType is "m.thread" and EventID is the thread root.
type RelatesTo struct {
	RelType       string      `json:"rel_type"`
	EventID       ref.EventID `json:"event_id"`
	IsFallingBack bool        `json:"is_falling_back,omitempty"`
	InReplyTo     *InReplyTo  `json:"m.in_reply_to,omitempty"`
}

// InReplyTo references a specific event being replied to within a thread.
type InReplyTo struct {
	EventID ref.EventID `json:"event_id"`
}

// NewTextMessage creates a plain text message with no thread context.
// Use NewTargetedTextMessage for machine-originated messages that should
// be delivered to a specific agent.
func NewTextMessage(body string) MessageContent {
	return MessageContent{
		MsgType: "m.text",
		Body:    body,
	}
}

// NewTargetedTextMessage creates a plain text message scoped to a specific
// agent. The target is the fully-qualified Matrix user ID of the intended
// recipient. The agent's message pump will only inject messages whose
// target matches its own user ID.
func NewTargetedTextMessage(body string, target ref.UserID) MessageContent {
	return MessageContent{
		MsgType: "m.text",
		Body:    body,
		Target:  target.String(),
	}
}

// NewThreadReply creates a message that replies within an existing thread.
// threadRootID is the event ID of the thread's root message.
func NewThreadReply(threadRootID ref.EventID, body string) MessageContent {
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
	EventID        ref.EventID    `json:"event_id"`
	Type           ref.EventType  `json:"type"`
	Sender         ref.UserID     `json:"sender"`
	OriginServerTS int64          `json:"origin_server_ts"`
	Content        map[string]any `json:"content"`
	RoomID         ref.RoomID     `json:"room_id,omitempty"`
	StateKey       *string        `json:"state_key,omitempty"`
	Unsigned       *EventUnsigned `json:"unsigned,omitempty"`
}

// EventUnsigned holds optional unsigned data attached to events.
type EventUnsigned struct {
	Age           int64  `json:"age,omitempty"`
	TransactionID string `json:"transaction_id,omitempty"`
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
	NextBatch string          `json:"next_batch"`
	Presence  PresenceSection `json:"presence,omitempty"`
	Rooms     RoomsSection    `json:"rooms"`
}

// PresenceSection contains presence events from the /sync response.
// Each event reports one user's presence state change. Services that
// don't need presence filter it out via their sync filter
// ("presence": {"types": []}) and receive an empty section.
type PresenceSection struct {
	Events []PresenceEvent `json:"events"`
}

// PresenceEvent is a single m.presence event from the /sync response.
type PresenceEvent struct {
	Type    string               `json:"type"`
	Sender  ref.UserID           `json:"sender"`
	Content PresenceEventContent `json:"content"`
}

// PresenceEventContent carries the presence state for a single user.
type PresenceEventContent struct {
	// Presence is the user's current state: "online", "unavailable",
	// or "offline".
	Presence string `json:"presence"`

	// LastActiveAgo is milliseconds since the user was last active.
	// Zero when unknown or when the user is currently active.
	LastActiveAgo int64 `json:"last_active_ago,omitempty"`

	// CurrentlyActive is true when the user is actively using a
	// client right now (not just connected but idle).
	CurrentlyActive bool `json:"currently_active,omitempty"`

	// StatusMsg is an optional user-set status message.
	StatusMsg string `json:"status_msg,omitempty"`
}

// SetPresenceRequest is the JSON body for
// PUT /_matrix/client/v3/presence/{userId}/status.
type SetPresenceRequest struct {
	// Presence is the desired state: "online", "unavailable", or "offline".
	Presence string `json:"presence"`

	// StatusMsg is an optional human-readable status message.
	StatusMsg string `json:"status_msg,omitempty"`
}

// RoomsSection contains per-room sync data grouped by membership state.
// Map keys are room IDs; encoding/json uses ref.RoomID's TextUnmarshaler
// for automatic validation at deserialization.
type RoomsSection struct {
	Join   map[ref.RoomID]JoinedRoom  `json:"join,omitempty"`
	Invite map[ref.RoomID]InvitedRoom `json:"invite,omitempty"`
	Leave  map[ref.RoomID]LeftRoom    `json:"leave,omitempty"`
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
	UserID ref.UserID `json:"user_id"`
}

// SendEventResponse is returned by SendMessage, SendEvent, and SendStateEvent.
type SendEventResponse struct {
	EventID ref.EventID `json:"event_id"`
}

// WhoAmIResponse is returned by WhoAmI.
type WhoAmIResponse struct {
	UserID   ref.UserID `json:"user_id"`
	DeviceID string     `json:"device_id,omitempty"`
}

// ResolveAliasResponse is returned by ResolveAlias.
type ResolveAliasResponse struct {
	RoomID  ref.RoomID `json:"room_id"`
	Servers []string   `json:"servers"`
}

// UploadResponse is returned by UploadMedia.
type UploadResponse struct {
	ContentURI string `json:"content_uri"`
}

// JoinedRoomsResponse is returned by JoinedRooms.
type JoinedRoomsResponse struct {
	JoinedRooms []ref.RoomID `json:"joined_rooms"`
}

// RoomMember represents a member of a Matrix room.
type RoomMember struct {
	UserID      ref.UserID `json:"user_id"`
	DisplayName string     `json:"display_name"`
	Membership  string     `json:"membership"`
	AvatarURL   string     `json:"avatar_url,omitempty"`
}

// RoomMembersResponse is returned by the /members endpoint.
type RoomMembersResponse struct {
	Chunk []RoomMemberEvent `json:"chunk"`
}

// RoomMemberEvent is a member state event from the /members endpoint.
type RoomMemberEvent struct {
	Type     string            `json:"type"`
	StateKey string            `json:"state_key"`
	Sender   ref.UserID        `json:"sender"`
	Content  RoomMemberContent `json:"content"`
}

// RoomMemberContent is the content of a m.room.member state event.
type RoomMemberContent struct {
	Membership  string `json:"membership"`
	DisplayName string `json:"displayname,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

// KickRequest is the request body for kicking a user from a room.
type KickRequest struct {
	UserID ref.UserID `json:"user_id"`
	Reason string     `json:"reason,omitempty"`
}

// DisplayNameResponse is returned by the /profile/{userId}/displayname endpoint.
type DisplayNameResponse struct {
	DisplayName string `json:"displayname"`
}

// ServerVersionsResponse is returned by Client.ServerVersions.
type ServerVersionsResponse struct {
	Versions         []string        `json:"versions"`
	UnstableFeatures map[string]bool `json:"unstable_features,omitempty"`
}

// TURNCredentialsResponse is returned by the /_matrix/client/v3/voip/turnServer
// endpoint. Contains time-limited HMAC-SHA1 credentials for TURN server access.
// The homeserver generates these from its configured turn_secret (shared with coturn).
type TURNCredentialsResponse struct {
	// Username is the TURN username (typically a Unix timestamp).
	Username string `json:"username"`
	// Password is the HMAC-SHA1 credential derived from the shared secret.
	Password string `json:"password"`
	// URIs lists the TURN server URIs (e.g., ["turn:host:3478?transport=udp"]).
	URIs []string `json:"uris"`
	// TTL is the credential lifetime in seconds.
	TTL int `json:"ttl"`
}

// LoginRequest is the request body for password login.
type LoginRequest struct {
	Type                     string `json:"type"`
	User                     string `json:"user"`
	Password                 string `json:"password"`
	DeviceID                 string `json:"device_id,omitempty"`
	InitialDeviceDisplayName string `json:"initial_device_display_name,omitempty"`
}
