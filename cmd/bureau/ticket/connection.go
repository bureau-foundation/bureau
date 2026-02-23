// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/observe"
)

// Sandbox-standard paths for the ticket service role. When an agent
// declares required_services: ["ticket"], the daemon bind-mounts the
// ticket service socket and writes a service token in the token
// subdirectory.
const (
	sandboxSocketPath = "/run/bureau/service/ticket.sock"
	sandboxTokenPath  = "/run/bureau/service/token/ticket.token"
)

// TicketConnection manages connection parameters for ticket commands.
// Supports two modes:
//
// Direct mode (default): connects using --socket and --token-file,
// designed for agents running inside sandboxes where the daemon has
// bind-mounted the service socket and provisioned a token file.
//
// Service mode (--service): mints a token via the daemon's observe
// socket using the operator's saved session (~/.config/bureau/session.json
// from "bureau login"). Returns both the token and the host-side
// service socket path. Designed for operator CLI use from the host.
//
// Excluded from JSON Schema generation since MCP callers don't specify
// socket paths — the service connection is established by the hosting
// sandbox.
type TicketConnection struct {
	SocketPath   string
	TokenPath    string
	ServiceMode  bool
	DaemonSocket string
}

// MintResult holds the raw pieces from daemon-mediated token minting.
// Returned by MintServiceToken for callers that need the individual
// components (e.g., the viewer needs raw token bytes for ServiceSource
// and TTL for refresh scheduling). Short-lived commands use connect()
// which calls MintServiceToken internally.
type MintResult struct {
	// SocketPath is the host-side Unix socket path for the ticket
	// service's CBOR listener, resolved by the daemon from the
	// m.bureau.room_service binding in the machine's config room.
	SocketPath string

	// TokenBytes is the signed service token (CBOR + Ed25519
	// signature). Pass to service.NewServiceClientFromToken or
	// ticketui.ServiceSource.
	TokenBytes []byte

	// TTLSeconds is the token lifetime. Callers running long-lived
	// sessions should refresh at 80% of TTL by calling
	// MintServiceToken again.
	TTLSeconds int
}

// AddFlags registers connection flags. In direct mode (default):
// --socket and --token-file with defaults from BUREAU_TICKET_SOCKET
// and BUREAU_TICKET_TOKEN environment variables. In service mode:
// --daemon-socket for the daemon's observe socket.
func (c *TicketConnection) AddFlags(flagSet *pflag.FlagSet) {
	socketDefault := defaultTicketSocketPath()
	if envSocket := os.Getenv("BUREAU_TICKET_SOCKET"); envSocket != "" {
		socketDefault = envSocket
	}
	tokenDefault := defaultTicketTokenPath()
	if envToken := os.Getenv("BUREAU_TICKET_TOKEN"); envToken != "" {
		tokenDefault = envToken
	}

	flagSet.StringVar(&c.SocketPath, "socket", socketDefault, "ticket service socket path (direct mode)")
	flagSet.StringVar(&c.TokenPath, "token-file", tokenDefault, "path to service token file (direct mode)")
	flagSet.BoolVar(&c.ServiceMode, "service", false, "connect via daemon token minting (requires 'bureau login')")
	flagSet.StringVar(&c.DaemonSocket, "daemon-socket", observe.DefaultDaemonSocket, "daemon observe socket path (service mode)")
}

// defaultTicketSocketPath returns the default ticket service socket path.
// Inside a sandbox, the daemon bind-mounts the ticket service socket at
// the sandboxSocketPath. Outside a sandbox, the same path is used as the
// default — operators running the CLI directly should use --service mode
// instead.
func defaultTicketSocketPath() string {
	return sandboxSocketPath
}

// defaultTicketTokenPath returns the default ticket service token path.
// The daemon-provisioned token is at sandboxTokenPath. Operators outside
// a sandbox should use --service mode instead.
func defaultTicketTokenPath() string {
	return sandboxTokenPath
}

// connect creates a service client from the connection parameters.
// In service mode, mints a token via the daemon and uses the returned
// socket path. In direct mode, reads the token from a file.
func (c *TicketConnection) connect() (*service.ServiceClient, error) {
	if c.ServiceMode {
		result, err := c.MintServiceToken()
		if err != nil {
			return nil, err
		}
		return service.NewServiceClientFromToken(result.SocketPath, result.TokenBytes), nil
	}
	return service.NewServiceClient(c.SocketPath, c.TokenPath)
}

// MintServiceToken mints a signed service token via the daemon's
// observe socket. Loads the operator session from the well-known path
// (~/.config/bureau/session.json) on each call, so token rotation in
// the session file is picked up automatically.
//
// Returns the raw token bytes and service socket path for callers that
// need them individually (e.g., the viewer's ServiceSource and refresh
// loop). Short-lived commands should use connect() instead.
//
// Requires --service mode and a valid operator session from "bureau login".
func (c *TicketConnection) MintServiceToken() (*MintResult, error) {
	operatorSession, err := cli.LoadSession()
	if err != nil {
		return nil, err
	}

	tokenResponse, err := observe.MintServiceToken(
		c.DaemonSocket,
		"ticket",
		operatorSession.UserID,
		operatorSession.AccessToken,
	)
	if err != nil {
		return nil, fmt.Errorf("mint service token: %w", err)
	}

	tokenBytes, err := tokenResponse.TokenBytes()
	if err != nil {
		return nil, fmt.Errorf("decode service token: %w", err)
	}

	return &MintResult{
		SocketPath: tokenResponse.SocketPath,
		TokenBytes: tokenBytes,
		TTLSeconds: tokenResponse.TTLSeconds,
	}, nil
}

// callContext returns a context with a reasonable timeout for service
// calls derived from the provided parent. Most ticket operations are
// fast (in-memory index queries), but batch creates can involve
// multiple Matrix writes.
func callContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 30*time.Second)
}
