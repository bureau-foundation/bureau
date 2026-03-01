// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// bureau-viewer is a standalone TUI for browsing and managing Bureau
// tickets. Designed as a Bureau CLI plugin: `bureau viewer` dispatches
// to this binary via PATH lookup when the plugin architecture is active.
//
// Two modes of operation:
//
// File mode (default): loads tickets from a JSONL file (typically
// .beads/issues.jsonl) and watches it for changes via inotify. No
// Bureau infrastructure required — works offline with local beads.
//
// Service mode (--service): connects to the ticket service via the
// Bureau daemon's observe socket. Authenticates using the operator's
// saved session from "bureau login", mints a service token, and
// subscribes to a live stream of ticket updates with full read-write
// capability.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	ticketcmd "github.com/bureau-foundation/bureau/cmd/bureau/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/ticketui"
	"github.com/bureau-foundation/bureau/lib/version"
	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	if err := run(); err != nil {
		if coder, ok := err.(interface{ ExitCode() int }); ok {
			os.Exit(coder.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var connection ticketcmd.TicketConnection
	var filePath string
	var roomFlag string
	var logOutput string

	flagSet := pflag.NewFlagSet("bureau-viewer", pflag.ContinueOnError)
	flagSet.StringVar(&filePath, "file", "", "path to beads JSONL file (default: .beads/issues.jsonl)")
	connection.AddFlags(flagSet)
	flagSet.StringVar(&roomFlag, "room", "", "room alias or ID (skip room selector when using --service)")
	flagSet.StringVar(&logOutput, "log-output", "", "write JSON log records to this file (in addition to TUI display)")
	flagSet.BoolP("help", "h", false, "show help")

	// Handle --version before flag parsing to match other Bureau binaries.
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		version.Print("bureau-viewer")
		return nil
	}

	if err := flagSet.Parse(os.Args[1:]); err != nil {
		if err == pflag.ErrHelp {
			printHelp(flagSet)
			return nil
		}
		return err
	}

	if help, _ := flagSet.GetBool("help"); help {
		printHelp(flagSet)
		return nil
	}

	args := flagSet.Args()
	if len(args) > 0 {
		return cli.Validation("unexpected argument: %s", args[0])
	}

	if connection.ServiceMode {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
		return runServiceViewer(ctx, logger, &connection, roomFlag, logOutput)
	}

	if filePath == "" {
		filePath = ".beads/issues.jsonl"
	}

	source, cleanup, err := ticketui.WatchBeadsFile(filePath)
	if err != nil {
		return cli.Validation("cannot load tickets from %s: %w", filePath, err).
			WithHint("Check that the file exists and contains valid JSONL. Use --service to connect to the ticket service instead.")
	}
	defer cleanup()

	model := ticketui.NewModel(source)
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseAllMotion())
	_, err = program.Run()
	return err
}

func printHelp(flagSet *pflag.FlagSet) {
	fmt.Fprintf(os.Stderr, `Bureau ticket viewer — interactive terminal UI for browsing tickets.

By default, loads tickets from .beads/issues.jsonl in the current
directory. Use --file to specify an alternate JSONL path.

With --service, connects to the ticket service via the daemon's
observe socket. The viewer authenticates using your operator session
(from "bureau login"), mints a service token for the ticket service,
and subscribes to a live stream of ticket updates.

Usage:
  bureau-viewer [flags]

Examples:
  # Open the viewer with default beads file
  bureau viewer

  # Open with a specific file
  bureau viewer --file path/to/issues.jsonl

  # Connect to the ticket service
  bureau viewer --service

  # Connect to a specific room via non-default daemon socket
  bureau viewer --service --daemon-socket /tmp/bureau-dev/run/observe.sock --room iree/general

Flags:
`)
	flagSet.SetOutput(os.Stderr)
	flagSet.PrintDefaults()
}

