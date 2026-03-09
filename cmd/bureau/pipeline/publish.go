// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/pipelinedef"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	schemapipeline "github.com/bureau-foundation/bureau/lib/schema/pipeline"
)

// pipelinePublishParams holds the parameters for the pipeline publish command.
type pipelinePublishParams struct {
	cli.SessionConfig
	cli.JSONOutput
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name for resolving room aliases (auto-detected from machine.conf)"`
	FlakeRef   string `json:"flake_ref"   flag:"flake"       desc:"Nix flake reference (e.g., github:bureau-foundation/bureau-hf/v1.0.0)"`
	URL        string `json:"url"         flag:"url"         desc:"URL to fetch pipeline JSONC from (HTTPS required)"`
	File       string `json:"file"        flag:"file"        desc:"local pipeline JSONC file path"`
	DryRun     bool   `json:"dry_run"     flag:"dry-run"     desc:"validate only, do not publish to Matrix"`
}

// pipelinePublishResult is the JSON output for pipeline publish.
type pipelinePublishResult struct {
	Ref          string             `json:"ref"                desc:"pipeline reference (state key)"`
	Source       string             `json:"source"             desc:"source type: flake, url, or file"`
	SourceRef    string             `json:"source_ref"         desc:"source reference (flake ref, URL, or file path)"`
	RoomAlias    ref.RoomAlias      `json:"room_alias"         desc:"target room alias"`
	RoomID       ref.RoomID         `json:"room_id,omitempty"  desc:"target room Matrix ID"`
	PipelineName string             `json:"pipeline_name"      desc:"pipeline name"`
	EventID      ref.EventID        `json:"event_id,omitempty" desc:"created state event ID"`
	Origin       *ref.ContentOrigin `json:"origin,omitempty"   desc:"recorded origin for update tracking"`
	DryRun       bool               `json:"dry_run"            desc:"true if publish was simulated"`
}

