// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/ticketui"
	"github.com/bureau-foundation/bureau/observe"
	tea "github.com/charmbracelet/bubbletea"
)

// ViewerCommand returns the "viewer" subcommand that launches the
// interactive ticket viewer TUI.
func ViewerCommand() *cli.Command {
	var filePath string
	var serviceMode bool
	var roomFlag string
	var socketPath string

	return &cli.Command{
		Name:    "viewer",
		Summary: "Interactive ticket viewer",
		Description: `Launch an interactive terminal UI for browsing tickets.

By default, loads tickets from .beads/issues.jsonl in the current
directory. Use --file to specify an alternate JSONL path.

With --service, connects to the ticket service via the daemon's
observe socket. The viewer authenticates using your operator session
(from "bureau login"), mints a service token for the ticket service,
and subscribes to a live stream of ticket updates. Use --room to
specify the room directly, or omit it to choose from a list of
available rooms.`,
		Usage: "bureau ticket viewer [flags]",
		Examples: []cli.Example{
			{
				Description: "Open the ticket viewer with default beads file",
				Command:     "bureau ticket viewer",
			},
			{
				Description: "Open with a specific file",
				Command:     "bureau ticket viewer --file path/to/issues.jsonl",
			},
			{
				Description: "Connect to the ticket service",
				Command:     "bureau ticket viewer --service",
			},
			{
				Description: "Connect to a specific room",
				Command:     "bureau ticket viewer --service --room iree/general",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("viewer", pflag.ContinueOnError)
			flagSet.StringVar(&filePath, "file", "", "path to beads JSONL file (default: .beads/issues.jsonl)")
			flagSet.BoolVar(&serviceMode, "service", false, "connect to ticket service via daemon")
			flagSet.StringVar(&roomFlag, "room", "", "room alias or ID (skip room selector when using --service)")
			flagSet.StringVar(&socketPath, "socket", observe.DefaultDaemonSocket, "daemon observe socket path")
			return flagSet
		},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}

			if serviceMode {
				return runServiceViewer(ctx, logger, socketPath, roomFlag)
			}

			if filePath == "" {
				filePath = ".beads/issues.jsonl"
			}

			source, err := ticketui.LoadBeadsFile(filePath)
			if err != nil {
				return cli.Internal("load tickets from %s: %w", filePath, err)
			}

			model := ticketui.NewModel(source)
			program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseAllMotion())
			_, err = program.Run()
			return err
		},
	}
}

// runServiceViewer implements the --service viewer mode. It authenticates
// the operator, mints a service token for the ticket service, resolves
// the room to subscribe to, and runs the TUI backed by a ServiceSource
// with live updates.
func runServiceViewer(ctx context.Context, logger *slog.Logger, daemonSocket string, roomFlag string) error {
	operatorSession, err := cli.LoadSession()
	if err != nil {
		return err
	}

	// Mint a service token for the ticket service via the daemon.
	tokenResponse, err := observe.MintServiceToken(
		daemonSocket,
		"ticket",
		operatorSession.UserID,
		operatorSession.AccessToken,
	)
	if err != nil {
		return cli.Internal("mint service token: %w", err)
	}

	tokenBytes, err := tokenResponse.TokenBytes()
	if err != nil {
		return cli.Internal("decode service token: %w", err)
	}

	ticketSocketPath := tokenResponse.SocketPath

	// Resolve which room to subscribe to. If --room was specified, use
	// it directly. Otherwise, query the service for available rooms and
	// let the user pick.
	roomID, err := resolveViewerRoom(ctx, logger, ticketSocketPath, tokenBytes, roomFlag)
	if err != nil {
		return err
	}

	// Create the service source — starts connecting immediately.
	source := ticketui.NewServiceSource(ticketSocketPath, tokenBytes, roomID, logger)
	defer source.Close()

	// Start background token refresh at 80% of TTL. The refresh
	// goroutine mints a new token before the current one expires and
	// updates the source atomically. The source uses the new token on
	// the next reconnection.
	refreshContext, refreshCancel := context.WithCancel(context.Background())
	defer refreshCancel()
	go refreshServiceToken(refreshContext, source, daemonSocket, operatorSession, tokenResponse.TTLSeconds, logger)

	model := ticketui.NewModel(source)
	model.SetOperatorID(operatorSession.UserID)
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseAllMotion())
	_, err = program.Run()
	return err
}

