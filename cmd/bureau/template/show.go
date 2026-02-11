// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"fmt"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
	libtmpl "github.com/bureau-foundation/bureau/lib/template"
)

// showCommand returns the "show" subcommand for displaying a template.
func showCommand() *cli.Command {
	var (
		serverName string
		raw        bool
	)

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
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("show", pflag.ContinueOnError)
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name for resolving room aliases")
			flagSet.BoolVar(&raw, "raw", false, "show the template as stored, without resolving inheritance")
			return flagSet
		},
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

			if raw {
				ref, err := schema.ParseTemplateRef(templateRefString)
				if err != nil {
					return fmt.Errorf("parsing template reference: %w", err)
				}
				content, err := libtmpl.Fetch(ctx, session, ref, serverName)
				if err != nil {
					return err
				}
				return printTemplateJSON(content)
			}

			resolved, err := libtmpl.Resolve(ctx, session, templateRefString, serverName)
			if err != nil {
				return err
			}
			return printTemplateJSON(resolved)
		},
	}
}
