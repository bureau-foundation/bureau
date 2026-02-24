// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/stewardship"
	ticketschema "github.com/bureau-foundation/bureau/lib/schema/ticket"
)

// stewardshipCommand returns the "stewardship" subcommand group for
// querying and managing stewardship declarations through the ticket
// service.
func stewardshipCommand() *cli.Command {
	return &cli.Command{
		Name:    "stewardship",
		Summary: "Query and manage stewardship declarations",
		Description: `View and manage stewardship declarations that govern ticket review.

Stewardship declarations map resource patterns to responsible principals
with tiered review escalation. When a ticket's affects entries match a
declaration's resource patterns and the ticket type is in gate_types,
the ticket service auto-configures review gates with the declared tier
structure.

Commands connect to the ticket service socket. Use --service for
operator CLI access, or run inside a sandbox where the socket is
provisioned automatically.`,
		Subcommands: []*cli.Command{
			stewardshipListCommand(),
			stewardshipResolveCommand(),
			stewardshipSetCommand(),
		},
	}
}

// --- stewardship list ---

type stewardshipListParams struct {
	TicketConnection
	cli.JSONOutput
	Room string `json:"room" flag:"room,r" desc:"room ID (optional, scope to one room)"`
}

// stewardshipListEntry mirrors the service response for a single
// declaration in the stewardship-list result.
type stewardshipListEntry struct {
	RoomID           ref.RoomID                    `json:"room_id"`
	StateKey         string                        `json:"state_key"`
	ResourcePatterns []string                      `json:"resource_patterns"`
	GateTypes        []string                      `json:"gate_types,omitempty"`
	NotifyTypes      []string                      `json:"notify_types,omitempty"`
	OverlapPolicy    string                        `json:"overlap_policy,omitempty"`
	Description      string                        `json:"description,omitempty"`
	Tiers            []stewardship.StewardshipTier `json:"tiers"`
	DigestInterval   string                        `json:"digest_interval,omitempty"`
}

