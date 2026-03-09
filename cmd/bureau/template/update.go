// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/templatedef"
	"github.com/bureau-foundation/bureau/messaging"
)

// templateUpdateParams holds the parameters for the template update command.
type templateUpdateParams struct {
	cli.SessionConfig
	cli.JSONOutput
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name for resolving room aliases (auto-detected from machine.conf)"`
	System     string `json:"system"      flag:"system"      desc:"Nix system for flake evaluation (default: current system)"`
	DryRun     bool   `json:"dry_run"     flag:"dry-run"     desc:"show what would change without publishing"`
	Yes        bool   `json:"yes"         flag:"yes"         desc:"skip confirmation prompts, publish all updates"`
}

// templateUpdateResult is the JSON output for one template's update status.
type templateUpdateResult struct {
	Name        string   `json:"name"                    desc:"template name (state key)"`
	Status      string   `json:"status"                  desc:"updated, current, skipped, or error"`
	Source      string   `json:"source,omitempty"         desc:"source type: flake or url"`
	OldRevision string   `json:"old_revision,omitempty"   desc:"previous git revision (flake only, short hash)"`
	NewRevision string   `json:"new_revision,omitempty"   desc:"new git revision (flake only, short hash)"`
	OldHash     string   `json:"old_hash,omitempty"       desc:"previous content hash"`
	NewHash     string   `json:"new_hash,omitempty"       desc:"new content hash"`
	Change      string   `json:"change,omitempty"         desc:"structural, payload-only, metadata, or no change"`
	Fields      []string `json:"fields,omitempty"         desc:"changed field names"`
	EventID     string   `json:"event_id,omitempty"       desc:"Matrix event ID of published update"`
	Error       string   `json:"error,omitempty"          desc:"error message if check failed"`
	DryRun      bool     `json:"dry_run"                  desc:"true if no publish was performed"`
}

// updateCommand returns the "update" subcommand for checking and applying
// template updates from their original source (flake or URL).
func updateCommand() *cli.Command {
	var params templateUpdateParams

	return &cli.Command{
		Name:    "update",
		Summary: "Check for and apply template updates from their source",
		Description: `Check templates for updates from their original source and optionally
publish the new versions. Templates published with --flake or --url have
origin metadata that records where they came from — this command
re-evaluates that source and compares the content hash.

If the argument is a template reference (contains ':'), only that
template is checked. If the argument is a room alias localpart (no ':'),
all templates in the room with origin metadata are checked.

Templates published from local files have no origin and are skipped.

After publishing an update, the daemon detects the template change via
/sync and restarts affected sandboxes automatically.`,
		Usage: "bureau template update [flags] <room-alias-localpart | template-ref>",
		Examples: []cli.Example{
			{
				Description: "Check a single template for updates",
				Command:     "bureau template update bureau/template:bureau-agent-claude",
			},
			{
				Description: "Check all templates in a room",
				Command:     "bureau template update bureau/template",
			},
			{
				Description: "Dry-run: show what would change without publishing",
				Command:     "bureau template update --dry-run bureau/template",
			},
			{
				Description: "Non-interactive: publish all updates without prompting",
				Command:     "bureau template update --yes bureau/template",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]templateUpdateResult{} },
		RequiredGrants: []string{"command/template/update"},
		Annotations:    cli.Create(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) != 1 {
				return cli.Validation("usage: bureau template update [flags] <room-alias-localpart | template-ref>")
			}

			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()

			session, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}

			params.ServerName = cli.ResolveServerName(params.ServerName)
			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return cli.Validation("invalid --server-name: %w", err)
			}

			// When --json is set, treat as dry-run to avoid MCP tools
			// performing writes without explicit --yes.
			dryRun := params.DryRun
			if params.JSONOutput.OutputJSON && !params.Yes {
				dryRun = true
			}

			argument := args[0]
			singleTemplate := strings.Contains(argument, ":")

			var targets []updateTarget

			if singleTemplate {
				templateRef, err := schema.ParseTemplateRef(argument)
				if err != nil {
					return cli.Validation("invalid template reference: %w", err).
						WithHint("Template references have the form namespace/template:name (e.g., bureau/template:bureau-agent-claude).")
				}

				content, err := templatedef.Fetch(ctx, session, templateRef, serverName)
				if err != nil {
					return err
				}

				targets = []updateTarget{{
					name:       templateRef.Template,
					ref:        templateRef,
					content:    content,
					serverName: serverName,
				}}
			} else {
				roomTargets, err := discoverTemplatesInRoom(ctx, session, argument, serverName)
				if err != nil {
					return err
				}
				targets = roomTargets
			}

			if len(targets) == 0 {
				logger.Info("no templates found", "target", argument)
				return nil
			}

			results := make([]templateUpdateResult, 0, len(targets))

			for index := range targets {
				result := checkTemplateUpdate(ctx, &targets[index], params.System, logger)
				results = append(results, result)
			}

			// Publish updates (unless dry-run).
			if !dryRun {
				for index := range results {
					if results[index].Status != "updated" {
						continue
					}
					target := &targets[index]

					if !params.Yes {
						if !confirmUpdate(&results[index]) {
							results[index].Status = "skipped"
							results[index].DryRun = true
							continue
						}
					}

					eventID, err := templatedef.Push(ctx, session, target.ref, *target.newContent, serverName)
					if err != nil {
						results[index].Status = "error"
						results[index].Error = fmt.Sprintf("publishing: %v", err)
						continue
					}
					results[index].EventID = eventID.EventID.String()
				}
			} else {
				for index := range results {
					results[index].DryRun = true
				}
			}

			// Emit output.
			if done, err := params.EmitJSON(results); done {
				return err
			}

			printUpdateResults(results)
			return nil
		},
	}
}

