// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestToolError_ErrorWithoutHint(t *testing.T) {
	err := Validation("missing required flag --fleet")
	if err.Error() != "missing required flag --fleet" {
		t.Errorf("Error() = %q, want %q", err.Error(), "missing required flag --fleet")
	}
}

func TestToolError_ErrorWithHint(t *testing.T) {
	err := Validation("missing required flag --fleet").
		WithHint("Pass --fleet <localpart> or run 'bureau machine doctor'.")

	want := "missing required flag --fleet\n\nPass --fleet <localpart> or run 'bureau machine doctor'."
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestToolError_WithHintReturnsReceiver(t *testing.T) {
	original := Validation("bad input")
	chained := original.WithHint("fix it")
	if original != chained {
		t.Error("WithHint should return the same pointer")
	}
}

func TestToolError_WithHintPreservesCategory(t *testing.T) {
	err := NotFound("machine %q not found", "gpu-box").
		WithHint("Run 'bureau machine list' to see available machines.")

	if err.Category != CategoryNotFound {
		t.Errorf("Category = %q, want %q", err.Category, CategoryNotFound)
	}
}

func TestToolError_HintSurvivesErrorsAs(t *testing.T) {
	inner := Validation("bad fleet").WithHint("use bureau/fleet/prod format")
	wrapped := fmt.Errorf("setup failed: %w", inner)

	var toolErr *ToolError
	if !errors.As(wrapped, &toolErr) {
		t.Fatal("errors.As should find ToolError in wrapped chain")
	}
	if toolErr.Hint != "use bureau/fleet/prod format" {
		t.Errorf("Hint = %q after unwrap, want %q", toolErr.Hint, "use bureau/fleet/prod format")
	}
}

func TestToolError_EmptyHintNotAppended(t *testing.T) {
	err := Internal("unexpected failure")
	if strings.Contains(err.Error(), "\n\n") {
		t.Error("empty hint should not add blank line to error message")
	}
}

func TestToolError_AllCategories(t *testing.T) {
	tests := []struct {
		name     string
		err      *ToolError
		category ErrorCategory
	}{
		{"Validation", Validation("bad"), CategoryValidation},
		{"NotFound", NotFound("missing"), CategoryNotFound},
		{"Forbidden", Forbidden("denied"), CategoryForbidden},
		{"Conflict", Conflict("duplicate"), CategoryConflict},
		{"Transient", Transient("timeout"), CategoryTransient},
		{"Internal", Internal("bug"), CategoryInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.err.Category != test.category {
				t.Errorf("Category = %q, want %q", test.err.Category, test.category)
			}
			// All constructors should support WithHint.
			hinted := test.err.WithHint("try again")
			if hinted.Hint != "try again" {
				t.Errorf("Hint = %q after WithHint, want %q", hinted.Hint, "try again")
			}
		})
	}
}
