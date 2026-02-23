// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"log/slog"
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