// viewerRoomInfo mirrors the CBOR response from the list-rooms action.
// Defined here because the server-side type is in the ticket service
// binary; the wire format is the contract.
type viewerRoomInfo struct {
	RoomID string `cbor:"room_id"`
	Alias  string `cbor:"alias,omitempty"`
	Prefix string `cbor:"prefix,omitempty"`
}

// resolveViewerRoom determines the room ID to subscribe to. If roomFlag
// is non-empty, it is used directly (as either a room ID or alias). If
// empty, queries the ticket service for available rooms and presents a
// selection prompt if there is more than one.
func resolveViewerRoom(ctx context.Context, logger *slog.Logger, socketPath string, tokenBytes []byte, roomFlag string) (string, error) {
	if roomFlag != "" {
		return roomFlag, nil
	}

	// Query the ticket service for available rooms.
	client := service.NewServiceClientFromToken(socketPath, tokenBytes)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var rooms []viewerRoomInfo
	if err := client.Call(ctx, "list-rooms", nil, &rooms); err != nil {
		return "", cli.Internal("list rooms: %w", err)
	}

	if len(rooms) == 0 {
		return "", cli.Validation("ticket service has no rooms — enable ticket management in a room first")
	}

	if len(rooms) == 1 {
		room := rooms[0]
		label := room.RoomID
		if room.Alias != "" {
			label = room.Alias
		}
		logger.Info("connecting to room", "room", label)
		return room.RoomID, nil
	}

	// Multiple rooms — prompt the user to choose.
	fmt.Fprintf(os.Stderr, "Available rooms:\n")
	for index, room := range rooms {
		label := room.RoomID
		if room.Alias != "" {
			label = room.Alias
		}
		fmt.Fprintf(os.Stderr, "  %d. %s\n", index+1, label)
	}
	fmt.Fprintf(os.Stderr, "\nSelect room [1-%d]: ", len(rooms))

	var selection int
	if _, err := fmt.Scan(&selection); err != nil {
		return "", cli.Internal("reading room selection: %w", err)
	}
	if selection < 1 || selection > len(rooms) {
		return "", cli.Validation("invalid selection: %d", selection)
	}

	return rooms[selection-1].RoomID, nil
}

// refreshServiceToken periodically mints a new service token before the
// current one expires. Runs until the context is cancelled (viewer exit).
// Mints at 80% of TTL to provide comfortable margin before expiry.
func refreshServiceToken(
	ctx context.Context,
	source *ticketui.ServiceSource,
	daemonSocket string,
	session *cli.OperatorSession,
	ttlSeconds int,
	logger *slog.Logger,
) {
	if ttlSeconds <= 0 {
		return
	}

	// Refresh at 80% of TTL.
	refreshInterval := time.Duration(float64(ttlSeconds)*0.8) * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(refreshInterval):
		}

		tokenResponse, err := observe.MintServiceToken(
			daemonSocket,
			"ticket",
			session.UserID,
			session.AccessToken,
		)
		if err != nil {
			logger.Error("token refresh failed", "error", err)
			continue
		}

		newTokenBytes, err := tokenResponse.TokenBytes()
		if err != nil {
			logger.Error("decode refreshed token", "error", err)
			continue
		}

		source.SetToken(newTokenBytes)
		logger.Info("service token refreshed",
			"ttl_seconds", tokenResponse.TTLSeconds,
			"next_refresh", refreshInterval,
		)
	}
}
