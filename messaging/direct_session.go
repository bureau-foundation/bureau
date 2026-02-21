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

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/secret"
)

// DirectSession is an authenticated Matrix session.
// It wraps a Client with an access token for making authenticated API calls.
// DirectSessions are lightweight and safe to create in large numbers.
//
// The access token is stored in a secret.Buffer (mmap-backed, locked against
// swap, excluded from core dumps). The caller must call Close when the DirectSession
// is no longer needed.
type DirectSession struct {
	client      *Client
	accessToken *secret.Buffer
	userID      ref.UserID
	deviceID    string

	// transactionCounter generates unique transaction IDs for idempotent sends.
	transactionCounter atomic.Int64
}

// UserID returns the fully-qualified Matrix user ID (e.g., "@alice:bureau.local").
func (s *DirectSession) UserID() ref.UserID {
	return s.userID
}

// AccessToken returns the access token as a heap string. This creates a brief
// copy from the mmap-backed buffer — use only at API boundaries that require
// a string (e.g., writing to JSON, logging). Prefer passing the DirectSession
// itself when possible.
func (s *DirectSession) AccessToken() string {
	return s.accessToken.String()
}

// DeviceID returns the device ID for this session.
func (s *DirectSession) DeviceID() string {
	return s.deviceID
}

// CloseIdleConnections closes idle HTTP connections in the underlying
// transport's connection pool. Call this after a sync error to force
// the next request to establish a fresh TCP connection.
func (s *DirectSession) CloseIdleConnections() {
	s.client.CloseIdleConnections()
}

// Close releases the access token memory (zeros, unlocks, unmaps).
// Idempotent — safe to call multiple times.
func (s *DirectSession) Close() error {
	if s.accessToken != nil {
		return s.accessToken.Close()
	}
	return nil
}

// WhoAmI validates the access token and returns the user ID.
// Useful for checking whether a stored token is still valid.
func (s *DirectSession) WhoAmI(ctx context.Context) (ref.UserID, error) {
	body, err := s.client.doRequest(ctx, http.MethodGet, "/_matrix/client/v3/account/whoami", s.accessToken, nil)
	if err != nil {
		return ref.UserID{}, fmt.Errorf("messaging: whoami failed: %w", err)
	}

	var response WhoAmIResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return ref.UserID{}, fmt.Errorf("messaging: failed to parse whoami response: %w", err)
	}
	return response.UserID, nil
}

