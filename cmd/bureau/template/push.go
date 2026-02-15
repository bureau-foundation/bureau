// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"fmt"
	"os"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	libtmpl "github.com/bureau-foundation/bureau/lib/template"
)

// templatePushParams holds the parameters for the template push command.
type templatePushParams struct {
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name for resolving room aliases" default:"bureau.local"`
	DryRun     bool   `json:"dry_run"     flag:"dry-run"     desc:"validate only, do not publish to Matrix"`
	OutputJSON bool   `json:"-"           flag:"json"         desc:"output as JSON"`
}

// templatePushResult is the JSON output for template push.
type templatePushResult struct {
	Ref          string `json:"ref"`
	File         string `json:"file"`
	RoomAlias    string `json:"room_alias"`
	RoomID       string `json:"room_id,omitempty"`
	TemplateName string `json:"template_name"`
	EventID      string `json:"event_id,omitempty"`
	DryRun       bool   `json:"dry_run"`
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
				Command:     "bureau template push iree/template:amdgpu-developer agent.json",
			},
			{
				Description: "Dry-run: validate and check inheritance without publishing",
				Command:     "bureau template push --dry-run iree/template:amdgpu-developer agent.json",
			},
		},
		Flags: func() *pflag.FlagSet {
			return cli.FlagsFromParams("push", &params)
		},
		Params: func() any { return &params },
		Run: func(args []string) error {
			if len(args) != 2 {
				return fmt.Errorf("usage: bureau template push [flags] <template-ref> <file>")
			}

			templateRefString := args[0]
			filePath := args[1]

			// Parse the template reference.
			ref, err := schema.ParseTemplateRef(templateRefString)
			if err != nil {
				return fmt.Errorf("parsing template reference: %w", err)
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
				return fmt.Errorf("%s: %d validation issue(s) found", filePath, len(issues))
			}

			// Connect to Matrix for inheritance verification and publishing.
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

			// Verify inheritance target exists (if any).
			if content.Inherits != "" {
				parentRef, err := schema.ParseTemplateRef(content.Inherits)
				if err != nil {
					return fmt.Errorf("invalid inherits reference %q: %w", content.Inherits, err)
				}
				if _, err := libtmpl.Fetch(ctx, session, parentRef, params.ServerName); err != nil {
					return fmt.Errorf("parent template %q not found in Matrix: %w", content.Inherits, err)
				}
				fmt.Fprintf(os.Stderr, "parent template %q: found\n", content.Inherits)
			}

			if params.DryRun {
				if params.OutputJSON {
					return cli.WriteJSON(templatePushResult{
						Ref:          ref.String(),
						File:         filePath,
						RoomAlias:    roomAlias,
						RoomID:       roomID,
						TemplateName: ref.Template,
						DryRun:       true,
					})
				}
				fmt.Fprintf(os.Stdout, "%s: valid (dry-run, not published)\n", filePath)
				fmt.Fprintf(os.Stdout, "  target room: %s (%s)\n", roomAlias, roomID)
				fmt.Fprintf(os.Stdout, "  template name: %s\n", ref.Template)
				return nil
			}

			// Publish the template as a state event.
			eventID, err := session.SendStateEvent(ctx, roomID, schema.EventTypeTemplate, ref.Template, content)
			if err != nil {
				return fmt.Errorf("publishing template: %w", err)
			}

			if params.OutputJSON {
				return cli.WriteJSON(templatePushResult{
					Ref:          ref.String(),
					File:         filePath,
					RoomAlias:    roomAlias,
					RoomID:       roomID,
					TemplateName: ref.Template,
					EventID:      eventID,
					DryRun:       false,
				})
			}

			fmt.Fprintf(os.Stdout, "published %s to %s (event: %s)\n", ref.String(), roomAlias, eventID)
			return nil
		},
	}
}
