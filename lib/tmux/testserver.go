// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package tmux

import (
	"path/filepath"
	"testing"

	"github.com/bureau-foundation/bureau/lib/testutil"
)

// NewTestServer creates an isolated tmux server for testing. The server:
//   - Uses a short /tmp path to stay within the 108-byte Unix socket limit
//   - Passes -f /dev/null to prevent loading the user's ~/.tmux.conf
//   - Creates a _guard session running "sleep infinity" to keep the server
//     alive (tmux exits when its last session ends)
//   - Registers t.Cleanup to kill the server when the test completes
//
// All test tmux commands MUST use the returned Server. A bare "tmux"
// command without -S targets the default server, which may be the session
// the test agent is running in — killing that server kills the agent.
func NewTestServer(t *testing.T) *Server {
	t.Helper()

	socketPath := filepath.Join(testutil.SocketDir(t), "tmux.sock")
	server := NewServer(socketPath, "/dev/null")

	// Create a guard session to keep the server alive. The server starts
	// when the first session is created, and the config is read at that
	// point. "sleep infinity" never exits, so the server survives until
	// our cleanup kills it.
	if err := server.NewSession("_guard", "sleep", "infinity"); err != nil {
		t.Fatalf("start tmux test server: %v", err)
	}

	t.Cleanup(func() {
		server.KillServer()
	})

	return server
}
