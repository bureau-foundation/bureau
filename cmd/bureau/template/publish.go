// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/templatedef"
)

// templatePublishParams holds the parameters for the template publish command.
type templatePublishParams struct {
	cli.SessionConfig
	cli.JSONOutput
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name for resolving room aliases (auto-detected from machine.conf)"`
	FlakeRef   string `json:"flake_ref"   flag:"flake"       desc:"Nix flake reference (e.g., github:bureau-foundation/bureau-discord/v1.2.0)"`
	URL        string `json:"url"         flag:"url"         desc:"URL to fetch template JSONC from (HTTPS required)"`
	File       string `json:"file"        flag:"file"        desc:"local template JSONC file path"`
	System     string `json:"system"      flag:"system"      desc:"Nix system for flake evaluation (default: current system)"`
	DryRun     bool   `json:"dry_run"     flag:"dry-run"     desc:"validate only, do not publish to Matrix"`
}

// templatePublishResult is the JSON output for template publish.
type templatePublishResult struct {
	Ref          string             `json:"ref"                desc:"template reference (state key)"`
	Source       string             `json:"source"             desc:"source type: flake, url, or file"`
	SourceRef    string             `json:"source_ref"         desc:"source reference (flake ref, URL, or file path)"`
	RoomAlias    ref.RoomAlias      `json:"room_alias"         desc:"target room alias"`
	RoomID       ref.RoomID         `json:"room_id,omitempty"  desc:"target room Matrix ID"`
	TemplateName string             `json:"template_name"      desc:"template name"`
	EventID      ref.EventID        `json:"event_id,omitempty" desc:"created state event ID"`
	Origin       *ref.ContentOrigin `json:"origin,omitempty"   desc:"recorded origin for update tracking"`
	DryRun       bool               `json:"dry_run"            desc:"true if publish was simulated"`
}

