// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package log

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/service"
)

// artifactConnectionConfig is the ServiceConnectionConfig for the
// artifact service role, used by list, show, and export commands.
var artifactConnectionConfig = cli.ServiceConnectionConfig{
	Role:          "artifact",
	SocketEnvVar:  "BUREAU_ARTIFACT_SOCKET",
	TokenEnvVar:   "BUREAU_ARTIFACT_TOKEN",
	SandboxSocket: "/run/bureau/service/artifact.sock",
	SandboxToken:  "/run/bureau/service/token/artifact.token",
}

// telemetryConnectionConfig is the ServiceConnectionConfig for the
// telemetry service role, used by the tail command.
var telemetryConnectionConfig = cli.ServiceConnectionConfig{
	Role:          "telemetry",
	SocketEnvVar:  "BUREAU_TELEMETRY_SOCKET",
	TokenEnvVar:   "BUREAU_TELEMETRY_TOKEN",
	SandboxSocket: "/run/bureau/service/telemetry.sock",
	SandboxToken:  "/run/bureau/service/token/telemetry.token",
}

// ticketConnectionConfig is the ServiceConnectionConfig for the ticket
// service role, used by the tail command for ticket ID resolution.
var ticketConnectionConfig = cli.ServiceConnectionConfig{
	Role:          "ticket",
	SocketEnvVar:  "BUREAU_TICKET_SOCKET",
	TokenEnvVar:   "BUREAU_TICKET_TOKEN",
	SandboxSocket: "/run/bureau/service/ticket.sock",
	SandboxToken:  "/run/bureau/service/token/ticket.token",
}

// LogArtifactConnection manages connection parameters for log commands
// that read from the artifact service (list, show, export).
type LogArtifactConnection struct {
	cli.ServiceConnection
}

// AddFlags initializes the artifact service configuration and registers
// connection flags.
func (c *LogArtifactConnection) AddFlags(flagSet *pflag.FlagSet) {
	c.ServiceConnection = cli.NewServiceConnection(artifactConnectionConfig)
	c.ServiceConnection.AddFlags(flagSet)
}

// connect creates an artifact client from the connection parameters.
func (c *LogArtifactConnection) connect() (*artifactstore.Client, error) {
	if c.ServiceMode {
		result, err := c.MintServiceToken()
		if err != nil {
			if strings.Contains(err.Error(), "no service binding found") {
				return nil, cli.NotFound("artifact service not available on this machine").
					WithHint("The artifact service must be deployed as a fleet service.")
			}
			return nil, err
		}
		return artifactstore.NewClientFromToken(result.SocketPath, result.TokenBytes), nil
	}
	return artifactstore.NewClient(c.SocketPath, c.TokenPath)
}

// LogTelemetryConnection manages connection parameters for the tail
// command. Unlike the request-response ServiceClient used by other
// telemetry commands, tail needs a raw streaming connection. This type
// provides flag management and credential resolution, but the caller
// opens the streaming socket manually.
type LogTelemetryConnection struct {
	cli.ServiceConnection
}

// AddFlags initializes the telemetry service configuration and
// registers connection flags.
func (c *LogTelemetryConnection) AddFlags(flagSet *pflag.FlagSet) {
	c.ServiceConnection = cli.NewServiceConnection(telemetryConnectionConfig)
	c.ServiceConnection.AddFlags(flagSet)
}

