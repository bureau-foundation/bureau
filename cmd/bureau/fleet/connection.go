// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"os"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/service"
)

// Sandbox-standard paths for the fleet service role. When an agent
// declares required_services: ["fleet"], the daemon bind-mounts the
// fleet controller socket at this path and writes a service token to
// the tokens directory.
const (
	sandboxSocketPath = "/run/bureau/service/fleet.sock"
	sandboxTokenPath  = "/run/bureau/tokens/fleet"
)

// defaultFleetSocketPath returns the default fleet controller socket path.
// Inside a sandbox (detected by the presence of the RequiredServices mount
// point), the standard /run/bureau/service/fleet.sock path is used.
// Outside a sandbox, the host-side principal socket path is used as a
// fallback for direct CLI access.
func defaultFleetSocketPath() string {
	if _, err := os.Stat(sandboxSocketPath); err == nil {
		return sandboxSocketPath
	}
	return principal.SocketPath("service/fleet/main")
}

// defaultFleetTokenPath returns the default fleet service token path.
// Inside a sandbox, the daemon-provisioned token at /run/bureau/tokens/fleet
// is used. Outside a sandbox, the same path is returned as a fallback
// (it will only exist if the daemon minted tokens for this principal).
func defaultFleetTokenPath() string {
	if _, err := os.Stat(sandboxTokenPath); err == nil {
		return sandboxTokenPath
	}
	return sandboxTokenPath
}

// FleetConnection manages socket and token flags for fleet commands.
// Implements [cli.FlagBinder] so it integrates with the params struct
// system while handling dynamic defaults from environment variables.
// Excluded from JSON Schema generation since MCP callers don't specify
// socket paths — the service connection is established by the hosting
// sandbox.
type FleetConnection struct {
	SocketPath string
	TokenPath  string
}

// AddFlags registers --socket and --token-file flags with dynamic defaults
// from BUREAU_FLEET_SOCKET and BUREAU_FLEET_TOKEN environment variables.
// If neither environment variable is set, defaults are detected from the
// runtime environment: inside a sandbox, the RequiredServices mount points
// are used; outside, the host-side principal socket path.
func (c *FleetConnection) AddFlags(flagSet *pflag.FlagSet) {
	socketDefault := defaultFleetSocketPath()
	if envSocket := os.Getenv("BUREAU_FLEET_SOCKET"); envSocket != "" {
		socketDefault = envSocket
	}
	tokenDefault := defaultFleetTokenPath()
	if envToken := os.Getenv("BUREAU_FLEET_TOKEN"); envToken != "" {
		tokenDefault = envToken
	}

	flagSet.StringVar(&c.SocketPath, "socket", socketDefault, "fleet controller socket path")
	flagSet.StringVar(&c.TokenPath, "token-file", tokenDefault, "path to service token file")
}

// connect creates a service client from the connection parameters.
func (c *FleetConnection) connect() (*service.ServiceClient, error) {
	return service.NewServiceClient(c.SocketPath, c.TokenPath)
}
