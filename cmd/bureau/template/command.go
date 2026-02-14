// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// Command returns the "template" command group.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "template",
		Summary: "Manage sandbox templates",
		Description: `Manage Bureau sandbox templates stored as Matrix state events.

Templates define the sandbox configuration for agent processes: command to
run, filesystem mounts, namespace isolation, resource limits, security
options, environment variables, and agent payload. Templates support
multi-level inheritance — a child template inherits from a parent and
overrides specific fields.

Template references use the format:

  <room-alias-localpart>:<template-name>

For example: "bureau/template:base", "iree/template:amdgpu-developer".

Template files use JSONC (JSON with comments): the same format stored in
Matrix state events, plus // line comments and /* block comments */ for
documentation. Use "bureau template show --raw" to export a template for
editing.

All commands that access Matrix require an operator session. Run
"bureau login" first to authenticate.`,
		Subcommands: []*cli.Command{
			listCommand(),
			showCommand(),
			pushCommand(),
			validateCommand(),
			diffCommand(),
			impactCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "List templates in the built-in templates room",
				Command:     "bureau template list bureau/template",
			},
			{
				Description: "Show the fully resolved base-networked template (with inheritance)",
				Command:     "bureau template show bureau/template:base-networked",
			},
			{
				Description: "Show a template without resolving inheritance",
				Command:     "bureau template show --raw bureau/template:base-networked",
			},
			{
				Description: "Validate a local template file",
				Command:     "bureau template validate my-agent.json",
			},
			{
				Description: "Push a local template to Matrix",
				Command:     "bureau template push bureau/template:my-agent my-agent.json",
			},
			{
				Description: "Diff a Matrix template against a local file",
				Command:     "bureau template diff bureau/template:my-agent my-agent.json",
			},
			{
				Description: "Show which principals would be affected by a template change",
				Command:     "bureau template impact bureau/template:base",
			},
			{
				Description: "Classify changes before pushing a modified template",
				Command:     "bureau template impact bureau/template:my-agent my-agent.json",
			},
		},
	}
}

// printTemplateJSON marshals a TemplateContent as indented JSON to stdout.
func printTemplateJSON(content any) error {
	return cli.WriteJSON(content)
}
