// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

// DaemonStatusFilename is the filename for the daemon status file within
// the run directory (/run/bureau/). Both the daemon (writer) and the
// doctor (reader) must use this constant to ensure they agree on the path.
const DaemonStatusFilename = "daemon-status.json"

// DaemonStatus is the JSON structure written by the daemon to
// <run-dir>/daemon-status.json. The daemon writes this file on startup
// and after version reconciliation. The doctor reads it to verify binary
// currency and session validity without requiring root access — no
// /proc/PID/exe reading, no session.json access.
//
// This type is the contract between the daemon (writer) and the doctor
// (reader). It lives in lib/schema/ to guarantee both sides agree on
// field names and JSON tags. Adding, removing, or renaming fields here
// requires updating both the daemon's writeDaemonStatus() and the
// doctor's readDaemonStatus().
type DaemonStatus struct {
	// DaemonBinaryPath is the absolute filesystem path of the running
	// daemon binary (resolved from os.Executable via EvalSymlinks).
	DaemonBinaryPath string `json:"daemon_binary_path"`

	// DaemonBinaryHash is the SHA256 hex digest of the running daemon
	// binary. The doctor compares this against the hash of the installed
	// binary at /var/bureau/bin/bureau-daemon to detect version drift.
	DaemonBinaryHash string `json:"daemon_binary_hash"`

	// LauncherBinaryHash is the SHA256 hex digest of the running
	// launcher binary, obtained via the launcher's IPC status action.
	// The doctor compares this against the installed launcher binary.
	LauncherBinaryHash string `json:"launcher_binary_hash"`

	// MachineUserID is the full Matrix user ID of this machine
	// (e.g., "@bureau/fleet/prod/machine/sharkbox:bureau.local").
	// The doctor uses this to verify machine identity without reading
	// the launcher's session.json (which is bureau:bureau 0700).
	MachineUserID string `json:"machine_user_id"`

	// HostEnvironmentPath is the Nix store path of the bureau-host-env
	// derivation that the daemon is currently using to resolve service
	// binaries. Empty if no BureauVersion has been reconciled.
	HostEnvironmentPath string `json:"host_environment_path,omitempty"`

	// StartedAt is the ISO 8601 timestamp of when the daemon started
	// (or when the status was last refreshed after version reconciliation).
	StartedAt string `json:"started_at"`
}
