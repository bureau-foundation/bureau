// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// pipelineShowParams holds the parameters for the pipeline show command.
type pipelineShowParams struct {
	cli.JSONOutput
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name for resolving room aliases" default:"bureau.local"`
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
			if len(args) != 1 {
				return fmt.Errorf("usage: bureau pipeline show [flags] <pipeline-ref>")
			}

			ref, err := schema.ParsePipelineRef(args[0])
			if err != nil {
				return fmt.Errorf("parsing pipeline reference: %w", err)
			}

			ctx, cancel, session, err := cli.ConnectOperator()
			if err != nil {
				return err
			}
			defer cancel()

			roomAlias := ref.RoomAlias(params.ServerName)
			roomID, err := session.ResolveAlias(ctx, roomAlias)
			if err != nil {
				return fmt.Errorf("resolving room alias %q: %w", roomAlias, err)
			}

			raw, err := session.GetStateEvent(ctx, roomID, schema.EventTypePipeline, ref.Pipeline)
			if err != nil {
				return fmt.Errorf("fetching pipeline %q from %s: %w", ref.Pipeline, roomAlias, err)
			}

			var content schema.PipelineContent
			if err := json.Unmarshal(raw, &content); err != nil {
				return fmt.Errorf("parsing pipeline content: %w", err)
			}

			return printPipelineJSON(&content)
		},
	}
}
