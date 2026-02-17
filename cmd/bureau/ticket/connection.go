// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"context"
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/service"
)

// Sandbox-standard paths for the ticket service role. When an agent
// declares required_services: ["ticket"], the daemon bind-mounts the
// ticket service socket and writes a service token in the token
// subdirectory.
const (
	sandboxSocketPath = "/run/bureau/service/ticket.sock"
	sandboxTokenPath  = "/run/bureau/service/token/ticket.token"
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
// If neither environment variable is set, defaults are detected from the
// runtime environment: inside a sandbox, the RequiredServices mount points
// are used; outside, the host-side principal socket path.
func (c *TicketConnection) AddFlags(flagSet *pflag.FlagSet) {
	socketDefault := defaultTicketSocketPath()
	if envSocket := os.Getenv("BUREAU_TICKET_SOCKET"); envSocket != "" {
		socketDefault = envSocket
	}
	tokenDefault := defaultTicketTokenPath()
	if envToken := os.Getenv("BUREAU_TICKET_TOKEN"); envToken != "" {
		tokenDefault = envToken
	}

	flagSet.StringVar(&c.SocketPath, "socket", socketDefault, "ticket service socket path")
	flagSet.StringVar(&c.TokenPath, "token-file", tokenDefault, "path to service token file")
}

// defaultTicketSocketPath returns the default ticket service socket path.
// Inside a sandbox (detected by the presence of the RequiredServices mount
// point), the standard /run/bureau/service/ticket.sock path is used.
// Outside a sandbox, the host-side principal socket path is used as a
// fallback for direct CLI access.
func defaultTicketSocketPath() string {
	if _, err := os.Stat(sandboxSocketPath); err == nil {
		return sandboxSocketPath
	}
	return principal.SocketPath("service/ticket/main")
}

// defaultTicketTokenPath returns the default ticket service token path.
// Inside a sandbox, the daemon-provisioned token at
// /run/bureau/service/token/ticket.token is used. Outside a sandbox,
// the same path is returned as a fallback (it will only exist if the
// daemon minted tokens for this principal).
func defaultTicketTokenPath() string {
	if _, err := os.Stat(sandboxTokenPath); err == nil {
		return sandboxTokenPath
	}
	return sandboxTokenPath
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
