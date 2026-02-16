// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
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
type stateGetParams struct {
	cli.SessionConfig
	StateKey string `json:"state_key" flag:"key" desc:"state key for the event (default: empty string)"`
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
		Run: func(args []string) error {
			if len(args) < 1 {
				return fmt.Errorf("usage: bureau matrix state get [flags] <room> [<event-type>]")
			}
			if len(args) > 2 {
				return fmt.Errorf("unexpected argument: %s", args[2])
			}

			roomTarget := args[0]

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			matrixSession, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return fmt.Errorf("connect: %w", err)
			}

			roomID, err := resolveRoom(ctx, matrixSession, roomTarget)
			if err != nil {
				return err
			}

			if len(args) == 1 {
				// No event type: get all state.
				events, err := matrixSession.GetRoomState(ctx, roomID)
				if err != nil {
					return fmt.Errorf("get room state: %w", err)
				}
				return cli.WriteJSON(events)
			}

			// Specific event type with optional --key.
			eventType := args[1]

			content, err := matrixSession.GetStateEvent(ctx, roomID, eventType, params.StateKey)
			if err != nil {
				return fmt.Errorf("get state event: %w", err)
			}
			return cli.WriteJSON(content)
		},
	}
}

// stateSetParams holds the parameters for the matrix state set command.
type stateSetParams struct {
	cli.SessionConfig
	StateKey string `json:"state_key" flag:"key"   desc:"state key for the event (default: empty string)"`
	UseStdin bool   `json:"use_stdin" flag:"stdin"  desc:"read JSON body from stdin instead of positional argument"`
	cli.JSONOutput
}

// stateSetResult is the JSON output for matrix state set.
type stateSetResult struct {
	EventID string `json:"event_id" desc:"created state event's Matrix ID"`
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
		Run: func(args []string) error {
			if len(args) < 2 {
				return fmt.Errorf("usage: bureau matrix state set [flags] <room> <event-type> [<json-body>]")
			}

			roomTarget := args[0]
			eventType := args[1]

			var jsonBody string
			if params.UseStdin {
				if len(args) > 2 {
					return fmt.Errorf("unexpected argument %q: --stdin reads body from stdin, not positional args", args[2])
				}
				data, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				jsonBody = string(data)
			} else {
				if len(args) < 3 {
					return fmt.Errorf("missing JSON body (provide as last argument or use --stdin)")
				}
				if len(args) > 3 {
					return fmt.Errorf("unexpected argument: %s", args[3])
				}
				jsonBody = args[2]
			}

			// Validate that the body is valid JSON before sending.
			var content json.RawMessage
			if err := json.Unmarshal([]byte(jsonBody), &content); err != nil {
				return fmt.Errorf("invalid JSON body: %w", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			matrixSession, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return fmt.Errorf("connect: %w", err)
			}

			roomID, err := resolveRoom(ctx, matrixSession, roomTarget)
			if err != nil {
				return err
			}

			eventID, err := matrixSession.SendStateEvent(ctx, roomID, eventType, params.StateKey, content)
			if err != nil {
				return fmt.Errorf("set state event: %w", err)
			}

			if done, err := params.EmitJSON(stateSetResult{EventID: eventID}); done {
				return err
			}

			fmt.Fprintln(os.Stdout, eventID)
			return nil
		},
	}
}
