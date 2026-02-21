// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// pipelineShowParams holds the parameters for the pipeline show command.
// PipelineRef is positional in CLI mode (args[0]) and a named property in
// JSON/MCP mode.
type pipelineShowParams struct {
	cli.JSONOutput
	PipelineRef string `json:"pipeline_ref" desc:"pipeline reference (e.g. bureau/pipeline:dev-workspace-init)" required:"true"`
	ServerName  string `json:"server_name"  flag:"server-name" desc:"Matrix server name for resolving room aliases" default:"bureau.local"`
}

// showCommand returns the "show" subcommand for displaying a pipeline.
func showCommand() *cli.Command {
	var params pipelineShowParams

	return &cli.Command{
		Name:    "show",
		Summary: "Show a pipeline definition",
		Description: `Display a pipeline's definition: description, variables, steps, and
log configuration. The pipeline is fetched from Matrix as an
m.bureau.pipeline state event and printed as formatted JSON.

Unlike templates, pipelines do not support inheritance — what you
see is what the executor runs.`,
		Usage: "bureau pipeline show [flags] <pipeline-ref>",
		Examples: []cli.Example{
			{
				Description: "Show the built-in workspace init pipeline",
				Command:     "bureau pipeline show bureau/pipeline:dev-workspace-init",
			},
		},
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/pipeline/show"},
		Annotations:    cli.ReadOnly(),
		Run: func(args []string) error {
			// In CLI mode, pipeline ref comes as a positional argument.
			// In JSON/MCP mode, it's populated from the JSON input.
			if len(args) == 1 {
				params.PipelineRef = args[0]
			} else if len(args) > 1 {
				return cli.Validation("usage: bureau pipeline show [flags] <pipeline-ref>")
			}
			if params.PipelineRef == "" {
				return cli.Validation("pipeline reference is required\n\nusage: bureau pipeline show [flags] <pipeline-ref>")
			}

			pipelineRef, err := schema.ParsePipelineRef(params.PipelineRef)
			if err != nil {
				return cli.Validation("parsing pipeline reference: %w", err)
			}

			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return fmt.Errorf("invalid --server-name: %w", err)
			}

			ctx, cancel, session, err := cli.ConnectOperator()
			if err != nil {
				return err
			}
			defer cancel()

			roomAlias := pipelineRef.RoomAlias(serverName)
			roomID, err := session.ResolveAlias(ctx, roomAlias)
			if err != nil {
				return cli.NotFound("resolving room alias %q: %w", roomAlias, err)
			}

			raw, err := session.GetStateEvent(ctx, roomID, schema.EventTypePipeline, pipelineRef.Pipeline)
			if err != nil {
				return cli.NotFound("fetching pipeline %q from %s: %w", pipelineRef.Pipeline, roomAlias, err)
			}

			var content schema.PipelineContent
			if err := json.Unmarshal(raw, &content); err != nil {
				return cli.Internal("parsing pipeline content: %w", err)
			}

			return printPipelineJSON(&content)
		},
	}
}
