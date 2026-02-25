// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// IsRoot returns true if the current process has effective UID 0.
func IsRoot() bool {
	return os.Geteuid() == 0
}

// ExecuteFixes runs the fix action for each fixable failure, updating
// results in place. In dry-run mode, no fixes are executed and an empty
// Outcome is returned.
//
// Elevated fixes (Result.Elevated == true) are skipped when the process
// is not running as root — they are counted in Outcome.ElevatedSkipped.
//
// The isPermissionDenied callback classifies errors as permission
// failures. Matrix doctor passes a function that checks M_FORBIDDEN;
// machine doctor uses the default OS-level check (EPERM/EACCES).
// When nil, only syscall EPERM and EACCES are classified as permission
// denied.
func ExecuteFixes(ctx context.Context, results []Result, dryRun bool, isPermissionDenied func(error) bool) Outcome {
	if dryRun {
		return Outcome{}
	}
	if isPermissionDenied == nil {
		isPermissionDenied = isOSPermissionDenied
	}

	var outcome Outcome
	root := IsRoot()

	for i := range results {
		if results[i].Status != StatusFail || results[i].fix == nil {
			continue
		}
		if results[i].Elevated && !root {
			outcome.ElevatedSkipped++
			continue
		}
		if err := results[i].fix(ctx); err != nil {
			if isPermissionDenied(err) {
				outcome.PermissionDenied = true
				results[i].Message = fmt.Sprintf("%s (insufficient permissions)", results[i].Message)
			} else {
				results[i].Message = fmt.Sprintf("%s (fix failed: %v)", results[i].Message, err)
			}
		} else {
			results[i].Status = StatusFixed
			outcome.FixedCount++
		}
	}

	return outcome
}

// isOSPermissionDenied returns true if err wraps EPERM or EACCES.
func isOSPermissionDenied(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

// BuildJSON builds the JSON output struct from results and outcome
// metadata.
func BuildJSON(results []Result, dryRun bool, outcome Outcome) JSONOutput {
	anyFailed := false
	for _, result := range results {
		if result.Status == StatusFail {
			anyFailed = true
			break
		}
	}
	return JSONOutput{
		Checks:           results,
		OK:               !anyFailed,
		DryRun:           dryRun,
		PermissionDenied: outcome.PermissionDenied,
		ElevatedSkipped:  outcome.ElevatedSkipped,
	}
}

// MarkRepaired updates results that now pass but were failing in a
// previous iteration — these were repaired by a fix even if they
// didn't directly carry the fix closure. Call this after the final
// iteration with the set of names that failed in any earlier iteration.
func MarkRepaired(results []Result, repairedNames map[string]bool) {
	for i := range results {
		if results[i].Status == StatusPass && repairedNames[results[i].Name] {
			results[i].Status = StatusFixed
		}
	}
}
