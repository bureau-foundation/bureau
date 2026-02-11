// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/bureau-foundation/bureau/lib/secret"
)

// Session is an authenticated Matrix session.
// It wraps a Client with an access token for making authenticated API calls.
// Sessions are lightweight and safe to create in large numbers.
//
// The access token is stored in a secret.Buffer (mmap-backed, locked against
// swap, excluded from core dumps). The caller must call Close when the Session
// is no longer needed.
type Session struct {
	client      *Client
	accessToken *secret.Buffer
	userID      string
	deviceID    string

	// transactionCounter generates unique transaction IDs for idempotent sends.
	transactionCounter atomic.Int64
}

// UserID returns the fully-qualified Matrix user ID (e.g., "@alice:bureau.local").
func (s *Session) UserID() string {
	return s.userID
}

// AccessToken returns the access token as a heap string. This creates a brief
// copy from the mmap-backed buffer — use only at API boundaries that require
// a string (e.g., writing to JSON, logging). Prefer passing the Session
// itself when possible.
func (s *Session) AccessToken() string {
	return s.accessToken.String()
}

// DeviceID returns the device ID for this session.
func (s *Session) DeviceID() string {
	return s.deviceID
}

// Close releases the access token memory (zeros, unlocks, unmaps).
// Idempotent — safe to call multiple times.
func (s *Session) Close() error {
	if s.accessToken != nil {
		return s.accessToken.Close()
	}
	return nil
}

// WhoAmI validates the access token and returns the user ID.
// Useful for checking whether a stored token is still valid.
func (s *Session) WhoAmI(ctx context.Context) (string, error) {
	body, err := s.client.doRequest(ctx, http.MethodGet, "/_matrix/client/v3/account/whoami", s.accessToken, nil)
	if err != nil {
		return "", fmt.Errorf("messaging: whoami failed: %w", err)
	}

	var response WhoAmIResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("messaging: failed to parse whoami response: %w", err)
	}
	return response.UserID, nil
}

// CreateRoom creates a new Matrix room.
func (s *Session) CreateRoom(ctx context.Context, request CreateRoomRequest) (*CreateRoomResponse, error) {
	body, err := s.client.doRequest(ctx, http.MethodPost, "/_matrix/client/v3/createRoom", s.accessToken, request)
	if err != nil {
		return nil, fmt.Errorf("messaging: create room failed: %w", err)
	}

	var response CreateRoomResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("messaging: failed to parse createRoom response: %w", err)
	}

	s.client.logger.Info("created matrix room",
		"room_id", response.RoomID,
		"alias", request.Alias,
		"name", request.Name,
	)
	return &response, nil
}

// JoinRoom joins a room by ID or alias. Returns the room ID.
func (s *Session) JoinRoom(ctx context.Context, roomIDOrAlias string) (string, error) {
	path := "/_matrix/client/v3/join/" + url.PathEscape(roomIDOrAlias)
	body, err := s.client.doRequest(ctx, http.MethodPost, path, s.accessToken, struct{}{})
	if err != nil {
		return "", fmt.Errorf("messaging: join room %q failed: %w", roomIDOrAlias, err)
	}

	var response struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("messaging: failed to parse join response: %w", err)
	}
	return response.RoomID, nil
}

// InviteUser invites a user to a room.
func (s *Session) InviteUser(ctx context.Context, roomID, userID string) error {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/invite", url.PathEscape(roomID))
	_, err := s.client.doRequest(ctx, http.MethodPost, path, s.accessToken, InviteRequest{UserID: userID})
	if err != nil {
		return fmt.Errorf("messaging: invite %q to %q failed: %w", userID, roomID, err)
	}
	return nil
}

// SendMessage sends a message to a room. The content includes thread context
// if this is a thread reply (see NewTextMessage and NewThreadReply).
// Returns the event ID of the sent message.
func (s *Session) SendMessage(ctx context.Context, roomID string, content MessageContent) (string, error) {
	return s.SendEvent(ctx, roomID, "m.room.message", content)
}

