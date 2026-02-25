// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"errors"
	"testing"
)

func TestPassResult(t *testing.T) {
	result := Pass("test check", "all good")
	if result.Status != StatusPass {
		t.Errorf("Pass() status = %q, want %q", result.Status, StatusPass)
	}
	if result.Name != "test check" {
		t.Errorf("Pass() name = %q, want %q", result.Name, "test check")
	}
	if result.HasFix() {
		t.Error("Pass() should not have a fix")
	}
}

func TestFailResult(t *testing.T) {
	result := Fail("test check", "broken")
	if result.Status != StatusFail {
		t.Errorf("Fail() status = %q, want %q", result.Status, StatusFail)
	}
	if result.HasFix() {
		t.Error("Fail() should not have a fix")
	}
}

func TestFailWithFixResult(t *testing.T) {
	result := FailWithFix("test check", "broken", "repair it",
		func(ctx context.Context) error { return nil })
	if result.Status != StatusFail {
		t.Errorf("FailWithFix() status = %q, want %q", result.Status, StatusFail)
	}
	if !result.HasFix() {
		t.Error("FailWithFix() should have a fix")
	}
	if result.FixHint != "repair it" {
		t.Errorf("FailWithFix() fix hint = %q, want %q", result.FixHint, "repair it")
	}
	if result.Elevated {
		t.Error("FailWithFix() should not be elevated")
	}
}

func TestFailElevatedResult(t *testing.T) {
	result := FailElevated("test check", "needs root", "run as root",
		func(ctx context.Context) error { return nil })
	if result.Status != StatusFail {
		t.Errorf("FailElevated() status = %q, want %q", result.Status, StatusFail)
	}
	if !result.HasFix() {
		t.Error("FailElevated() should have a fix")
	}
	if !result.Elevated {
		t.Error("FailElevated() should be elevated")
	}
}

func TestWarnResult(t *testing.T) {
	result := Warn("test check", "heads up")
	if result.Status != StatusWarn {
		t.Errorf("Warn() status = %q, want %q", result.Status, StatusWarn)
	}
}

func TestSkipResult(t *testing.T) {
	result := Skip("test check", "skipped: prerequisite failed")
	if result.Status != StatusSkip {
		t.Errorf("Skip() status = %q, want %q", result.Status, StatusSkip)
	}
}

func TestExecuteFixesDryRun(t *testing.T) {
	fixCalled := false
	results := []Result{
		FailWithFix("check", "broken", "fix it", func(ctx context.Context) error {
			fixCalled = true
			return nil
		}),
	}

	outcome := ExecuteFixes(context.Background(), results, true, nil)

	if fixCalled {
		t.Error("ExecuteFixes(dryRun=true) should not call fix actions")
	}
	if outcome.FixedCount != 0 {
		t.Errorf("ExecuteFixes(dryRun=true) fixed count = %d, want 0", outcome.FixedCount)
	}
	if results[0].Status != StatusFail {
		t.Errorf("ExecuteFixes(dryRun=true) should not change status, got %q", results[0].Status)
	}
}

func TestExecuteFixesSuccess(t *testing.T) {
	results := []Result{
		Pass("ok check", "fine"),
		FailWithFix("broken check", "broken", "fix it", func(ctx context.Context) error {
			return nil
		}),
		Fail("unfixable", "no fix available"),
	}

	outcome := ExecuteFixes(context.Background(), results, false, nil)

	if outcome.FixedCount != 1 {
		t.Errorf("ExecuteFixes() fixed count = %d, want 1", outcome.FixedCount)
	}
	if results[1].Status != StatusFixed {
		t.Errorf("ExecuteFixes() should set status to fixed, got %q", results[1].Status)
	}
	// Pass and unfixable fail should be unchanged.
	if results[0].Status != StatusPass {
		t.Errorf("pass result should be unchanged, got %q", results[0].Status)
	}
	if results[2].Status != StatusFail {
		t.Errorf("unfixable result should be unchanged, got %q", results[2].Status)
	}
}

