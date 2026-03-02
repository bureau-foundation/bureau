// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package log

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/netutil"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
)

// streamDialTimeout is the maximum time to wait for a streaming
// connection to the telemetry service.
const streamDialTimeout = 5 * time.Second

type tailParams struct {
	LogTelemetryConnection
}

func tailCommand() *cli.Command {
	var params tailParams

	return &cli.Command{
		Name:    "tail",
		Summary: "Stream live terminal output from running sandboxes",
		Description: `Stream raw terminal output from running sandboxed processes in real
time. The target argument is either a source pattern (glob syntax) or
a ticket ID (tkt-*).

Source patterns use Bureau's standard glob syntax on entity localparts:

  **                              all sources
  my_bureau/fleet/prod/**         everything in a fleet
  **/agent/reviewer               a specific agent across fleets
  my_bureau/fleet/prod/agent/*    all agents in a fleet

When the target starts with "tkt-", it is resolved as a ticket ID:
the ticket's output log attachment is fetched and its source is used
as the subscription pattern. This requires the ticket service to be
available.

The stream stays open until interrupted (Ctrl+C). Output is written
to stdout as raw bytes, preserving ANSI escape sequences.`,
		Usage: "bureau log tail <target> [flags]",
		Examples: []cli.Example{
			{
				Description: "Stream all output",
				Command:     "bureau log tail '**' --service",
			},
			{
				Description: "Stream output from a specific agent",
				Command:     "bureau log tail 'my_bureau/fleet/prod/agent/reviewer' --service",
			},
			{
				Description: "Stream output for a pipeline ticket",
				Command:     "bureau log tail tkt-a3f9 --service",
			},
		},
		Params:         func() any { return &params },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/log/tail"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("target argument required (source pattern or ticket ID)\n\nUsage: bureau log tail <target> [flags]")
			}
			target := args[0]

			// Resolve the subscription pattern. Ticket IDs are resolved
			// via the ticket service; everything else is used as a glob
			// pattern directly.
			pattern := target
			if strings.HasPrefix(target, "tkt-") {
				logger.Info("resolving ticket to source", "ticket", target)
				source, err := resolveTicketSource(ctx, params.ServiceMode, params.DaemonSocket, target)
				if err != nil {
					return err
				}
				pattern = source
				logger.Info("resolved ticket to source", "ticket", target, "source", source)
			}

			socketPath, tokenBytes, err := params.resolveStreamCredentials()
			if err != nil {
				return err
			}

			// Set up signal handling for clean shutdown.
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			return runTailStream(ctx, socketPath, tokenBytes, pattern, logger)
		},
	}
}

// runTailStream opens a streaming connection to the telemetry service's
// "tail" action, subscribes to the given pattern, and writes matching
// OutputDelta data to stdout until the context is cancelled.
func runTailStream(ctx context.Context, socketPath string, tokenBytes []byte, pattern string, logger *slog.Logger) error {
	// Connect to the telemetry service.
	dialer := net.Dialer{Timeout: streamDialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return cli.Internal("connecting to telemetry service at %s: %w", socketPath, err).
			WithHint("Check that the telemetry service is running. " +
				"Use 'bureau telemetry status --service' to verify.")
	}
	defer conn.Close()

	encoder := codec.NewEncoder(conn)
	decoder := codec.NewDecoder(conn)

	// Send the handshake: action + token. The socket server verifies
	// the token and routes to the "tail" stream handler.
	handshake := map[string]any{
		"action": "tail",
		"token":  tokenBytes,
	}
	if err := encoder.Encode(handshake); err != nil {
		return fmt.Errorf("writing handshake: %w", err)
	}

	// Wait for the readiness signal.
	var ack telemetry.StreamAck
	if err := decoder.Decode(&ack); err != nil {
		return fmt.Errorf("reading handshake response: %w", err)
	}
	if !ack.OK {
		return fmt.Errorf("tail handshake rejected: %s", ack.Error)
	}

	// Subscribe to the source pattern.
	control := telemetry.TailControl{
		Subscribe: []string{pattern},
	}
	if err := encoder.Encode(control); err != nil {
		return fmt.Errorf("sending subscription: %w", err)
	}

	logger.Info("streaming output", "pattern", pattern)

	// Close the connection when the context is cancelled to unblock
	// the blocking decoder.Decode below.
	streamDone := make(chan struct{})
	defer close(streamDone)

	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-streamDone:
		}
	}()

	// Read frames until the connection closes or context is cancelled.
	for {
		var frame telemetry.TailFrame
		if err := decoder.Decode(&frame); err != nil {
			if ctx.Err() != nil {
				// Clean shutdown via signal.
				return nil
			}
			if netutil.IsExpectedCloseError(err) {
				return nil
			}
			return fmt.Errorf("reading tail frame: %w", err)
		}

		switch frame.Type {
		case "batch":
			if err := writeBatchOutputDeltas(frame.Batch); err != nil {
				return err
			}

		case "heartbeat":
			// Keepalive — nothing to display.

		default:
			logger.Warn("unknown tail frame type", "type", frame.Type)
		}
	}
}

// writeBatchOutputDeltas decodes a raw CBOR TelemetryBatch and writes
// all OutputDelta data to stdout. Only the Data field of each delta
// is written — metadata (source, sequence, timestamp) is discarded
// since the user wants to see the terminal output stream, not
// structured telemetry.
func writeBatchOutputDeltas(rawBatch codec.RawMessage) error {
	if len(rawBatch) == 0 {
		return nil
	}

	var batch telemetry.TelemetryBatch
	if err := codec.Unmarshal(rawBatch, &batch); err != nil {
		return fmt.Errorf("decoding telemetry batch: %w", err)
	}

	for index := range batch.OutputDeltas {
		if len(batch.OutputDeltas[index].Data) == 0 {
			continue
		}
		if _, err := os.Stdout.Write(batch.OutputDeltas[index].Data); err != nil {
			return fmt.Errorf("writing output: %w", err)
		}
	}

	return nil
}