// CreateRoom creates a new Matrix room.
func (s *DirectSession) CreateRoom(ctx context.Context, request CreateRoomRequest) (*CreateRoomResponse, error) {
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

// JoinRoom joins a room by ID. Returns the room ID.
func (s *DirectSession) JoinRoom(ctx context.Context, roomID ref.RoomID) (ref.RoomID, error) {
	path := "/_matrix/client/v3/join/" + url.PathEscape(roomID.String())
	body, err := s.client.doRequest(ctx, http.MethodPost, path, s.accessToken, struct{}{})
	if err != nil {
		return ref.RoomID{}, fmt.Errorf("messaging: join room %s failed: %w", roomID, err)
	}

	var response struct {
		RoomID ref.RoomID `json:"room_id"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return ref.RoomID{}, fmt.Errorf("messaging: failed to parse join response: %w", err)
	}
	return response.RoomID, nil
}

// InviteUser invites a user to a room.
func (s *DirectSession) InviteUser(ctx context.Context, roomID ref.RoomID, userID ref.UserID) error {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/invite", url.PathEscape(roomID.String()))
	_, err := s.client.doRequest(ctx, http.MethodPost, path, s.accessToken, InviteRequest{UserID: userID})
	if err != nil {
		return fmt.Errorf("messaging: invite %q to %q failed: %w", userID, roomID, err)
	}
	return nil
}

// SendMessage sends a message to a room. The content includes thread context
// if this is a thread reply (see NewTextMessage and NewThreadReply).
// Returns the event ID of the sent message.
func (s *DirectSession) SendMessage(ctx context.Context, roomID ref.RoomID, content MessageContent) (string, error) {
	return s.SendEvent(ctx, roomID, "m.room.message", content)
}

// SendEvent sends an event of any type to a room.
// Uses Matrix's idempotent PUT with a transaction ID.
// Returns the event ID.
func (s *DirectSession) SendEvent(ctx context.Context, roomID ref.RoomID, eventType ref.EventType, content any) (string, error) {
	transactionID := s.nextTransactionID()
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/%s/%s",
		url.PathEscape(roomID.String()),
		url.PathEscape(eventType.String()),
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
func (s *DirectSession) SendStateEvent(ctx context.Context, roomID ref.RoomID, eventType ref.EventType, stateKey string, content any) (string, error) {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/%s/%s",
		url.PathEscape(roomID.String()),
		url.PathEscape(eventType.String()),
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
func (s *DirectSession) GetStateEvent(ctx context.Context, roomID ref.RoomID, eventType ref.EventType, stateKey string) (json.RawMessage, error) {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/%s/%s",
		url.PathEscape(roomID.String()),
		url.PathEscape(eventType.String()),
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
func (s *DirectSession) GetRoomState(ctx context.Context, roomID ref.RoomID) ([]Event, error) {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state", url.PathEscape(roomID.String()))

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
func (s *DirectSession) RoomMessages(ctx context.Context, roomID ref.RoomID, options RoomMessagesOptions) (*RoomMessagesResponse, error) {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages", url.PathEscape(roomID.String()))

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
func (s *DirectSession) ThreadMessages(ctx context.Context, roomID ref.RoomID, threadRootID string, options ThreadMessagesOptions) (*ThreadMessagesResponse, error) {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/relations/%s/m.thread",
		url.PathEscape(roomID.String()),
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
func (s *DirectSession) Sync(ctx context.Context, options SyncOptions) (*SyncResponse, error) {
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
func (s *DirectSession) ResolveAlias(ctx context.Context, alias ref.RoomAlias) (ref.RoomID, error) {
	path := "/_matrix/client/v3/directory/room/" + url.PathEscape(alias.String())
	body, err := s.client.doRequest(ctx, http.MethodGet, path, s.accessToken, nil)
	if err != nil {
		return ref.RoomID{}, fmt.Errorf("messaging: resolve alias %q failed: %w", alias, err)
	}

	var response ResolveAliasResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return ref.RoomID{}, fmt.Errorf("messaging: failed to parse resolve alias response: %w", err)
	}
	return response.RoomID, nil
}

// UploadMedia uploads content to the homeserver's media repository.
// Returns the MXC URI (e.g., "mxc://bureau.local/abc123").
func (s *DirectSession) UploadMedia(ctx context.Context, contentType string, body io.Reader) (string, error) {
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
func (s *DirectSession) JoinedRooms(ctx context.Context) ([]ref.RoomID, error) {
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
func (s *DirectSession) LeaveRoom(ctx context.Context, roomID ref.RoomID) error {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/leave", url.PathEscape(roomID.String()))
	_, err := s.client.doRequest(ctx, http.MethodPost, path, s.accessToken, struct{}{})
	if err != nil {
		return fmt.Errorf("messaging: leave room %q failed: %w", roomID, err)
	}
	return nil
}

// GetRoomMembers returns the members of a room.
func (s *DirectSession) GetRoomMembers(ctx context.Context, roomID ref.RoomID) ([]RoomMember, error) {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/members", url.PathEscape(roomID.String()))
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
func (s *DirectSession) KickUser(ctx context.Context, roomID ref.RoomID, userID ref.UserID, reason string) error {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/kick", url.PathEscape(roomID.String()))
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
func (s *DirectSession) GetDisplayName(ctx context.Context, userID ref.UserID) (string, error) {
	path := "/_matrix/client/v3/profile/" + url.PathEscape(userID.String()) + "/displayname"
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
func (s *DirectSession) TURNCredentials(ctx context.Context) (*TURNCredentialsResponse, error) {
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
// Both password Buffers are read but not closed — the caller retains ownership.
//
// Corresponds to POST /_matrix/client/v3/account/password.
func (s *DirectSession) ChangePassword(ctx context.Context, currentPassword, newPassword *secret.Buffer) error {
	if currentPassword == nil {
		return fmt.Errorf("messaging: current password is required for password change")
	}
	if newPassword == nil {
		return fmt.Errorf("messaging: new password is required for password change")
	}

	// The Matrix password change endpoint requires User Interactive Auth.
	// We embed the auth block directly rather than doing a two-step UIA
	// flow, because password-based auth is always available.
	// Passwords are converted to strings at the JSON serialization boundary.
	requestBody := map[string]any{
		"new_password": newPassword.String(),
		"auth": map[string]any{
			"type":     "m.login.password",
			"user":     s.userID.String(),
			"password": currentPassword.String(),
		},
	}

	_, err := s.client.doRequest(ctx, http.MethodPost,
		"/_matrix/client/v3/account/password", s.accessToken, requestBody)
	if err != nil {
		return fmt.Errorf("messaging: change password failed: %w", err)
	}
	return nil
}

// DeactivateUser deactivates a Matrix account via the Synapse admin API.
// The session must belong to a server administrator. The target user's
// access tokens are invalidated immediately and the account can no longer
// log in.
//
// When erase is true, the server also removes the user's display name,
// avatar, and other profile data.
//
// Corresponds to POST /_synapse/admin/v1/deactivate/{user_id}.
// Not all homeservers implement this endpoint — Continuwuity returns
// M_UNRECOGNIZED. Callers should fall back to [DirectSession.ResetUserPassword]
// with logoutDevices=true when this method fails.
func (s *DirectSession) DeactivateUser(ctx context.Context, userID ref.UserID, erase bool) error {
	path := "/_synapse/admin/v1/deactivate/" + url.PathEscape(userID.String())
	requestBody := map[string]any{
		"erase": erase,
	}

	_, err := s.client.doRequest(ctx, http.MethodPost, path, s.accessToken, requestBody)
	if err != nil {
		return fmt.Errorf("messaging: deactivate user %q failed: %w", userID, err)
	}
	return nil
}

// ResetUserPassword changes a user's password via the Synapse admin API.
// The session must belong to a server administrator.
//
// When logoutDevices is true, all of the user's access tokens are
// invalidated immediately. This is the primary mechanism for forcing a
// running daemon to detect an auth failure and trigger emergency shutdown:
// the daemon's next /sync attempt receives M_UNKNOWN_TOKEN.
//
// Corresponds to POST /_synapse/admin/v1/reset_password/{user_id}.
func (s *DirectSession) ResetUserPassword(ctx context.Context, userID ref.UserID, newPassword string, logoutDevices bool) error {
	path := "/_synapse/admin/v1/reset_password/" + url.PathEscape(userID.String())
	requestBody := map[string]any{
		"new_password":   newPassword,
		"logout_devices": logoutDevices,
	}

	_, err := s.client.doRequest(ctx, http.MethodPost, path, s.accessToken, requestBody)
	if err != nil {
		return fmt.Errorf("messaging: reset password for %q failed: %w", userID, err)
	}
	return nil
}

// LogoutAll invalidates all access tokens for the session's user by calling
// the Matrix client API POST /_matrix/client/v3/logout/all. After this
// call, every session for this user (including the caller's own) is
// invalidated.
//
// This is part of the core Matrix spec and supported by all homeservers,
// unlike the Synapse admin API endpoints ([DirectSession.DeactivateUser],
// [DirectSession.ResetUserPassword]) which are server-specific.
//
// In the revocation workflow, the admin reads the machine's saved session
// file to obtain its access token, creates a session from it, and calls
// LogoutAll. The daemon's next /sync attempt receives M_UNKNOWN_TOKEN and
// triggers emergency shutdown.
func (s *DirectSession) LogoutAll(ctx context.Context) error {
	_, err := s.client.doRequest(ctx, http.MethodPost, "/_matrix/client/v3/logout/all", s.accessToken, map[string]any{})
	if err != nil {
		return fmt.Errorf("messaging: logout all sessions failed: %w", err)
	}
	return nil
}

// nextTransactionID generates a unique transaction ID for idempotent event sending.
// Format: "bureau-<timestamp_ms>-<counter>" to ensure uniqueness across restarts.
func (s *DirectSession) nextTransactionID() string {
	counter := s.transactionCounter.Add(1)
	return fmt.Sprintf("bureau-%d-%d", time.Now().UnixMilli(), counter)
}