// updateTarget holds a template to check for updates.
type updateTarget struct {
	name       string
	ref        schema.TemplateRef
	content    *schema.TemplateContent
	serverName ref.ServerName
	newContent *schema.TemplateContent // populated if update available
}

// discoverTemplatesInRoom reads all templates from a room and returns
// those that have origin metadata (eligible for update checking).
func discoverTemplatesInRoom(ctx context.Context, session messaging.Session, roomLocalpart string, serverName ref.ServerName) ([]updateTarget, error) {
	roomAlias := ref.MustParseRoomAlias(schema.FullRoomAlias(roomLocalpart, serverName))

	roomID, err := session.ResolveAlias(ctx, roomAlias)
	if err != nil {
		return nil, cli.NotFound("resolving room alias %q: %w", roomAlias, err).
			WithHint("Check the room alias localpart and server name. " +
				"Run 'bureau matrix room list' to see available rooms.")
	}

	events, err := session.GetRoomState(ctx, roomID)
	if err != nil {
		return nil, cli.Internal("reading room state: %w", err)
	}

	var targets []updateTarget

	for _, event := range events {
		if event.Type != schema.EventTypeTemplate {
			continue
		}
		if event.StateKey == nil || *event.StateKey == "" {
			continue
		}

		// Unmarshal the Content map into TemplateContent.
		contentJSON, err := json.Marshal(event.Content)
		if err != nil {
			continue
		}
		var content schema.TemplateContent
		if err := json.Unmarshal(contentJSON, &content); err != nil {
			continue
		}

		templateRefString := roomLocalpart + ":" + *event.StateKey
		templateRef, err := schema.ParseTemplateRef(templateRefString)
		if err != nil {
			continue
		}

		targets = append(targets, updateTarget{
			name:       *event.StateKey,
			ref:        templateRef,
			content:    &content,
			serverName: serverName,
		})
	}

	return targets, nil
}

// checkTemplateUpdate checks a single template for updates from its origin.
func checkTemplateUpdate(ctx context.Context, target *updateTarget, system string, logger *slog.Logger) templateUpdateResult {
	result := templateUpdateResult{
		Name: target.name,
	}

	if target.content.Origin == nil {
		result.Status = "skipped"
		return result
	}

	origin := target.content.Origin

	var newContent schema.TemplateContent
	var newOrigin *ref.ContentOrigin

	switch {
	case origin.FlakeRef != "":
		result.Source = "flake"
		result.OldRevision = cli.ShortRevision(origin.ResolvedRev)
		result.OldHash = origin.ContentHash
		logger.Info("checking flake for updates",
			"template", target.name,
			"flake_ref", origin.FlakeRef,
			"current_rev", result.OldRevision,
		)

		resolvedSystem, err := cli.ResolveSystem(system)
		if err != nil {
			result.Status = "error"
			result.Error = err.Error()
			return result
		}
		source, err := cli.LoadFromFlake(ctx, origin.FlakeRef, "bureauTemplate."+resolvedSystem, logger)
		if err != nil {
			result.Status = "error"
			result.Error = err.Error()
			return result
		}
		if err := json.Unmarshal(source.Data, &newContent); err != nil {
			result.Status = "error"
			result.Error = fmt.Sprintf("parsing flake output: %v", err)
			return result
		}
		contentCopy := newContent
		contentCopy.Origin = nil
		hash, err := cli.ComputeContentHash(contentCopy)
		if err != nil {
			result.Status = "error"
			result.Error = fmt.Sprintf("computing content hash: %v", err)
			return result
		}
		source.Origin.ContentHash = hash
		newOrigin = source.Origin

	case origin.URL != "":
		result.Source = "url"
		result.OldHash = origin.ContentHash
		logger.Info("checking URL for updates",
			"template", target.name,
			"url", origin.URL,
		)

		source, err := cli.LoadFromURL(ctx, origin.URL, logger)
		if err != nil {
			result.Status = "error"
			result.Error = err.Error()
			return result
		}
		if err := json.Unmarshal(source.Data, &newContent); err != nil {
			result.Status = "error"
			result.Error = fmt.Sprintf("parsing URL content: %v", err)
			return result
		}
		contentCopy := newContent
		contentCopy.Origin = nil
		hash, err := cli.ComputeContentHash(contentCopy)
		if err != nil {
			result.Status = "error"
			result.Error = fmt.Sprintf("computing content hash: %v", err)
			return result
		}
		source.Origin.ContentHash = hash
		newOrigin = source.Origin

	default:
		result.Status = "skipped"
		return result
	}

	result.NewHash = newOrigin.ContentHash
	if newOrigin.ResolvedRev != "" {
		result.NewRevision = cli.ShortRevision(newOrigin.ResolvedRev)
	}

	if origin.ContentHash == newOrigin.ContentHash {
		result.Status = "current"
		return result
	}

	// Content has changed — classify the change.
	result.Status = "updated"
	result.Change, result.Fields = classifyTemplateChange(target.content, &newContent)

	// Set origin on the new content for publishing.
	newContent.Origin = newOrigin
	target.newContent = &newContent

	return result
}

