// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-observe-relay is the per-session relay process forked by the daemon
// for each observation session. It attaches to a Bureau-managed tmux session
// and relays terminal I/O between the tmux PTY and a unix socket inherited
// from the daemon.
//
// This binary is not invoked directly by users. The daemon spawns it with:
//
//	bureau-observe-relay <tmux-session-name>
//
// The daemon passes a connected unix socket on fd 3. The relay reads and
// writes the observation protocol on this fd while the daemon bridges the
// other end to the transport layer.
//
// Environment variables:
//
//	BUREAU_TMUX_SOCKET       tmux server socket path (default: /run/bureau/tmux.sock)
//	BUREAU_OBSERVE_READONLY  set to "1" for read-only observation (no input relay)
package main

import (
	"fmt"
	"net"
	"os"

	"github.com/bureau-foundation/bureau/observe"
)

// daemonSocketFD is the file descriptor number where the daemon passes
// the connected unix socket. This is part of the spawning contract
// defined in OBSERVATION.md (Contract 1).
const daemonSocketFD = 3

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bureau-observe-relay: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: bureau-observe-relay <tmux-session-name>\n\n")
		fmt.Fprintf(os.Stderr, "This binary is forked by the daemon. It is not intended for direct use.\n")
		return fmt.Errorf("tmux session name argument required")
	}
	sessionName := os.Args[1]

	// Read configuration from environment. The daemon sets these before
	// forking the relay process.
	serverSocket := os.Getenv("BUREAU_TMUX_SOCKET")
	if serverSocket == "" {
		serverSocket = "/run/bureau/tmux.sock"
	}

	readOnly := os.Getenv("BUREAU_OBSERVE_READONLY") == "1"

	// Inherit fd 3 as the daemon connection. The daemon holds the other
	// end and bridges it to the transport stream. os.NewFile returns nil
	// if the fd is invalid (e.g., the binary was invoked outside the
	// daemon's fork path).
	socketFile := os.NewFile(uintptr(daemonSocketFD), "daemon-socket")
	if socketFile == nil {
		return fmt.Errorf("fd %d not available (this binary must be forked by the daemon)", daemonSocketFD)
	}

	// Convert the raw file descriptor to a net.Conn so the observe
	// library gets proper socket semantics (shutdown, deadline support).
	// FileConn dups the fd internally, so we close the original.
	connection, err := net.FileConn(socketFile)
	socketFile.Close()
	if err != nil {
		return fmt.Errorf("opening daemon socket on fd %d: %w", daemonSocketFD, err)
	}
	defer connection.Close()

	return observe.Relay(connection, serverSocket, sessionName, readOnly)
}
