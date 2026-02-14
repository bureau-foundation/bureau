// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// Command returns the "pipeline" command group.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "pipeline",
		Summary: "Manage automation pipelines",
		Description: `Manage Bureau automation pipelines stored as Matrix state events.

Pipelines define structured sequences of steps — shell commands, Matrix
state event publications, and interactive sessions — that run inside
Bureau sandboxes. They are the automation primitive for workspace setup,
service lifecycle, maintenance, and deployment.

Pipeline references use the format:

  <room-alias-localpart>:<pipeline-name>

For example: "bureau/pipeline:dev-workspace-init", "iree/pipeline:deploy".

Pipeline files use JSONC (JSON with comments): the same format stored in
Matrix state events, plus // line comments and /* block comments */ for
documentation.

All commands that access Matrix require an operator session. Run
"bureau login" first to authenticate.`,
		Subcommands: []*cli.Command{
			listCommand(),
			showCommand(),
			validateCommand(),
			pushCommand(),
			executeCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "List pipelines in the built-in pipeline room",
				Command:     "bureau pipeline list bureau/pipeline",
			},
			{
				Description: "Show a pipeline's definition",
				Command:     "bureau pipeline show bureau/pipeline:dev-workspace-init",
			},
			{
				Description: "Validate a local pipeline file",
				Command:     "bureau pipeline validate my-pipeline.jsonc",
			},
			{
				Description: "Push a local pipeline to Matrix",
				Command:     "bureau pipeline push bureau/pipeline:my-pipeline my-pipeline.jsonc",
			},
			{
				Description: "Execute a pipeline on a machine",
				Command:     "bureau pipeline execute bureau/pipeline:dev-workspace-init --machine machine/workstation --param PROJECT=iree",
			},
		},
	}
}

// printPipelineJSON marshals a PipelineContent as indented JSON to stdout.
func printPipelineJSON(content any) error {
	return cli.WriteJSON(content)
}
