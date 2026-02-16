// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"os"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/service"
)

// Default paths inside a Bureau sandbox. The socket path is derived
// from the fleet controller's principal localpart; the token path
// matches the daemon's token provisioning convention.
var (
	defaultSocketPath = principal.SocketPath("service/fleet/main")
	defaultTokenPath  = "/run/bureau/tokens/fleet"
)

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
func (c *FleetConnection) AddFlags(flagSet *pflag.FlagSet) {
	socketDefault := defaultSocketPath
	if envSocket := os.Getenv("BUREAU_FLEET_SOCKET"); envSocket != "" {
		socketDefault = envSocket
	}
	tokenDefault := defaultTokenPath
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
