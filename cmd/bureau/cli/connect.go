// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

// ConnectOperator loads the operator session from "bureau login" and
// creates an authenticated Matrix session. Returns the session and a
// context with a 30-second timeout derived from the provided parent.
// The caller must defer cancel().
//
// Used by CLI commands that perform operator-level Matrix operations
// (listing templates, fetching pipelines, pushing state events, etc.).
func ConnectOperator(parent context.Context) (context.Context, context.CancelFunc, messaging.Session, error) {
	operatorSession, err := LoadSession()
	if err != nil {
		return nil, nil, nil, err
	}

	ctx, cancel := context.WithTimeout(parent, 30*time.Second)

	logger := NewClientLogger(slog.LevelWarn)

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: operatorSession.Homeserver,
		Logger:        logger,
	})
	if err != nil {
		cancel()
		return nil, nil, nil, Internal("create matrix client: %w", err)
	}

	userID, err := ref.ParseUserID(operatorSession.UserID)
	if err != nil {
		cancel()
		return nil, nil, nil, Internal("parse operator user ID: %w", err)
	}

	session, err := client.SessionFromToken(userID, operatorSession.AccessToken)
	if err != nil {
		cancel()
		return nil, nil, nil, Internal("create session: %w", err)
	}

	return ctx, cancel, session, nil
}

// ResolveRoom resolves a room string that may be a room ID (!...),
// a full room alias (#localpart:server), or a bare alias localpart.
// Returns a validated ref.RoomID.
//
// Resolution rules:
//   - Starts with '!': parsed as a room ID directly.
//   - Starts with '#': parsed as a room alias and resolved via Matrix.
//   - Anything else: treated as a bare alias localpart. The server
//     name is extracted from the operator session's user ID to form
//     #<localpart>:<server>, then resolved.
//
// Requires an active operator session (from "bureau login") for alias
// resolution. Commands running inside sandboxes (direct mode) should
// not call this — they receive room IDs from the daemon at socket
// provisioning time.
func ResolveRoom(ctx context.Context, room string) (ref.RoomID, error) {
	if room == "" {
		return ref.RoomID{}, Validation("--room is required")
	}

	// Already a room ID — parse and validate.
	if strings.HasPrefix(room, "!") {
		return ref.ParseRoomID(room)
	}

	// Full alias (#localpart:server) or bare localpart — resolve.
	alias := room
	if !strings.HasPrefix(alias, "#") {
		// Bare localpart: derive server name from operator session.
		operatorSession, err := LoadSession()
		if err != nil {
			return ref.RoomID{}, fmt.Errorf("resolving room alias: %w", err)
		}
		serverName, err := ref.ServerFromUserID(operatorSession.UserID)
		if err != nil {
			return ref.RoomID{}, fmt.Errorf("extracting server from operator user ID: %w", err)
		}
		alias = "#" + room + ":" + serverName.String()
	}

	roomAlias, err := ref.ParseRoomAlias(alias)
	if err != nil {
		return ref.RoomID{}, fmt.Errorf("invalid room alias %q: %w", alias, err)
	}

	resolveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	operatorSession, err := LoadSession()
	if err != nil {
		return ref.RoomID{}, fmt.Errorf("resolving room alias: %w", err)
	}

	logger := NewClientLogger(slog.LevelWarn)
	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: operatorSession.Homeserver,
		Logger:        logger,
	})
	if err != nil {
		return ref.RoomID{}, fmt.Errorf("create matrix client for alias resolution: %w", err)
	}

	userID, err := ref.ParseUserID(operatorSession.UserID)
	if err != nil {
		return ref.RoomID{}, fmt.Errorf("parse operator user ID: %w", err)
	}

	session, err := client.SessionFromToken(userID, operatorSession.AccessToken)
	if err != nil {
		return ref.RoomID{}, fmt.Errorf("create session for alias resolution: %w", err)
	}
	defer session.Close()

	roomID, err := session.ResolveAlias(resolveCtx, roomAlias)
	if err != nil {
		return ref.RoomID{}, fmt.Errorf("resolving alias %s: %w", roomAlias, err)
	}

	return roomID, nil
}
