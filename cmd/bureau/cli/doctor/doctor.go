// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package doctor

import "context"

// Status is the outcome of a single health check.
type Status string

const (
	StatusPass  Status = "pass"
	StatusFail  Status = "fail"
	StatusWarn  Status = "warn"
	StatusSkip  Status = "skip"
	StatusFixed Status = "fixed"
)

// FixAction is a function that repairs a failed check. Domain-specific
// dependencies (Matrix session, config paths, etc.) are captured in the
// closure at check-construction time. The context carries cancellation
// and timeout.
type FixAction func(ctx context.Context) error

// Result holds the outcome of a single health check. Fixable failures
// carry a FixHint (human description) and an unexported fix function.
// Fixes that require root privileges set Elevated to true.
type Result struct {
	Name     string `json:"name"               desc:"health check name"`
	Status   Status `json:"status"             desc:"check outcome: pass, fail, warn, skip, fixed"`
	Message  string `json:"message"            desc:"human-readable check result"`
	FixHint  string `json:"fix_hint,omitempty"  desc:"suggested fix description"`
	Elevated bool   `json:"elevated,omitempty"  desc:"true if fix requires root privileges"`
	fix      FixAction
}

// HasFix reports whether this result carries a fix action.
func (r *Result) HasFix() bool {
	return r.fix != nil
}

// Pass creates a passing check result.
func Pass(name, message string) Result {
	return Result{Name: name, Status: StatusPass, Message: message}
}

// Fail creates a failing check result with no automatic fix.
func Fail(name, message string) Result {
	return Result{Name: name, Status: StatusFail, Message: message}
}

// FailWithFix creates a failing check result with an automatic fix.
func FailWithFix(name, message, fixHint string, fix FixAction) Result {
	return Result{Name: name, Status: StatusFail, Message: message, FixHint: fixHint, fix: fix}
}

// FailElevated creates a failing check result that requires root to fix.
// When ExecuteFixes encounters an elevated fix and the process is not
// running as root, it skips the fix and counts it in Outcome.ElevatedSkipped.
func FailElevated(name, message, fixHint string, fix FixAction) Result {
	return Result{Name: name, Status: StatusFail, Message: message, FixHint: fixHint, Elevated: true, fix: fix}
}

// Warn creates a warning check result. Warnings do not cause the doctor
// command to exit with a non-zero status.
func Warn(name, message string) Result {
	return Result{Name: name, Status: StatusWarn, Message: message}
}

// Skip creates a skipped check result. Checks are skipped when a
// prerequisite check failed (e.g., socket checks skip when services
// are not running).
func Skip(name, message string) Result {
	return Result{Name: name, Status: StatusSkip, Message: message}
}

// Outcome holds the aggregate results of a fix pass.
type Outcome struct {
	// FixedCount is the number of successfully applied fixes.
	FixedCount int

	// PermissionDenied is true if any fix failed due to insufficient
	// permissions (Matrix M_FORBIDDEN or OS EPERM/EACCES).
	PermissionDenied bool

	// PermissionDeniedHint is domain-specific guidance printed when
	// PermissionDenied is true. For matrix doctor, this might suggest
	// using a credential file; for machine doctor, this is unused
	// (the elevated-skipped section provides the guidance instead).
	PermissionDeniedHint string

	// ElevatedSkipped is the number of fixes skipped because they
	// require root and the current process is not running as root.
	ElevatedSkipped int
}

// JSONOutput is the JSON output structure for doctor commands.
type JSONOutput struct {
	Checks           []Result `json:"checks"                      desc:"list of health check results"`
	OK               bool     `json:"ok"                          desc:"true if all checks passed"`
	DryRun           bool     `json:"dry_run,omitempty"           desc:"true if fixes were simulated"`
	PermissionDenied bool     `json:"permission_denied,omitempty" desc:"true if fixes failed due to insufficient permissions"`
	ElevatedSkipped  int      `json:"elevated_skipped,omitempty"  desc:"count of fixes skipped because root is required"`
}