// runServiceViewer implements the --service viewer mode. It mints a
// service token via the shared TicketConnection, resolves the room to
// subscribe to, and runs the TUI backed by a ServiceSource with live
// updates and automatic token refresh.
//
// Background logging (from the service source and token refresh) is
// routed through a TUILogHandler that displays warnings and errors in
// the status bar instead of writing to stderr (which would corrupt
// the alt-screen display). An optional file logger captures all
// records to a JSONL file for post-mortem debugging.
func runServiceViewer(ctx context.Context, logger *slog.Logger, connection *ticketcmd.TicketConnection, roomFlag string, logOutput string) error {
	mintResult, err := connection.MintServiceToken()
	if err != nil {
		return err
	}

	operatorSession, err := cli.LoadSession()
	if err != nil {
		return err
	}

	roomID, err := resolveViewerRoom(ctx, logger, mintResult.SocketPath, mintResult.TokenBytes, roomFlag)
	if err != nil {
		return err
	}

	tuiHandler := ticketui.NewTUILogHandler(slog.LevelWarn)

	var backgroundLogger *slog.Logger
	if logOutput != "" {
		fileHandler, fileCloser, fileErr := openFileLogHandler(logOutput)
		if fileErr != nil {
			return cli.Validation("cannot open log file %s: %w", logOutput, fileErr)
		}
		defer fileCloser()
		backgroundLogger = slog.New(fanoutHandler{tuiHandler, fileHandler})
	} else {
		backgroundLogger = slog.New(tuiHandler)
	}

	source := ticketui.NewServiceSource(mintResult.SocketPath, mintResult.TokenBytes, roomID, backgroundLogger)
	defer source.Close()

	refreshContext, refreshCancel := context.WithCancel(context.Background())
	defer refreshCancel()
	go refreshServiceToken(refreshContext, source, connection, mintResult.TTLSeconds, backgroundLogger)

	model := ticketui.NewModel(source)
	model.SetOperatorID(operatorSession.UserID)
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseAllMotion())

	tuiHandler.SetProgram(program)

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

	client := service.NewServiceClientFromToken(socketPath, tokenBytes)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var rooms []viewerRoomInfo
	if err := client.Call(ctx, "list-rooms", nil, &rooms); err != nil {
		return "", err
	}

	if len(rooms) == 0 {
		return "", cli.NotFound("ticket service has no rooms configured").
			WithHint("Run 'bureau ticket enable --space <space> --host <machine>' to enable ticket management.")
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
		return "", cli.Validation("failed to read room selection: %w", err)
	}
	if selection < 1 || selection > len(rooms) {
		return "", cli.Validation("invalid selection %d: must be between 1 and %d", selection, len(rooms))
	}

	return rooms[selection-1].RoomID, nil
}

// refreshServiceToken periodically mints a new service token before the
// current one expires. Runs until the context is cancelled (viewer exit).
// Mints at 80% of TTL to provide comfortable margin before expiry.
func refreshServiceToken(
	ctx context.Context,
	source *ticketui.ServiceSource,
	connection *ticketcmd.TicketConnection,
	ttlSeconds int,
	logger *slog.Logger,
) {
	if ttlSeconds <= 0 {
		return
	}

	refreshInterval := time.Duration(float64(ttlSeconds)*0.8) * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(refreshInterval):
		}

		mintResult, err := connection.MintServiceToken()
		if err != nil {
			logger.Error("token refresh failed", "error", err)
			continue
		}

		source.SetToken(mintResult.TokenBytes)
		logger.Info("service token refreshed",
			"ttl_seconds", mintResult.TTLSeconds,
			"next_refresh", refreshInterval,
		)
	}
}

// openFileLogHandler creates a slog.JSONHandler that writes to the
// given file path. Returns the handler, a cleanup function to close
// the file, and any error. The file is created or truncated.
func openFileLogHandler(path string) (slog.Handler, func(), error) {
	file, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	handler := slog.NewJSONHandler(file, &slog.HandlerOptions{Level: slog.LevelDebug})
	return handler, func() { file.Close() }, nil
}

// fanoutHandler is a slog.Handler that sends each record to multiple
// underlying handlers. A record is enabled if any sub-handler is
// enabled for that level.
type fanoutHandler []slog.Handler

func (handlers fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (handlers fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	for _, handler := range handlers {
		if handler.Enabled(ctx, record.Level) {
			if err := handler.Handle(ctx, record); err != nil {
				return err
			}
		}
	}
	return nil
}

func (handlers fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	derived := make(fanoutHandler, len(handlers))
	for index, handler := range handlers {
		derived[index] = handler.WithAttrs(attrs)
	}
	return derived
}

func (handlers fanoutHandler) WithGroup(name string) slog.Handler {
	derived := make(fanoutHandler, len(handlers))
	for index, handler := range handlers {
		derived[index] = handler.WithGroup(name)
	}
	return derived
}