// publishCommand returns the "publish" subcommand for publishing pipelines
// from flake references, URLs, or local files.
func publishCommand() *cli.Command {
	var params pipelinePublishParams

	return &cli.Command{
		Name:    "publish",
		Summary: "Publish a pipeline to Matrix from a flake, URL, or file",
		Description: `Publish a pipeline to Matrix from one of three sources:

  --flake <ref>   Evaluate a Nix flake, read its bureauPipeline output,
                  and publish with origin tracking. The flake reference
                  is recorded so 'bureau pipeline update' can check for
                  newer versions.

  --url <url>     Fetch pipeline JSONC from a URL, validate, and publish
                  with origin tracking.

  --file <path>   Read pipeline JSONC from a local file, validate, and
                  publish. Equivalent to 'bureau pipeline push'.

Exactly one of --flake, --url, or --file must be specified. The pipeline
reference (positional argument) specifies which room and state key to
publish to.

For flake sources, the bureauPipeline output must be an attribute set
using the same field names as the PipelineContent JSON wire format
(snake_case). Pipelines are system-agnostic — no per-system attribute
is needed (tools come from the template/environment, not the pipeline).`,
		Usage: "bureau pipeline publish [flags] <pipeline-ref>",
		Examples: []cli.Example{
			{
				Description: "Publish from a Nix flake",
				Command:     "bureau pipeline publish --flake github:bureau-foundation/bureau-hf/v1.0.0 bureau/pipeline:model-ingest",
			},
			{
				Description: "Publish from a URL",
				Command:     "bureau pipeline publish --url https://raw.githubusercontent.com/.../pipeline.jsonc bureau/pipeline:model-ingest",
			},
			{
				Description: "Publish from a local file",
				Command:     "bureau pipeline publish --file ./pipeline.jsonc bureau/pipeline:model-ingest",
			},
			{
				Description: "Dry-run: validate without publishing",
				Command:     "bureau pipeline publish --flake github:bureau-foundation/bureau-hf --dry-run bureau/pipeline:model-ingest",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &pipelinePublishResult{} },
		RequiredGrants: []string{"command/pipeline/publish"},
		Annotations:    cli.Create(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) != 1 {
				return cli.Validation("usage: bureau pipeline publish [flags] <pipeline-ref>")
			}

			pipelineRef, err := schema.ParsePipelineRef(args[0])
			if err != nil {
				return cli.Validation("invalid pipeline reference: %w", err).
					WithHint("Pipeline references have the form namespace/pipeline:name (e.g., bureau/pipeline:model-ingest).")
			}

			// Validate exactly one source is specified.
			sourceCount := 0
			if params.FlakeRef != "" {
				sourceCount++
			}
			if params.URL != "" {
				sourceCount++
			}
			if params.File != "" {
				sourceCount++
			}
			if sourceCount != 1 {
				return cli.Validation("exactly one of --flake, --url, or --file must be specified")
			}

			// Load content from the specified source.
			var source *cli.SourceContent

			switch {
			case params.FlakeRef != "":
				// Pipelines are system-agnostic — the flake attr is
				// "bureauPipeline" (no per-system qualifier).
				source, err = cli.LoadFromFlake(ctx, params.FlakeRef, "bureauPipeline", logger)
				if err != nil {
					return err
				}
			case params.URL != "":
				source, err = cli.LoadFromURL(ctx, params.URL, logger)
				if err != nil {
					return err
				}
			case params.File != "":
				source, err = cli.LoadFromFile(params.File)
				if err != nil {
					return err
				}
			}

			// Unmarshal into the typed PipelineContent.
			var content schemapipeline.PipelineContent
			if err := json.Unmarshal(source.Data, &content); err != nil {
				return cli.Internal("parsing %s output as PipelineContent: %w", source.Source, err).
					WithHint("The content must use snake_case field names matching the PipelineContent JSON wire format.")
			}

			// Compute content hash and set origin.
			if source.Origin != nil {
				contentCopy := content
				contentCopy.Origin = nil
				hash, err := cli.ComputeContentHash(contentCopy)
				if err != nil {
					return cli.Internal("computing content hash: %w", err)
				}
				source.Origin.ContentHash = hash
			}
			content.Origin = source.Origin

			// Validate the pipeline content.
			issues := pipelinedef.Validate(&content)
			if len(issues) > 0 {
				for _, issue := range issues {
					logger.Warn("validation issue", "issue", issue)
				}
				return cli.Validation("%s: %d validation issue(s) found", source.Ref, len(issues))
			}

			// Connect to Matrix.
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
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

			// Dry-run: resolve room and verify it exists without publishing.
			if params.DryRun {
				roomAlias := pipelineRef.RoomAlias(serverName)
				roomID, err := session.ResolveAlias(ctx, roomAlias)
				if err != nil {
					return cli.NotFound("resolving target room %q: %w", roomAlias, err).
						WithHint("The pipeline room must exist before publishing. " +
							"Run 'bureau matrix setup' to create standard rooms, or check the namespace.")
				}

				if done, err := params.EmitJSON(pipelinePublishResult{
					Ref:          pipelineRef.String(),
					Source:       source.Source,
					SourceRef:    source.Ref,
					RoomAlias:    roomAlias,
					RoomID:       roomID,
					PipelineName: pipelineRef.Pipeline,
					Origin:       source.Origin,
					DryRun:       true,
				}); done {
					return err
				}
				fmt.Fprintf(os.Stdout, "%s: valid (dry-run, not published)\n", source.Ref)
				fmt.Fprintf(os.Stdout, "  source: %s\n", source.Source)
				fmt.Fprintf(os.Stdout, "  target room: %s (%s)\n", roomAlias, roomID)
				fmt.Fprintf(os.Stdout, "  pipeline name: %s\n", pipelineRef.Pipeline)
				if source.Origin != nil {
					fmt.Fprintf(os.Stdout, "  origin: %s\n", cli.OriginSummary(source.Origin))
				}
				return nil
			}

			// Publish the pipeline.
			result, err := pipelinedef.Push(ctx, session, pipelineRef, content, serverName)
			if err != nil {
				return cli.Internal("publishing pipeline: %w", err)
			}

			if done, err := params.EmitJSON(pipelinePublishResult{
				Ref:          pipelineRef.String(),
				Source:       source.Source,
				SourceRef:    source.Ref,
				RoomAlias:    result.RoomAlias,
				RoomID:       result.RoomID,
				PipelineName: pipelineRef.Pipeline,
				EventID:      result.EventID,
				Origin:       source.Origin,
				DryRun:       false,
			}); done {
				return err
			}

			fmt.Fprintf(os.Stdout, "published %s to %s (event: %s)\n", pipelineRef.String(), result.RoomAlias, result.EventID)
			if source.Origin != nil {
				fmt.Fprintf(os.Stdout, "  origin: %s\n", cli.OriginSummary(source.Origin))
			}
			return nil
		},
	}
}
