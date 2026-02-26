// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/observe"
)

// ServiceConnection manages socket and token flags for CLI commands
// that connect to Bureau services. Supports two modes:
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
// Implements [FlagBinder] so it integrates with the params struct system
// when embedded in command parameter structs. Excluded from JSON Schema
// generation since MCP callers don't specify socket paths — the service
// connection is established by the hosting sandbox.
//
// Not used directly — each service defines a wrapper type (e.g.,
// TicketConnection) that embeds ServiceConnection and overrides AddFlags
// to supply the service-specific configuration. This allows zero-value
// construction in params structs while keeping the shared logic here.
// Create configured instances with [NewServiceConnection].
type ServiceConnection struct {
	SocketPath   string
	TokenPath    string
	ServiceMode  bool
	DaemonSocket string

	// Unexported configuration set at construction time.
	serviceRole string
	socketEnv   string
	tokenEnv    string
	socketPath  string
	tokenPath   string
}

// ServiceConnectionConfig configures a [ServiceConnection] for a
// specific service role.
type ServiceConnectionConfig struct {
	// Role is the service role name used for daemon token minting
	// (e.g., "ticket", "fleet", "artifact"). Must match the role
	// registered in the daemon's service directory.
	Role string

	// SocketEnvVar is the environment variable name for overriding
	// the default socket path (e.g., "BUREAU_TICKET_SOCKET").
	SocketEnvVar string

	// TokenEnvVar is the environment variable name for overriding
	// the default token file path (e.g., "BUREAU_TICKET_TOKEN").
	TokenEnvVar string

	// SandboxSocket is the default socket path inside a sandbox
	// (e.g., "/run/bureau/service/ticket.sock").
	SandboxSocket string

	// SandboxToken is the default token file path inside a sandbox
	// (e.g., "/run/bureau/service/token/ticket.token").
	SandboxToken string
}

// NewServiceConnection creates a ServiceConnection configured for a
// specific service role. The returned value is ready to embed in a
// command params struct — call AddFlags during flag registration.
func NewServiceConnection(config ServiceConnectionConfig) ServiceConnection {
	return ServiceConnection{
		serviceRole: config.Role,
		socketEnv:   config.SocketEnvVar,
		tokenEnv:    config.TokenEnvVar,
		socketPath:  config.SandboxSocket,
		tokenPath:   config.SandboxToken,
	}
}

// AddFlags registers --socket, --token-file, --service, and
// --daemon-socket flags. Socket and token defaults come from
// environment variables if set, otherwise from the sandbox paths
// configured at construction time.
func (c *ServiceConnection) AddFlags(flagSet *pflag.FlagSet) {
	socketDefault := c.socketPath
	if envSocket := os.Getenv(c.socketEnv); envSocket != "" {
		socketDefault = envSocket
	}
	tokenDefault := c.tokenPath
	if envToken := os.Getenv(c.tokenEnv); envToken != "" {
		tokenDefault = envToken
	}

	flagSet.StringVar(&c.SocketPath, "socket", socketDefault, c.serviceRole+" service socket path (direct mode)")
	flagSet.StringVar(&c.TokenPath, "token-file", tokenDefault, "path to service token file (direct mode)")
	flagSet.BoolVar(&c.ServiceMode, "service", false, "connect via daemon token minting (requires 'bureau login')")
	flagSet.StringVar(&c.DaemonSocket, "daemon-socket", observe.DefaultDaemonSocket, "daemon observe socket path (service mode)")
}

// MintResult holds the raw pieces from daemon-mediated token minting.
// Short-lived commands typically use a service-specific connect()
// method that calls MintServiceToken internally. Long-lived callers
// (e.g., TUI viewers with token refresh loops) use MintServiceToken
// directly and access the individual fields.
type MintResult struct {
	// SocketPath is the host-side Unix socket path for the service's
	// CBOR listener, resolved by the daemon from the
	// m.bureau.room_service binding in the machine's config room.
	SocketPath string

	// TokenBytes is the signed service token (CBOR + Ed25519
	// signature).
	TokenBytes []byte

	// TTLSeconds is the token lifetime. Callers running long-lived
	// sessions should refresh at 80% of TTL by calling
	// MintServiceToken again.
	TTLSeconds int
}

// MintServiceToken mints a signed service token via the daemon's
// observe socket. Loads the operator session from the well-known path
// (~/.config/bureau/session.json) on each call, so token rotation in
// the session file is picked up automatically.
//
// Requires --service mode and a valid operator session from "bureau login".
func (c *ServiceConnection) MintServiceToken() (*MintResult, error) {
	if c.serviceRole == "" {
		return nil, Internal("ServiceConnection not configured: call AddFlags before MintServiceToken")
	}

	operatorSession, err := LoadSession()
	if err != nil {
		return nil, err
	}

	tokenResponse, err := observe.MintServiceToken(
		c.DaemonSocket,
		c.serviceRole,
		operatorSession.UserID,
		operatorSession.AccessToken,
	)
	if err != nil {
		return nil, Transient("mint %s service token via daemon at %s: %w", c.serviceRole, c.DaemonSocket, err).
			WithHint("Is the Bureau daemon running? Check with 'bureau service list'. " +
				"The daemon must be started before using --service mode.")
	}

	tokenBytes, err := tokenResponse.TokenBytes()
	if err != nil {
		return nil, Internal("decode service token: %w", err)
	}

	return &MintResult{
		SocketPath: tokenResponse.SocketPath,
		TokenBytes: tokenBytes,
		TTLSeconds: tokenResponse.TTLSeconds,
	}, nil
}
