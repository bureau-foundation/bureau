// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/messaging"
)

// SessionData is the JSON structure of session.json, written by the
// launcher during first-boot registration of a principal.
type SessionData struct {
	HomeserverURL string `json:"homeserver_url"`
	UserID        string `json:"user_id"`
	AccessToken   string `json:"access_token"`
}

// LoadSession reads the Matrix session from stateDir/session.json and
// returns an authenticated client and session. The homeserverURL
// parameter overrides the URL stored in session.json when non-empty,
// allowing the caller to redirect to a different homeserver without
// modifying the session file.
//
// The access token is moved into mmap-backed guarded memory by the
// messaging library. The raw JSON bytes are zeroed after parsing.
//
// The caller must call Session.Close when the session is no longer
// needed to release the guarded memory.
func LoadSession(stateDir, homeserverURL string, logger *slog.Logger) (*messaging.Client, *messaging.DirectSession, error) {
	sessionPath := filepath.Join(stateDir, "session.json")

	jsonData, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading session from %s: %w", sessionPath, err)
	}

	var data SessionData
	if err := json.Unmarshal(jsonData, &data); err != nil {
		secret.Zero(jsonData)
		return nil, nil, fmt.Errorf("parsing session from %s: %w", sessionPath, err)
	}
	secret.Zero(jsonData)

	if data.AccessToken == "" {
		return nil, nil, fmt.Errorf("session file %s has empty access token", sessionPath)
	}

	serverURL := homeserverURL
	if serverURL == "" {
		serverURL = data.HomeserverURL
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: serverURL,
		Logger:        logger,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("creating matrix client: %w", err)
	}

	userID, err := ref.ParseUserID(data.UserID)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid user_id in %s: %w", sessionPath, err)
	}

	session, err := client.SessionFromToken(userID, data.AccessToken)
	if err != nil {
		return nil, nil, err
	}
	return client, session, nil
}

// SaveSession writes a Matrix session to stateDir/session.json. The
// homeserverURL is stored alongside the user ID and access token so
// that LoadSession can reconstruct the client later.
//
// The JSON bytes are zeroed after writing to limit the window during
// which the access token exists in process memory as cleartext.
func SaveSession(stateDir, homeserverURL string, session *messaging.DirectSession) error {
	data := SessionData{
		HomeserverURL: homeserverURL,
		UserID:        session.UserID().String(),
		AccessToken:   session.AccessToken(),
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshaling session: %w", err)
	}

	sessionPath := filepath.Join(stateDir, "session.json")
	writeError := os.WriteFile(sessionPath, jsonData, 0600)
	secret.Zero(jsonData)

	if writeError != nil {
		return fmt.Errorf("writing session to %s: %w", sessionPath, writeError)
	}

	return nil
}

// ValidateSession calls WhoAmI to verify the session's access token
// is still valid and returns the authenticated user ID. This should
// be called once at startup after LoadSession.
func ValidateSession(ctx context.Context, session messaging.Session) (ref.UserID, error) {
	userID, err := session.WhoAmI(ctx)
	if err != nil {
		return ref.UserID{}, fmt.Errorf("validating matrix session: %w", err)
	}
	return userID, nil
}
