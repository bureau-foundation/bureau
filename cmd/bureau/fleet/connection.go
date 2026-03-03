// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/service"
)

// FleetConnectionConfig is the ServiceConnectionConfig for the fleet
// service role. Exported so that other CLI packages (e.g., service)
// can build their own FleetConnection using the same socket paths and
// environment variable names.
var FleetConnectionConfig = cli.ServiceConnectionConfig{
	Role:          "fleet",
	SocketEnvVar:  "BUREAU_FLEET_SOCKET",
	TokenEnvVar:   "BUREAU_FLEET_TOKEN",
	SandboxSocket: "/run/bureau/service/fleet.sock",
	SandboxToken:  "/run/bureau/service/token/fleet.token",
}

// FleetConnection manages connection parameters for fleet commands.
// Embeds [cli.ServiceConnection] for shared flag registration and
// daemon token minting. The [Connect] method creates a fleet-specific
// [service.ServiceClient] with tailored error messages.
//
// Excluded from JSON Schema generation since MCP callers don't specify
// socket paths — the service connection is established by the hosting
// sandbox.
type FleetConnection struct {
	cli.ServiceConnection
}

// AddFlags initializes the fleet service configuration and registers
// connection flags. Safe to call on a zero-value FleetConnection —
// the embedded ServiceConnection is configured before flag registration.
func (c *FleetConnection) AddFlags(flagSet *pflag.FlagSet) {
	c.ServiceConnection = cli.NewServiceConnection(FleetConnectionConfig)
	c.ServiceConnection.AddFlags(flagSet)
}

// Connect creates a service client from the connection parameters.
// In service mode, mints a token via the daemon and uses the returned
// socket path. In direct mode, reads the token from a file.
func (c *FleetConnection) Connect() (*service.ServiceClient, error) {
	if c.ServiceMode {
		result, err := c.mintFleetToken()
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

// mintFleetToken wraps MintServiceToken with fleet-specific error
// classification. The "no service binding found" error indicates the
// fleet controller isn't enabled on this machine — a distinct condition
// from a generic daemon communication failure.
func (c *FleetConnection) mintFleetToken() (*cli.MintResult, error) {
	result, err := c.MintServiceToken()
	if err == nil {
		return result, nil
	}

	if strings.Contains(err.Error(), "no service binding found") {
		return nil, cli.NotFound("fleet controller not enabled on this machine").
			WithHint("Run 'bureau fleet enable <fleet-localpart> --host <machine>' to set up service bindings.")
	}
	return nil, err
}

// CallContext returns a context with a 30-second timeout for fleet
// controller socket calls. Fleet operations involve in-memory index
// queries or single Matrix writes, so 30 seconds is generous.
func CallContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 30*time.Second)
}
