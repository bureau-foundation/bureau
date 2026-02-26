// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"context"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/service"
)

// ticketConnectionConfig is the ServiceConnectionConfig for the ticket
// service role. Shared between AddFlags (for zero-value params struct
// construction) and any explicit callers.
var ticketConnectionConfig = cli.ServiceConnectionConfig{
	Role:          "ticket",
	SocketEnvVar:  "BUREAU_TICKET_SOCKET",
	TokenEnvVar:   "BUREAU_TICKET_TOKEN",
	SandboxSocket: "/run/bureau/service/ticket.sock",
	SandboxToken:  "/run/bureau/service/token/ticket.token",
}

// TicketConnection manages connection parameters for ticket commands.
// Embeds [cli.ServiceConnection] for shared flag registration and
// daemon token minting. The connect() method creates a ticket-specific
// [service.ServiceClient].
//
// Excluded from JSON Schema generation since MCP callers don't specify
// socket paths — the service connection is established by the hosting
// sandbox.
type TicketConnection struct {
	cli.ServiceConnection
}

// AddFlags initializes the ticket service configuration and registers
// connection flags. Safe to call on a zero-value TicketConnection —
// the embedded ServiceConnection is configured before flag registration.
func (c *TicketConnection) AddFlags(flagSet *pflag.FlagSet) {
	c.ServiceConnection = cli.NewServiceConnection(ticketConnectionConfig)
	c.ServiceConnection.AddFlags(flagSet)
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

// callContext returns a context with a reasonable timeout for service
// calls derived from the provided parent. Most ticket operations are
// fast (in-memory index queries), but batch creates can involve
// multiple Matrix writes.
func callContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 30*time.Second)
}

// addResolvedRoom resolves a room string (ID, alias, or bare
// localpart) and adds it to a service call fields map. If the room
// string is empty, the field is not added (for commands where --room
// is optional). Returns an error only if the room string is non-empty
// but cannot be resolved.
func addResolvedRoom(ctx context.Context, fields map[string]any, room string) error {
	if room == "" {
		return nil
	}
	roomID, err := cli.ResolveRoom(ctx, room)
	if err != nil {
		return err
	}
	fields["room"] = roomID.String()
	return nil
}
