// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

// sendParams holds the parameters for the matrix send command. Room and
// Message are positional in CLI mode (args[0], args[1]) and named
// properties in JSON/MCP mode.
type sendParams struct {
	cli.SessionConfig
	Room      string `json:"room"       desc:"room alias (#...) or room ID (!...)" required:"true"`
	Message   string `json:"message"    desc:"message body to send" required:"true"`
	ThreadID  string `json:"thread_id"  flag:"thread"     desc:"event ID of thread root to reply within"`
	EventType string `json:"event_type" flag:"event-type" desc:"custom event type (default: m.room.message)"`
	cli.JSONOutput
}

// sendResult is the JSON output for matrix send.
type sendResult struct {
	EventID ref.EventID `json:"event_id" desc:"sent event's Matrix ID"`
}

// SendCommand returns the "send" subcommand for sending messages to Matrix rooms.
func SendCommand() *cli.Command {
	var params sendParams

	return &cli.Command{
		Name:    "send",
		Summary: "Send a message to a Matrix room",
		Description: `Send a message to a Matrix room. The room can be specified as a room
alias (#bureau/system:bureau.local) or a room ID (!abc:bureau.local).
Aliases are resolved automatically.

By default, sends a plain text m.room.message. Use --thread to send a
reply within an existing thread. Use --event-type to send a custom
event type instead of m.room.message (for advanced use cases like
Bureau protocol events).`,
		Usage: "bureau matrix send [flags] <room> <message>",
		Examples: []cli.Example{
			{
				Description: "Send a plain text message",
				Command:     "bureau matrix send --credential-file ./creds '#bureau/system:bureau.local' 'Hello, world!'",
			},
			{
				Description: "Reply within a thread",
				Command:     "bureau matrix send --credential-file ./creds --thread '$event_id' '!room:bureau.local' 'Thread reply'",
			},
			{
				Description: "Send a custom event type",
				Command:     "bureau matrix send --credential-file ./creds --event-type m.bureau.status '!room:bureau.local' '{\"status\":\"active\"}'",
			},
		},
		Annotations:    cli.Create(),
		Output:         func() any { return &sendResult{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/send"},
		Run: func(args []string) error {
			// In CLI mode, room and message come as positional arguments.
			// In JSON/MCP mode, they're populated from the JSON input.
			switch len(args) {
			case 0:
				// MCP path: params already populated from JSON.
			case 2:
				params.Room = args[0]
				params.Message = args[1]
			default:
				return cli.Validation("usage: bureau matrix send [flags] <room> <message>")
			}
			if params.Room == "" {
				return cli.Validation("room is required\n\nusage: bureau matrix send [flags] <room> <message>")
			}
			if params.Message == "" {
				return cli.Validation("message is required\n\nusage: bureau matrix send [flags] <room> <message>")
			}

			roomTarget := params.Room
			messageBody := params.Message

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			matrixSession, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return cli.Internal("connect: %w", err)
			}

			roomID, err := resolveRoom(ctx, matrixSession, roomTarget)
			if err != nil {
				return err
			}

			var eventID ref.EventID
			if params.EventType != "" {
				// Custom event type: send raw content. The message body is
				// expected to be JSON, but we send it as a text message
				// content structure for consistency.
				eventID, err = matrixSession.SendEvent(ctx, roomID, ref.EventType(params.EventType), messaging.NewTextMessage(messageBody))
			} else if params.ThreadID != "" {
				threadRootID, parseErr := ref.ParseEventID(params.ThreadID)
				if parseErr != nil {
					return cli.Validation("invalid thread event ID %q: %w", params.ThreadID, parseErr)
				}
				eventID, err = matrixSession.SendMessage(ctx, roomID, messaging.NewThreadReply(threadRootID, messageBody))
			} else {
				eventID, err = matrixSession.SendMessage(ctx, roomID, messaging.NewTextMessage(messageBody))
			}
			if err != nil {
				return cli.Internal("send message: %w", err)
			}

			if done, err := params.EmitJSON(sendResult{EventID: eventID}); done {
				return err
			}

			fmt.Fprintln(os.Stdout, eventID)
			return nil
		},
	}
}

// resolveRoom resolves a room target to a room ID. If the target looks like an
// alias (starts with #), it is resolved via the homeserver. Otherwise it is
// parsed as a room ID (must start with !).
func resolveRoom(ctx context.Context, session messaging.Session, target string) (ref.RoomID, error) {
	if strings.HasPrefix(target, "#") {
		alias, err := ref.ParseRoomAlias(target)
		if err != nil {
			return ref.RoomID{}, cli.Validation("invalid room alias %q: %w", target, err)
		}
		roomID, err := session.ResolveAlias(ctx, alias)
		if err != nil {
			return ref.RoomID{}, cli.NotFound("resolve alias %q: %w", target, err)
		}
		return roomID, nil
	}
	if !strings.HasPrefix(target, "!") {
		return ref.RoomID{}, cli.Validation("room must be an alias (#...) or room ID (!...): got %q", target)
	}
	roomID, err := ref.ParseRoomID(target)
	if err != nil {
		return ref.RoomID{}, cli.Validation("invalid room ID %q: %w", target, err)
	}
	return roomID, nil
}
