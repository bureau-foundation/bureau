// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	libtmpl "github.com/bureau-foundation/bureau/lib/templatedef"
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
	Ref          string                 `json:"ref"                    desc:"template reference (state key)"`
	Source       string                 `json:"source"                 desc:"source type: flake, url, or file"`
	SourceRef    string                 `json:"source_ref"             desc:"source reference (flake ref, URL, or file path)"`
	RoomAlias    ref.RoomAlias          `json:"room_alias"             desc:"target room alias"`
	RoomID       ref.RoomID             `json:"room_id,omitempty"      desc:"target room Matrix ID"`
	TemplateName string                 `json:"template_name"          desc:"template name"`
	EventID      ref.EventID            `json:"event_id,omitempty"     desc:"created state event ID"`
	Origin       *schema.TemplateOrigin `json:"origin,omitempty"     desc:"recorded origin for update tracking"`
	DryRun       bool                   `json:"dry_run"                desc:"true if publish was simulated"`
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

			// Load template content from the specified source.
			var content *schema.TemplateContent
			var origin *schema.TemplateOrigin
			var sourceType, sourceRef string

			switch {
			case params.FlakeRef != "":
				sourceType = "flake"
				sourceRef = params.FlakeRef
				content, origin, err = loadFromFlake(ctx, params.FlakeRef, params.System, logger)
			case params.URL != "":
				sourceType = "url"
				sourceRef = params.URL
				content, origin, err = loadFromURL(ctx, params.URL, logger)
			case params.File != "":
				sourceType = "file"
				sourceRef = params.File
				content, err = readTemplateFile(params.File)
			}
			if err != nil {
				return err
			}

			// Set origin on the content (nil for file sources).
			content.Origin = origin

			// Validate the template content.
			issues := validateTemplateContent(content)
			if len(issues) > 0 {
				for _, issue := range issues {
					logger.Warn("validation issue", "issue", issue)
				}
				return cli.Validation("%s: %d validation issue(s) found", sourceRef, len(issues))
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
					if _, err := libtmpl.Fetch(ctx, session, parentRef, serverName); err != nil {
						return cli.NotFound("parent template %q not found in Matrix: %w", parentRefString, err).
							WithHint("Publish the parent template first.")
					}
					logger.Info("parent template found", "parent", parentRefString)
				}

				if done, err := params.EmitJSON(templatePublishResult{
					Ref:          templateRef.String(),
					Source:       sourceType,
					SourceRef:    sourceRef,
					RoomAlias:    roomAlias,
					RoomID:       roomID,
					TemplateName: templateRef.Template,
					Origin:       origin,
					DryRun:       true,
				}); done {
					return err
				}
				fmt.Fprintf(os.Stdout, "%s: valid (dry-run, not published)\n", sourceRef)
				fmt.Fprintf(os.Stdout, "  source: %s\n", sourceType)
				fmt.Fprintf(os.Stdout, "  target room: %s (%s)\n", roomAlias, roomID)
				fmt.Fprintf(os.Stdout, "  template name: %s\n", templateRef.Template)
				if origin != nil {
					fmt.Fprintf(os.Stdout, "  origin: %s\n", originSummary(origin))
				}
				return nil
			}

			// Publish the template.
			result, err := libtmpl.Push(ctx, session, templateRef, *content, serverName)
			if err != nil {
				return cli.Internal("publishing template: %w", err)
			}

			if done, err := params.EmitJSON(templatePublishResult{
				Ref:          templateRef.String(),
				Source:       sourceType,
				SourceRef:    sourceRef,
				RoomAlias:    result.RoomAlias,
				RoomID:       result.RoomID,
				TemplateName: templateRef.Template,
				EventID:      result.EventID,
				Origin:       origin,
				DryRun:       false,
			}); done {
				return err
			}

			fmt.Fprintf(os.Stdout, "published %s to %s (event: %s)\n", templateRef.String(), result.RoomAlias, result.EventID)
			if origin != nil {
				fmt.Fprintf(os.Stdout, "  origin: %s\n", originSummary(origin))
			}
			return nil
		},
	}
}

