// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/service"
)

// telemetryConnectionConfig is the ServiceConnectionConfig for the
// telemetry service role. Shared between AddFlags (for zero-value
// params struct construction) and any explicit callers.
var telemetryConnectionConfig = cli.ServiceConnectionConfig{
	Role:          "telemetry",
	SocketEnvVar:  "BUREAU_TELEMETRY_SOCKET",
	TokenEnvVar:   "BUREAU_TELEMETRY_TOKEN",
	SandboxSocket: "/run/bureau/service/telemetry.sock",
	SandboxToken:  "/run/bureau/service/token/telemetry.token",
}

// TelemetryConnection manages connection parameters for telemetry
// commands. Embeds [cli.ServiceConnection] for shared flag
// registration and daemon token minting. The connect() method creates
// a telemetry-specific [service.ServiceClient].
//
// Excluded from JSON Schema generation since MCP callers don't
// specify socket paths — the service connection is established by the
// hosting sandbox.
type TelemetryConnection struct {
	cli.ServiceConnection
}

// AddFlags initializes the telemetry service configuration and
// registers connection flags. Safe to call on a zero-value
// TelemetryConnection — the embedded ServiceConnection is configured
// before flag registration.
func (c *TelemetryConnection) AddFlags(flagSet *pflag.FlagSet) {
	c.ServiceConnection = cli.NewServiceConnection(telemetryConnectionConfig)
	c.ServiceConnection.AddFlags(flagSet)
}

// connect creates a service client from the connection parameters.
// In service mode, mints a token via the daemon and uses the returned
// socket path. In direct mode, reads the token from a file.
func (c *TelemetryConnection) connect() (*service.ServiceClient, error) {
	if c.ServiceMode {
		result, err := c.mintTelemetryToken()
		if err != nil {
			return nil, err
		}
		return service.NewServiceClientFromToken(result.SocketPath, result.TokenBytes), nil
	}
	client, err := service.NewServiceClient(c.SocketPath, c.TokenPath)
	if err != nil {
		return nil, cli.Internal("connecting to telemetry service: %w", err).
			WithHint("In direct mode, check that --socket points to a valid telemetry service socket " +
				"and --token-file points to a valid service token.\n" +
				"From the host, use --service mode instead: 'bureau telemetry <command> --service'.")
	}
	return client, nil
}

// mintTelemetryToken wraps MintServiceToken with telemetry-specific
// error classification. The "no service binding found" error indicates
// the telemetry service isn't enabled for this machine — a distinct
// condition from a generic daemon communication failure.
func (c *TelemetryConnection) mintTelemetryToken() (*cli.MintResult, error) {
	result, err := c.MintServiceToken()
	if err == nil {
		return result, nil
	}

	if strings.Contains(err.Error(), "no service binding found") {
		return nil, cli.NotFound("telemetry service not available on this machine").
			WithHint("The telemetry service must be deployed as a fleet service. " +
				"Check fleet service placement with 'bureau fleet service list'.")
	}
	return nil, err
}

// callContext returns a context with a reasonable timeout for service
// calls derived from the provided parent. Telemetry queries scan
// time-partitioned SQLite tables and may involve moderate I/O for
// wide time ranges.
func callContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 30*time.Second)
}