// SendEvent sends an event of any type to a room.
// Uses Matrix's idempotent PUT with a transaction ID.
// Returns the event ID.
func (s *Session) SendEvent(ctx context.Context, roomID, eventType string, content any) (string, error) {
	transactionID := s.nextTransactionID()
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/%s/%s",
		url.PathEscape(roomID),
		url.PathEscape(eventType),
		url.PathEscape(transactionID),
	)

	body, err := s.client.doRequest(ctx, http.MethodPut, path, s.accessToken, content)
	if err != nil {
		return "", fmt.Errorf("messaging: send event to %q failed: %w", roomID, err)
	}

	var response SendEventResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("messaging: failed to parse send response: %w", err)
	}
	return response.EventID, nil
}

// SendStateEvent sends a state event to a room.
// State events use PUT with the event type and state key in the path.
// Returns the event ID.
func (s *Session) SendStateEvent(ctx context.Context, roomID, eventType, stateKey string, content any) (string, error) {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/%s/%s",
		url.PathEscape(roomID),
		url.PathEscape(eventType),
		url.PathEscape(stateKey),
	)

	body, err := s.client.doRequest(ctx, http.MethodPut, path, s.accessToken, content)
	if err != nil {
		return "", fmt.Errorf("messaging: send state event to %q failed: %w", roomID, err)
	}

	var response SendEventResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("messaging: failed to parse send state response: %w", err)
	}
	return response.EventID, nil
}

// GetStateEvent fetches a specific state event's content from a room.
// Returns the raw JSON content — the caller is responsible for unmarshaling
// into the appropriate type (e.g., schema.MachineConfig).
//
// If the state event does not exist, returns a *MatrixError with code M_NOT_FOUND.
func (s *Session) GetStateEvent(ctx context.Context, roomID, eventType, stateKey string) (json.RawMessage, error) {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/%s/%s",
		url.PathEscape(roomID),
		url.PathEscape(eventType),
		url.PathEscape(stateKey),
	)

	body, err := s.client.doRequest(ctx, http.MethodGet, path, s.accessToken, nil)
	if err != nil {
		return nil, fmt.Errorf("messaging: get state event %s/%s in %q failed: %w", eventType, stateKey, roomID, err)
	}
	return json.RawMessage(body), nil
}

// GetRoomState fetches all current state events from a room.
// Returns the full event objects including type, state_key, sender, etc.
func (s *Session) GetRoomState(ctx context.Context, roomID string) ([]Event, error) {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state", url.PathEscape(roomID))

	body, err := s.client.doRequest(ctx, http.MethodGet, path, s.accessToken, nil)
	if err != nil {
		return nil, fmt.Errorf("messaging: get room state for %q failed: %w", roomID, err)
	}

	var events []Event
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, fmt.Errorf("messaging: failed to parse room state response: %w", err)
	}
	return events, nil
}

// RoomMessages fetches messages from a room with pagination.
func (s *Session) RoomMessages(ctx context.Context, roomID string, options RoomMessagesOptions) (*RoomMessagesResponse, error) {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages", url.PathEscape(roomID))

	query := url.Values{}
	if options.From != "" {
		query.Set("from", options.From)
	}
	direction := options.Direction
	if direction == "" {
		direction = "b" // backward (newest first) by default
	}
	query.Set("dir", direction)
	if options.Limit > 0 {
		query.Set("limit", strconv.Itoa(options.Limit))
	}

	body, err := s.client.doRequest(ctx, http.MethodGet, path, s.accessToken, nil, query)
	if err != nil {
		return nil, fmt.Errorf("messaging: room messages for %q failed: %w", roomID, err)
	}

	var response RoomMessagesResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("messaging: failed to parse messages response: %w", err)
	}
	return &response, nil
}

