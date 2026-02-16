// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// --- create ---

type createParams struct {
	TicketConnection
	cli.JSONOutput
	Room      string   `json:"room"       flag:"room,r"      desc:"room ID" required:"true"`
	Title     string   `json:"title"      flag:"title"       desc:"ticket title" required:"true"`
	Body      string   `json:"body"       flag:"body"        desc:"full description (markdown)"`
	Type      string   `json:"type"       flag:"type,t"      desc:"ticket type (task, bug, feature, epic, chore, docs, question)" required:"true"`
	Priority  int      `json:"priority"   flag:"priority,p"  desc:"priority (0=critical, 1=high, 2=medium, 3=low, 4=backlog)" default:"2"`
	Labels    []string `json:"labels"     flag:"label,l"     desc:"labels (repeatable)"`
	Parent    string   `json:"parent"     flag:"parent"      desc:"parent ticket ID"`
	BlockedBy []string `json:"blocked_by" flag:"blocked-by"  desc:"blocking ticket IDs (repeatable)"`
}

func createCommand() *cli.Command {
	var params createParams

	return &cli.Command{
		Name:    "create",
		Summary: "Create a new ticket",
		Description: `Create a new ticket in a room. The ticket starts with status "open"
and the ID is auto-generated from the room, timestamp, and title.

Required fields: --room, --title, --type. Priority defaults to P2
(medium) if not specified.`,
		Usage: "bureau ticket create --room ROOM --title TITLE --type TYPE [flags]",
		Examples: []cli.Example{
			{
				Description: "Create a bug ticket",
				Command:     "bureau ticket create --room '!abc:bureau.local' --title 'Memory leak in proxy' --type bug --priority 1",
			},
			{
				Description: "Create a task with labels",
				Command:     "bureau ticket create --room '!abc:bureau.local' --title 'Add auth middleware' --type task --label auth --label security",
			},
			{
				Description: "Create a subtask of an epic",
				Command:     "bureau ticket create --room '!abc:bureau.local' --title 'Implement login flow' --type task --parent tkt-epic1",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &createResult{} },
		RequiredGrants: []string{"command/ticket/create"},
		Run: func(args []string) error {
			if params.Room == "" {
				return fmt.Errorf("--room is required")
			}
			if params.Title == "" {
				return fmt.Errorf("--title is required")
			}
			if params.Type == "" {
				return fmt.Errorf("--type is required")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			fields := map[string]any{
				"room":     params.Room,
				"title":    params.Title,
				"type":     params.Type,
				"priority": params.Priority,
			}
			if params.Body != "" {
				fields["body"] = params.Body
			}
			if len(params.Labels) > 0 {
				fields["labels"] = params.Labels
			}
			if params.Parent != "" {
				fields["parent"] = params.Parent
			}
			if len(params.BlockedBy) > 0 {
				fields["blocked_by"] = params.BlockedBy
			}

			var result createResult
			if err := client.Call(ctx, "create", fields, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			fmt.Println(result.ID)
			return nil
		},
	}
}

// --- update ---

type updateParams struct {
	TicketConnection
	cli.JSONOutput
	Room     string   `json:"room"     flag:"room,r"     desc:"room ID or alias localpart (or use room-qualified ticket ref)"`
	Ticket   string   `json:"ticket"   desc:"ticket ID" required:"true"`
	Title    string   `json:"title"    flag:"title"      desc:"new title"`
	Body     string   `json:"body"     flag:"body"       desc:"new body"`
	Status   string   `json:"status"   flag:"status,s"   desc:"new status (open, in_progress, blocked, closed)"`
	Priority int      `json:"priority" flag:"priority,p"  desc:"new priority (0-4, -1 to skip)" default:"-1"`
	Type     string   `json:"type"     flag:"type,t"     desc:"new type"`
	Labels   []string `json:"labels"   flag:"label,l"    desc:"replace labels (repeatable)"`
	Assignee string   `json:"assignee" flag:"assignee"   desc:"assign to Matrix user ID (requires in_progress status)"`
	Parent   string   `json:"parent"   flag:"parent"     desc:"set parent ticket ID"`
}

func updateCommand() *cli.Command {
	var params updateParams

	return &cli.Command{
		Name:    "update",
		Summary: "Update a ticket",
		Description: `Modify fields on an existing ticket. Only specified fields are changed;
omitted fields are left as-is.

Status transitions are validated: for example, transitioning to
"in_progress" requires --assignee. Attempting to claim a ticket that
is already in_progress returns a contention error with the current
assignee.`,
		Usage: "bureau ticket update <ticket-id> [flags]",
		Examples: []cli.Example{
			{
				Description: "Claim a ticket (must be open)",
				Command:     "bureau ticket update tkt-a3f9 --status in_progress --assignee '@me:bureau.local'",
			},
			{
				Description: "Change priority",
				Command:     "bureau ticket update tkt-a3f9 --priority 0",
			},
			{
				Description: "Add labels",
				Command:     "bureau ticket update tkt-a3f9 --label auth --label urgent",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &mutationResult{} },
		RequiredGrants: []string{"command/ticket/update"},
		Run: func(args []string) error {
			if len(args) == 1 {
				params.Ticket = args[0]
			} else if len(args) > 1 {
				return fmt.Errorf("expected 1 positional argument, got %d", len(args))
			}
			if params.Ticket == "" {
				return fmt.Errorf("ticket ID is required\n\nUsage: bureau ticket update <ticket-id> [flags]")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			// Build the request with pointer semantics: only include
			// fields that were explicitly set. The service interprets
			// nil pointers as "not provided."
			fields := map[string]any{"ticket": params.Ticket}
			if params.Room != "" {
				fields["room"] = params.Room
			}
			if params.Title != "" {
				fields["title"] = params.Title
			}
			if params.Body != "" {
				fields["body"] = params.Body
			}
			if params.Status != "" {
				fields["status"] = params.Status
			}
			if params.Priority >= 0 {
				fields["priority"] = params.Priority
			}
			if params.Type != "" {
				fields["type"] = params.Type
			}
			if params.Labels != nil {
				fields["labels"] = params.Labels
			}
			if params.Assignee != "" {
				fields["assignee"] = params.Assignee
			}
			if params.Parent != "" {
				fields["parent"] = params.Parent
			}

			var result mutationResult
			if err := client.Call(ctx, "update", fields, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			fmt.Fprintf(os.Stderr, "%s updated (status=%s)\n", result.ID, result.Content.Status)
			return nil
		},
	}
}

// --- close ---

type closeParams struct {
	TicketConnection
	cli.JSONOutput
	Room   string `json:"room"   flag:"room,r" desc:"room ID or alias localpart (or use room-qualified ticket ref)"`
	Ticket string `json:"ticket" desc:"ticket ID" required:"true"`
	Reason string `json:"reason" flag:"reason" desc:"close reason"`
}

func closeCommand() *cli.Command {
	var params closeParams

	return &cli.Command{
		Name:    "close",
		Summary: "Close a ticket",
		Description: `Transition a ticket to "closed" with an optional reason. If the
ticket was in_progress, the assignee is auto-cleared.`,
		Usage: "bureau ticket close <ticket-id> [flags]",
		Examples: []cli.Example{
			{
				Description: "Close a ticket",
				Command:     "bureau ticket close tkt-a3f9",
			},
			{
				Description: "Close with a reason",
				Command:     "bureau ticket close tkt-a3f9 --reason 'Fixed in commit abc123'",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &mutationResult{} },
		RequiredGrants: []string{"command/ticket/close"},
		Run: func(args []string) error {
			if len(args) == 1 {
				params.Ticket = args[0]
			} else if len(args) > 1 {
				return fmt.Errorf("expected 1 positional argument, got %d", len(args))
			}
			if params.Ticket == "" {
				return fmt.Errorf("ticket ID is required\n\nUsage: bureau ticket close <ticket-id>")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			fields := map[string]any{"ticket": params.Ticket}
			if params.Room != "" {
				fields["room"] = params.Room
			}
			if params.Reason != "" {
				fields["reason"] = params.Reason
			}

			var result mutationResult
			if err := client.Call(ctx, "close", fields, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			fmt.Fprintf(os.Stderr, "%s closed\n", result.ID)
			return nil
		},
	}
}

// --- reopen ---

type reopenParams struct {
	TicketConnection
	cli.JSONOutput
	Room   string `json:"room"   flag:"room,r" desc:"room ID or alias localpart (or use room-qualified ticket ref)"`
	Ticket string `json:"ticket" desc:"ticket ID" required:"true"`
}

func reopenCommand() *cli.Command {
	var params reopenParams

	return &cli.Command{
		Name:    "reopen",
		Summary: "Reopen a closed ticket",
		Description: `Transition a ticket from "closed" back to "open", clearing the
close timestamp and reason.`,
		Usage:          "bureau ticket reopen <ticket-id> [flags]",
		Params:         func() any { return &params },
		Output:         func() any { return &mutationResult{} },
		RequiredGrants: []string{"command/ticket/reopen"},
		Run: func(args []string) error {
			if len(args) == 1 {
				params.Ticket = args[0]
			} else if len(args) > 1 {
				return fmt.Errorf("expected 1 positional argument, got %d", len(args))
			}
			if params.Ticket == "" {
				return fmt.Errorf("ticket ID is required\n\nUsage: bureau ticket reopen <ticket-id>")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			fields := map[string]any{"ticket": params.Ticket}
			if params.Room != "" {
				fields["room"] = params.Room
			}
			var result mutationResult
			if err := client.Call(ctx, "reopen", fields, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			fmt.Fprintf(os.Stderr, "%s reopened\n", result.ID)
			return nil
		},
	}
}

// --- batch ---

type batchParams struct {
	TicketConnection
	cli.JSONOutput
	File string `json:"-" flag:"file,f" desc:"path to JSON file containing batch ticket definitions"`
	Room string `json:"room" flag:"room,r" desc:"room ID" required:"true"`
}

// batchTicket is a single ticket definition in a batch file. The
// structure mirrors the service's batchCreateEntry CBOR request.
type batchTicket struct {
	Ref       string              `json:"ref"`
	Title     string              `json:"title"`
	Body      string              `json:"body,omitempty"`
	Type      string              `json:"type"`
	Priority  int                 `json:"priority"`
	Labels    []string            `json:"labels,omitempty"`
	Parent    string              `json:"parent,omitempty"`
	BlockedBy []string            `json:"blocked_by,omitempty"`
	Gates     []schema.TicketGate `json:"gates,omitempty"`
}

func batchCommand() *cli.Command {
	var params batchParams

	return &cli.Command{
		Name:    "batch",
		Summary: "Create multiple tickets from a JSON file",
		Description: `Create a batch of tickets with symbolic back-references. Each ticket
in the file has a "ref" field for symbolic naming. The "blocked_by" and
"parent" fields can reference other tickets by their ref (resolved to
real IDs server-side).

The file must contain a JSON array of ticket objects. All tickets are
validated before any are written — if any ticket is invalid, none are
created.`,
		Usage: "bureau ticket batch --room ROOM --file FILE [flags]",
		Examples: []cli.Example{
			{
				Description: "Create tickets from a file",
				Command:     "bureau ticket batch --room '!abc:bureau.local' --file tickets.json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &batchCreateResult{} },
		RequiredGrants: []string{"command/ticket/batch"},
		Run: func(args []string) error {
			if params.Room == "" {
				return fmt.Errorf("--room is required")
			}
			if params.File == "" {
				return fmt.Errorf("--file is required")
			}

			data, err := os.ReadFile(params.File)
			if err != nil {
				return fmt.Errorf("reading %s: %w", params.File, err)
			}

			var tickets []batchTicket
			if err := json.Unmarshal(data, &tickets); err != nil {
				return fmt.Errorf("parsing %s: %w", params.File, err)
			}

			if len(tickets) == 0 {
				return fmt.Errorf("file contains no tickets")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			// Use a longer timeout for batch operations.
			ctx, cancel := callContext()
			defer cancel()

			fields := map[string]any{
				"room":    params.Room,
				"tickets": tickets,
			}

			var result batchCreateResult
			if err := client.Call(ctx, "batch-create", fields, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			fmt.Fprintf(os.Stderr, "created %d tickets in %s\n", len(result.Refs), result.Room)
			for ref, ticketID := range result.Refs {
				fmt.Printf("%s → %s\n", ref, ticketID)
			}
			return nil
		},
	}
}
