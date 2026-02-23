// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
)

// StateCommand returns the "state" subcommand group for reading and writing
// Matrix room state events.
func StateCommand() *cli.Command {
	return &cli.Command{
		Name:    "state",
		Summary: "Get or set room state events",
		Description: `Read and write Matrix room state events. State events represent
persistent room configuration (power levels, topic, Bureau machine
config, etc.).

Each state event is identified by its event type and state key. The
state key defaults to "" (empty string) when not specified.`,
		Subcommands: []*cli.Command{
			stateGetCommand(),
			stateSetCommand(),
		},
	}
}

// stateGetParams holds the parameters for the matrix state get command.
// Room and EventType are positional in CLI mode and named properties in
// JSON/MCP mode.
type stateGetParams struct {
	cli.SessionConfig
	Room      string `json:"room"       desc:"room alias (#...) or room ID (!...)" required:"true"`
	EventType string `json:"event_type" desc:"state event type to fetch (omit for all state)"`
	StateKey  string `json:"state_key"  flag:"key" desc:"state key for the event (default: empty string)"`
}

// stateGetCommand returns the "get" subcommand under "state".
func stateGetCommand() *cli.Command {
	var params stateGetParams

	return &cli.Command{
		Name:    "get",
		Summary: "Get state events from a room",
		Description: `Fetch state events from a Matrix room. With an event type, returns the
content of that specific state event. Without an event type, returns all
state events in the room.

The state key defaults to "" (empty string). Many state events use the
empty state key; Bureau protocol events typically use a principal name
or machine ID as the state key.`,
		Usage: "bureau matrix state get [flags] <room> [<event-type>]",
		Examples: []cli.Example{
			{
				Description: "Get all state events in a room",
				Command:     "bureau matrix state get --credential-file ./creds '!room:bureau.local'",
			},
			{
				Description: "Get a specific state event",
				Command:     "bureau matrix state get --credential-file ./creds '!room:bureau.local' m.room.topic",
			},
			{
				Description: "Get a state event with a non-empty state key",
				Command:     "bureau matrix state get --credential-file ./creds --key '@machine/work:bureau.local' '!room:bureau.local' m.bureau.machine_config",
			},
		},
		Annotations:    cli.ReadOnly(),
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/state/get"},
		Run: func(ctx context.Context, args []string, _ *slog.Logger) error {
			// In CLI mode, room and optional event type come as positional
			// arguments. In JSON/MCP mode, they're populated from JSON input.
			switch len(args) {
			case 0:
				// MCP path: params already populated from JSON.
			case 1:
				params.Room = args[0]
			case 2:
				params.Room = args[0]
				params.EventType = args[1]
			default:
				return cli.Validation("usage: bureau matrix state get [flags] <room> [<event-type>]")
			}
			if params.Room == "" {
				return cli.Validation("room is required\n\nusage: bureau matrix state get [flags] <room> [<event-type>]")
			}

			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			matrixSession, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return cli.Internal("connect: %w", err)
			}

			roomID, err := resolveRoom(ctx, matrixSession, params.Room)
			if err != nil {
				return err
			}

			if params.EventType == "" {
				// No event type: get all state.
				events, err := matrixSession.GetRoomState(ctx, roomID)
				if err != nil {
					return cli.Internal("get room state: %w", err)
				}
				return cli.WriteJSON(events)
			}

			// Specific event type with optional --key.
			content, err := matrixSession.GetStateEvent(ctx, roomID, ref.EventType(params.EventType), params.StateKey)
			if err != nil {
				return cli.Internal("get state event: %w", err)
			}
			return cli.WriteJSON(content)
		},
	}
}

// stateSetParams holds the parameters for the matrix state set command.
// Room, EventType, and Body are positional in CLI mode and named
// properties in JSON/MCP mode.
type stateSetParams struct {
	cli.SessionConfig
	Room      string `json:"room"       desc:"room alias (#...) or room ID (!...)" required:"true"`
	EventType string `json:"event_type" desc:"state event type to set" required:"true"`
	Body      string `json:"body"       desc:"JSON body for the state event content"`
	StateKey  string `json:"state_key"  flag:"key"   desc:"state key for the event (default: empty string)"`
	UseStdin  bool   `json:"use_stdin"  flag:"stdin"  desc:"read JSON body from stdin instead of positional argument"`
	cli.JSONOutput
}

