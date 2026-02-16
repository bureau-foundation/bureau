// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/tidwall/jsonc"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
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
		Run: func(args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("usage: bureau template validate <file>")
			}

			path := args[0]
			content, err := readTemplateFile(path)
			if err != nil {
				return err
			}

			issues := validateTemplateContent(content)

			if done, err := params.EmitJSON(templateValidationResult{
				File:   path,
				Valid:  len(issues) == 0,
				Issues: issues,
			}); done {
				return err
			}

			if len(issues) > 0 {
				for _, issue := range issues {
					fmt.Fprintf(os.Stderr, "  - %s\n", issue)
				}
				return fmt.Errorf("%s: %d validation issue(s) found", path, len(issues))
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
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	// Strip comments and trailing commas before parsing as standard JSON.
	stripped := jsonc.ToJSON(data)

	var content schema.TemplateContent
	if err := json.Unmarshal(stripped, &content); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	return &content, nil
}

// validateTemplateContent checks a TemplateContent for common issues. Returns
// a list of human-readable descriptions. An empty list means the template is
// valid.
func validateTemplateContent(content *schema.TemplateContent) []string {
	var issues []string

	if content.Description == "" {
		issues = append(issues, "description is empty (every template should have a human-readable description)")
	}

	// If this template doesn't inherit, it should define enough to be usable.
	if content.Inherits == "" {
		if len(content.Command) == 0 {
			issues = append(issues, "no command defined and no parent template to inherit from")
		}
	} else {
		// Validate the inherits reference is parseable.
		if _, err := schema.ParseTemplateRef(content.Inherits); err != nil {
			issues = append(issues, fmt.Sprintf("inherits reference is invalid: %v", err))
		}
	}

	// Validate filesystem mounts.
	for index, mount := range content.Filesystem {
		if mount.Dest == "" {
			issues = append(issues, fmt.Sprintf("filesystem[%d]: dest is required", index))
		}
		if mount.Type != "" && mount.Type != "tmpfs" {
			issues = append(issues, fmt.Sprintf("filesystem[%d]: unknown type %q (expected \"\" for bind or \"tmpfs\")", index, mount.Type))
		}
		if mount.Mode != "" && mount.Mode != "ro" && mount.Mode != "rw" {
			issues = append(issues, fmt.Sprintf("filesystem[%d]: unknown mode %q (expected \"ro\" or \"rw\")", index, mount.Mode))
		}
		if mount.Type == "tmpfs" && mount.Source != "" {
			issues = append(issues, fmt.Sprintf("filesystem[%d]: tmpfs mounts should not have a source path", index))
		}
		if mount.Type == "" && mount.Source == "" {
			issues = append(issues, fmt.Sprintf("filesystem[%d]: bind mounts require a source path", index))
		}
	}

	// Validate resource limits.
	if content.Resources != nil {
		if content.Resources.CPUShares < 0 {
			issues = append(issues, "resources.cpu_shares must be non-negative")
		}
		if content.Resources.MemoryLimitMB < 0 {
			issues = append(issues, "resources.memory_limit_mb must be non-negative")
		}
		if content.Resources.PidsLimit < 0 {
			issues = append(issues, "resources.pids_limit must be non-negative")
		}
	}

	// Validate roles have non-empty commands.
	for roleName, command := range content.Roles {
		if len(command) == 0 {
			issues = append(issues, fmt.Sprintf("roles[%q]: command is empty", roleName))
		}
	}

	return issues
}
