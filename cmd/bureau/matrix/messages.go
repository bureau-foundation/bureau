// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

// messagesParams holds the parameters for the matrix messages command.
type messagesParams struct {
	cli.SessionConfig
	Room      string `json:"room"      desc:"room alias (#...) or room ID (!...)" required:"true"`
	Limit     int    `json:"limit"     flag:"limit"     desc:"maximum number of events to return" default:"50"`
	From      string `json:"from"      flag:"from"      desc:"pagination token from a previous response's end field"`
	Direction string `json:"direction" flag:"direction"  desc:"pagination direction: backward (newest first) or forward (oldest first)" default:"backward"`
	Thread    string `json:"thread"    flag:"thread"    desc:"thread root event ID — fetch only messages in this thread"`
	cli.JSONOutput
}

// messagesResult is the JSON output for the messages command.
type messagesResult struct {
	Start  string            `json:"start,omitempty" desc:"pagination token for the start of the returned window"`
	End    string            `json:"end,omitempty"   desc:"pagination token for the end of the window (pass as --from for the next page)"`
	Events []messaging.Event `json:"events"         desc:"message events in the requested order"`
}

// MessagesCommand returns the "messages" subcommand for fetching room message
// history.
func MessagesCommand() *cli.Command {
	var params messagesParams

	return &cli.Command{
		Name:    "messages",
		Summary: "Fetch message history from a room",
		Description: `Fetch messages from a Matrix room timeline with pagination support.
By default, returns the 50 most recent events (newest first).

Use --from with a pagination token from a previous response to fetch
the next page. Use --direction to control whether pagination moves
backward (toward older events) or forward (toward newer events).

Use --thread to fetch messages from a specific thread instead of the
main room timeline.`,
		Usage: "bureau matrix messages [flags] <room>",
		Examples: []cli.Example{
			{
				Description: "Fetch the 20 most recent messages",
				Command:     "bureau matrix messages --credential-file ./creds --limit 20 '#bureau/system:bureau.local'",
			},
			{
				Description: "Fetch the next page using a pagination token",
				Command:     "bureau matrix messages --credential-file ./creds --from s1234_5678 '#bureau/system:bureau.local'",
			},
			{
				Description: "Fetch thread messages",
				Command:     "bureau matrix messages --credential-file ./creds --thread '$event_id' '!room:bureau.local'",
			},
			{
				Description: "Fetch as JSON for programmatic use",
				Command:     "bureau matrix messages --json --credential-file ./creds '!room:bureau.local'",
			},
		},
		Annotations:    cli.ReadOnly(),
		Output:         func() any { return &messagesResult{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/messages"},
		Run: func(ctx context.Context, args []string, _ *slog.Logger) error {
			if len(args) == 1 {
				params.Room = args[0]
			} else if len(args) > 1 {
				return cli.Validation("unexpected argument: %s\n\nusage: bureau matrix messages [flags] <room>", args[1])
			}
			if params.Room == "" {
				return cli.Validation("room is required\n\nusage: bureau matrix messages [flags] <room>")
			}

			direction, err := parseDirection(params.Direction)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			session, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return cli.Internal("connect: %w", err)
			}

			roomID, err := resolveRoom(ctx, session, params.Room)
			if err != nil {
				return err
			}

			var result messagesResult

			if params.Thread != "" {
				threadRootID, err := ref.ParseEventID(params.Thread)
				if err != nil {
					return cli.Validation("invalid thread event ID %q: %w", params.Thread, err)
				}
				response, err := session.ThreadMessages(ctx, roomID, threadRootID, messaging.ThreadMessagesOptions{
					From:  params.From,
					Limit: params.Limit,
				})
				if err != nil {
					return cli.Internal("fetch thread messages: %w", err)
				}
				result.Events = response.Chunk
				result.End = response.NextBatch
			} else {
				response, err := session.RoomMessages(ctx, roomID, messaging.RoomMessagesOptions{
					From:      params.From,
					Direction: direction,
					Limit:     params.Limit,
				})
				if err != nil {
					return cli.Internal("fetch room messages: %w", err)
				}
				result.Start = response.Start
				result.End = response.End
				result.Events = response.Chunk
			}

			if result.Events == nil {
				result.Events = []messaging.Event{}
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			printMessagesHuman(&result, params.Limit)
			return nil
		},
	}
}

// parseDirection converts a human-readable direction to the Matrix API value.
func parseDirection(direction string) (string, error) {
	switch direction {
	case "backward", "b":
		return "b", nil
	case "forward", "f":
		return "f", nil
	default:
		return "", cli.Validation("invalid direction %q: must be \"backward\" or \"forward\"", direction)
	}
}

// printMessagesHuman prints messages in a human-readable tabular format.
func printMessagesHuman(result *messagesResult, requestedLimit int) {
	if len(result.Events) == 0 {
		fmt.Fprintln(os.Stdout, "No messages found.")
		return
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "TIMESTAMP\tSENDER\tTYPE\tCONTENT")
	for _, event := range result.Events {
		summary := eventSummary(event)
		if len(summary) > 80 {
			summary = summary[:77] + "..."
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n",
			formatTimestamp(event.OriginServerTS),
			event.Sender,
			event.Type,
			summary,
		)
	}
	writer.Flush()

	// Pagination hint.
	if result.End != "" {
		fmt.Fprintf(os.Stdout, "\nPagination: --from %s (%d events returned)\n", result.End, len(result.Events))
	}
}