// confirmUpdate prompts the user for confirmation to publish an update.
// Returns true if the user confirms.
func confirmUpdate(result *templateUpdateResult) bool {
	fmt.Fprintf(os.Stderr, "\n%s: update available\n", result.Name)
	fmt.Fprintf(os.Stderr, "  source: %s\n", result.Source)
	if result.OldRevision != "" && result.NewRevision != "" {
		fmt.Fprintf(os.Stderr, "  revision: %s → %s\n", result.OldRevision, result.NewRevision)
	}
	fmt.Fprintf(os.Stderr, "  content hash: %s → %s\n", result.OldHash, result.NewHash)
	if len(result.Fields) > 0 {
		fmt.Fprintf(os.Stderr, "  changed fields: %s\n", strings.Join(result.Fields, ", "))
	}
	if result.Change != "" {
		classification := result.Change
		if result.Change == "structural" {
			classification += " (restart required)"
		}
		fmt.Fprintf(os.Stderr, "  classification: %s\n", classification)
	}
	fmt.Fprintf(os.Stderr, "  Publish update? [y/N] ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
	return answer == "y" || answer == "yes"
}

// printUpdateResults formats and prints the update results for CLI output.
func printUpdateResults(results []templateUpdateResult) {
	var updated, current, skipped, errored int

	for index := range results {
		result := &results[index]

		switch result.Status {
		case "updated":
			if result.DryRun {
				fmt.Fprintf(os.Stdout, "%s: update available (dry-run)\n", result.Name)
			} else if result.EventID != "" {
				fmt.Fprintf(os.Stdout, "%s: updated (event: %s)\n", result.Name, result.EventID)
			} else {
				fmt.Fprintf(os.Stdout, "%s: skipped by user\n", result.Name)
			}
			if result.Source == "flake" && result.OldRevision != "" {
				fmt.Fprintf(os.Stdout, "  revision: %s → %s\n", result.OldRevision, result.NewRevision)
			}
			if len(result.Fields) > 0 {
				classification := result.Change
				if result.Change == "structural" {
					classification += " (restart required)"
				}
				fmt.Fprintf(os.Stdout, "  changed: %s (%s)\n", strings.Join(result.Fields, ", "), classification)
			}
			updated++
		case "current":
			fmt.Fprintf(os.Stdout, "%s: up to date\n", result.Name)
			current++
		case "skipped":
			if result.DryRun {
				skipped++
			} else {
				fmt.Fprintf(os.Stdout, "%s: skipped (no origin)\n", result.Name)
				skipped++
			}
		case "error":
			fmt.Fprintf(os.Stderr, "%s: error: %s\n", result.Name, result.Error)
			errored++
		}
	}

	total := len(results)
	fmt.Fprintf(os.Stdout, "\n%d checked", total)
	if updated > 0 {
		fmt.Fprintf(os.Stdout, ", %d updated", updated)
	}
	if current > 0 {
		fmt.Fprintf(os.Stdout, ", %d current", current)
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stdout, ", %d skipped", skipped)
	}
	if errored > 0 {
		fmt.Fprintf(os.Stdout, ", %d error(s)", errored)
	}
	fmt.Fprintln(os.Stdout)
}