// publishCommand returns the "publish" subcommand for publishing templates
// from flake references, URLs, or local files.
func publishCommand() *cli.Command {
	var params templatePublishParams

	return &cli.Command{
		Name:    "publish",
		Summary: "Publish a template to Matrix from a flake, URL, or file",
		Description: `Publish a template to Matrix from one of three sources:

  --flake <ref>   Evaluate a Nix flake, read its bureauTemplate output,
                  and publish with origin tracking. The flake reference
                  is recorded so 'bureau template update' can check for
                  newer versions.

  --url <url>     Fetch template JSONC from a URL, validate, and publish
                  with origin tracking.

  --file <path>   Read template JSONC from a local file, validate, and
                  publish. Equivalent to 'bureau template push'.

Exactly one of --flake, --url, or --file must be specified. The template
reference (positional argument) specifies which room and state key to
publish to.

For flake sources, the bureauTemplate.<system> output must be an
attribute set using the same field names as the TemplateContent JSON
wire format (snake_case). Store paths from Nix string interpolation
are resolved during evaluation.`,
		Usage: "bureau template publish [flags] <template-ref>",
		Examples: []cli.Example{
			{
				Description: "Publish from a Nix flake",
				Command:     "bureau template publish --flake github:bureau-foundation/bureau-discord/v1.2.0 bureau/template:discord",
			},
			{
				Description: "Publish from a URL",
				Command:     "bureau template publish --url https://raw.githubusercontent.com/.../template.jsonc bureau/template:discord",
			},
			{
				Description: "Publish from a local file",
				Command:     "bureau template publish --file ./template.jsonc bureau/template:discord",
			},
			{
				Description: "Dry-run: validate without publishing",
				Command:     "bureau template publish --flake github:bureau-foundation/bureau-discord --dry-run bureau/template:discord",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &templatePublishResult{} },
		RequiredGrants: []string{"command/template/publish"},
		Annotations:    cli.Create(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) != 1 {
				return cli.Validation("usage: bureau template publish [flags] <template-ref>")
			}

			templateRef, err := schema.ParseTemplateRef(args[0])
			if err != nil {
				return cli.Validation("invalid template reference: %w", err).
					WithHint("Template references have the form namespace/template:name (e.g., bureau/template:discord).")
			}

			// Validate exactly one source is specified.
			sourceCount := 0
			if params.FlakeRef != "" {
				sourceCount++
			}
			if params.URL != "" {
				sourceCount++
			}
			if params.File != "" {
				sourceCount++
			}
			if sourceCount != 1 {
				return cli.Validation("exactly one of --flake, --url, or --file must be specified")
			}

			// Load content from the specified source.
			var source *cli.SourceContent

			switch {
			case params.FlakeRef != "":
				system, err := cli.ResolveSystem(params.System)
				if err != nil {
					return err
				}
				source, err = cli.LoadFromFlake(ctx, params.FlakeRef, "bureauTemplate."+system, logger)
				if err != nil {
					return err
				}
			case params.URL != "":
				source, err = cli.LoadFromURL(ctx, params.URL, logger)
				if err != nil {
					return err
				}
			case params.File != "":
				source, err = cli.LoadFromFile(params.File)
				if err != nil {
					return err
				}
			}

			// Unmarshal into the typed TemplateContent.
			var content schema.TemplateContent
			if err := json.Unmarshal(source.Data, &content); err != nil {
				return cli.Internal("parsing %s output as TemplateContent: %w", source.Source, err).
					WithHint("The content must use snake_case field names matching the TemplateContent JSON wire format.")
			}

			// Compute content hash and set origin.
			if source.Origin != nil {
				contentCopy := content
				contentCopy.Origin = nil
				hash, err := cli.ComputeContentHash(contentCopy)
				if err != nil {
					return cli.Internal("computing content hash: %w", err)
				}
				source.Origin.ContentHash = hash
			}
			content.Origin = source.Origin

			// Validate the template content.
			issues := templatedef.Validate(&content)
			if len(issues) > 0 {
				for _, issue := range issues {
					logger.Warn("validation issue", "issue", issue)
				}
				return cli.Validation("%s: %d validation issue(s) found", source.Ref, len(issues))
			}

			// Connect to Matrix.
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			session, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}

			params.ServerName = cli.ResolveServerName(params.ServerName)

			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return cli.Validation("invalid --server-name: %w", err)
			}

			// Dry-run: resolve room and verify inheritance without publishing.
			if params.DryRun {
				roomAlias := templateRef.RoomAlias(serverName)
				roomID, err := session.ResolveAlias(ctx, roomAlias)
				if err != nil {
					return cli.NotFound("resolving target room %q: %w", roomAlias, err).
						WithHint("The template room must exist before publishing. " +
							"Run 'bureau matrix setup' to create standard rooms, or check the namespace.")
				}

				for index, parentRefString := range content.Inherits {
					parentRef, err := schema.ParseTemplateRef(parentRefString)
					if err != nil {
						return cli.Validation("inherits[%d] reference %q is invalid: %w", index, parentRefString, err)
					}
					if _, err := templatedef.Fetch(ctx, session, parentRef, serverName); err != nil {
						return cli.NotFound("parent template %q not found in Matrix: %w", parentRefString, err).
							WithHint("Publish the parent template first.")
					}
					logger.Info("parent template found", "parent", parentRefString)
				}

				if done, err := params.EmitJSON(templatePublishResult{
					Ref:          templateRef.String(),
					Source:       source.Source,
					SourceRef:    source.Ref,
					RoomAlias:    roomAlias,
					RoomID:       roomID,
					TemplateName: templateRef.Template,
					Origin:       source.Origin,
					DryRun:       true,
				}); done {
					return err
				}
				fmt.Fprintf(os.Stdout, "%s: valid (dry-run, not published)\n", source.Ref)
				fmt.Fprintf(os.Stdout, "  source: %s\n", source.Source)
				fmt.Fprintf(os.Stdout, "  target room: %s (%s)\n", roomAlias, roomID)
				fmt.Fprintf(os.Stdout, "  template name: %s\n", templateRef.Template)
				if source.Origin != nil {
					fmt.Fprintf(os.Stdout, "  origin: %s\n", cli.OriginSummary(source.Origin))
				}
				return nil
			}

			// Publish the template.
			result, err := templatedef.Push(ctx, session, templateRef, content, serverName)
			if err != nil {
				return cli.Internal("publishing template: %w", err)
			}

			if done, err := params.EmitJSON(templatePublishResult{
				Ref:          templateRef.String(),
				Source:       source.Source,
				SourceRef:    source.Ref,
				RoomAlias:    result.RoomAlias,
				RoomID:       result.RoomID,
				TemplateName: templateRef.Template,
				EventID:      result.EventID,
				Origin:       source.Origin,
				DryRun:       false,
			}); done {
				return err
			}

			fmt.Fprintf(os.Stdout, "published %s to %s (event: %s)\n", templateRef.String(), result.RoomAlias, result.EventID)
			if source.Origin != nil {
				fmt.Fprintf(os.Stdout, "  origin: %s\n", cli.OriginSummary(source.Origin))
			}
			return nil
		},
	}
}
