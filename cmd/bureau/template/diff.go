// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
	libtmpl "github.com/bureau-foundation/bureau/lib/template"
)

// diffCommand returns the "diff" subcommand for comparing a Matrix template
// against a local file.
func diffCommand() *cli.Command {
	var serverName string

	return &cli.Command{
		Name:    "diff",
		Summary: "Compare a Matrix template against a local file",
		Description: `Fetch a template from Matrix and compare it against a local file.
Shows the differences between the two versions in a line-by-line format.

Both versions are serialized as JSON for comparison (comments in the
local file are stripped). The comparison is performed on the raw template
content, not the resolved inheritance chain — use "bureau template show
--raw" to inspect the stored version.`,
		Usage: "bureau template diff [flags] <template-ref> <file>",
		Examples: []cli.Example{
			{
				Description: "Compare the Matrix version of a template against a local file",
				Command:     "bureau template diff iree/template:amdgpu-developer agent.json",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("diff", pflag.ContinueOnError)
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name for resolving room aliases")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) != 2 {
				return fmt.Errorf("usage: bureau template diff [flags] <template-ref> <file>")
			}

			templateRefString := args[0]
			filePath := args[1]

			// Parse the template reference.
			ref, err := schema.ParseTemplateRef(templateRefString)
			if err != nil {
				return fmt.Errorf("parsing template reference: %w", err)
			}

			// Read the local file.
			localContent, err := readTemplateFile(filePath)
			if err != nil {
				return err
			}

			// Fetch the Matrix version.
			ctx, cancel, session, err := cli.ConnectOperator()
			if err != nil {
				return err
			}
			defer cancel()

			remoteContent, err := libtmpl.Fetch(ctx, session, ref, serverName)
			if err != nil {
				return err
			}

			// Serialize both to JSON for comparison.
			remoteJSON, err := json.MarshalIndent(remoteContent, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal remote template: %w", err)
			}
			localJSON, err := json.MarshalIndent(localContent, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal local template: %w", err)
			}

			remoteLines := strings.Split(string(remoteJSON), "\n")
			localLines := strings.Split(string(localJSON), "\n")

			// Simple line-by-line diff. For templates (which are typically
			// small JSON objects), this is sufficient to highlight changes.
			differences := lineDiff(remoteLines, localLines)

			if len(differences) == 0 {
				fmt.Fprintf(os.Stdout, "no differences (Matrix and %s are identical)\n", filePath)
				return nil
			}

			fmt.Fprintf(os.Stdout, "--- %s (Matrix)\n", ref.String())
			fmt.Fprintf(os.Stdout, "+++ %s (local)\n", filePath)
			for _, line := range differences {
				fmt.Fprintln(os.Stdout, line)
			}
			return nil
		},
	}
}

// lineDiff produces a minimal unified-style diff between two slices of lines.
// Lines present only in "old" are prefixed with "- ", lines only in "new"
// with "+ ", and context lines with "  ". This uses a simple longest common
// subsequence approach suitable for small documents (template JSON is
// typically under 100 lines).
func lineDiff(old, new []string) []string {
	// Build LCS table.
	oldLength := len(old)
	newLength := len(new)
	table := make([][]int, oldLength+1)
	for i := range table {
		table[i] = make([]int, newLength+1)
	}
	for i := 1; i <= oldLength; i++ {
		for j := 1; j <= newLength; j++ {
			if old[i-1] == new[j-1] {
				table[i][j] = table[i-1][j-1] + 1
			} else if table[i-1][j] >= table[i][j-1] {
				table[i][j] = table[i-1][j]
			} else {
				table[i][j] = table[i][j-1]
			}
		}
	}

	// Walk backwards to produce diff lines.
	var lines []string
	i, j := oldLength, newLength
	for i > 0 || j > 0 {
		if i > 0 && j > 0 && old[i-1] == new[j-1] {
			lines = append(lines, "  "+old[i-1])
			i--
			j--
		} else if j > 0 && (i == 0 || table[i][j-1] >= table[i-1][j]) {
			lines = append(lines, "+ "+new[j-1])
			j--
		} else {
			lines = append(lines, "- "+old[i-1])
			i--
		}
	}

	// Reverse to get forward order.
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}

	// Check if there are actual differences (not just context).
	hasDifferences := false
	for _, line := range lines {
		if strings.HasPrefix(line, "+ ") || strings.HasPrefix(line, "- ") {
			hasDifferences = true
			break
		}
	}
	if !hasDifferences {
		return nil
	}

	return lines
}
