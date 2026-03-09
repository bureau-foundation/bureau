// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

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
	"github.com/bureau-foundation/bureau/lib/pipelinedef"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	schemapipeline "github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/messaging"
)

// pipelineUpdateParams holds the parameters for the pipeline update command.
type pipelineUpdateParams struct {
	cli.SessionConfig
	cli.JSONOutput
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name for resolving room aliases (auto-detected from machine.conf)"`
	DryRun     bool   `json:"dry_run"     flag:"dry-run"     desc:"show what would change without publishing"`
	Yes        bool   `json:"yes"         flag:"yes"         desc:"skip confirmation prompts, publish all updates"`
}

// pipelineUpdateResult is the JSON output for one pipeline's update status.
type pipelineUpdateResult struct {
	Name        string `json:"name"                    desc:"pipeline name (state key)"`
	Status      string `json:"status"                  desc:"updated, current, skipped, or error"`
	Source      string `json:"source,omitempty"         desc:"source type: flake or url"`
	OldRevision string `json:"old_revision,omitempty"   desc:"previous git revision (flake only, short hash)"`
	NewRevision string `json:"new_revision,omitempty"   desc:"new git revision (flake only, short hash)"`
	OldHash     string `json:"old_hash,omitempty"       desc:"previous content hash"`
	NewHash     string `json:"new_hash,omitempty"       desc:"new content hash"`
	EventID     string `json:"event_id,omitempty"       desc:"Matrix event ID of published update"`
	Error       string `json:"error,omitempty"          desc:"error message if check failed"`
	DryRun      bool   `json:"dry_run"                  desc:"true if no publish was performed"`
}

// pipelineUpdateTarget holds a pipeline to check for updates.
type pipelineUpdateTarget struct {
	name       string
	ref        schema.PipelineRef
	content    *schemapipeline.PipelineContent
	serverName ref.ServerName
	newContent *schemapipeline.PipelineContent // populated if update available
}

// updateCommand returns the "update" subcommand for checking and applying
// pipeline updates from their original source (flake or URL).
func updateCommand() *cli.Command {
	var params pipelineUpdateParams

	return &cli.Command{
		Name:    "update",
		Summary: "Check for and apply pipeline updates from their source",
		Description: `Check pipelines for updates from their original source and optionally
publish the new versions. Pipelines published with --flake or --url have
origin metadata that records where they came from — this command
re-evaluates that source and compares the content hash.

If the argument is a pipeline reference (contains ':'), only that
pipeline is checked. If the argument is a room alias localpart (no ':'),
all pipelines in the room with origin metadata are checked.

Pipelines published from local files have no origin and are skipped.`,
		Usage: "bureau pipeline update [flags] <room-alias-localpart | pipeline-ref>",
		Examples: []cli.Example{
			{
				Description: "Check a single pipeline for updates",
				Command:     "bureau pipeline update bureau/pipeline:model-ingest",
			},
			{
				Description: "Check all pipelines in a room",
				Command:     "bureau pipeline update bureau/pipeline",
			},
			{
				Description: "Dry-run: show what would change without publishing",
				Command:     "bureau pipeline update --dry-run bureau/pipeline",
			},
			{
				Description: "Non-interactive: publish all updates without prompting",
				Command:     "bureau pipeline update --yes bureau/pipeline",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]pipelineUpdateResult{} },
		RequiredGrants: []string{"command/pipeline/update"},
		Annotations:    cli.Create(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) != 1 {
				return cli.Validation("usage: bureau pipeline update [flags] <room-alias-localpart | pipeline-ref>")
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
			singlePipeline := strings.Contains(argument, ":")

			var targets []pipelineUpdateTarget

			if singlePipeline {
				pipelineRef, err := schema.ParsePipelineRef(argument)
				if err != nil {
					return cli.Validation("invalid pipeline reference: %w", err).
						WithHint("Pipeline references have the form namespace/pipeline:name (e.g., bureau/pipeline:model-ingest).")
				}

				content, err := fetchPipeline(ctx, session, pipelineRef, serverName)
				if err != nil {
					return err
				}

				targets = []pipelineUpdateTarget{{
					name:       pipelineRef.Pipeline,
					ref:        pipelineRef,
					content:    content,
					serverName: serverName,
				}}
			} else {
				roomTargets, err := discoverPipelinesInRoom(ctx, session, argument, serverName)
				if err != nil {
					return err
				}
				targets = roomTargets
			}

			if len(targets) == 0 {
				logger.Info("no pipelines found", "target", argument)
				return nil
			}

			results := make([]pipelineUpdateResult, 0, len(targets))

			for index := range targets {
				result := checkPipelineUpdate(ctx, &targets[index], logger)
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
						if !confirmPipelineUpdate(&results[index]) {
							results[index].Status = "skipped"
							results[index].DryRun = true
							continue
						}
					}

					eventID, err := pipelinedef.Push(ctx, session, target.ref, *target.newContent, serverName)
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

			printPipelineUpdateResults(results)
			return nil
		},
	}
}

