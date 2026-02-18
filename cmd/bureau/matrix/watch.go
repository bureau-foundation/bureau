// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/messaging"
)

// watchParams holds the parameters for the matrix watch command.
type watchParams struct {
	cli.SessionConfig
	Room       string   `json:"room"        desc:"room alias (#...) or room ID (!...)" required:"true"`
	EventTypes []string `json:"event_types" flag:"type"  desc:"filter to specific event types (repeatable; trailing * for prefix match)"`
	ShowState  bool     `json:"show_state"  flag:"state" desc:"include state change events in addition to timeline events"`
	History    int      `json:"history"     flag:"history" desc:"fetch N recent events before starting the live tail" default:"0"`
	cli.JSONOutput
}

// WatchCommand returns the "watch" subcommand for live-tailing room events.
func WatchCommand() *cli.Command {
	var params watchParams

	return &cli.Command{
		Name:    "watch",
		Summary: "Live-tail events in a room",
		Description: `Watch a Matrix room in real time by long-polling /sync. Events are
printed as they arrive — one line per event in human mode, one JSON
object per line (JSONL) in --json mode.

By default, only timeline events (messages, commands, reactions) are
shown. Use --state to also include state change events (membership
changes, config updates, etc.).

Use --type to filter to specific event types. Supports exact match
and trailing * for prefix match (e.g., --type 'm.bureau.*').

Use --history N to show the N most recent events before the live tail
starts. Without --history, the tail begins from the current moment.

Press Ctrl-C to stop.`,
		Usage: "bureau matrix watch [flags] <room>",
		Examples: []cli.Example{
			{
				Description: "Watch all events in a room",
				Command:     "bureau matrix watch --credential-file ./creds '#bureau/machine:bureau.local'",
			},
			{
				Description: "Watch only Bureau events",
				Command:     "bureau matrix watch --credential-file ./creds --type 'm.bureau.*' '#bureau/machine:bureau.local'",
			},
			{
				Description: "Watch with 10 lines of history",
				Command:     "bureau matrix watch --credential-file ./creds --history 10 '!room:bureau.local'",
			},
			{
				Description: "Watch as JSONL for programmatic consumption",
				Command:     "bureau matrix watch --json --credential-file ./creds '#bureau/system:bureau.local'",
			},
		},
		Annotations:    cli.ReadOnly(),
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/watch"},
		Run: func(args []string) error {
			if len(args) == 1 {
				params.Room = args[0]
			} else if len(args) > 1 {
				return cli.Validation("unexpected argument: %s\n\nusage: bureau matrix watch [flags] <room>", args[1])
			}
			if params.Room == "" {
				return cli.Validation("room is required\n\nusage: bureau matrix watch [flags] <room>")
			}

			// Use a signal-aware context for clean Ctrl-C shutdown. The
			// 30-second timeout used by other commands is not appropriate
			// here — watch runs indefinitely until interrupted.
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			session, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return cli.Internal("connect: %w", err)
			}

			roomID, err := resolveRoom(ctx, session, params.Room)
			if err != nil {
				return err
			}

			return runWatchLoop(ctx, session, roomID, &params)
		},
	}
}

// syncLongPollTimeout is the long-poll timeout in milliseconds sent to the
// homeserver. The server will hold the connection open for this duration
// waiting for new events before returning an empty response.
const syncLongPollTimeout = 30000

// runWatchLoop is the core sync loop for the watch command.
func runWatchLoop(ctx context.Context, session messaging.Session, roomID string, params *watchParams) error {
	encoder := json.NewEncoder(os.Stdout)

	// Optionally print recent history before starting the live tail.
	if params.History > 0 {
		if err := printWatchHistory(ctx, session, roomID, params, encoder); err != nil {
			return err
		}
	}

	// Initial sync with timeout=0 to get the current position without
	// fetching historical state. We only need the next_batch token.
	initialResponse, err := session.Sync(ctx, messaging.SyncOptions{
		SetTimeout: true,
		Timeout:    0,
	})
	if err != nil {
		// Context cancellation during initial sync is not an error.
		if ctx.Err() != nil {
			return nil
		}
		return cli.Internal("initial sync: %w", err)
	}
	nextBatch := initialResponse.NextBatch

	// Long-poll loop. Each iteration blocks for up to 30 seconds waiting
	// for new events, then prints any that match the filters.
	for {
		if ctx.Err() != nil {
			return nil
		}

		response, err := session.Sync(ctx, messaging.SyncOptions{
			Since:      nextBatch,
			Timeout:    syncLongPollTimeout,
			SetTimeout: true,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return cli.Internal("sync: %w", err)
		}
		nextBatch = response.NextBatch

		joinedRoom, ok := response.Rooms.Join[roomID]
		if !ok {
			continue
		}

		// Timeline events (messages, commands, etc.).
		for _, event := range joinedRoom.Timeline.Events {
			if matchesTypeFilter(event.Type, params.EventTypes) {
				emitWatchEvent(event, params, encoder)
			}
		}

		// State events (only if --state is set).
		if params.ShowState {
			for _, event := range joinedRoom.State.Events {
				if matchesTypeFilter(event.Type, params.EventTypes) {
					emitWatchEvent(event, params, encoder)
				}
			}
		}
	}
}

// printWatchHistory fetches and prints the N most recent events from the room
// timeline before starting the live tail.
func printWatchHistory(ctx context.Context, session messaging.Session, roomID string, params *watchParams, encoder *json.Encoder) error {
	response, err := session.RoomMessages(ctx, roomID, messaging.RoomMessagesOptions{
		Direction: "b",
		Limit:     params.History,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return cli.Internal("fetch history: %w", err)
	}

	// RoomMessages with direction "b" returns newest first. Reverse to
	// print in chronological order.
	events := response.Chunk
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}

	for _, event := range events {
		if matchesTypeFilter(event.Type, params.EventTypes) {
			emitWatchEvent(event, params, encoder)
		}
	}

	return nil
}

// emitWatchEvent prints a single event in the appropriate format.
func emitWatchEvent(event messaging.Event, params *watchParams, encoder *json.Encoder) {
	if params.OutputJSON {
		encoder.Encode(event)
	} else {
		fmt.Fprintln(os.Stdout, formatEventOneLine(event))
	}
}