// resolveStreamCredentials returns the socket path and token bytes for
// the telemetry service. In service mode, mints a token via the daemon.
// In direct mode, reads the token from a file.
func (c *LogTelemetryConnection) resolveStreamCredentials() (socketPath string, tokenBytes []byte, err error) {
	if c.ServiceMode {
		result, err := c.MintServiceToken()
		if err != nil {
			if strings.Contains(err.Error(), "no service binding found") {
				return "", nil, cli.NotFound("telemetry service not available on this machine").
					WithHint("The telemetry service must be deployed as a fleet service. " +
						"Check fleet service placement with 'bureau fleet service list'.")
			}
			return "", nil, err
		}
		return result.SocketPath, result.TokenBytes, nil
	}

	tokenBytes, err = os.ReadFile(c.TokenPath)
	if err != nil {
		return "", nil, cli.Internal("reading telemetry service token from %s: %w", c.TokenPath, err).
			WithHint("In direct mode, check that --token-file points to a valid service token.\n" +
				"From the host, use --service mode instead: 'bureau log tail <pattern> --service'.")
	}
	if len(tokenBytes) == 0 {
		return "", nil, fmt.Errorf("telemetry service token file %s is empty", c.TokenPath)
	}
	return c.SocketPath, tokenBytes, nil
}

// resolveTicketSource resolves a ticket ID to a source localpart by
// fetching the ticket, finding its log attachment, and extracting the
// source from the attachment's artifact tag name. Returns the source
// localpart for use as a tail subscription pattern.
func resolveTicketSource(ctx context.Context, serviceMode bool, daemonSocket string, ticketID string) (string, error) {
	conn := cli.NewServiceConnection(ticketConnectionConfig)
	conn.ServiceMode = serviceMode
	conn.DaemonSocket = daemonSocket

	var client *service.ServiceClient
	var err error

	if serviceMode {
		result, err := conn.MintServiceToken()
		if err != nil {
			if strings.Contains(err.Error(), "no service binding found") {
				return "", cli.NotFound("ticket service not available on this machine").
					WithHint("Ticket resolution requires the ticket service. " +
						"Use a source pattern instead of a ticket ID, or deploy the ticket service.")
			}
			return "", err
		}
		client = service.NewServiceClientFromToken(result.SocketPath, result.TokenBytes)
	} else {
		client, err = service.NewServiceClient(conn.SocketPath, conn.TokenPath)
		if err != nil {
			return "", cli.Internal("connecting to ticket service: %w", err).
				WithHint("Ticket resolution requires the ticket service. " +
					"Use a source pattern instead of a ticket ID.")
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Fetch the ticket. The "show" action returns ticket content
	// including attachments.
	var result struct {
		Content struct {
			Attachments []struct {
				Ref         string `json:"ref"`
				Label       string `json:"label,omitempty"`
				ContentType string `json:"content_type,omitempty"`
			} `json:"attachments,omitempty"`
		} `json:"content"`
	}
	if err := client.Call(ctx, "show", map[string]any{"ticket": ticketID}, &result); err != nil {
		return "", fmt.Errorf("fetching ticket %s: %w", ticketID, err)
	}

	// Find the log attachment. The pipeline executor attaches output
	// logs with content_type "application/x-bureau-log" and a ref
	// that is the artifact tag name (e.g., "log/source/session-id").
	for _, attachment := range result.Content.Attachments {
		if attachment.ContentType == "application/x-bureau-log" {
			// The ref is the artifact tag: "log/<source-localpart>/<session-id>".
			// Extract the source localpart by stripping "log/" prefix and
			// the last path segment (session ID).
			tagName := attachment.Ref
			if !strings.HasPrefix(tagName, "log/") {
				continue
			}
			remainder := strings.TrimPrefix(tagName, "log/")
			lastSlash := strings.LastIndex(remainder, "/")
			if lastSlash < 0 {
				continue
			}
			sourceLocalpart := remainder[:lastSlash]
			return sourceLocalpart, nil
		}
	}

	return "", cli.NotFound("ticket %s has no output log attachment", ticketID).
		WithHint(fmt.Sprintf("Only pipeline tickets have output logs attached. "+
			"Check the ticket's attachments with 'bureau ticket show %s'.", ticketID))
}

// callContext returns a context with a 30-second timeout for artifact
// service calls.
func callContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 30*time.Second)
}

// formatBytes formats a byte count as a human-readable string.
func formatBytes(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
