// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"context"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/service"
)

// modelConnectionConfig is the ServiceConnectionConfig for the model
// service role.
var modelConnectionConfig = cli.ServiceConnectionConfig{
	Role:          "model",
	SocketEnvVar:  "BUREAU_MODEL_SOCKET",
	TokenEnvVar:   "BUREAU_MODEL_TOKEN",
	SandboxSocket: "/run/bureau/service/model.sock",
	SandboxToken:  "/run/bureau/service/token/model.token",
}

// ModelConnection manages connection parameters for model commands.
// Embeds [cli.ServiceConnection] for shared flag registration and
// daemon token minting. Excluded from JSON Schema generation since
// MCP callers don't specify socket paths.
type ModelConnection struct {
	cli.ServiceConnection
}

// AddFlags initializes the model service configuration and registers
// connection flags. Safe to call on a zero-value ModelConnection.
func (c *ModelConnection) AddFlags(flagSet *pflag.FlagSet) {
	c.ServiceConnection = cli.NewServiceConnection(modelConnectionConfig)
	c.ServiceConnection.AddFlags(flagSet)
}

// connect creates a service client from the connection parameters.
// In service mode, mints a token via the daemon. In direct mode,
// reads the token from a file.
func (c *ModelConnection) connect() (*service.ServiceClient, error) {
	if c.ServiceMode {
		result, err := c.mintModelToken()
		if err != nil {
			return nil, err
		}
		return service.NewServiceClientFromToken(result.SocketPath, result.TokenBytes), nil
	}
	client, err := service.NewServiceClient(c.SocketPath, c.TokenPath)
	if err != nil {
		return nil, cli.Internal("connecting to model service: %w", err).
			WithHint("In direct mode, check that --socket points to a valid model service socket " +
				"and --token-file points to a valid service token.\n" +
				"From the host, use --service mode instead: 'bureau model <command> --service'.")
	}
	return client, nil
}

// mintModelToken wraps MintServiceToken with model-specific error
// classification.
func (c *ModelConnection) mintModelToken() (*cli.MintResult, error) {
	result, err := c.MintServiceToken()
	if err == nil {
		return result, nil
	}

	if strings.Contains(err.Error(), "no service binding found") {
		return nil, cli.NotFound("model service not enabled on this machine").
			WithHint("The model service must be deployed and registered in the " +
				"machine's fleet configuration. Check 'bureau service list'.")
	}
	return nil, err
}

// callContext returns a context with a timeout for non-streaming
// service calls (list, status, embed). Streaming calls (complete)
// manage their own deadlines.
func callContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 30*time.Second)
}
