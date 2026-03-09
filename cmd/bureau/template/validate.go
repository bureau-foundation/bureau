// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/tidwall/jsonc"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/templatedef"
)

// templateValidateParams holds the parameters for the template validate command.
type templateValidateParams struct {
	cli.JSONOutput
}

// templateValidationResult is the JSON output for template validate.
type templateValidationResult struct {
	File   string   `json:"file"             desc:"validated template file path"`
	Valid  bool     `json:"valid"            desc:"true if template is valid"`
	Issues []string `json:"issues,omitempty" desc:"validation issues found"`
}

// validateCommand returns the "validate" subcommand for validating template files.
func validateCommand() *cli.Command {
	var params templateValidateParams

	return &cli.Command{
		Name:    "validate",
		Summary: "Validate a local template JSON file",
		Description: `Validate a local template definition file. Checks that the JSON is
well-formed and conforms to the TemplateContent schema. Does not access
Matrix — use "bureau template push --dry-run" to also verify that
inheritance targets exist.

Template files use JSON — the same format stored in Matrix state events.
Use "bureau template show --raw" to export a template for editing.`,
		Usage: "bureau template validate <file>",
		Examples: []cli.Example{
			{
				Description: "Validate a template definition",
				Command:     "bureau template validate my-agent.json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &templateValidationResult{} },
		RequiredGrants: []string{"command/template/validate"},
		Annotations:    cli.ReadOnly(),
		Run: func(_ context.Context, args []string, logger *slog.Logger) error {
			if len(args) != 1 {
				return cli.Validation("usage: bureau template validate <file>")
			}

			path := args[0]
			content, err := readTemplateFile(path)
			if err != nil {
				return err
			}

			issues := templatedef.Validate(content)

			if done, err := params.EmitJSON(templateValidationResult{
				File:   path,
				Valid:  len(issues) == 0,
				Issues: issues,
			}); done {
				return err
			}

			if len(issues) > 0 {
				for _, issue := range issues {
					logger.Warn("validation issue", "issue", issue)
				}
				return cli.Validation("%s: %d validation issue(s) found", path, len(issues))
			}

			fmt.Fprintf(os.Stdout, "%s: valid\n", path)
			return nil
		},
	}
}

// readTemplateFile reads a JSONC template file from disk and parses it into a
// TemplateContent. JSONC extends JSON with // line comments, /* block comments */,
// and trailing commas — comments are stripped before parsing. This is the same
// format stored in Matrix state events, plus comments for human documentation.
//
// Returns a descriptive error if the file cannot be read or the JSON is malformed.
func readTemplateFile(path string) (*schema.TemplateContent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, cli.Validation("reading %s: %w", path, err)
	}
	return parseTemplateBytes(data)
}

// parseTemplateBytes parses JSONC bytes into a TemplateContent. Comments
// and trailing commas are stripped before parsing.
func parseTemplateBytes(data []byte) (*schema.TemplateContent, error) {
	// Strip comments and trailing commas before parsing as standard JSON.
	stripped := jsonc.ToJSON(data)

	var content schema.TemplateContent
	if err := json.Unmarshal(stripped, &content); err != nil {
		return nil, cli.Validation("parsing template JSON: %w", err)
	}

	return &content, nil
}
