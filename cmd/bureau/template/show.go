// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	libtmpl "github.com/bureau-foundation/bureau/lib/templatedef"
)

// templateShowParams holds the parameters for the template show command.
// TemplateRef is positional in CLI mode (args[0]) and a named property in
// JSON/MCP mode.
type templateShowParams struct {
	cli.JSONOutput
	TemplateRef string `json:"template_ref" desc:"template reference (e.g. bureau/template:base-networked)" required:"true"`
	ServerName  string `json:"server_name"  flag:"server-name" desc:"Matrix server name for resolving room aliases" default:"bureau.local"`
	Raw         bool   `json:"raw"          flag:"raw"         desc:"show the template as stored, without resolving inheritance"`
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
		Run: func(ctx context.Context, args []string, _ *slog.Logger) error {
			// In CLI mode, template ref comes as a positional argument.
			// In JSON/MCP mode, it's populated from the JSON input.
			if len(args) == 1 {
				params.TemplateRef = args[0]
			} else if len(args) > 1 {
				return cli.Validation("usage: bureau template show [flags] <template-ref>")
			}
			if params.TemplateRef == "" {
				return cli.Validation("template reference is required\n\nusage: bureau template show [flags] <template-ref>")
			}

			templateRefString := params.TemplateRef

			ctx, cancel, session, err := cli.ConnectOperator(ctx)
			if err != nil {
				return err
			}
			defer cancel()

			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return fmt.Errorf("invalid --server-name: %w", err)
			}

			if params.Raw {
				templateRef, err := schema.ParseTemplateRef(templateRefString)
				if err != nil {
					return cli.Validation("parsing template reference: %w", err)
				}
				content, err := libtmpl.Fetch(ctx, session, templateRef, serverName)
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