// ThreadMessages fetches all messages in a thread.
// threadRootID is the event ID of the thread's root message.
// Uses the /relations endpoint to get events related to the root via m.thread.
func (s *Session) ThreadMessages(ctx context.Context, roomID, threadRootID string, options ThreadMessagesOptions) (*ThreadMessagesResponse, error) {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/relations/%s/m.thread",
		url.PathEscape(roomID),
		url.PathEscape(threadRootID),
	)

	query := url.Values{}
	if options.From != "" {
		query.Set("from", options.From)
	}
	if options.Limit > 0 {
		query.Set("limit", strconv.Itoa(options.Limit))
	}

	body, err := s.client.doRequest(ctx, http.MethodGet, path, s.accessToken, nil, query)
	if err != nil {
		return nil, fmt.Errorf("messaging: thread messages for %q in %q failed: %w", threadRootID, roomID, err)
	}

	var response ThreadMessagesResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("messaging: failed to parse thread messages response: %w", err)
	}
	return &response, nil
}

// Sync performs an incremental sync with the homeserver.
// For initial sync, leave options.Since empty.
// For long-polling, set options.Timeout to the desired wait in milliseconds.
func (s *Session) Sync(ctx context.Context, options SyncOptions) (*SyncResponse, error) {
	query := url.Values{}
	if options.Since != "" {
		query.Set("since", options.Since)
	}
	if options.SetTimeout {
		query.Set("timeout", strconv.Itoa(options.Timeout))
	}
	if options.Filter != "" {
		query.Set("filter", options.Filter)
	}

	path := "/_matrix/client/v3/sync"
	body, err := s.client.doRequest(ctx, http.MethodGet, path, s.accessToken, nil, query)
	if err != nil {
		return nil, fmt.Errorf("messaging: sync failed: %w", err)
	}

	var response SyncResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("messaging: failed to parse sync response: %w", err)
	}
	return &response, nil
}

// ResolveAlias resolves a room alias (e.g., "#agents:bureau.local") to a room ID.
func (s *Session) ResolveAlias(ctx context.Context, alias string) (string, error) {
	path := "/_matrix/client/v3/directory/room/" + url.PathEscape(alias)
	body, err := s.client.doRequest(ctx, http.MethodGet, path, s.accessToken, nil)
	if err != nil {
		return "", fmt.Errorf("messaging: resolve alias %q failed: %w", alias, err)
	}

	var response ResolveAliasResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("messaging: failed to parse resolve alias response: %w", err)
	}
	return response.RoomID, nil
}

// UploadMedia uploads content to the homeserver's media repository.
// Returns the MXC URI (e.g., "mxc://bureau.local/abc123").
func (s *Session) UploadMedia(ctx context.Context, contentType string, body io.Reader) (string, error) {
	responseBody, err := s.client.doRequestRaw(ctx, http.MethodPost,
		"/_matrix/media/v3/upload", s.accessToken, contentType, body)
	if err != nil {
		return "", fmt.Errorf("messaging: media upload failed: %w", err)
	}

	var response UploadResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return "", fmt.Errorf("messaging: failed to parse upload response: %w", err)
	}
	return response.ContentURI, nil
}

// JoinedRooms returns the list of room IDs the user has joined.
func (s *Session) JoinedRooms(ctx context.Context) ([]string, error) {
	body, err := s.client.doRequest(ctx, http.MethodGet, "/_matrix/client/v3/joined_rooms", s.accessToken, nil)
	if err != nil {
		return nil, fmt.Errorf("messaging: joined rooms failed: %w", err)
	}

	var response JoinedRoomsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("messaging: failed to parse joined rooms response: %w", err)
	}
	return response.JoinedRooms, nil
}

// LeaveRoom leaves a room by ID.
func (s *Session) LeaveRoom(ctx context.Context, roomID string) error {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/leave", url.PathEscape(roomID))
	_, err := s.client.doRequest(ctx, http.MethodPost, path, s.accessToken, struct{}{})
	if err != nil {
		return fmt.Errorf("messaging: leave room %q failed: %w", roomID, err)
	}
	return nil
}