// loadFromFlake evaluates a Nix flake and reads the bureauTemplate output.
func loadFromFlake(ctx context.Context, flakeRef string, system string, logger *slog.Logger) (*schema.TemplateContent, *schema.TemplateOrigin, error) {
	if system == "" {
		system = runtime.GOARCH
		switch system {
		case "amd64":
			system = "x86_64-linux"
		case "arm64":
			system = "aarch64-linux"
		default:
			return nil, nil, cli.Validation("unsupported architecture %q; specify --system explicitly", runtime.GOARCH)
		}
	}

	nixPath, err := exec.LookPath("nix")
	if err != nil {
		return nil, nil, cli.Validation("nix not found on PATH: %w", err).
			WithHint("The Nix package manager is required for --flake. Install it with 'script/setup-nix'.")
	}

	// Evaluate the bureauTemplate output.
	templateAttr := fmt.Sprintf("%s#bureauTemplate.%s", flakeRef, system)
	logger.Info("evaluating flake template", "attr", templateAttr)

	evalCmd := exec.CommandContext(ctx, nixPath, "eval", "--json", templateAttr)
	evalCmd.Stderr = os.Stderr
	evalOutput, err := evalCmd.Output()
	if err != nil {
		return nil, nil, cli.Internal("nix eval %s: %w", templateAttr, err).
			WithHint(fmt.Sprintf("Ensure the flake exports a bureauTemplate.%s attribute.", system))
	}

	var content schema.TemplateContent
	if err := json.Unmarshal(evalOutput, &content); err != nil {
		return nil, nil, cli.Internal("parsing bureauTemplate output as TemplateContent: %w", err).
			WithHint("The bureauTemplate output must use snake_case field names matching the TemplateContent JSON wire format.")
	}

	// Get flake metadata for the resolved revision.
	logger.Info("reading flake metadata", "flake_ref", flakeRef)

	metaCmd := exec.CommandContext(ctx, nixPath, "flake", "metadata", "--json", flakeRef)
	metaCmd.Stderr = os.Stderr
	metaOutput, err := metaCmd.Output()
	if err != nil {
		return nil, nil, cli.Internal("nix flake metadata %s: %w", flakeRef, err).
			WithHint("Ensure the flake reference is valid and accessible.")
	}

	var metadata struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(metaOutput, &metadata); err != nil {
		return nil, nil, cli.Internal("parsing flake metadata: %w", err)
	}

	// Compute content hash of the template (for change detection).
	contentHash, err := computeContentHash(&content)
	if err != nil {
		return nil, nil, cli.Internal("computing content hash: %w", err)
	}

	origin := &schema.TemplateOrigin{
		FlakeRef:    flakeRef,
		ResolvedRev: metadata.Revision,
		ContentHash: contentHash,
	}

	return &content, origin, nil
}

// loadFromURL fetches template JSONC from a URL and parses it.
func loadFromURL(ctx context.Context, rawURL string, logger *slog.Logger) (*schema.TemplateContent, *schema.TemplateOrigin, error) {
	if !strings.HasPrefix(rawURL, "https://") {
		return nil, nil, cli.Validation("URL must use HTTPS: %s", rawURL)
	}

	logger.Info("fetching template", "url", rawURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, cli.Internal("creating HTTP request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, cli.Internal("fetching %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, cli.Internal("fetching %s: HTTP %d", rawURL, resp.StatusCode)
	}

	// Read with a size limit to prevent abuse.
	const maxTemplateSize = 1 << 20 // 1 MB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTemplateSize+1))
	if err != nil {
		return nil, nil, cli.Internal("reading response body: %w", err)
	}
	if len(body) > maxTemplateSize {
		return nil, nil, cli.Validation("template exceeds 1 MB size limit")
	}

	content, err := parseTemplateBytes(body)
	if err != nil {
		return nil, nil, cli.Internal("parsing template from %s: %w", rawURL, err)
	}

	contentHash, err := computeContentHash(content)
	if err != nil {
		return nil, nil, cli.Internal("computing content hash: %w", err)
	}

	origin := &schema.TemplateOrigin{
		URL:         rawURL,
		ContentHash: contentHash,
	}

	return content, origin, nil
}

// computeContentHash computes a SHA-256 hash of the template content for
// change detection. The content is serialized as canonical JSON (sorted
// keys, no whitespace) before hashing.
func computeContentHash(content *schema.TemplateContent) (string, error) {
	// Marshal without origin — the hash is of the template definition
	// itself, not the metadata about where it came from. This prevents
	// the hash from changing when only origin metadata changes.
	contentCopy := *content
	contentCopy.Origin = nil
	data, err := json.Marshal(contentCopy)
	if err != nil {
		return "", fmt.Errorf("marshaling template for hash: %w", err)
	}
	hash := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", hash), nil
}

// originSummary returns a one-line summary of a template origin for display.
func originSummary(origin *schema.TemplateOrigin) string {
	switch {
	case origin.FlakeRef != "":
		if origin.ResolvedRev != "" {
			shortRev := origin.ResolvedRev
			if len(shortRev) > 12 {
				shortRev = shortRev[:12]
			}
			return fmt.Sprintf("flake %s (rev %s)", origin.FlakeRef, shortRev)
		}
		return fmt.Sprintf("flake %s", origin.FlakeRef)
	case origin.URL != "":
		return fmt.Sprintf("url %s", origin.URL)
	default:
		return "unknown"
	}
}
