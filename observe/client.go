// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import "io"

// DefaultDaemonSocket is the path where the daemon listens for
// observation requests. Separate from the launcher IPC socket
// (/run/bureau/launcher.sock) to keep observation traffic off the
// privileged IPC channel.
const DefaultDaemonSocket = "/run/bureau/observe.sock"

// Session represents an active observation session from the client's
// perspective. It wraps the daemon connection and provides terminal
// I/O relay with status reporting.
type Session struct {
	// Metadata received from the remote side on connect.
	Metadata MetadataPayload

	// connection is the daemon socket carrying the observation protocol.
	connection io.ReadWriteCloser
}

// Connect establishes an observation session through the local daemon.
// It sends the ObserveRequest as JSON, reads the ObserveResponse, and
// if successful, reads the initial metadata and history messages.
//
// daemonSocket is the path to the daemon's observation unix socket
// (typically DefaultDaemonSocket).
//
// The returned Session is ready for Run(). The caller must close it
// when done.
func Connect(daemonSocket string, request ObserveRequest) (*Session, error) {
	// Implementation in bd-nuc (observation client library bead).
	panic("observe.Connect not yet implemented")
}

// Run relays terminal I/O between the observation session and the
// provided reader/writer (typically os.Stdin and os.Stdout). It handles:
//
//   - Writing history dump to output (for local tmux scrollback capture)
//   - Copying remote terminal output → output (stdout)
//   - Copying input (stdin) → remote terminal input
//   - Sending resize messages on SIGWINCH
//   - Printing status messages on connect/reconnect
//
// Run blocks until the session ends, the reader is exhausted, or an
// error occurs. Returns nil on clean shutdown.
func (session *Session) Run(input io.Reader, output io.Writer) error {
	// Implementation in bd-nuc (observation client library bead).
	panic("Session.Run not yet implemented")
}

// Close terminates the observation session and releases resources.
func (session *Session) Close() error {
	if session.connection != nil {
		return session.connection.Close()
	}
	return nil
}
