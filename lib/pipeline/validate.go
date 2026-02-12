// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"fmt"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// Validate checks a PipelineContent for structural issues. Returns a
// list of human-readable issue descriptions. An empty list means the
// pipeline is valid.
//
// Structural checks include:
//   - At least one step is required
//   - Each step must have a non-empty Name
//   - Each step must set exactly one of Run, Publish, or AssertState
//   - Check, Interactive, and GracePeriod are only valid on Run steps
//   - When is valid on Run, Publish, and AssertState steps (conditional guard)
//   - Publish steps must have EventType, Room, and Content
//   - AssertState steps must have Room, EventType, Field, and exactly one condition
//   - AssertState OnMismatch must be "fail" or "abort" (or empty, defaulting to "fail")
//   - Timeout (when present) must be parseable by time.ParseDuration
//   - Log.Room must be non-empty when Log is set
//   - OnFailure steps are validated with the same rules as regular steps
func Validate(content *schema.PipelineContent) []string {
	var issues []string

	if len(content.Steps) == 0 {
		issues = append(issues, "pipeline has no steps (at least one step is required)")
	}

	for index, step := range content.Steps {
		prefix := fmt.Sprintf("steps[%d]", index)
		issues = append(issues, validateStep(step, prefix)...)
	}

	for index, step := range content.OnFailure {
		prefix := fmt.Sprintf("on_failure[%d]", index)
		issues = append(issues, validateStep(step, prefix)...)
	}

	// Log room must be non-empty when Log is configured.
	if content.Log != nil && content.Log.Room == "" {
		issues = append(issues, "log.room is required when log is configured")
	}

	return issues
}

// validateStep checks a single pipeline step for structural issues.
// The prefix identifies the step's position (e.g., "steps[0]" or
// "on_failure[1]") for error messages.
func validateStep(step schema.PipelineStep, prefix string) []string {
	var issues []string

	if step.Name == "" {
		issues = append(issues, fmt.Sprintf("%s: name is required", prefix))
	} else {
		prefix = fmt.Sprintf("%s %q", prefix, step.Name)
	}

	hasRun := step.Run != ""
	hasPublish := step.Publish != nil
	hasAssertState := step.AssertState != nil

	actionCount := 0
	if hasRun {
		actionCount++
	}
	if hasPublish {
		actionCount++
	}
	if hasAssertState {
		actionCount++
	}

	switch {
	case actionCount > 1:
		issues = append(issues, fmt.Sprintf("%s: run, publish, and assert_state are mutually exclusive (set exactly one)", prefix))
	case actionCount == 0:
		issues = append(issues, fmt.Sprintf("%s: must set exactly one of run, publish, or assert_state", prefix))
	}

	// Fields that are only meaningful for Run steps. When is a
	// general-purpose conditional guard and is valid on Run, Publish,
	// and AssertState steps (e.g., conditional state publication based
	// on a variable).
	if !hasRun {
		if step.Check != "" {
			issues = append(issues, fmt.Sprintf("%s: check is only valid on run steps", prefix))
		}
		if step.Interactive {
			issues = append(issues, fmt.Sprintf("%s: interactive is only valid on run steps", prefix))
		}
		if step.GracePeriod != "" {
			issues = append(issues, fmt.Sprintf("%s: grace_period is only valid on run steps", prefix))
		}
	}

	// Publish step must have event_type and content.
	if hasPublish {
		if step.Publish.EventType == "" {
			issues = append(issues, fmt.Sprintf("%s: publish.event_type is required", prefix))
		}
		if step.Publish.Room == "" {
			issues = append(issues, fmt.Sprintf("%s: publish.room is required", prefix))
		}
		if step.Publish.Content == nil {
			issues = append(issues, fmt.Sprintf("%s: publish.content is required", prefix))
		}
	}

	// AssertState validation.
	if hasAssertState {
		issues = append(issues, validateAssertState(step.AssertState, prefix)...)
	}

	// Timeout must be parseable when present.
	if step.Timeout != "" {
		if _, err := time.ParseDuration(step.Timeout); err != nil {
			issues = append(issues, fmt.Sprintf("%s: invalid timeout %q: %v", prefix, step.Timeout, err))
		}
	}

	// GracePeriod must be parseable when present.
	if step.GracePeriod != "" {
		if _, err := time.ParseDuration(step.GracePeriod); err != nil {
			issues = append(issues, fmt.Sprintf("%s: invalid grace_period %q: %v", prefix, step.GracePeriod, err))
		}
	}

	return issues
}

// validateAssertState checks assert_state-specific fields.
func validateAssertState(assertState *schema.PipelineAssertState, prefix string) []string {
	var issues []string

	if assertState.Room == "" {
		issues = append(issues, fmt.Sprintf("%s: assert_state.room is required", prefix))
	}
	if assertState.EventType == "" {
		issues = append(issues, fmt.Sprintf("%s: assert_state.event_type is required", prefix))
	}
	if assertState.Field == "" {
		issues = append(issues, fmt.Sprintf("%s: assert_state.field is required", prefix))
	}

	// Exactly one condition must be set.
	conditionCount := 0
	if assertState.Equals != "" {
		conditionCount++
	}
	if assertState.NotEquals != "" {
		conditionCount++
	}
	if len(assertState.In) > 0 {
		conditionCount++
	}
	if len(assertState.NotIn) > 0 {
		conditionCount++
	}

	switch conditionCount {
	case 0:
		issues = append(issues, fmt.Sprintf("%s: assert_state requires exactly one condition (equals, not_equals, in, or not_in)", prefix))
	case 1:
		// Valid.
	default:
		issues = append(issues, fmt.Sprintf("%s: assert_state conditions are mutually exclusive (set exactly one of equals, not_equals, in, not_in)", prefix))
	}

	// OnMismatch must be a valid value.
	switch assertState.OnMismatch {
	case "", "fail", "abort":
		// Valid.
	default:
		issues = append(issues, fmt.Sprintf("%s: assert_state.on_mismatch must be \"fail\" or \"abort\", got %q", prefix, assertState.OnMismatch))
	}

	return issues
}
