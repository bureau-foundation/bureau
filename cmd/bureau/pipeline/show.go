// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// showCommand returns the "show" subcommand for displaying a pipeline.
func showCommand() *cli.Command {
	var serverName string

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
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("show", pflag.ContinueOnError)
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name for resolving room aliases")
			return flagSet
		},
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

			roomAlias := ref.RoomAlias(serverName)
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
