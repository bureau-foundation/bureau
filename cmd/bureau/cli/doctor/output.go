// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"fmt"
	"os"
	"strings"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// PrintChecklist prints check results as a human-readable checklist.
// Elevated fixes that were skipped (because the process is not root)
// are grouped into a separate section at the bottom with actionable
// guidance.
func PrintChecklist(results []Result, fixMode, dryRun bool, outcome Outcome) error {
	anyFailed := false
	fixableCount := 0
	fixedCount := 0
	var elevatedHints []string

	for _, result := range results {
		prefix := strings.ToUpper(string(result.Status))
		fmt.Fprintf(os.Stdout, "[%-5s]  %-40s  %s\n", prefix, result.Name, result.Message)

		switch result.Status {
		case StatusFail:
			anyFailed = true
			if result.FixHint != "" {
				fixableCount++
				if dryRun {
					elevationNote := ""
					if result.Elevated {
						elevationNote = " (requires sudo)"
					}
					fmt.Fprintf(os.Stdout, "         %-40s  would fix: %s%s\n", "", result.FixHint, elevationNote)
				}
				if result.Elevated {
					elevatedHints = append(elevatedHints, result.FixHint)
				}
			}
		case StatusFixed:
			fixedCount++
		}
	}

	fmt.Fprintln(os.Stdout)

	if anyFailed {
		if dryRun && fixableCount > 0 {
			fmt.Fprintf(os.Stdout, "%d issue(s) would be repaired. Run without --dry-run to apply.\n", fixableCount)
		} else if !fixMode && fixableCount > 0 {
			fmt.Fprintf(os.Stdout, "Run with --fix to repair %d issue(s).\n", fixableCount)
		} else {
			fmt.Fprintln(os.Stdout, "Some checks failed.")
		}
		if outcome.PermissionDenied {
			fmt.Fprintln(os.Stdout)
			if outcome.PermissionDeniedHint != "" {
				fmt.Fprintln(os.Stdout, outcome.PermissionDeniedHint)
			} else {
				fmt.Fprintln(os.Stdout, "Some fixes failed due to insufficient permissions.")
			}
		}
		if outcome.ElevatedSkipped > 0 {
			fmt.Fprintln(os.Stdout)
			fmt.Fprintf(os.Stdout, "%d fix(es) require root privileges:\n", outcome.ElevatedSkipped)
			for _, hint := range elevatedHints {
				fmt.Fprintf(os.Stdout, "  - %s\n", hint)
			}
			fmt.Fprintln(os.Stdout)
			fmt.Fprintln(os.Stdout, "Re-run with sudo to apply these fixes:")
			fmt.Fprintln(os.Stdout, "  sudo bureau machine doctor --fix")
		}
		return &cli.ExitError{Code: 1}
	}

	if fixedCount > 0 {
		fmt.Fprintf(os.Stdout, "%d issue(s) repaired.\n", fixedCount)
		return nil
	}

	fmt.Fprintln(os.Stdout, "All checks passed.")
	return nil
}