// GetRoomMembers returns the members of a room.
func (s *Session) GetRoomMembers(ctx context.Context, roomID string) ([]RoomMember, error) {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/members", url.PathEscape(roomID))
	body, err := s.client.doRequest(ctx, http.MethodGet, path, s.accessToken, nil)
	if err != nil {
		return nil, fmt.Errorf("messaging: get room members for %q failed: %w", roomID, err)
	}

	var response RoomMembersResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("messaging: failed to parse room members response: %w", err)
	}

	members := make([]RoomMember, len(response.Chunk))
	for index, event := range response.Chunk {
		members[index] = RoomMember{
			UserID:      event.StateKey,
			DisplayName: event.Content.DisplayName,
			Membership:  event.Content.Membership,
			AvatarURL:   event.Content.AvatarURL,
		}
	}
	return members, nil
}

// KickUser removes a user from a room with an optional reason.
func (s *Session) KickUser(ctx context.Context, roomID, userID, reason string) error {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/kick", url.PathEscape(roomID))
	_, err := s.client.doRequest(ctx, http.MethodPost, path, s.accessToken, KickRequest{
		UserID: userID,
		Reason: reason,
	})
	if err != nil {
		return fmt.Errorf("messaging: kick %q from %q failed: %w", userID, roomID, err)
	}
	return nil
}

// GetDisplayName fetches the display name for a Matrix user from their profile.
// Returns an empty string (not an error) if the user has no display name set.
func (s *Session) GetDisplayName(ctx context.Context, userID string) (string, error) {
	path := "/_matrix/client/v3/profile/" + url.PathEscape(userID) + "/displayname"
	body, err := s.client.doRequest(ctx, http.MethodGet, path, s.accessToken, nil)
	if err != nil {
		return "", fmt.Errorf("messaging: get display name for %q failed: %w", userID, err)
	}

	var response DisplayNameResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("messaging: failed to parse display name response: %w", err)
	}
	return response.DisplayName, nil
}

// TURNCredentials fetches time-limited TURN server credentials from the
// homeserver. The homeserver generates these using its configured turn_secret
// (shared with coturn via HMAC-SHA1). Returns the TURN URIs, ephemeral
// username/password, and credential lifetime.
//
// Corresponds to GET /_matrix/client/v3/voip/turnServer.
func (s *Session) TURNCredentials(ctx context.Context) (*TURNCredentialsResponse, error) {
	body, err := s.client.doRequest(ctx, http.MethodGet,
		"/_matrix/client/v3/voip/turnServer", s.accessToken, nil)
	if err != nil {
		return nil, fmt.Errorf("messaging: get TURN credentials failed: %w", err)
	}

	var response TURNCredentialsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("messaging: failed to parse TURN credentials: %w", err)
	}
	return &response, nil
}

// ChangePassword changes the account password. The Matrix spec requires User
// Interactive Authentication (UIA) for this endpoint, so the caller must
// provide the current password. The server invalidates all other access tokens
// for the account (logout_devices defaults to true in the spec).
//
// Corresponds to POST /_matrix/client/v3/account/password.
func (s *Session) ChangePassword(ctx context.Context, currentPassword, newPassword string) error {
	if currentPassword == "" {
		return fmt.Errorf("messaging: current password is required for password change")
	}
	if newPassword == "" {
		return fmt.Errorf("messaging: new password is required for password change")
	}

	// The Matrix password change endpoint requires User Interactive Auth.
	// We embed the auth block directly rather than doing a two-step UIA
	// flow, because password-based auth is always available.
	requestBody := map[string]any{
		"new_password": newPassword,
		"auth": map[string]any{
			"type":     "m.login.password",
			"user":     s.userID,
			"password": currentPassword,
		},
	}

	_, err := s.client.doRequest(ctx, http.MethodPost,
		"/_matrix/client/v3/account/password", s.accessToken, requestBody)
	if err != nil {
		return fmt.Errorf("messaging: change password failed: %w", err)
	}
	return nil
}

// nextTransactionID generates a unique transaction ID for idempotent event sending.
// Format: "bureau-<timestamp_ms>-<counter>" to ensure uniqueness across restarts.
func (s *Session) nextTransactionID() string {
	counter := s.transactionCounter.Add(1)
	return fmt.Sprintf("bureau-%d-%d", time.Now().UnixMilli(), counter)
}
