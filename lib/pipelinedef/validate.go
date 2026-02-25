// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipelinedef

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
)

// outputNamePattern matches valid output names. Same character set as
// variable names (identifiers): start with a letter or underscore,
// followed by letters, digits, or underscores. Anchored to the full
// string.
var outputNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
func Validate(content *pipeline.PipelineContent) []string {
	var issues []string

	if len(content.Steps) == 0 {
		issues = append(issues, "pipeline has no steps (at least one step is required)")
	}

	// Step names must be unique across the pipeline. Duplicate names
	// would cause OUTPUT_<step>_<name> variable collisions, silently
	// overwriting earlier step outputs with later ones.
	stepNames := make(map[string]int, len(content.Steps))
	for index, step := range content.Steps {
		if step.Name != "" {
			if firstIndex, exists := stepNames[step.Name]; exists {
				issues = append(issues, fmt.Sprintf(
					"steps[%d] %q: duplicate step name (first used at steps[%d])",
					index, step.Name, firstIndex,
				))
			} else {
				stepNames[step.Name] = index
			}
		}
	}

	for index, step := range content.Steps {
		prefix := fmt.Sprintf("steps[%d]", index)
		issues = append(issues, validateStep(step, prefix)...)
	}

	for index, step := range content.OnFailure {
		prefix := fmt.Sprintf("on_failure[%d]", index)
		issues = append(issues, validateStep(step, prefix)...)

		// On-failure steps cannot have outputs. They run after the
		// main pipeline has failed — there are no subsequent steps to
		// consume their outputs, and the pipeline result has conclusion
		// "failure" which produces no pipeline-level outputs.
		if len(step.Outputs) > 0 {
			issues = append(issues, fmt.Sprintf("%s %q: outputs are not allowed on on_failure steps", prefix, step.Name))
		}
	}

	// Pipeline-level output declarations.
	for name, output := range content.Outputs {
		prefix := fmt.Sprintf("outputs[%q]", name)
		if !outputNamePattern.MatchString(name) {
			issues = append(issues, fmt.Sprintf(
				"%s: output name must be a valid identifier ([A-Za-z_][A-Za-z0-9_]*)",
				prefix,
			))
		}
		if output.Value == "" {
			issues = append(issues, fmt.Sprintf("%s: value is required", prefix))
		}
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
func validateStep(step pipeline.PipelineStep, prefix string) []string {
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
		if len(step.Outputs) > 0 {
			issues = append(issues, fmt.Sprintf("%s: outputs are only valid on run steps", prefix))
		}
	}

	// Validate output declarations.
	if len(step.Outputs) > 0 {
		issues = append(issues, validateStepOutputs(step.Outputs, prefix)...)
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
func validateAssertState(assertState *pipeline.PipelineAssertState, prefix string) []string {
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
	case "", pipeline.MismatchFail, pipeline.MismatchAbort:
		// Valid.
	default:
		issues = append(issues, fmt.Sprintf("%s: assert_state.on_mismatch must be \"fail\" or \"abort\", got %q", prefix, assertState.OnMismatch))
	}

	return issues
}

// validateStepOutputs validates the output declarations on a single step.
// Parses each raw declaration (string or object form) and checks:
//   - Output names are valid identifiers
//   - Each output has a non-empty path
//   - content_type is only set when artifact is true
func validateStepOutputs(outputs map[string]json.RawMessage, prefix string) []string {
	var issues []string

	for name, raw := range outputs {
		outputPrefix := fmt.Sprintf("%s: outputs[%q]", prefix, name)

		if !outputNamePattern.MatchString(name) {
			issues = append(issues, fmt.Sprintf(
				"%s: output name must be a valid identifier ([A-Za-z_][A-Za-z0-9_]*)",
				outputPrefix,
			))
		}

		parsed, err := pipeline.ParseStepOutputs(map[string]json.RawMessage{name: raw})
		if err != nil {
			issues = append(issues, fmt.Sprintf("%s: %v", outputPrefix, err))
			continue
		}

		output := parsed[name]
		if strings.TrimSpace(output.Path) == "" {
			issues = append(issues, fmt.Sprintf("%s: path is required", outputPrefix))
		}
		if output.ContentType != "" && !output.Artifact {
			issues = append(issues, fmt.Sprintf("%s: content_type is only valid when artifact is true", outputPrefix))
		}
	}

	return issues
}