func TestExecuteFixesPermissionDenied(t *testing.T) {
	permErr := errors.New("permission denied")
	results := []Result{
		FailWithFix("check", "broken", "fix it", func(ctx context.Context) error {
			return permErr
		}),
	}

	outcome := ExecuteFixes(context.Background(), results, false, func(err error) bool {
		return err == permErr
	})

	if !outcome.PermissionDenied {
		t.Error("ExecuteFixes() should set PermissionDenied when isPermissionDenied returns true")
	}
	if results[0].Status != StatusFail {
		t.Errorf("permission denied fix should remain failed, got %q", results[0].Status)
	}
	if outcome.FixedCount != 0 {
		t.Errorf("permission denied fix should not count as fixed, got %d", outcome.FixedCount)
	}
}

func TestExecuteFixesFixError(t *testing.T) {
	results := []Result{
		FailWithFix("check", "broken", "fix it", func(ctx context.Context) error {
			return errors.New("fix exploded")
		}),
	}

	outcome := ExecuteFixes(context.Background(), results, false, nil)

	if outcome.FixedCount != 0 {
		t.Errorf("failed fix should not count, got %d", outcome.FixedCount)
	}
	if results[0].Status != StatusFail {
		t.Errorf("failed fix should remain failed, got %q", results[0].Status)
	}
	if results[0].Message != "broken (fix failed: fix exploded)" {
		t.Errorf("failed fix should append error, got %q", results[0].Message)
	}
}

func TestExecuteFixesElevatedSkippedWhenNotRoot(t *testing.T) {
	// This test can only verify the elevated-skip path if the test
	// process is not running as root. In CI environments running as
	// root, skip.
	if IsRoot() {
		t.Skip("test requires non-root process")
	}

	fixCalled := false
	results := []Result{
		FailElevated("elevated check", "needs root", "create user", func(ctx context.Context) error {
			fixCalled = true
			return nil
		}),
		FailWithFix("normal check", "broken", "fix it", func(ctx context.Context) error {
			return nil
		}),
	}

	outcome := ExecuteFixes(context.Background(), results, false, nil)

	if fixCalled {
		t.Error("elevated fix should not be called when not root")
	}
	if outcome.ElevatedSkipped != 1 {
		t.Errorf("elevated skipped = %d, want 1", outcome.ElevatedSkipped)
	}
	if results[0].Status != StatusFail {
		t.Errorf("elevated result should remain failed, got %q", results[0].Status)
	}
	// Non-elevated fix should still run.
	if results[1].Status != StatusFixed {
		t.Errorf("non-elevated result should be fixed, got %q", results[1].Status)
	}
	if outcome.FixedCount != 1 {
		t.Errorf("fixed count = %d, want 1", outcome.FixedCount)
	}
}

func TestBuildJSON(t *testing.T) {
	results := []Result{
		Pass("check1", "ok"),
		Fail("check2", "broken"),
	}
	outcome := Outcome{FixedCount: 0, PermissionDenied: true, ElevatedSkipped: 2}

	output := BuildJSON(results, true, outcome)

	if output.OK {
		t.Error("BuildJSON() should be not OK when a check fails")
	}
	if !output.DryRun {
		t.Error("BuildJSON() should reflect dry run")
	}
	if !output.PermissionDenied {
		t.Error("BuildJSON() should reflect permission denied")
	}
	if output.ElevatedSkipped != 2 {
		t.Errorf("BuildJSON() elevated skipped = %d, want 2", output.ElevatedSkipped)
	}
	if len(output.Checks) != 2 {
		t.Errorf("BuildJSON() checks count = %d, want 2", len(output.Checks))
	}
}

func TestBuildJSONAllPass(t *testing.T) {
	results := []Result{
		Pass("check1", "ok"),
		Pass("check2", "ok"),
	}

	output := BuildJSON(results, false, Outcome{})

	if !output.OK {
		t.Error("BuildJSON() should be OK when all checks pass")
	}
}

func TestMarkRepaired(t *testing.T) {
	results := []Result{
		Pass("repaired check", "now passing"),
		Pass("always passed", "fine"),
		Fail("still broken", "bad"),
	}
	repairedNames := map[string]bool{
		"repaired check": true,
		"still broken":   true,
	}

	MarkRepaired(results, repairedNames)

	if results[0].Status != StatusFixed {
		t.Errorf("repaired check should be marked fixed, got %q", results[0].Status)
	}
	if results[1].Status != StatusPass {
		t.Errorf("always-passed check should remain pass, got %q", results[1].Status)
	}
	// Fail results are not touched by MarkRepaired (only pass → fixed).
	if results[2].Status != StatusFail {
		t.Errorf("still-broken check should remain fail, got %q", results[2].Status)
	}
}
