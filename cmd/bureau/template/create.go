// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	libtmpl "github.com/bureau-foundation/bureau/lib/templatedef"
)

// templateCreateParams holds the parameters for the template create command.
type templateCreateParams struct {
	cli.SessionConfig
	cli.JSONOutput
	ServerName      string   `json:"server_name"      flag:"server-name"      desc:"Matrix server name for resolving room aliases (auto-detected from machine.conf)"`
	Inherits        []string `json:"inherits"         flag:"inherits"         desc:"parent template reference (repeatable, left-to-right merge order)"`
	Description     string   `json:"description"      flag:"description"      desc:"human-readable description of the template (required)"`
	Env             []string `json:"env"              flag:"env"              desc:"environment variable KEY=VALUE (repeatable)"`
	Mount           []string `json:"mount"            flag:"mount"            desc:"filesystem mount source:dest[:mode] or tmpfs:dest (repeatable)"`
	Command         string   `json:"command"          flag:"command"          desc:"entrypoint command (space-separated)"`
	Environment     string   `json:"environment"      flag:"environment"      desc:"Nix store path for sandbox /usr/local"`
	RequiredService []string `json:"required_service" flag:"required-service" desc:"required service role (repeatable)"`
	CreateDir       []string `json:"create_dir"       flag:"create-dir"       desc:"directory to create inside sandbox (repeatable)"`
	DefaultPayload  string   `json:"default_payload"  flag:"default-payload"  desc:"default payload JSON object"`
	DryRun          bool     `json:"dry_run"          flag:"dry-run"          desc:"validate only, do not publish to Matrix"`
}

// templateCreateResult is the JSON output for template create.
type templateCreateResult struct {
	Ref          string        `json:"ref"                desc:"template reference"`
	RoomAlias    ref.RoomAlias `json:"room_alias"         desc:"target room alias"`
	RoomID       ref.RoomID    `json:"room_id,omitempty"  desc:"target room Matrix ID"`
	TemplateName string        `json:"template_name"      desc:"template name (state key)"`
	EventID      ref.EventID   `json:"event_id,omitempty" desc:"created state event ID"`
	DryRun       bool          `json:"dry_run"            desc:"true if create was simulated"`
}

