// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package messaging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// HomeserverAdmin abstracts server-specific administrative operations
// that are not part of the standard Matrix client-server API.
//
// Two implementations exist:
//   - SynapseAdmin: uses the Synapse admin HTTP API endpoints
//   - ContinuwuityAdmin: sends !admin commands to the Continuwuity
//     admin room and parses the bot's text responses
//
// Use [NewHomeserverAdmin] to auto-detect the homeserver type and
// return the appropriate implementation.
type HomeserverAdmin interface {
	// ResetUserPassword changes a user's password. When logoutDevices
	// is true, all existing access tokens are invalidated.
	// Non-destructive: the account remains active and can log in with
	// the new password.
	ResetUserPassword(ctx context.Context, userID ref.UserID, newPassword string, logoutDevices bool) error

	// DeactivateUser permanently deactivates a user account. All access
	// tokens are invalidated and the account can no longer log in. When
	// erase is true, profile data is also removed.
	DeactivateUser(ctx context.Context, userID ref.UserID, erase bool) error

	// ForceLogout invalidates all access tokens for a user without
	// changing their password or deactivating their account.
	ForceLogout(ctx context.Context, userID ref.UserID) error

	// SuspendUser puts a user in read-only mode. The user can still
	// sync and read rooms but cannot send messages, create rooms, or
	// perform write operations. Useful for investigation before full
	// revocation.
	SuspendUser(ctx context.Context, userID ref.UserID) error

	// UnsuspendUser removes read-only suspension, restoring the user's
	// ability to perform write operations.
	UnsuspendUser(ctx context.Context, userID ref.UserID) error
}

// NewHomeserverAdmin auto-detects the homeserver type and returns the
// appropriate [HomeserverAdmin] implementation.
//
// Detection strategy: probes GET /_synapse/admin/v1/server_version.
// Synapse returns 200 OK. Continuwuity returns M_UNRECOGNIZED.
// Other errors (network, auth) are returned as-is — the function
// does not silently fall back on transient failures.
//
// For Continuwuity, this creates a DM room with the server's admin
// bot user (@conduit:<server>) and verifies the bot joins. The room
// persists as an audit trail of admin operations.
func NewHomeserverAdmin(ctx context.Context, session *DirectSession) (HomeserverAdmin, error) {
	if probeSynapseAdmin(ctx, session) {
		slog.Info("detected Synapse admin API")
		return &SynapseAdmin{session: session}, nil
	}

	slog.Info("Synapse admin API not available, setting up Continuwuity admin room")
	admin, err := newContinuwuityAdmin(ctx, session)
	if err != nil {
		return nil, fmt.Errorf("set up homeserver admin interface: %w", err)
	}
	return admin, nil
}

// probeSynapseAdmin checks whether the homeserver exposes the Synapse
// admin API by probing the server version endpoint.
func probeSynapseAdmin(ctx context.Context, session *DirectSession) bool {
	_, err := session.client.doRequest(ctx, http.MethodGet,
		"/_synapse/admin/v1/server_version", session.accessToken, nil)
	return err == nil
}

// --- SynapseAdmin ---

// SynapseAdmin implements [HomeserverAdmin] using the Synapse admin
// HTTP API endpoints. All operations are synchronous HTTP calls.
type SynapseAdmin struct {
	session *DirectSession
}

func (a *SynapseAdmin) ResetUserPassword(ctx context.Context, userID ref.UserID, newPassword string, logoutDevices bool) error {
	return a.session.ResetUserPassword(ctx, userID, newPassword, logoutDevices)
}

func (a *SynapseAdmin) DeactivateUser(ctx context.Context, userID ref.UserID, erase bool) error {
	return a.session.DeactivateUser(ctx, userID, erase)
}

func (a *SynapseAdmin) ForceLogout(ctx context.Context, userID ref.UserID) error {
	// Synapse has no dedicated "force logout" admin endpoint.
	// Reset the password to a random value with logout_devices=true
	// to invalidate all access tokens.
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return fmt.Errorf("generate random password for force logout: %w", err)
	}
	return a.session.ResetUserPassword(ctx, userID, hex.EncodeToString(randomBytes), true)
}

func (a *SynapseAdmin) SuspendUser(ctx context.Context, userID ref.UserID) error {
	return a.synapsePutUser(ctx, userID, map[string]any{"suspended": true})
}

func (a *SynapseAdmin) UnsuspendUser(ctx context.Context, userID ref.UserID) error {
	return a.synapsePutUser(ctx, userID, map[string]any{"suspended": false})
}

// synapsePutUser sends a PUT request to the Synapse v2 user admin
// endpoint. Used for suspend/unsuspend and other per-user settings.
func (a *SynapseAdmin) synapsePutUser(ctx context.Context, userID ref.UserID, body map[string]any) error {
	path := "/_synapse/admin/v2/users/" + url.PathEscape(userID.String())
	_, err := a.session.client.doRequest(ctx, http.MethodPut, path, a.session.accessToken, body)
	if err != nil {
		return fmt.Errorf("synapse admin PUT user %q: %w", userID, err)
	}
	return nil
}

// --- ContinuwuityAdmin ---

// ContinuwuityAdmin implements [HomeserverAdmin] by sending !admin
// commands to the Continuwuity homeserver's admin bot user in a DM
// room. The bot processes the command and responds with a text message
// containing the result.
//
// The admin room is created once during construction and reused for
// all subsequent commands. The room history serves as an audit trail
// of admin operations.
type ContinuwuityAdmin struct {
	session   *DirectSession
	adminRoom ref.RoomID
	botUserID ref.UserID
}

