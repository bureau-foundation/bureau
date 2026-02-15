// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"context"
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/lib/service"
)

// Default paths inside a Bureau sandbox.
const (
	defaultSocketPath = "/run/bureau/principal/service/ticket/main.sock"
	defaultTokenPath  = "/run/bureau/tokens/ticket"
)

// TicketConnection manages socket and token flags for ticket commands.
// Implements [cli.FlagBinder] so it integrates with the params struct
// system while handling dynamic defaults from environment variables.
// Excluded from JSON Schema generation since MCP callers don't specify
// socket paths — the service connection is established by the hosting
// sandbox.
type TicketConnection struct {
	SocketPath string
	TokenPath  string
}

// AddFlags registers --socket and --token-file flags with dynamic defaults
// from BUREAU_TICKET_SOCKET and BUREAU_TICKET_TOKEN environment variables.
func (c *TicketConnection) AddFlags(flagSet *pflag.FlagSet) {
	socketDefault := defaultSocketPath
	if envSocket := os.Getenv("BUREAU_TICKET_SOCKET"); envSocket != "" {
		socketDefault = envSocket
	}
	tokenDefault := defaultTokenPath
	if envToken := os.Getenv("BUREAU_TICKET_TOKEN"); envToken != "" {
		tokenDefault = envToken
	}

	flagSet.StringVar(&c.SocketPath, "socket", socketDefault, "ticket service socket path")
	flagSet.StringVar(&c.TokenPath, "token-file", tokenDefault, "path to service token file")
}

// connect creates a service client from the connection parameters.
func (c *TicketConnection) connect() (*service.ServiceClient, error) {
	return service.NewServiceClient(c.SocketPath, c.TokenPath)
}

// callContext returns a context with a reasonable timeout for service
// calls. Most ticket operations are fast (in-memory index queries),
// but batch creates can involve multiple Matrix writes.
func callContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}
