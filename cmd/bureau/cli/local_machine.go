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

// ResolveLocalMachine discovers the local machine's localpart. Resolution
// priority:
//
//  1. /etc/bureau/machine.conf — BUREAU_MACHINE_NAME. This is the canonical
//     source: world-readable, contains no secrets, written by "bureau machine
//     doctor --fix". Operators and agents can always read it.
//  2. Launcher session file — fallback for minimal setups where machine.conf
//     doesn't exist (e.g., early bootstrap before doctor runs). The session
//     file is only readable by the bureau user, so this path fails for
//     operators — but if machine.conf exists, they never hit it.
//
// The session file path can be overridden with BUREAU_LAUNCHER_SESSION.
// The machine.conf path can be overridden with BUREAU_MACHINE_CONF.
//
// Returns the machine localpart (e.g., "bureau/fleet/prod/machine/workstation").
func ResolveLocalMachine() (string, error) {
	// Try machine.conf first — public configuration, no secrets.
	if conf := LoadMachineConf(); conf.MachineName != "" {
		return conf.MachineName, nil
	}

	// Fall back to the launcher session file. This requires read access
	// to /var/lib/bureau/session.json (bureau user only).
	sessionPath := os.Getenv("BUREAU_LAUNCHER_SESSION")
	if sessionPath == "" {
		sessionPath = DefaultLauncherSessionPath
	}

	data, err := os.ReadFile(sessionPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", NotFound(
				"no machine identity found: %s has no BUREAU_MACHINE_NAME and "+
					"no launcher session at %s — is Bureau deployed on this machine? "+
					"Run 'bureau machine doctor --fix' to set up machine.conf",
				MachineConfPath(), sessionPath,
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
