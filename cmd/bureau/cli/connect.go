// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/messaging"
)

// ConnectOperator loads the operator session from "bureau login" and
// creates an authenticated Matrix session. Returns the session and a
// context with a 30-second timeout. The caller must defer cancel().
//
// Used by CLI commands that perform operator-level Matrix operations
// (listing templates, fetching pipelines, pushing state events, etc.).
func ConnectOperator() (context.Context, context.CancelFunc, *messaging.Session, error) {
	operatorSession, err := LoadSession()
	if err != nil {
		return nil, nil, nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: operatorSession.Homeserver,
		Logger:        logger,
	})
	if err != nil {
		cancel()
		return nil, nil, nil, fmt.Errorf("create matrix client: %w", err)
	}

	session, err := client.SessionFromToken(operatorSession.UserID, operatorSession.AccessToken)
	if err != nil {
		cancel()
		return nil, nil, nil, fmt.Errorf("create session: %w", err)
	}

	return ctx, cancel, session, nil
}
