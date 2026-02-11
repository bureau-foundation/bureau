// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	libpipeline "github.com/bureau-foundation/bureau/lib/pipeline"
)

// validateCommand returns the "validate" subcommand for validating pipeline files.
func validateCommand() *cli.Command {
	return &cli.Command{
		Name:    "validate",
		Summary: "Validate a local pipeline JSONC file",
		Description: `Validate a local pipeline definition file. Checks that the JSONC is
well-formed and conforms to the PipelineContent schema: at least one
step, each step has a name, Run and Publish are mutually exclusive,
timeouts parse correctly, and so on.

Does not access Matrix — this is a purely local check. Use
"bureau pipeline push --dry-run" to additionally verify that the
target room exists before publishing.

Pipeline files use JSONC: JSON extended with // line comments,
/* block comments */, and trailing commas. Comments are stripped
before validation.`,
		Usage: "bureau pipeline validate <file>",
		Examples: []cli.Example{
			{
				Description: "Validate a pipeline definition",
				Command:     "bureau pipeline validate my-pipeline.jsonc",
			},
		},
		Run: func(args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("usage: bureau pipeline validate <file>")
			}

			path := args[0]
			content, err := libpipeline.ReadFile(path)
			if err != nil {
				return err
			}

			issues := libpipeline.Validate(content)
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