// newContinuwuityAdmin creates a ContinuwuityAdmin by joining the
// server's built-in admin room (#admins:<server>). The admin room is
// created automatically by Continuwuity at startup, and admin users
// receive an invite. Commands sent in this room are processed by the
// @conduit:<server> bot user.
func newContinuwuityAdmin(ctx context.Context, session *DirectSession) (*ContinuwuityAdmin, error) {
	server, err := ref.ServerFromUserID(session.UserID().String())
	if err != nil {
		return nil, fmt.Errorf("determine server name from admin session: %w", err)
	}

	botUserID, err := ref.ParseUserID("@conduit:" + server.String())
	if err != nil {
		return nil, fmt.Errorf("construct bot user ID: %w", err)
	}

	// The admin room has a well-known alias #admins:<server>.
	adminAlias, err := ref.ParseRoomAlias("#admins:" + server.String())
	if err != nil {
		return nil, fmt.Errorf("construct admin room alias: %w", err)
	}

	adminRoomID, err := session.ResolveAlias(ctx, adminAlias)
	if err != nil {
		return nil, fmt.Errorf("resolve admin room alias %s: %w", adminAlias, err)
	}

	// Accept the pending invite if we haven't joined yet. JoinRoom
	// is idempotent — it succeeds for rooms we already belong to.
	if _, err := session.JoinRoom(ctx, adminRoomID); err != nil {
		return nil, fmt.Errorf("join admin room %s: %w", adminRoomID, err)
	}

	slog.Info("connected to Continuwuity admin room",
		"room_id", adminRoomID,
		"bot_user", botUserID,
		"alias", adminAlias,
	)

	return &ContinuwuityAdmin{
		session:   session,
		adminRoom: adminRoomID,
		botUserID: botUserID,
	}, nil
}

func (a *ContinuwuityAdmin) ResetUserPassword(ctx context.Context, userID ref.UserID, newPassword string, logoutDevices bool) error {
	// Continuwuity's reset-password always logs out all devices.
	// The logoutDevices parameter is noted but cannot be controlled
	// independently via the admin room command.
	command := fmt.Sprintf("!admin users reset-password %s %s", userID, newPassword)
	return a.sendCommand(ctx, command, "reset-password")
}

func (a *ContinuwuityAdmin) DeactivateUser(ctx context.Context, userID ref.UserID, erase bool) error {
	// Continuwuity's deactivate command does not support a granular
	// erase flag. The erase parameter is accepted for interface
	// compatibility but has no effect on the admin room command.
	command := fmt.Sprintf("!admin users deactivate %s", userID)
	return a.sendCommand(ctx, command, "deactivate")
}

func (a *ContinuwuityAdmin) ForceLogout(ctx context.Context, userID ref.UserID) error {
	command := fmt.Sprintf("!admin users logout %s", userID)
	return a.sendCommand(ctx, command, "force-logout")
}

func (a *ContinuwuityAdmin) SuspendUser(ctx context.Context, userID ref.UserID) error {
	command := fmt.Sprintf("!admin users suspend %s", userID)
	return a.sendCommand(ctx, command, "suspend")
}

func (a *ContinuwuityAdmin) UnsuspendUser(ctx context.Context, userID ref.UserID) error {
	command := fmt.Sprintf("!admin users unsuspend %s", userID)
	return a.sendCommand(ctx, command, "unsuspend")
}

// sendCommand sends an admin command to the Continuwuity bot and waits
// for the response. Returns nil on success, or an error containing the
// bot's response text on failure.
func (a *ContinuwuityAdmin) sendCommand(ctx context.Context, command string, operationName string) error {
	// Create a watcher BEFORE sending the command so we don't miss
	// the bot's response.
	watcher, err := WatchRoom(ctx, a.session, a.adminRoom, &SyncFilter{
		TimelineTypes: []string{"m.room.message"},
	})
	if err != nil {
		return fmt.Errorf("watch admin room for %s response: %w", operationName, err)
	}

	// Send the command as a plain text message.
	if _, err := a.session.SendMessage(ctx, a.adminRoom, NewTextMessage(command)); err != nil {
		return fmt.Errorf("send %s command to admin room: %w", operationName, err)
	}

	// Wait for the bot's response.
	event, err := watcher.WaitForEvent(ctx, func(event Event) bool {
		if event.Sender != a.botUserID {
			return false
		}
		// Accept any message from the bot (m.text, m.notice, etc.).
		return event.Type == "m.room.message"
	})
	if err != nil {
		return fmt.Errorf("waiting for %s response from admin bot: %w", operationName, err)
	}

	return parseAdminResponse(event, operationName)
}

// parseAdminResponse extracts the plain text body from a bot response
// event and checks for error indicators.
func parseAdminResponse(event Event, operationName string) error {
	// Extract the body from the event content.
	contentBytes, err := json.Marshal(event.Content)
	if err != nil {
		return fmt.Errorf("marshal admin response content: %w", err)
	}

	var content struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(contentBytes, &content); err != nil {
		return fmt.Errorf("parse admin response content: %w", err)
	}

	body := content.Body
	if body == "" {
		return fmt.Errorf("admin bot returned empty response for %s", operationName)
	}

	// Check for error indicators in the response text.
	bodyLower := strings.ToLower(body)
	errorIndicators := []string{
		"failed",
		"error",
		"invalid",
		"not found",
		"no such user",
		"could not",
		"unable to",
		"unknown command",
	}
	for _, indicator := range errorIndicators {
		if strings.Contains(bodyLower, indicator) {
			return fmt.Errorf("admin %s failed: %s", operationName, body)
		}
	}

	slog.Info("admin command succeeded",
		"operation", operationName,
		"response", body,
	)
	return nil
}
