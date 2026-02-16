// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
	libtmpl "github.com/bureau-foundation/bureau/lib/template"
)

// templatePushParams holds the parameters for the template push command.
type templatePushParams struct {
	cli.SessionConfig
	cli.JSONOutput
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name for resolving room aliases" default:"bureau.local"`
	DryRun     bool   `json:"dry_run"     flag:"dry-run"     desc:"validate only, do not publish to Matrix"`
}

// templatePushResult is the JSON output for template push.
type templatePushResult struct {
	Ref          string `json:"ref"                    desc:"template reference (state key)"`
	File         string `json:"file"                   desc:"source template file path"`
	RoomAlias    string `json:"room_alias"             desc:"target room alias"`
	RoomID       string `json:"room_id,omitempty"      desc:"target room Matrix ID"`
	TemplateName string `json:"template_name"          desc:"template name"`
	EventID      string `json:"event_id,omitempty"     desc:"created state event ID"`
	DryRun       bool   `json:"dry_run"                desc:"true if push was simulated"`
}

// pushCommand returns the "push" subcommand for publishing a template to Matrix.
func pushCommand() *cli.Command {
	var params templatePushParams

	return &cli.Command{
		Name:    "push",
		Summary: "Publish a local template to Matrix",
		Description: `Read a template definition from a local JSONC file, validate it, and
publish it as an m.bureau.template state event in Matrix. The template
reference specifies which room and state key to use. Comments are
stripped before publishing.

If the template inherits from a parent, the parent's existence in Matrix
is verified before publishing (unless --dry-run is used, which only
performs local validation).

Use --dry-run to validate the file and check that inheritance targets
exist without actually publishing.`,
		Usage: "bureau template push [flags] <template-ref> <file>",
		Examples: []cli.Example{
			{
				Description: "Push a template to Matrix",
				Command:     "bureau template push --credential-file ./creds iree/template:amdgpu-developer agent.json",
			},
			{
				Description: "Dry-run: validate and check inheritance without publishing",
				Command:     "bureau template push --credential-file ./creds --dry-run iree/template:amdgpu-developer agent.json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &templatePushResult{} },
		RequiredGrants: []string{"command/template/push"},
		Annotations:    cli.Create(),
		Run: func(args []string) error {
			if len(args) != 2 {
				return cli.Validation("usage: bureau template push [flags] <template-ref> <file>")
			}

			templateRefString := args[0]
			filePath := args[1]

			// Parse the template reference.
			ref, err := schema.ParseTemplateRef(templateRefString)
			if err != nil {
				return cli.Validation("parsing template reference: %w", err)
			}

			// Read and validate the local file.
			content, err := readTemplateFile(filePath)
			if err != nil {
				return err
			}

			issues := validateTemplateContent(content)
			if len(issues) > 0 {
				for _, issue := range issues {
					fmt.Fprintf(os.Stderr, "  - %s\n", issue)
				}
				return cli.Validation("%s: %d validation issue(s) found", filePath, len(issues))
			}

			// Connect to Matrix for inheritance verification and publishing.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			session, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}

			// Dry-run: resolve room and verify inheritance without publishing.
			if params.DryRun {
				roomAlias := ref.RoomAlias(params.ServerName)
				roomID, err := session.ResolveAlias(ctx, roomAlias)
				if err != nil {
					return cli.NotFound("resolving target room %q: %w", roomAlias, err)
				}

				for index, parentRefString := range content.Inherits {
					parentRef, err := schema.ParseTemplateRef(parentRefString)
					if err != nil {
						return cli.Validation("inherits[%d] reference %q is invalid: %w", index, parentRefString, err)
					}
					if _, err := libtmpl.Fetch(ctx, session, parentRef, params.ServerName); err != nil {
						return cli.NotFound("parent template %q not found in Matrix: %w", parentRefString, err)
					}
					fmt.Fprintf(os.Stderr, "parent template %q: found\n", parentRefString)
				}

				if done, err := params.EmitJSON(templatePushResult{
					Ref:          ref.String(),
					File:         filePath,
					RoomAlias:    roomAlias,
					RoomID:       roomID,
					TemplateName: ref.Template,
					DryRun:       true,
				}); done {
					return err
				}
				fmt.Fprintf(os.Stdout, "%s: valid (dry-run, not published)\n", filePath)
				fmt.Fprintf(os.Stdout, "  target room: %s (%s)\n", roomAlias, roomID)
				fmt.Fprintf(os.Stdout, "  template name: %s\n", ref.Template)
				return nil
			}

			// Publish the template via the library function.
			result, err := libtmpl.Push(ctx, session, ref, *content, params.ServerName)
			if err != nil {
				return cli.Internal("publishing template: %w", err)
			}

			if done, err := params.EmitJSON(templatePushResult{
				Ref:          ref.String(),
				File:         filePath,
				RoomAlias:    result.RoomAlias,
				RoomID:       result.RoomID,
				TemplateName: ref.Template,
				EventID:      result.EventID,
				DryRun:       false,
			}); done {
				return err
			}

			fmt.Fprintf(os.Stdout, "published %s to %s (event: %s)\n", ref.String(), result.RoomAlias, result.EventID)
			return nil
		},
	}
}
