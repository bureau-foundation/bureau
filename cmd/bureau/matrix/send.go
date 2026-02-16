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
	"github.com/bureau-foundation/bureau/messaging"
)

// sendParams holds the parameters for the matrix send command.
type sendParams struct {
	cli.SessionConfig
	ThreadID  string `json:"thread_id"  flag:"thread"     desc:"event ID of thread root to reply within"`
	EventType string `json:"event_type" flag:"event-type" desc:"custom event type (default: m.room.message)"`
	cli.JSONOutput
}

// sendResult is the JSON output for matrix send.
type sendResult struct {
	EventID string `json:"event_id" desc:"sent event's Matrix ID"`
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
			if len(args) < 2 {
				return fmt.Errorf("usage: bureau matrix send [flags] <room> <message>")
			}
			if len(args) > 2 {
				return fmt.Errorf("unexpected argument: %s (room and message should each be a single argument)", args[2])
			}

			roomTarget := args[0]
			messageBody := args[1]

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

			var eventID string
			if params.EventType != "" {
				// Custom event type: send raw content. The message body is
				// expected to be JSON, but we send it as a text message
				// content structure for consistency.
				eventID, err = matrixSession.SendEvent(ctx, roomID, params.EventType, messaging.NewTextMessage(messageBody))
			} else if params.ThreadID != "" {
				eventID, err = matrixSession.SendMessage(ctx, roomID, messaging.NewThreadReply(params.ThreadID, messageBody))
			} else {
				eventID, err = matrixSession.SendMessage(ctx, roomID, messaging.NewTextMessage(messageBody))
			}
			if err != nil {
				return fmt.Errorf("send message: %w", err)
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
// returned as-is (assumed to be a room ID starting with !).
func resolveRoom(ctx context.Context, session *messaging.Session, target string) (string, error) {
	if strings.HasPrefix(target, "#") {
		roomID, err := session.ResolveAlias(ctx, target)
		if err != nil {
			return "", fmt.Errorf("resolve alias %q: %w", target, err)
		}
		return roomID, nil
	}
	if !strings.HasPrefix(target, "!") {
		return "", fmt.Errorf("room must be an alias (#...) or room ID (!...): got %q", target)
	}
	return target, nil
}
