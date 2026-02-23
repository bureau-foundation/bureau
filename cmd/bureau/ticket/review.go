// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// reviewCommand returns the "review" subcommand for setting a reviewer's
// disposition on a ticket in review status. The caller must be in the
// ticket's reviewer list.
func reviewCommand() *cli.Command {
	var params reviewParams

	return &cli.Command{
		Name:    "review",
		Summary: "Set review disposition on a ticket",
		Description: `Set your review disposition on a ticket that is in "review" status.
You must be listed as a reviewer on the ticket. Exactly one of
--approve, --request-changes, or --comment must be specified.

Dispositions:
  --approve           Looks good, ready to proceed
  --request-changes   Must fix before proceeding
  --comment           Feedback provided, no blocking opinion`,
		Usage: "bureau ticket review <ticket-id> --approve|--request-changes|--comment [flags]",
		Examples: []cli.Example{
			{
				Description: "Approve a review",
				Command:     "bureau ticket review tkt-a3f9 --approve",
			},
			{
				Description: "Request changes",
				Command:     "bureau ticket review tkt-a3f9 --request-changes",
			},
			{
				Description: "Leave a comment without blocking",
				Command:     "bureau ticket review tkt-a3f9 --comment",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &mutationResult{} },
		Annotations:    cli.Idempotent(),
		RequiredGrants: []string{"command/ticket/review"},
		Run: func(args []string) error {
			if len(args) == 1 {
				params.Ticket = args[0]
			} else if len(args) > 1 {
				return cli.Validation("expected 1 positional argument, got %d", len(args))
			}
			if params.Ticket == "" {
				return cli.Validation("ticket ID is required\n\nUsage: bureau ticket review <ticket-id> --approve|--request-changes|--comment")
			}

			// Exactly one disposition flag must be set.
			disposition, err := resolveDisposition(params.Approve, params.RequestChanges, params.Comment)
			if err != nil {
				return err
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			fields := map[string]any{
				"ticket":      params.Ticket,
				"disposition": disposition,
			}
			if params.Room != "" {
				fields["room"] = params.Room
			}

			var result mutationResult
			if err := client.Call(ctx, "set-disposition", fields, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			fmt.Fprintf(os.Stderr, "%s: disposition set to %s\n", result.ID, disposition)
			return nil
		},
	}
}

type reviewParams struct {
	TicketConnection
	cli.JSONOutput
	Room           string `json:"room"   flag:"room,r" desc:"room ID or alias localpart (or use room-qualified ticket ref)"`
	Ticket         string `json:"ticket" desc:"ticket ID" required:"true"`
	Approve        bool   `json:"-"      flag:"approve"          desc:"approve the review"`
	RequestChanges bool   `json:"-"      flag:"request-changes"  desc:"request changes"`
	Comment        bool   `json:"-"      flag:"comment"          desc:"leave a comment without blocking"`
}

// resolveDisposition validates that exactly one disposition flag is set
// and returns the corresponding disposition string.
func resolveDisposition(approve, requestChanges, comment bool) (string, error) {
	count := 0
	if approve {
		count++
	}
	if requestChanges {
		count++
	}
	if comment {
		count++
	}

	if count == 0 {
		return "", cli.Validation("one of --approve, --request-changes, or --comment is required")
	}
	if count > 1 {
		return "", cli.Validation("only one of --approve, --request-changes, or --comment can be specified")
	}

	switch {
	case approve:
		return "approved", nil
	case requestChanges:
		return "changes_requested", nil
	default:
		return "commented", nil
	}
}