// stateSetResult is the JSON output for matrix state set.
type stateSetResult struct {
	EventID ref.EventID `json:"event_id" desc:"created state event's Matrix ID"`
}

// stateSetCommand returns the "set" subcommand under "state".
func stateSetCommand() *cli.Command {
	var params stateSetParams

	return &cli.Command{
		Name:    "set",
		Summary: "Set a state event in a room",
		Description: `Set a state event in a Matrix room. The JSON body is the content of the
state event. It can be provided as the last positional argument or read
from stdin with --stdin.

The state key defaults to "" (empty string). Use --state-key to set a
specific state key.`,
		Usage: "bureau matrix state set [flags] <room> <event-type> [<json-body>]",
		Examples: []cli.Example{
			{
				Description: "Set a room topic",
				Command:     `bureau matrix state set --credential-file ./creds '!room:bureau.local' m.room.topic '{"topic":"New topic"}'`,
			},
			{
				Description: "Set a Bureau machine config with a state key",
				Command:     `bureau matrix state set --credential-file ./creds --key '@machine/work:bureau.local' '!room:bureau.local' m.bureau.machine_config '{"principals":[]}'`,
			},
			{
				Description: "Set state from stdin",
				Command:     `echo '{"topic":"Piped topic"}' | bureau matrix state set --credential-file ./creds --stdin '!room:bureau.local' m.room.topic`,
			},
		},
		Annotations:    cli.Idempotent(),
		Output:         func() any { return &stateSetResult{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/state/set"},
		Run: func(ctx context.Context, args []string, _ *slog.Logger) error {
			// In CLI mode, room, event type, and optional body come as
			// positional arguments. In JSON/MCP mode, they're populated
			// from JSON input.
			switch {
			case len(args) == 0:
				// MCP path: params already populated from JSON.
			case len(args) >= 2 && len(args) <= 3:
				params.Room = args[0]
				params.EventType = args[1]
				if len(args) == 3 {
					params.Body = args[2]
				}
			default:
				return cli.Validation("usage: bureau matrix state set [flags] <room> <event-type> [<json-body>]")
			}
			if params.Room == "" {
				return cli.Validation("room is required\n\nusage: bureau matrix state set [flags] <room> <event-type> [<json-body>]")
			}
			if params.EventType == "" {
				return cli.Validation("event type is required\n\nusage: bureau matrix state set [flags] <room> <event-type> [<json-body>]")
			}

			var jsonBody string
			if params.UseStdin {
				if params.Body != "" {
					return cli.Validation("--stdin and body argument/field are mutually exclusive")
				}
				data, err := io.ReadAll(os.Stdin)
				if err != nil {
					return cli.Internal("read stdin: %w", err)
				}
				jsonBody = string(data)
			} else {
				if params.Body == "" {
					return cli.Validation("missing JSON body (provide as last argument, body field, or use --stdin)")
				}
				jsonBody = params.Body
			}

			// Validate that the body is valid JSON before sending.
			var content json.RawMessage
			if err := json.Unmarshal([]byte(jsonBody), &content); err != nil {
				return cli.Validation("invalid JSON body: %w", err)
			}

			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			matrixSession, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return cli.Internal("connect: %w", err)
			}

			roomID, err := resolveRoom(ctx, matrixSession, params.Room)
			if err != nil {
				return err
			}

			eventID, err := matrixSession.SendStateEvent(ctx, roomID, ref.EventType(params.EventType), params.StateKey, content)
			if err != nil {
				return cli.Internal("set state event: %w", err)
			}

			if done, err := params.EmitJSON(stateSetResult{EventID: eventID}); done {
				return err
			}

			fmt.Fprintln(os.Stdout, eventID)
			return nil
		},
	}
}