func stewardshipListCommand() *cli.Command {
	var params stewardshipListParams

	return &cli.Command{
		Name:    "list",
		Summary: "List stewardship declarations",
		Description: `List all stewardship declarations known to the ticket service.
Optionally scope to a single room with --room.`,
		Usage: "bureau ticket stewardship list [flags]",
		Examples: []cli.Example{
			{
				Description: "List all declarations",
				Command:     "bureau ticket stewardship list --json",
			},
			{
				Description: "List declarations in a specific room",
				Command:     "bureau ticket stewardship list --room '!abc:bureau.local'",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]stewardshipListEntry{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/stewardship/list"},
		Run: func(ctx context.Context, _ []string, logger *slog.Logger) error {
			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			fields := map[string]any{}
			if params.Room != "" {
				roomID, err := cli.ResolveRoom(ctx, params.Room)
				if err != nil {
					return err
				}
				fields["room"] = roomID.String()
			}

			var entries []stewardshipListEntry
			if err := client.Call(ctx, "stewardship-list", fields, &entries); err != nil {
				return err
			}

			if done, err := params.EmitJSON(entries); done {
				return err
			}

			if len(entries) == 0 {
				fmt.Println("No stewardship declarations found.")
				return nil
			}

			for _, entry := range entries {
				logger.Info("stewardship declaration",
					"room_id", entry.RoomID,
					"state_key", entry.StateKey,
					"patterns", entry.ResourcePatterns,
					"gate_types", entry.GateTypes,
					"tiers", len(entry.Tiers),
				)
			}
			return nil
		},
	}
}

// --- stewardship resolve ---

type stewardshipResolveParams struct {
	TicketConnection
	cli.JSONOutput
	Type     string `json:"type"     flag:"type,t"     desc:"ticket type to resolve against" required:"true"`
	Priority int    `json:"priority" flag:"priority,p"  desc:"ticket priority for P0 bypass evaluation" default:"2"`
}

// stewardshipResolveResponse mirrors the service response for
// stewardship-resolve.
type stewardshipResolveResponse struct {
	Gates      []ticketschema.TicketGate      `json:"gates"`
	Reviewers  []ticketschema.ReviewerEntry   `json:"reviewers"`
	Thresholds []ticketschema.TierThreshold   `json:"thresholds"`
	Matches    []stewardshipResolveMatchEntry `json:"matches"`
}

// stewardshipResolveMatchEntry describes a single declaration match.
type stewardshipResolveMatchEntry struct {
	RoomID          ref.RoomID `json:"room_id"`
	StateKey        string     `json:"state_key"`
	MatchedPattern  string     `json:"matched_pattern"`
	MatchedResource string     `json:"matched_resource"`
	OverlapPolicy   string     `json:"overlap_policy"`
	Description     string     `json:"description,omitempty"`
}

func stewardshipResolveCommand() *cli.Command {
	var params stewardshipResolveParams

	return &cli.Command{
		Name:    "resolve",
		Summary: "Preview stewardship resolution for resources",
		Description: `Dry-run stewardship resolution: given resource identifiers and a ticket
type, preview what review gates, reviewers, and thresholds would be
auto-configured. No ticket is created or modified.

Resources are passed as positional arguments. Use --type to specify the
ticket type for gate_types matching.`,
		Usage: "bureau ticket stewardship resolve RESOURCE [RESOURCE...] --type TYPE [flags]",
		Examples: []cli.Example{
			{
				Description: "Preview stewardship for a GPU resource change",
				Command:     "bureau ticket stewardship resolve fleet/gpu/a100 --type task --json",
			},
			{
				Description: "Check P0 bypass behavior",
				Command:     "bureau ticket stewardship resolve fleet/gpu/a100 --type bug --priority 0",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &stewardshipResolveResponse{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/stewardship/resolve"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("at least one resource argument is required\n\nUsage: bureau ticket stewardship resolve RESOURCE [RESOURCE...] --type TYPE")
			}
			if params.Type == "" {
				return cli.Validation("--type is required")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			fields := map[string]any{
				"affects":     args,
				"ticket_type": params.Type,
			}
			if params.Priority != 2 {
				fields["priority"] = params.Priority
			}

			var response stewardshipResolveResponse
			if err := client.Call(ctx, "stewardship-resolve", fields, &response); err != nil {
				return err
			}

			if done, err := params.EmitJSON(response); done {
				return err
			}

			if len(response.Matches) == 0 {
				fmt.Println("No matching stewardship declarations found.")
				return nil
			}

			for _, match := range response.Matches {
				logger.Info("match",
					"state_key", match.StateKey,
					"pattern", match.MatchedPattern,
					"resource", match.MatchedResource,
					"policy", match.OverlapPolicy,
				)
			}
			for _, gate := range response.Gates {
				logger.Info("gate", "id", gate.ID, "type", gate.Type)
			}
			for _, reviewer := range response.Reviewers {
				logger.Info("reviewer",
					"user_id", reviewer.UserID,
					"tier", reviewer.Tier,
					"disposition", reviewer.Disposition,
				)
			}
			return nil
		},
	}
}

// --- stewardship set ---

type stewardshipSetParams struct {
	TicketConnection
	cli.JSONOutput
	Room     string `json:"room"      flag:"room,r"      desc:"room ID to publish the declaration in" required:"true"`
	StateKey string `json:"state_key" flag:"state-key,k"  desc:"resource identifier (state key)" required:"true"`
	File     string `json:"-"         flag:"file,f"       desc:"path to JSON file containing StewardshipContent"`
}

// stewardshipSetResult is the response from stewardship-set.
type stewardshipSetResult struct {
	EventID ref.EventID `json:"event_id"`
}

func stewardshipSetCommand() *cli.Command {
	var params stewardshipSetParams

	return &cli.Command{
		Name:    "set",
		Summary: "Publish a stewardship declaration",
		Description: `Write an m.bureau.stewardship state event to a room. The declaration
is validated before publishing. The ticket service discovers it through
its Matrix /sync and updates the stewardship index automatically.

The declaration content is read from a JSON file (--file). The file
must contain a valid StewardshipContent object with version,
resource_patterns, tiers, and optionally gate_types, notify_types,
overlap_policy, and digest_interval.`,
		Usage: "bureau ticket stewardship set --room ROOM --state-key KEY --file FILE [flags]",
		Examples: []cli.Example{
			{
				Description: "Publish a GPU stewardship declaration",
				Command:     "bureau ticket stewardship set --room '!abc:bureau.local' --state-key fleet/gpu --file gpu-stewardship.json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &stewardshipSetResult{} },
		Annotations:    cli.Create(),
		RequiredGrants: []string{"command/ticket/stewardship/set"},
		Run: func(ctx context.Context, _ []string, logger *slog.Logger) error {
			if params.Room == "" {
				return cli.Validation("--room is required")
			}
			if params.StateKey == "" {
				return cli.Validation("--state-key is required")
			}
			if params.File == "" {
				return cli.Validation("--file is required")
			}

			roomID, err := cli.ResolveRoom(ctx, params.Room)
			if err != nil {
				return err
			}

			content, err := loadStewardshipContent(params.File)
			if err != nil {
				return err
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			fields := map[string]any{
				"room":      roomID.String(),
				"state_key": params.StateKey,
				"content":   content,
			}

			var result stewardshipSetResult
			if err := client.Call(ctx, "stewardship-set", fields, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			logger.Info("stewardship declaration published",
				"room", roomID,
				"state_key", params.StateKey,
				"event_id", result.EventID,
			)
			return nil
		},
	}
}

// loadStewardshipContent reads and validates a StewardshipContent from
// a JSON file.
func loadStewardshipContent(path string) (stewardship.StewardshipContent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return stewardship.StewardshipContent{}, fmt.Errorf("reading %s: %w", path, err)
	}

	var content stewardship.StewardshipContent
	if err := json.Unmarshal(data, &content); err != nil {
		return stewardship.StewardshipContent{}, fmt.Errorf("parsing %s: %w", path, err)
	}

	if err := content.Validate(); err != nil {
		return stewardship.StewardshipContent{}, err
	}

	return content, nil
}
