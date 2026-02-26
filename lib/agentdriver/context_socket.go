// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agentdriver

import (
	"context"
	"log/slog"

	"github.com/bureau-foundation/bureau/lib/service"
)

// DefaultWrapperContextSocketPath is the standard location for the
// wrapper's context socket inside a sandbox. The socket is unreachable
// from outside the namespace, so no authentication is needed.
const DefaultWrapperContextSocketPath = "/run/bureau/agent-context.sock"

// currentContextResponse is the wire-format response for the
// "current-context" action on the wrapper context socket.
type currentContextResponse struct {
	ContextID string `cbor:"context_id"`
}

// newWrapperContextSocketServer creates a SocketServer that exposes
// the current checkpoint context ID over a Unix socket. Tools running
// inside the sandbox (CLI commands, MCP tools) query this socket to
// discover the ctx-* identifier for inclusion in ticket mutations and
// other state-bearing operations.
//
// The socket uses unauthenticated handlers (Handle, not HandleAuth)
// because it lives inside the sandbox namespace and is unreachable
// from outside. The returned server should be started with Serve(ctx)
// in a goroutine; it shuts down when the context is cancelled.
func newWrapperContextSocketServer(
	socketPath string,
	contextIDGetter func() string,
	logger *slog.Logger,
) *service.SocketServer {
	server := service.NewSocketServer(socketPath, logger, nil)
	server.Handle("current-context", func(_ context.Context, _ []byte) (any, error) {
		return currentContextResponse{
			ContextID: contextIDGetter(),
		}, nil
	})
	return server
}