// fetchPipeline reads a single pipeline from Matrix by resolving the room
// alias and reading the state event.
func fetchPipeline(ctx context.Context, session messaging.Session, pipelineRef schema.PipelineRef, serverName ref.ServerName) (*schemapipeline.PipelineContent, error) {
	roomAlias := pipelineRef.RoomAlias(serverName)

	roomID, err := session.ResolveAlias(ctx, roomAlias)
	if err != nil {
		return nil, cli.NotFound("resolving room alias %q: %w", roomAlias, err).
			WithHint("Check the pipeline reference and server name.")
	}

	content, err := messaging.GetState[schemapipeline.PipelineContent](ctx, session, roomID, schema.EventTypePipeline, pipelineRef.Pipeline)
	if err != nil {
		return nil, cli.NotFound("reading pipeline %q from room %q: %w", pipelineRef.Pipeline, roomAlias, err).
			WithHint("Check that the pipeline exists. Run 'bureau pipeline list' to see available pipelines.")
	}

	return &content, nil
}

// discoverPipelinesInRoom reads all pipelines from a room and returns
// those eligible for update checking.
func discoverPipelinesInRoom(ctx context.Context, session messaging.Session, roomLocalpart string, serverName ref.ServerName) ([]pipelineUpdateTarget, error) {
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

	var targets []pipelineUpdateTarget

	for _, event := range events {
		if event.Type != schema.EventTypePipeline {
			continue
		}
		if event.StateKey == nil || *event.StateKey == "" {
			continue
		}

		contentJSON, err := json.Marshal(event.Content)
		if err != nil {
			continue
		}
		var content schemapipeline.PipelineContent
		if err := json.Unmarshal(contentJSON, &content); err != nil {
			continue
		}

		pipelineRefString := roomLocalpart + ":" + *event.StateKey
		pipelineRef, err := schema.ParsePipelineRef(pipelineRefString)
		if err != nil {
			continue
		}

		targets = append(targets, pipelineUpdateTarget{
			name:       *event.StateKey,
			ref:        pipelineRef,
			content:    &content,
			serverName: serverName,
		})
	}

	return targets, nil
}

// checkPipelineUpdate checks a single pipeline for updates from its origin.
func checkPipelineUpdate(ctx context.Context, target *pipelineUpdateTarget, logger *slog.Logger) pipelineUpdateResult {
	result := pipelineUpdateResult{
		Name: target.name,
	}

	if target.content.Origin == nil {
		result.Status = "skipped"
		return result
	}

	origin := target.content.Origin

	var newContent schemapipeline.PipelineContent
	var newOrigin *ref.ContentOrigin

	switch {
	case origin.FlakeRef != "":
		result.Source = "flake"
		result.OldRevision = cli.ShortRevision(origin.ResolvedRev)
		result.OldHash = origin.ContentHash
		logger.Info("checking flake for updates",
			"pipeline", target.name,
			"flake_ref", origin.FlakeRef,
			"current_rev", result.OldRevision,
		)

		// Pipelines are system-agnostic — use "bureauPipeline" directly.
		source, err := cli.LoadFromFlake(ctx, origin.FlakeRef, "bureauPipeline", logger)
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
			"pipeline", target.name,
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

	// Content has changed. All pipeline changes are structural — the
	// automation definition changed, unlike templates where some
	// changes (metadata-only) don't require sandbox restarts.
	result.Status = "updated"

	newContent.Origin = newOrigin
	target.newContent = &newContent

	return result
}

// confirmPipelineUpdate prompts the user for confirmation to publish a
// pipeline update. Returns true if the user confirms.
func confirmPipelineUpdate(result *pipelineUpdateResult) bool {
	fmt.Fprintf(os.Stderr, "\n%s: update available\n", result.Name)
	fmt.Fprintf(os.Stderr, "  source: %s\n", result.Source)
	if result.OldRevision != "" && result.NewRevision != "" {
		fmt.Fprintf(os.Stderr, "  revision: %s → %s\n", result.OldRevision, result.NewRevision)
	}
	fmt.Fprintf(os.Stderr, "  content hash: %s → %s\n", result.OldHash, result.NewHash)
	fmt.Fprintf(os.Stderr, "  Publish update? [y/N] ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
	return answer == "y" || answer == "yes"
}

// printPipelineUpdateResults formats and prints the update results for CLI output.
func printPipelineUpdateResults(results []pipelineUpdateResult) {
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
