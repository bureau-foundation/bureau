// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os"

	"github.com/bureau-foundation/bureau/lib/principal"
)

// DefaultLauncherStateDir is the default directory where the Bureau launcher
// stores its persistent state (keypair, session file). Matches the default
// value of the launcher's --state-dir flag.
const DefaultLauncherStateDir = "/var/lib/bureau"

// DefaultLauncherSessionPath is the default path to the launcher's session
// file, which contains the machine's Matrix identity. The launcher writes
// this file on first boot after registering with the homeserver.
const DefaultLauncherSessionPath = DefaultLauncherStateDir + "/session.json"

// ResolveLocalMachine reads the local launcher's session file to discover
// the machine localpart. The launcher registers a Matrix account for the
// machine on first boot and writes the session (including user_id) to disk.
//
// The session file path can be overridden with the BUREAU_LAUNCHER_SESSION
// environment variable.
//
// Returns the machine localpart (e.g., "machine/workstation") extracted from
// the Matrix user ID (e.g., "@machine/workstation:bureau.local").
func ResolveLocalMachine() (string, error) {
	sessionPath := os.Getenv("BUREAU_LAUNCHER_SESSION")
	if sessionPath == "" {
		sessionPath = DefaultLauncherSessionPath
	}

	data, err := os.ReadFile(sessionPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", NotFound(
				"no launcher session found at %s — is the Bureau launcher running on this machine? "+
					"(set BUREAU_LAUNCHER_SESSION to override the path)",
				sessionPath,
			)
		}
		return "", Internal("reading launcher session %s: %w", sessionPath, err)
	}

	// The launcher's session file has {homeserver_url, user_id, access_token}.
	// We only need user_id — parse minimally and don't retain the access token.
	var session struct {
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(data, &session); err != nil {
		return "", Internal("parsing launcher session %s: %w", sessionPath, err)
	}

	if session.UserID == "" {
		return "", Validation("launcher session %s has no user_id", sessionPath)
	}

	localpart, err := principal.LocalpartFromMatrixID(session.UserID)
	if err != nil {
		return "", Internal("extracting machine name from launcher session: %w", err)
	}

	return localpart, nil
}
