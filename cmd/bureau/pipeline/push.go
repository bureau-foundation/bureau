// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/pipelinedef"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// pipelinePushParams holds the parameters for the pipeline push command.
// PipelineRef and File are positional in CLI mode (args[0], args[1]) and
// named properties in JSON/MCP mode.
type pipelinePushParams struct {
	cli.JSONOutput
	PipelineRef string `json:"pipeline_ref" desc:"pipeline reference (e.g. bureau/pipeline:my-pipeline)" required:"true"`
	File        string `json:"file"         desc:"path to local JSONC pipeline definition file" required:"true"`
	ServerName  string `json:"server_name"  flag:"server-name" desc:"Matrix server name for resolving room aliases" default:"bureau.local"`
	DryRun      bool   `json:"dry_run"      flag:"dry-run"     desc:"validate only, do not publish to Matrix"`
}

// pushResult is the JSON output for pipeline push.
type pushResult struct {
	Ref          string        `json:"ref"                    desc:"pipeline reference (state key)"`
	File         string        `json:"file"                   desc:"source pipeline file path"`
	RoomAlias    ref.RoomAlias `json:"room_alias"             desc:"target room alias"`
	RoomID       ref.RoomID    `json:"room_id,omitempty"      desc:"target room Matrix ID"`
	PipelineName string        `json:"pipeline_name"          desc:"pipeline name"`
	EventID      ref.EventID   `json:"event_id,omitempty"     desc:"created state event ID"`
	DryRun       bool          `json:"dry_run"                desc:"true if push was simulated"`
}

// pushCommand returns the "push" subcommand for publishing a pipeline to Matrix.
func pushCommand() *cli.Command {
	var params pipelinePushParams

	return &cli.Command{
		Name:    "push",
		Summary: "Publish a local pipeline to Matrix",
		Description: `Read a pipeline definition from a local JSONC file, validate it, and
publish it as an m.bureau.pipeline state event in Matrix. The pipeline
reference specifies which room and state key to use. Comments are
stripped before publishing.

Use --dry-run to validate the file and verify the target room exists
without actually publishing.`,
		Usage: "bureau pipeline push [flags] <pipeline-ref> <file>",
		Examples: []cli.Example{
			{
				Description: "Push a pipeline to the built-in pipeline room",
				Command:     "bureau pipeline push bureau/pipeline:my-pipeline my-pipeline.jsonc",
			},
			{
				Description: "Dry-run: validate and check target room without publishing",
				Command:     "bureau pipeline push --dry-run bureau/pipeline:my-pipeline my-pipeline.jsonc",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &pushResult{} },
		RequiredGrants: []string{"command/pipeline/push"},
		Annotations:    cli.Create(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			// In CLI mode, pipeline ref and file come as positional arguments.
			// In JSON/MCP mode, they're populated from the JSON input.
			switch len(args) {
			case 0:
				// MCP path: params already populated from JSON.
			case 2:
				params.PipelineRef = args[0]
				params.File = args[1]
			default:
				return cli.Validation("usage: bureau pipeline push [flags] <pipeline-ref> <file>")
			}
			if params.PipelineRef == "" {
				return cli.Validation("pipeline reference is required\n\nusage: bureau pipeline push [flags] <pipeline-ref> <file>")
			}
			if params.File == "" {
				return cli.Validation("file path is required\n\nusage: bureau pipeline push [flags] <pipeline-ref> <file>")
			}

			pipelineRefString := params.PipelineRef
			filePath := params.File

			// Parse the pipeline reference.
			pipelineRef, err := schema.ParsePipelineRef(pipelineRefString)
			if err != nil {
				return cli.Validation("parsing pipeline reference: %w", err)
			}

			// Read and validate the local file.
			content, err := pipelinedef.ReadFile(filePath)
			if err != nil {
				return err
			}

			issues := pipelinedef.Validate(content)
			if len(issues) > 0 {
				for _, issue := range issues {
					logger.Warn("validation issue", "issue", issue)
				}
				return cli.Validation("%s: %d validation issue(s) found", filePath, len(issues))
			}

			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return fmt.Errorf("invalid --server-name: %w", err)
			}

			// Connect to Matrix for room verification and publishing.
			ctx, cancel, session, err := cli.ConnectOperator(ctx)
			if err != nil {
				return err
			}
			defer cancel()

			if params.DryRun {
				// Dry-run: verify target room exists without publishing.
				roomAlias := pipelineRef.RoomAlias(serverName)
				roomID, err := session.ResolveAlias(ctx, roomAlias)
				if err != nil {
					return cli.NotFound("resolving target room %q: %w", roomAlias, err)
				}

				if done, err := params.EmitJSON(pushResult{
					Ref:          pipelineRef.String(),
					File:         filePath,
					RoomAlias:    roomAlias,
					RoomID:       roomID,
					PipelineName: pipelineRef.Pipeline,
					DryRun:       true,
				}); done {
					return err
				}
				fmt.Fprintf(os.Stdout, "%s: valid (dry-run, not published)\n", filePath)
				fmt.Fprintf(os.Stdout, "  target room: %s (%s)\n", roomAlias, roomID)
				fmt.Fprintf(os.Stdout, "  pipeline name: %s\n", pipelineRef.Pipeline)
				return nil
			}

			result, err := pipelinedef.Push(ctx, session, pipelineRef, *content, serverName)
			if err != nil {
				return cli.Internal("publishing pipeline: %w", err)
			}

			if done, err := params.EmitJSON(pushResult{
				Ref:          pipelineRef.String(),
				File:         filePath,
				RoomAlias:    result.RoomAlias,
				RoomID:       result.RoomID,
				PipelineName: pipelineRef.Pipeline,
				EventID:      result.EventID,
				DryRun:       false,
			}); done {
				return err
			}

			fmt.Fprintf(os.Stdout, "published %s to %s (event: %s)\n", pipelineRef.String(), result.RoomAlias, result.EventID)
			return nil
		},
	}
}
