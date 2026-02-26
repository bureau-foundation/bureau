// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/observe"
)

// Sandbox-standard paths for the fleet service role. When an agent
// declares required_services: ["fleet"], the daemon bind-mounts the
// fleet controller socket and writes a service token in the token
// subdirectory.
const (
	sandboxSocketPath = "/run/bureau/service/fleet.sock"
	sandboxTokenPath  = "/run/bureau/service/token/fleet.token"
)

// hostFallbackSocketPath is the fleet controller socket path on the
// host side (outside a sandbox). This is the default run directory
// (/run/bureau/principal/) plus the service localpart and .sock suffix.
const hostFallbackSocketPath = "/run/bureau/principal/service/fleet/main.sock"

// defaultFleetSocketPath returns the default fleet controller socket path.
// Inside a sandbox (detected by the presence of the RequiredServices mount
// point), the standard /run/bureau/service/fleet.sock path is used.
// Outside a sandbox, the host-side principal socket path is used as a
// fallback for direct CLI access.
func defaultFleetSocketPath() string {
	if _, err := os.Stat(sandboxSocketPath); err == nil {
		return sandboxSocketPath
	}
	return hostFallbackSocketPath
}

// defaultFleetTokenPath returns the default fleet service token path.
// This is the daemon-provisioned path inside a sandbox. Operators running
// CLI commands from the host should use --service mode instead — there
// is no host-side fallback for token files.
func defaultFleetTokenPath() string {
	return sandboxTokenPath
}

// FleetConnection manages socket and token flags for fleet commands.
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
type FleetConnection struct {
	SocketPath   string
	TokenPath    string
	ServiceMode  bool
	DaemonSocket string
}

// MintResult holds the raw pieces from daemon-mediated token minting.
// Returned by MintServiceToken for callers that need the individual
// components (e.g., long-lived watchers need raw token bytes and TTL
// for refresh scheduling). Short-lived commands use connect() which
// calls MintServiceToken internally.
type MintResult struct {
	// SocketPath is the host-side Unix socket path for the fleet
	// controller's CBOR listener, resolved by the daemon from the
	// m.bureau.room_service binding in the machine's config room.
	SocketPath string

	// TokenBytes is the signed service token (CBOR + Ed25519
	// signature). Pass to service.NewServiceClientFromToken.
	TokenBytes []byte

	// TTLSeconds is the token lifetime. Callers running long-lived
	// sessions should refresh at 80% of TTL by calling
	// MintServiceToken again.
	TTLSeconds int
}

// AddFlags registers connection flags. In direct mode (default):
// --socket and --token-file with defaults from BUREAU_FLEET_SOCKET
// and BUREAU_FLEET_TOKEN environment variables. In service mode:
// --daemon-socket for the daemon's observe socket.
func (c *FleetConnection) AddFlags(flagSet *pflag.FlagSet) {
	socketDefault := defaultFleetSocketPath()
	if envSocket := os.Getenv("BUREAU_FLEET_SOCKET"); envSocket != "" {
		socketDefault = envSocket
	}
	tokenDefault := defaultFleetTokenPath()
	if envToken := os.Getenv("BUREAU_FLEET_TOKEN"); envToken != "" {
		tokenDefault = envToken
	}

	flagSet.StringVar(&c.SocketPath, "socket", socketDefault, "fleet controller socket path (direct mode)")
	flagSet.StringVar(&c.TokenPath, "token-file", tokenDefault, "path to service token file (direct mode)")
	flagSet.BoolVar(&c.ServiceMode, "service", false, "connect via daemon token minting (requires 'bureau login')")
	flagSet.StringVar(&c.DaemonSocket, "daemon-socket", observe.DefaultDaemonSocket, "daemon observe socket path (service mode)")
}

// connect creates a service client from the connection parameters.
// In service mode, mints a token via the daemon and uses the returned
// socket path. In direct mode, reads the token from a file.
func (c *FleetConnection) connect() (*service.ServiceClient, error) {
	if c.ServiceMode {
		result, err := c.MintServiceToken()
		if err != nil {
			return nil, err
		}
		return service.NewServiceClientFromToken(result.SocketPath, result.TokenBytes), nil
	}
	client, err := service.NewServiceClient(c.SocketPath, c.TokenPath)
	if err != nil {
		return nil, cli.Internal("connecting to fleet controller: %w", err).
			WithHint("In direct mode, check that --socket points to a valid fleet controller socket " +
				"and --token-file points to a valid service token.\n" +
				"From the host, use --service mode instead: 'bureau fleet <command> --service'.")
	}
	return client, nil
}

// MintServiceToken mints a signed service token via the daemon's
// observe socket. Loads the operator session from the well-known path
// (~/.config/bureau/session.json) on each call, so token rotation in
// the session file is picked up automatically.
//
// Returns the raw token bytes and service socket path for callers that
// need them individually. Short-lived commands should use connect()
// instead.
//
// Requires --service mode and a valid operator session from "bureau login".
func (c *FleetConnection) MintServiceToken() (*MintResult, error) {
	operatorSession, err := cli.LoadSession()
	if err != nil {
		return nil, err
	}

	tokenResponse, err := observe.MintServiceToken(
		c.DaemonSocket,
		"fleet",
		operatorSession.UserID,
		operatorSession.AccessToken,
	)
	if err != nil {
		if strings.Contains(err.Error(), "no service binding found") {
			return nil, cli.NotFound("fleet controller not enabled on this machine").
				WithHint("Run 'bureau fleet enable <fleet-localpart> --host <machine>' to set up service bindings.")
		}
		return nil, cli.Transient("minting fleet service token: %w", err).
			WithHint("Check that the Bureau daemon is running and the observe socket is accessible at " + c.DaemonSocket + ".")
	}

	tokenBytes, err := tokenResponse.TokenBytes()
	if err != nil {
		return nil, cli.Internal("decoding service token: %w", err)
	}

	return &MintResult{
		SocketPath: tokenResponse.SocketPath,
		TokenBytes: tokenBytes,
		TTLSeconds: tokenResponse.TTLSeconds,
	}, nil
}
