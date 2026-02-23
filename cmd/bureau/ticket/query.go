// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

// --- list ---

type listParams struct {
	TicketConnection
	cli.JSONOutput
	Room     string `json:"room"     flag:"room,r"    desc:"room ID (e.g. !abc123:bureau.local)" required:"true"`
	Status   string `json:"status"   flag:"status,s"   desc:"filter by status (open, in_progress, blocked, closed)"`
	Priority int    `json:"priority" flag:"priority,p"  desc:"filter by priority (0-4, -1 for all)" default:"-1"`
	Label    string `json:"label"    flag:"label,l"    desc:"filter by label"`
	Assignee string `json:"assignee" flag:"assignee"   desc:"filter by assignee (Matrix user ID)"`
	Type     string `json:"type"     flag:"type,t"     desc:"filter by type (task, bug, feature, epic, chore, docs, question, pipeline)"`
	Parent   string `json:"parent"   flag:"parent"     desc:"filter by parent ticket ID"`
}

func listCommand() *cli.Command {
	var params listParams

	return &cli.Command{
		Name:    "list",
		Summary: "List tickets with optional filters",
		Description: `Query tickets in a room with optional filters. All filter flags use
AND semantics: only tickets matching every specified filter are returned.

Results are sorted by priority (most urgent first), then by creation
time (oldest first).`,
		Usage: "bureau ticket list --room ROOM [flags]",
		Examples: []cli.Example{
			{
				Description: "List all open tickets in a room",
				Command:     "bureau ticket list --room '!abc:bureau.local' --status open",
			},
			{
				Description: "List high-priority bugs",
				Command:     "bureau ticket list --room '!abc:bureau.local' --status open --priority 1 --type bug",
			},
			{
				Description: "List tickets assigned to a principal",
				Command:     "bureau ticket list --room '!abc:bureau.local' --assignee '@iree/amdgpu/pm:bureau.local'",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]ticketEntry{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/list"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if params.Room == "" {
				return cli.Validation("--room is required")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			fields := map[string]any{"room": params.Room}
			if params.Status != "" {
				fields["status"] = params.Status
			}
			if params.Priority >= 0 {
				fields["priority"] = params.Priority
			}
			if params.Label != "" {
				fields["label"] = params.Label
			}
			if params.Assignee != "" {
				fields["assignee"] = params.Assignee
			}
			if params.Type != "" {
				fields["type"] = params.Type
			}
			if params.Parent != "" {
				fields["parent"] = params.Parent
			}

			var entries []ticketEntry
			if err := client.Call(ctx, "list", fields, &entries); err != nil {
				return err
			}

			if done, err := params.EmitJSON(entries); done {
				return err
			}

			if len(entries) == 0 {
				logger.Info("no tickets found")
				return nil
			}

			return writeTicketTable(entries)
		},
	}
}

// --- show ---

type showParams struct {
	TicketConnection
	cli.JSONOutput
	Room   string `json:"room"   flag:"room,r" desc:"room ID or alias localpart (or use room-qualified ticket ref)"`
	Ticket string `json:"ticket" desc:"ticket ID (bare or room-qualified)" required:"true"`
}

func showCommand() *cli.Command {
	var params showParams

	return &cli.Command{
		Name:    "show",
		Summary: "Show ticket details",
		Description: `Display full details for a single ticket, including computed fields
from the dependency graph (what it blocks, child progress) and
scoring dimensions.

The ticket is resolved via --room or a room-qualified ticket reference (e.g., iree/general/tkt-a3f9).`,
		Usage: "bureau ticket show <ticket-id> [flags]",
		Examples: []cli.Example{
			{
				Description: "Show a ticket",
				Command:     "bureau ticket show tkt-a3f9",
			},
			{
				Description: "Show as JSON",
				Command:     "bureau ticket show tkt-a3f9 --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &showResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/show"},
		Run: func(ctx context.Context, args []string, _ *slog.Logger) error {
			if len(args) == 1 {
				params.Ticket = args[0]
			} else if len(args) > 1 {
				return cli.Validation("expected 1 positional argument, got %d", len(args))
			}
			if params.Ticket == "" {
				return cli.Validation("ticket ID is required\n\nUsage: bureau ticket show <ticket-id>")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			fields := map[string]any{"ticket": params.Ticket}
			if params.Room != "" {
				fields["room"] = params.Room
			}
			var result showResult
			if err := client.Call(ctx, "show", fields, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			return writeShowDetail(result)
		},
	}
}

// --- ready ---

type readyParams struct {
	TicketConnection
	cli.JSONOutput
	Room string `json:"room" flag:"room,r" desc:"room ID" required:"true"`
}

func readyCommand() *cli.Command {
	var params readyParams

	return &cli.Command{
		Name:    "ready",
		Summary: "List ready tickets (unblocked, all gates satisfied)",
		Description: `Show tickets that are open with no open blockers and all gates
satisfied. These are the tickets available for assignment.

Sorted by priority (most urgent first), then creation time.`,
		Usage: "bureau ticket ready --room ROOM [flags]",
		Examples: []cli.Example{
			{
				Description: "List ready tickets",
				Command:     "bureau ticket ready --room '!abc:bureau.local'",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]ticketEntry{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/ready"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			return roomScopedQuery(ctx, logger, params.TicketConnection, params.Room, "ready", "ticket/ready", &params.JSONOutput)
		},
	}
}

// --- blocked ---

type blockedParams struct {
	TicketConnection
	cli.JSONOutput
	Room string `json:"room" flag:"room,r" desc:"room ID" required:"true"`
}

func blockedCommand() *cli.Command {
	var params blockedParams

	return &cli.Command{
		Name:    "blocked",
		Summary: "List blocked tickets",
		Description: `Show tickets that cannot be started: they have at least one
non-closed blocker or unsatisfied gate.`,
		Usage:          "bureau ticket blocked --room ROOM [flags]",
		Params:         func() any { return &params },
		Output:         func() any { return &[]ticketEntry{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/blocked"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			return roomScopedQuery(ctx, logger, params.TicketConnection, params.Room, "blocked", "ticket/blocked", &params.JSONOutput)
		},
	}
}

// --- ranked ---

type rankedParams struct {
	TicketConnection
	cli.JSONOutput
	Room string `json:"room" flag:"room,r" desc:"room ID" required:"true"`
}

func rankedCommand() *cli.Command {
	var params rankedParams

	return &cli.Command{
		Name:    "ranked",
		Summary: "List ready tickets sorted by composite score",
		Description: `Show ready tickets ranked by a composite score that weighs leverage
(what the ticket unblocks), urgency (downstream priority inheritance),
staleness (how long it's been actionable), and effort (note count).

This is the primary query for PM agents deciding what to assign next.`,
		Usage:          "bureau ticket ranked --room ROOM [flags]",
		Params:         func() any { return &params },
		Output:         func() any { return &[]rankedEntry{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/ranked"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if params.Room == "" {
				return cli.Validation("--room is required")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			var entries []rankedEntry
			if err := client.Call(ctx, "ranked", map[string]any{"room": params.Room}, &entries); err != nil {
				return err
			}

			if done, err := params.EmitJSON(entries); done {
				return err
			}

			if len(entries) == 0 {
				logger.Info("no ready tickets")
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
			fmt.Fprintf(writer, "ID\tPRI\tTYPE\tSCORE\tUNBLOCKS\tTITLE\n")
			for _, entry := range entries {
				fmt.Fprintf(writer, "%s\t%s\t%s\t%.1f\t%d\t%s\n",
					entry.ID,
					priorityLabel(entry.Content.Priority),
					entry.Content.Type,
					entry.Score.Composite,
					entry.Score.UnblockCount,
					truncate(entry.Content.Title, 60),
				)
			}
			return writer.Flush()
		},
	}
}

// --- grep ---

type grepParams struct {
	TicketConnection
	cli.JSONOutput
	Pattern  string `json:"pattern"  desc:"regex search pattern" required:"true"`
	Room     string `json:"room"     flag:"room,r"     desc:"room ID (optional, searches all rooms if omitted)"`
	Status   string `json:"status"   flag:"status,s"    desc:"filter by status (open, in_progress, closed, active, ready)"`
	Priority int    `json:"priority" flag:"priority,p"   desc:"filter by priority (0-4, -1 for all)" default:"-1"`
	Label    string `json:"label"    flag:"label,l"     desc:"filter by label"`
	Assignee string `json:"assignee" flag:"assignee"    desc:"filter by assignee (Matrix user ID)"`
	Type     string `json:"type"     flag:"type,t"      desc:"filter by type (task, bug, feature, epic, chore, docs, question, pipeline)"`
}

func grepCommand() *cli.Command {
	var params grepParams

	return &cli.Command{
		Name:    "grep",
		Summary: "Search tickets by regex",
		Description: `Search ticket titles, bodies, and notes with a regular expression.
If --room is specified, searches only that room. Otherwise searches
all rooms and includes the room ID in results.

Results can be narrowed with filter flags (--status, --priority, etc.).
The --status flag accepts concrete values (open, in_progress, closed) and
two synthetic values: "active" (open or in_progress) and "ready" (open
with all blockers closed and all gates satisfied).`,
		Usage: "bureau ticket grep <pattern> [flags]",
		Examples: []cli.Example{
			{
				Description: "Search for authentication-related tickets",
				Command:     "bureau ticket grep 'auth.*fail'",
			},
			{
				Description: "Search in a specific room",
				Command:     "bureau ticket grep 'memory leak' --room '!abc:bureau.local'",
			},
			{
				Description: "Search open tickets only",
				Command:     "bureau ticket grep 'timeout' --status open --room '!abc:bureau.local'",
			},
			{
				Description: "Search active (non-closed) tickets",
				Command:     "bureau ticket grep 'auth' --status active",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]ticketEntry{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/grep"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 1 {
				params.Pattern = args[0]
			} else if len(args) > 1 {
				return cli.Validation("expected 1 positional argument, got %d", len(args))
			}
			if params.Pattern == "" {
				return cli.Validation("search pattern is required\n\nUsage: bureau ticket grep <pattern>")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			fields := map[string]any{"pattern": params.Pattern}
			if params.Room != "" {
				fields["room"] = params.Room
			}
			if params.Status != "" {
				fields["status"] = params.Status
			}
			if params.Priority >= 0 {
				fields["priority"] = params.Priority
			}
			if params.Label != "" {
				fields["label"] = params.Label
			}
			if params.Assignee != "" {
				fields["assignee"] = params.Assignee
			}
			if params.Type != "" {
				fields["type"] = params.Type
			}

			var entries []ticketEntry
			if err := client.Call(ctx, "grep", fields, &entries); err != nil {
				return err
			}

			if done, err := params.EmitJSON(entries); done {
				return err
			}

			if len(entries) == 0 {
				logger.Info("no matches")
				return nil
			}

			return writeTicketTable(entries)
		},
	}
}

// --- search ---

type searchParams struct {
	TicketConnection
	cli.JSONOutput
	Query    string `json:"query"    desc:"search query (natural language, ticket IDs, or both)" required:"true"`
	Room     string `json:"room"     flag:"room,r"     desc:"room ID (optional, searches all rooms if omitted)"`
	Status   string `json:"status"   flag:"status,s"    desc:"filter by status (open, in_progress, closed, active, ready)"`
	Priority int    `json:"priority" flag:"priority,p"   desc:"filter by priority (0-4, -1 for all)" default:"-1"`
	Label    string `json:"label"    flag:"label,l"     desc:"filter by label"`
	Assignee string `json:"assignee" flag:"assignee"    desc:"filter by assignee (Matrix user ID)"`
	Type     string `json:"type"     flag:"type,t"      desc:"filter by type (task, bug, feature, epic, chore, docs, question, pipeline)"`
	Limit    int    `json:"limit"    flag:"limit,n"     desc:"maximum number of results" default:"50"`
}

func searchCommand() *cli.Command {
	var params searchParams

	return &cli.Command{
		Name:    "search",
		Summary: "Search tickets by relevance",
		Description: `Search tickets using BM25 full-text ranking across titles, bodies,
labels, and notes with relevance scoring.

Ticket IDs (e.g., tkt-a3f9) and artifact IDs (e.g., art-cafe1234) in
the query receive exact-match boosting. When a ticket ID is found, the
search automatically expands to include dependency neighbors: blockers,
dependents, parent, and children. Tickets that textually reference the
ID are also included with a lower boost.

If --room is specified, searches only that room. Otherwise searches all
rooms and includes the room ID in results.`,
		Usage: "bureau ticket search <query> [flags]",
		Examples: []cli.Example{
			{
				Description: "Search for authentication-related tickets",
				Command:     "bureau ticket search 'authentication token refresh'",
			},
			{
				Description: "Find a ticket and its neighborhood",
				Command:     "bureau ticket search 'tkt-a3f9'",
			},
			{
				Description: "Search open tickets in a room",
				Command:     "bureau ticket search 'memory leak' --room '!abc:bureau.local' --status open",
			},
			{
				Description: "Limit results",
				Command:     "bureau ticket search 'proxy' --limit 10",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]searchEntry{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/search"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 1 {
				params.Query = args[0]
			} else if len(args) > 1 {
				return cli.Validation("expected 1 positional argument, got %d", len(args))
			}
			if params.Query == "" {
				return cli.Validation("search query is required\n\nUsage: bureau ticket search <query>")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			fields := map[string]any{"query": params.Query}
			if params.Room != "" {
				fields["room"] = params.Room
			}
			if params.Status != "" {
				fields["status"] = params.Status
			}
			if params.Priority >= 0 {
				fields["priority"] = params.Priority
			}
			if params.Label != "" {
				fields["label"] = params.Label
			}
			if params.Assignee != "" {
				fields["assignee"] = params.Assignee
			}
			if params.Type != "" {
				fields["type"] = params.Type
			}
			if params.Limit > 0 {
				fields["limit"] = params.Limit
			}

			var entries []searchEntry
			if err := client.Call(ctx, "search", fields, &entries); err != nil {
				return err
			}

			if done, err := params.EmitJSON(entries); done {
				return err
			}

			if len(entries) == 0 {
				logger.Info("no matches")
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
			fmt.Fprintf(writer, "ID\tSCORE\tSTATUS\tPRI\tTYPE\tTITLE\n")
			for _, entry := range entries {
				fmt.Fprintf(writer, "%s\t%.1f\t%s\t%s\t%s\t%s\n",
					entry.ID,
					entry.Score,
					entry.Content.Status,
					priorityLabel(entry.Content.Priority),
					entry.Content.Type,
					truncate(entry.Content.Title, 50),
				)
			}
			return writer.Flush()
		},
	}
}

// --- stats ---

type statsParams struct {
	TicketConnection
	cli.JSONOutput
	Room string `json:"room" flag:"room,r" desc:"room ID" required:"true"`
}

func statsCommand() *cli.Command {
	var params statsParams

	return &cli.Command{
		Name:    "stats",
		Summary: "Show aggregate ticket statistics",
		Description: `Display ticket counts broken down by status, priority, and type
for a room.`,
		Usage:          "bureau ticket stats --room ROOM [flags]",
		Params:         func() any { return &params },
		Output:         func() any { return &ticketindex.Stats{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/stats"},
		Run: func(ctx context.Context, args []string, _ *slog.Logger) error {
			if params.Room == "" {
				return cli.Validation("--room is required")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			var stats ticketindex.Stats
			if err := client.Call(ctx, "stats", map[string]any{"room": params.Room}, &stats); err != nil {
				return err
			}

			if done, err := params.EmitJSON(stats); done {
				return err
			}

			fmt.Printf("Total: %d\n\n", stats.Total)

			if len(stats.ByStatus) > 0 {
				fmt.Println("By Status:")
				for _, status := range []string{"open", "in_progress", "blocked", "closed"} {
					if count, ok := stats.ByStatus[status]; ok {
						fmt.Printf("  %-14s %d\n", status, count)
					}
				}
				fmt.Println()
			}

			if len(stats.ByPriority) > 0 {
				fmt.Println("By Priority:")
				for priority := 0; priority <= 4; priority++ {
					if count, ok := stats.ByPriority[priority]; ok {
						fmt.Printf("  %-14s %d\n", priorityLabel(priority), count)
					}
				}
				fmt.Println()
			}

			if len(stats.ByType) > 0 {
				fmt.Println("By Type:")
				for _, typeName := range []string{"task", "bug", "feature", "epic", "chore", "docs", "question", "pipeline"} {
					if count, ok := stats.ByType[typeName]; ok {
						fmt.Printf("  %-14s %d\n", typeName, count)
					}
				}
			}

			return nil
		},
	}
}

// --- info ---

type infoParams struct {
	TicketConnection
	cli.JSONOutput
}

func infoCommand() *cli.Command {
	var params infoParams

	return &cli.Command{
		Name:    "info",
		Summary: "Show ticket service diagnostic information",
		Description: `Display service diagnostics: uptime, number of tracked rooms,
total tickets, and per-room summaries. Requires authentication.`,
		Usage:          "bureau ticket info [flags]",
		Params:         func() any { return &params },
		Output:         func() any { return &serviceInfo{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/info"},
		Run: func(ctx context.Context, args []string, _ *slog.Logger) error {
			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			var info serviceInfo
			if err := client.Call(ctx, "info", nil, &info); err != nil {
				return err
			}

			if done, err := params.EmitJSON(info); done {
				return err
			}

			fmt.Printf("Uptime:  %.0fs\n", info.UptimeSeconds)
			fmt.Printf("Rooms:   %d\n", info.Rooms)
			fmt.Printf("Tickets: %d\n", info.TotalTickets)

			if len(info.RoomDetails) > 0 {
				fmt.Println()
				writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
				fmt.Fprintf(writer, "ROOM\tTICKETS\tOPEN\tIN PROGRESS\tBLOCKED\tCLOSED\n")
				for _, room := range info.RoomDetails {
					fmt.Fprintf(writer, "%s\t%d\t%d\t%d\t%d\t%d\n",
						room.RoomID,
						room.Tickets,
						room.ByStatus["open"],
						room.ByStatus["in_progress"],
						room.ByStatus["blocked"],
						room.ByStatus["closed"],
					)
				}
				writer.Flush()
			}

			return nil
		},
	}
}

// --- deps ---

type depsParams struct {
	TicketConnection
	cli.JSONOutput
	Room   string `json:"room"   flag:"room,r" desc:"room ID or alias localpart (or use room-qualified ticket ref)"`
	Ticket string `json:"ticket" desc:"ticket ID" required:"true"`
}

func depsCommand() *cli.Command {
	var params depsParams

	return &cli.Command{
		Name:    "deps",
		Summary: "Show transitive dependency closure",
		Description: `Display all tickets that must be completed before this ticket
becomes ready, including indirect (transitive) dependencies.`,
		Usage:          "bureau ticket deps <ticket-id> [flags]",
		Params:         func() any { return &params },
		Output:         func() any { return &depsResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/deps"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 1 {
				params.Ticket = args[0]
			} else if len(args) > 1 {
				return cli.Validation("expected 1 positional argument, got %d", len(args))
			}
			if params.Ticket == "" {
				return cli.Validation("ticket ID is required\n\nUsage: bureau ticket deps <ticket-id>")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			fields := map[string]any{"ticket": params.Ticket}
			if params.Room != "" {
				fields["room"] = params.Room
			}
			var result depsResult
			if err := client.Call(ctx, "deps", fields, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			if len(result.Deps) == 0 {
				logger.Info("no dependencies", "ticket", params.Ticket)
				return nil
			}

			fmt.Printf("Dependencies of %s (%d):\n", result.Ticket, len(result.Deps))
			for _, dep := range result.Deps {
				fmt.Printf("  %s\n", dep)
			}
			return nil
		},
	}
}

// --- children ---

type childrenParams struct {
	TicketConnection
	cli.JSONOutput
	Room   string `json:"room"   flag:"room,r" desc:"room ID or alias localpart (or use room-qualified ticket ref)"`
	Ticket string `json:"ticket" desc:"parent ticket ID" required:"true"`
}

func childrenCommand() *cli.Command {
	var params childrenParams

	return &cli.Command{
		Name:    "children",
		Summary: "List child tickets of an epic",
		Description: `Display direct children of a parent ticket with a progress summary
showing how many are closed out of the total.`,
		Usage:          "bureau ticket children <ticket-id> [flags]",
		Params:         func() any { return &params },
		Output:         func() any { return &childrenResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/children"},
		Run: func(ctx context.Context, args []string, _ *slog.Logger) error {
			if len(args) == 1 {
				params.Ticket = args[0]
			} else if len(args) > 1 {
				return cli.Validation("expected 1 positional argument, got %d", len(args))
			}
			if params.Ticket == "" {
				return cli.Validation("ticket ID is required\n\nUsage: bureau ticket children <ticket-id>")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			fields := map[string]any{"ticket": params.Ticket}
			if params.Room != "" {
				fields["room"] = params.Room
			}
			var result childrenResult
			if err := client.Call(ctx, "children", fields, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			fmt.Printf("Children of %s: %d/%d closed\n\n", result.Parent, result.ChildClosed, result.ChildTotal)

			if len(result.Children) == 0 {
				return nil
			}

			return writeTicketTable(result.Children)
		},
	}
}

// --- epic-health ---

type epicHealthParams struct {
	TicketConnection
	cli.JSONOutput
	Room   string `json:"room"   flag:"room,r" desc:"room ID or alias localpart (or use room-qualified ticket ref)"`
	Ticket string `json:"ticket" desc:"epic ticket ID" required:"true"`
}

func epicHealthCommand() *cli.Command {
	var params epicHealthParams

	return &cli.Command{
		Name:    "epic-health",
		Summary: "Show health metrics for an epic",
		Description: `Display health metrics for an epic's children: parallelism width
(ready children), completion progress, active fraction (what
portion of remaining work is actionable), and critical dependency
depth (irreducible sequential steps).`,
		Usage:          "bureau ticket epic-health <ticket-id> [flags]",
		Params:         func() any { return &params },
		Output:         func() any { return &epicHealthResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/epic-health"},
		Run: func(ctx context.Context, args []string, _ *slog.Logger) error {
			if len(args) == 1 {
				params.Ticket = args[0]
			} else if len(args) > 1 {
				return cli.Validation("expected 1 positional argument, got %d", len(args))
			}
			if params.Ticket == "" {
				return cli.Validation("ticket ID is required\n\nUsage: bureau ticket epic-health <ticket-id>")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			fields := map[string]any{"ticket": params.Ticket}
			if params.Room != "" {
				fields["room"] = params.Room
			}
			var result epicHealthResult
			if err := client.Call(ctx, "epic-health", fields, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			health := result.Health
			fmt.Printf("Epic: %s\n", result.Ticket)
			fmt.Printf("  Progress:        %d/%d closed\n", health.ClosedChildren, health.TotalChildren)
			fmt.Printf("  Ready (parallel): %d\n", health.ReadyChildren)
			fmt.Printf("  Active fraction:  %.0f%%\n", health.ActiveFraction*100)
			fmt.Printf("  Critical depth:   %d\n", health.CriticalDepth)
			return nil
		},
	}
}

// --- Shared helpers ---

// roomScopedQuery handles the common pattern for room-scoped queries
// that return a list of ticket entries: check room, call service,
// output as JSON or table.
func roomScopedQuery(ctx context.Context, logger *slog.Logger, connection TicketConnection, room, action, grant string, jsonOutput *cli.JSONOutput) error {
	if room == "" {
		return cli.Validation("--room is required")
	}

	client, err := connection.connect()
	if err != nil {
		return err
	}

	ctx, cancel := callContext(ctx)
	defer cancel()

	var entries []ticketEntry
	if err := client.Call(ctx, action, map[string]any{"room": room}, &entries); err != nil {
		return err
	}

	if done, err := jsonOutput.EmitJSON(entries); done {
		return err
	}

	if len(entries) == 0 {
		logger.Info("no tickets found", "action", action)
		return nil
	}

	return writeTicketTable(entries)
}

// writeTicketTable writes a compact table of ticket entries to stdout.
func writeTicketTable(entries []ticketEntry) error {
	writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
	fmt.Fprintf(writer, "ID\tSTATUS\tPRI\tTYPE\tTITLE\n")
	for _, entry := range entries {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			entry.ID,
			entry.Content.Status,
			priorityLabel(entry.Content.Priority),
			entry.Content.Type,
			truncate(entry.Content.Title, 60),
		)
	}
	return writer.Flush()
}

// writeShowDetail writes a human-readable detail view of a ticket.
func writeShowDetail(result showResult) error {
	content := result.Content
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)

	fmt.Fprintf(writer, "ID:\t%s\n", result.ID)
	fmt.Fprintf(writer, "Room:\t%s\n", result.Room)
	fmt.Fprintf(writer, "Status:\t%s\n", content.Status)
	fmt.Fprintf(writer, "Priority:\t%s\n", priorityLabel(content.Priority))
	fmt.Fprintf(writer, "Type:\t%s\n", content.Type)
	fmt.Fprintf(writer, "Title:\t%s\n", content.Title)

	if !content.Assignee.IsZero() {
		fmt.Fprintf(writer, "Assignee:\t%s\n", content.Assignee.String())
	}
	if len(content.Labels) > 0 {
		fmt.Fprintf(writer, "Labels:\t%s\n", strings.Join(content.Labels, ", "))
	}
	if content.Parent != "" {
		fmt.Fprintf(writer, "Parent:\t%s\n", content.Parent)
	}
	if len(content.BlockedBy) > 0 {
		fmt.Fprintf(writer, "Blocked by:\t%s\n", strings.Join(content.BlockedBy, ", "))
	}
	if len(result.Blocks) > 0 {
		fmt.Fprintf(writer, "Blocks:\t%s\n", strings.Join(result.Blocks, ", "))
	}
	if result.ChildTotal > 0 {
		fmt.Fprintf(writer, "Children:\t%d/%d closed\n", result.ChildClosed, result.ChildTotal)
	}
	fmt.Fprintf(writer, "Created:\t%s\n", content.CreatedAt)
	fmt.Fprintf(writer, "Updated:\t%s\n", content.UpdatedAt)
	if !content.CreatedBy.IsZero() {
		fmt.Fprintf(writer, "Created by:\t%s\n", content.CreatedBy.String())
	}
	if content.ClosedAt != "" {
		fmt.Fprintf(writer, "Closed:\t%s\n", content.ClosedAt)
	}
	if content.CloseReason != "" {
		fmt.Fprintf(writer, "Close reason:\t%s\n", content.CloseReason)
	}

	writer.Flush()

	if content.Body != "" {
		fmt.Printf("\n%s\n", content.Body)
	}

	if len(content.Gates) > 0 {
		fmt.Println("\nGates:")
		for _, gate := range content.Gates {
			status := gate.Status
			if gate.SatisfiedBy != "" {
				status += " (by " + gate.SatisfiedBy + ")"
			}
			fmt.Printf("  %s  %s  %s\n", gate.ID, gate.Type, status)
		}
	}

	if len(content.Notes) > 0 {
		fmt.Println("\nNotes:")
		for _, note := range content.Notes {
			fmt.Printf("  [%s] %s: %s\n", note.CreatedAt, note.Author, note.Body)
		}
	}

	if result.Score != nil {
		score := result.Score
		fmt.Printf("\nScore: %.1f (unblocks=%d, borrowed_priority=%d, days_ready=%d, notes=%d)\n",
			score.Composite, score.UnblockCount, score.BorrowedPriority,
			score.DaysSinceReady, score.NoteCount)
	}

	return nil
}

// priorityLabel returns a human-readable label for a priority number.
func priorityLabel(priority int) string {
	switch priority {
	case 0:
		return "P0"
	case 1:
		return "P1"
	case 2:
		return "P2"
	case 3:
		return "P3"
	case 4:
		return "P4"
	default:
		return fmt.Sprintf("P%d", priority)
	}
}

// truncate shortens a string to maxLength, appending "..." if truncated.
func truncate(s string, maxLength int) string {
	if len(s) <= maxLength {
		return s
	}
	if maxLength <= 3 {
		return s[:maxLength]
	}
	return s[:maxLength-3] + "..."
}

// --- upcoming ---

type upcomingParams struct {
	TicketConnection
	cli.JSONOutput
	Room string `json:"room" flag:"room,r" desc:"filter to a single room"`
}

func upcomingCommand() *cli.Command {
	var params upcomingParams

	return &cli.Command{
		Name:    "upcoming",
		Summary: "List upcoming timer gates",
		Description: `Show pending timer gates across all rooms (or filtered to a single
room), sorted by target time ascending. Each entry shows the time until
fire, ticket title, and gate type (one-shot, cron, or interval).

This command is the data source for monitoring scheduled tasks and
recurring tickets.`,
		Usage: "bureau ticket upcoming [--room ROOM] [flags]",
		Examples: []cli.Example{
			{
				Description: "List all upcoming timer gates",
				Command:     "bureau ticket upcoming",
			},
			{
				Description: "List gates in a specific room",
				Command:     "bureau ticket upcoming --room '!abc:bureau.local'",
			},
			{
				Description: "JSON output for scripting",
				Command:     "bureau ticket upcoming --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]upcomingGateResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/upcoming"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			fields := map[string]any{}
			if params.Room != "" {
				fields["room"] = params.Room
			}

			var results []upcomingGateResult
			if err := client.Call(ctx, "upcoming-gates", fields, &results); err != nil {
				return err
			}

			if done, err := params.EmitJSON(results); done {
				return err
			}

			if len(results) == 0 {
				logger.Info("no upcoming timer gates")
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "UNTIL\tTICKET\tTITLE\tGATE\tTYPE\tTARGET")
			for _, entry := range results {
				gateType := "one-shot"
				if entry.Schedule != "" {
					gateType = "cron"
				} else if entry.Interval != "" {
					gateType = "interval"
				}

				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					entry.UntilFire,
					entry.TicketID,
					truncate(entry.Title, 40),
					entry.GateID,
					gateType,
					entry.Target,
				)
			}
			tw.Flush()
			return nil
		},
	}
}
