// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	libpipeline "github.com/bureau-foundation/bureau/lib/pipeline"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// pipelinePushParams holds the parameters for the pipeline push command.
type pipelinePushParams struct {
	cli.JSONOutput
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name for resolving room aliases" default:"bureau.local"`
	DryRun     bool   `json:"dry_run"     flag:"dry-run"     desc:"validate only, do not publish to Matrix"`
}

// pushResult is the JSON output for pipeline push.
type pushResult struct {
	Ref          string `json:"ref"`
	File         string `json:"file"`
	RoomAlias    string `json:"room_alias"`
	RoomID       string `json:"room_id,omitempty"`
	PipelineName string `json:"pipeline_name"`
	EventID      string `json:"event_id,omitempty"`
	DryRun       bool   `json:"dry_run"`
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
		RequiredGrants: []string{"command/pipeline/push"},
		Run: func(args []string) error {
			if len(args) != 2 {
				return fmt.Errorf("usage: bureau pipeline push [flags] <pipeline-ref> <file>")
			}

			pipelineRefString := args[0]
			filePath := args[1]

			// Parse the pipeline reference.
			ref, err := schema.ParsePipelineRef(pipelineRefString)
			if err != nil {
				return fmt.Errorf("parsing pipeline reference: %w", err)
			}

			// Read and validate the local file.
			content, err := libpipeline.ReadFile(filePath)
			if err != nil {
				return err
			}

			issues := libpipeline.Validate(content)
			if len(issues) > 0 {
				for _, issue := range issues {
					fmt.Fprintf(os.Stderr, "  - %s\n", issue)
				}
				return fmt.Errorf("%s: %d validation issue(s) found", filePath, len(issues))
			}

			// Connect to Matrix for room verification and publishing.
			ctx, cancel, session, err := cli.ConnectOperator()
			if err != nil {
				return err
			}
			defer cancel()

			// Verify the target room exists.
			roomAlias := principal.RoomAlias(ref.Room, params.ServerName)
			roomID, err := session.ResolveAlias(ctx, roomAlias)
			if err != nil {
				return fmt.Errorf("resolving target room %q: %w", roomAlias, err)
			}

			if params.DryRun {
				if done, err := params.EmitJSON(pushResult{
					Ref:          ref.String(),
					File:         filePath,
					RoomAlias:    roomAlias,
					RoomID:       roomID,
					PipelineName: ref.Pipeline,
					DryRun:       true,
				}); done {
					return err
				}
				fmt.Fprintf(os.Stdout, "%s: valid (dry-run, not published)\n", filePath)
				fmt.Fprintf(os.Stdout, "  target room: %s (%s)\n", roomAlias, roomID)
				fmt.Fprintf(os.Stdout, "  pipeline name: %s\n", ref.Pipeline)
				return nil
			}

			// Publish the pipeline as a state event.
			eventID, err := session.SendStateEvent(ctx, roomID, schema.EventTypePipeline, ref.Pipeline, content)
			if err != nil {
				return fmt.Errorf("publishing pipeline: %w", err)
			}

			if done, err := params.EmitJSON(pushResult{
				Ref:          ref.String(),
				File:         filePath,
				RoomAlias:    roomAlias,
				RoomID:       roomID,
				PipelineName: ref.Pipeline,
				EventID:      eventID,
				DryRun:       false,
			}); done {
				return err
			}

			fmt.Fprintf(os.Stdout, "published %s to %s (event: %s)\n", ref.String(), roomAlias, eventID)
			return nil
		},
	}
}