// createCommand returns the "create" subcommand for creating templates from
// CLI flags using inheritance.
func createCommand() *cli.Command {
	var params templateCreateParams

	return &cli.Command{
		Name:    "create",
		Summary: "Create a template from inheritance and CLI flags",
		Description: `Create a new template by inheriting from one or more parent templates
and overriding specific fields via flags. The template is published as an
m.bureau.template state event in Matrix.

This is the lightweight alternative to writing a JSONC file and using
"bureau template push". For simple inheritance-based composition — a
child template that inherits an environment and command from a parent
and adds project-specific overrides — flags are faster than files.

At least one --inherits flag is required. Fields not specified by flags
are left empty and filled from parent templates during inheritance
resolution. This is the same semantics as a JSONC file containing only
"inherits" and override fields.

For templates with complex fields like proxy_services (which have nested
structure), use "bureau template push" with a JSONC file instead.

Mount specs use a compact format:
  --mount /host/path:/sandbox/path        read-only bind mount (default)
  --mount /host/path:/sandbox/path:rw     read-write bind mount
  --mount tmpfs:/sandbox/path             tmpfs mount`,
		Usage: "bureau template create [flags] <template-ref>",
		Examples: []cli.Example{
			{
				Description: "Create a per-project Claude Code template with RAG",
				Command:     "bureau template create --inherits bureau/template:claude-code --description 'Claude Code for IREE with RAG' --required-service rag --default-payload '{\"project_context\": \"IREE ML compiler\"}' iree/template:claude-dev",
			},
			{
				Description: "Create a template with extra filesystem mounts",
				Command:     "bureau template create --inherits bureau/template:agent-base --description 'Data analyst with dataset access' --mount /data/datasets:/datasets:ro --env DATASET_ROOT=/datasets myteam/template:data-analyst",
			},
			{
				Description: "Dry-run: validate and check parents without publishing",
				Command:     "bureau template create --inherits bureau/template:claude-code --description 'test' --dry-run bureau/template:test-child",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &templateCreateResult{} },
		RequiredGrants: []string{"command/template/create"},
		Annotations:    cli.Create(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) != 1 {
				return cli.Validation("usage: bureau template create [flags] <template-ref>")
			}

			templateRef, err := schema.ParseTemplateRef(args[0])
			if err != nil {
				return cli.Validation("invalid template reference: %w", err).
					WithHint("Template references have the form namespace/template:name (e.g., iree/template:claude-dev).")
			}

			// At least one parent is required — create without inheritance
			// is just push, and push handles that case better (reads a full
			// template from a file).
			if len(params.Inherits) == 0 {
				return cli.Validation("at least one --inherits flag is required").
					WithHint("Use 'bureau template push' to publish a self-contained template from a file.")
			}
			if params.Description == "" {
				return cli.Validation("--description is required")
			}

			// Parse environment variables.
			environmentVariables, err := cli.ParseKeyValuePairs(params.Env)
			if err != nil {
				return cli.Validation("invalid --env: %w", err)
			}

			// Parse mount specs.
			var filesystem []schema.TemplateMount
			for _, spec := range params.Mount {
				mount, parseError := parseMountSpec(spec)
				if parseError != nil {
					return cli.Validation("invalid --mount %q: %w", spec, parseError)
				}
				filesystem = append(filesystem, mount)
			}

			// Parse command string.
			var command []string
			if params.Command != "" {
				command = strings.Fields(params.Command)
				if len(command) == 0 {
					return cli.Validation("--command must not be empty whitespace")
				}
			}

			// Parse default payload JSON.
			var defaultPayload map[string]any
			if params.DefaultPayload != "" {
				if err := json.Unmarshal([]byte(params.DefaultPayload), &defaultPayload); err != nil {
					return cli.Validation("invalid --default-payload JSON: %w", err)
				}
			}

			// Construct the template content from flags.
			content := schema.TemplateContent{
				Description:          params.Description,
				Inherits:             params.Inherits,
				Command:              command,
				Environment:          params.Environment,
				EnvironmentVariables: environmentVariables,
				Filesystem:           filesystem,
				RequiredServices:     params.RequiredService,
				CreateDirs:           params.CreateDir,
				DefaultPayload:       defaultPayload,
			}

			// Validate the constructed template.
			issues := validateTemplateContent(&content)
			if len(issues) > 0 {
				for _, issue := range issues {
					logger.Warn("validation issue", "issue", issue)
				}
				return cli.Validation("template has %d validation issue(s)", len(issues))
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

			// Dry-run: resolve room and verify parents without publishing.
			if params.DryRun {
				roomAlias := templateRef.RoomAlias(serverName)
				roomID, err := session.ResolveAlias(ctx, roomAlias)
				if err != nil {
					return cli.NotFound("resolving target room %q: %w", roomAlias, err).
						WithHint("The template room must exist before creating templates. " +
							"Run 'bureau matrix setup' to create standard rooms, or check the namespace.")
				}

				for index, parentRefString := range content.Inherits {
					parentRef, parseError := schema.ParseTemplateRef(parentRefString)
					if parseError != nil {
						return cli.Validation("--inherits[%d] reference %q is invalid: %w", index, parentRefString, parseError)
					}
					if _, fetchError := libtmpl.Fetch(ctx, session, parentRef, serverName); fetchError != nil {
						return cli.NotFound("parent template %q not found in Matrix: %w", parentRefString, fetchError).
							WithHint("Publish the parent template first with 'bureau template publish'.")
					}
					logger.Info("parent template found", "parent", parentRefString)
				}

				if done, emitError := params.EmitJSON(templateCreateResult{
					Ref:          templateRef.String(),
					RoomAlias:    roomAlias,
					RoomID:       roomID,
					TemplateName: templateRef.Template,
					DryRun:       true,
				}); done {
					return emitError
				}
				fmt.Fprintf(os.Stdout, "valid (dry-run, not published)\n")
				fmt.Fprintf(os.Stdout, "  target room: %s (%s)\n", roomAlias, roomID)
				fmt.Fprintf(os.Stdout, "  template name: %s\n", templateRef.Template)
				fmt.Fprintf(os.Stdout, "  inherits: %s\n", strings.Join(content.Inherits, ", "))
				return nil
			}

			// Publish the template.
			result, err := libtmpl.Push(ctx, session, templateRef, content, serverName)
			if err != nil {
				return cli.Internal("publishing template: %w", err)
			}

			if done, emitError := params.EmitJSON(templateCreateResult{
				Ref:          templateRef.String(),
				RoomAlias:    result.RoomAlias,
				RoomID:       result.RoomID,
				TemplateName: templateRef.Template,
				EventID:      result.EventID,
				DryRun:       false,
			}); done {
				return emitError
			}

			fmt.Fprintf(os.Stdout, "created %s in %s (event: %s)\n", templateRef.String(), result.RoomAlias, result.EventID)
			fmt.Fprintf(os.Stdout, "  inherits: %s\n", strings.Join(content.Inherits, ", "))
			return nil
		},
	}
}

// parseMountSpec parses a compact mount specification into a TemplateMount.
// Accepted formats:
//
//   - source:dest — read-only bind mount
//   - source:dest:ro — explicit read-only bind mount
//   - source:dest:rw — read-write bind mount
//   - tmpfs:dest — tmpfs mount (no source)
//
// The source and dest are absolute paths. When source is "tmpfs", the mount
// type is set to "tmpfs" and the source field is left empty.
func parseMountSpec(spec string) (schema.TemplateMount, error) {
	parts := strings.SplitN(spec, ":", 3)

	switch len(parts) {
	case 2:
		// source:dest or tmpfs:dest
		source, dest := parts[0], parts[1]
		if dest == "" {
			return schema.TemplateMount{}, fmt.Errorf("empty destination path")
		}
		if source == "tmpfs" {
			return schema.TemplateMount{
				Dest: dest,
				Type: "tmpfs",
			}, nil
		}
		if source == "" {
			return schema.TemplateMount{}, fmt.Errorf("empty source path (use 'tmpfs' for a tmpfs mount)")
		}
		return schema.TemplateMount{
			Source: source,
			Dest:   dest,
		}, nil

	case 3:
		// source:dest:mode
		source, dest, modeString := parts[0], parts[1], parts[2]
		if dest == "" {
			return schema.TemplateMount{}, fmt.Errorf("empty destination path")
		}
		if source == "tmpfs" {
			return schema.TemplateMount{}, fmt.Errorf("tmpfs mounts do not accept a mode (use tmpfs:dest)")
		}
		if source == "" {
			return schema.TemplateMount{}, fmt.Errorf("empty source path")
		}
		mode := schema.MountMode(modeString)
		if !mode.IsKnown() {
			return schema.TemplateMount{}, fmt.Errorf("unknown mode %q (expected %q or %q)", modeString, schema.MountModeRO, schema.MountModeRW)
		}
		return schema.TemplateMount{
			Source: source,
			Dest:   dest,
			Mode:   mode,
		}, nil

	default:
		return schema.TemplateMount{}, fmt.Errorf("expected source:dest[:mode] or tmpfs:dest, got %q", spec)
	}
}
