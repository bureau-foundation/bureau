// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package bridge provides a TCP-to-Unix socket forwarder for network-isolated
// sandboxes.
//
// Inside a bubblewrap sandbox with --unshare-net, the only network interface
// is the loopback adapter. The agent process cannot reach Unix sockets on
// the host filesystem (they are outside the mount namespace), and it cannot
// make TCP connections to the outside world. The bridge solves this by
// listening on a TCP port bound to 127.0.0.1 inside the sandbox and
// forwarding every accepted connection to the proxy's Unix socket on the
// host side.
//
// This allows agents to use standard HTTP client libraries with a localhost
// base URL:
//
//	ANTHROPIC_BASE_URL=http://127.0.0.1:8642/http/anthropic
//
// [Bridge] is the single type. Start validates that the target Unix socket
// is reachable, binds the TCP listener, and begins accepting connections in
// a background goroutine. Each connection is forwarded with bidirectional
// copy and half-close support (TCP FIN propagates as Unix socket shutdown
// and vice versa). Stop gracefully shuts down the listener; Wait blocks
// until all forwarded connections have drained. Addr returns the bound
// address, which may use an ephemeral port if port 0 was requested.
package bridge
