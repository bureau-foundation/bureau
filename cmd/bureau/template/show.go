// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"fmt"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
	libtmpl "github.com/bureau-foundation/bureau/lib/template"
)

// templateShowParams holds the parameters for the template show command.
type templateShowParams struct {
	cli.JSONOutput
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name for resolving room aliases" default:"bureau.local"`
	Raw        bool   `json:"raw"         flag:"raw"         desc:"show the template as stored, without resolving inheritance"`
}

// showCommand returns the "show" subcommand for displaying a template.
func showCommand() *cli.Command {
	var params templateShowParams

	return &cli.Command{
		Name:    "show",
		Summary: "Show a template (with inheritance resolved)",
		Description: `Display a sandbox template. By default, walks the full inheritance chain
and shows the merged result — what a sandbox would actually see.

Use --raw to show just the specific template as stored in Matrix, without
resolving inheritance. This is useful for seeing exactly what a child
template overrides versus what it inherits.`,
		Usage: "bureau template show [flags] <template-ref>",
		Examples: []cli.Example{
			{
				Description: "Show the fully resolved base-networked template",
				Command:     "bureau template show bureau/template:base-networked",
			},
			{
				Description: "Show the raw template (without inheritance resolution)",
				Command:     "bureau template show --raw bureau/template:base-networked",
			},
		},
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/template/show"},
		Annotations:    cli.ReadOnly(),
		Run: func(args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("usage: bureau template show [flags] <template-ref>")
			}

			templateRefString := args[0]

			ctx, cancel, session, err := cli.ConnectOperator()
			if err != nil {
				return err
			}
			defer cancel()

			if params.Raw {
				ref, err := schema.ParseTemplateRef(templateRefString)
				if err != nil {
					return fmt.Errorf("parsing template reference: %w", err)
				}
				content, err := libtmpl.Fetch(ctx, session, ref, params.ServerName)
				if err != nil {
					return err
				}
				return printTemplateJSON(content)
			}

			resolved, err := libtmpl.Resolve(ctx, session, templateRefString, params.ServerName)
			if err != nil {
				return err
			}
			return printTemplateJSON(resolved)
		},
	}
}
