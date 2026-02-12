// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bureau-foundation/bureau/lib/secret"
)

// OperatorSession holds the operator's Matrix authentication state.
// Stored at the well-known path returned by SessionFilePath and loaded
// automatically by CLI commands that require authentication (observe,
// list, dashboard). Analogous to SSH keys — set up once via
// "bureau login", then transparent.
type OperatorSession struct {
	// UserID is the operator's full Matrix user ID
	// (e.g., "@bureau-admin:bureau.local").
	UserID string `json:"user_id"`

	// AccessToken is the Matrix access token proving the operator's
	// identity. The daemon verifies this via /account/whoami.
	AccessToken string `json:"access_token"`

	// Homeserver is the base URL of the Matrix homeserver
	// (e.g., "http://localhost:6167"). Included so commands that talk
	// directly to the homeserver (like bureau matrix doctor) can use
	// the same session file.
	Homeserver string `json:"homeserver"`
}

// SessionFilePath returns the path to the operator's session file.
// Checks BUREAU_SESSION_FILE environment variable first, then falls
// back to ~/.config/bureau/session.json.
func SessionFilePath() string {
	if envPath := os.Getenv("BUREAU_SESSION_FILE"); envPath != "" {
		return envPath
	}

	configDirectory := os.Getenv("XDG_CONFIG_HOME")
	if configDirectory == "" {
		homeDirectory, err := os.UserHomeDir()
		if err != nil {
			// Fallback — this should rarely happen.
			return filepath.Join("/tmp", "bureau-session.json")
		}
		configDirectory = filepath.Join(homeDirectory, ".config")
	}
	return filepath.Join(configDirectory, "bureau", "session.json")
}

// LoadSession reads the operator session from the well-known path.
// Returns a clear error message directing the user to "bureau login"
// if no session exists.
func LoadSession() (*OperatorSession, error) {
	path := SessionFilePath()
	return LoadSessionFrom(path)
}

// LoadSessionFrom reads an operator session from a specific file path.
func LoadSessionFrom(path string) (*OperatorSession, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no Bureau session found at %s — run \"bureau login\" first", path)
		}
		return nil, fmt.Errorf("reading session file %s: %w", path, err)
	}

	var session OperatorSession
	if err := json.Unmarshal(data, &session); err != nil {
		secret.Zero(data)
		return nil, fmt.Errorf("parsing session file %s: %w", path, err)
	}
	secret.Zero(data)

	if session.UserID == "" {
		return nil, fmt.Errorf("session file %s has no user_id", path)
	}
	if session.AccessToken == "" {
		return nil, fmt.Errorf("session file %s has no access_token", path)
	}
	if session.Homeserver == "" {
		return nil, fmt.Errorf("session file %s has no homeserver", path)
	}

	return &session, nil
}

// SaveSession writes an operator session to the well-known path.
// Creates the parent directory with mode 0700 if it doesn't exist.
// The session file is written with mode 0600 (owner-only read/write)
// since it contains an access token.
func SaveSession(session *OperatorSession) error {
	path := SessionFilePath()
	return SaveSessionTo(session, path)
}

// SaveSessionTo writes an operator session to a specific file path.
func SaveSessionTo(session *OperatorSession, path string) error {
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling session: %w", err)
	}
	data = append(data, '\n')

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		secret.Zero(data)
		return fmt.Errorf("creating session directory %s: %w", directory, err)
	}

	writeError := os.WriteFile(path, data, 0600)
	secret.Zero(data)
	if writeError != nil {
		return fmt.Errorf("writing session file %s: %w", path, writeError)
	}

	return nil
}
