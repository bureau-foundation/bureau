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
//   - Each step must set exactly one of Run or Publish (not both, not neither)
//   - Check, When, and Interactive are only valid on Run steps
//   - Publish steps must have EventType and Content
//   - Timeout (when present) must be parseable by time.ParseDuration
//   - Log.Room must be non-empty when Log is set
func Validate(content *schema.PipelineContent) []string {
	var issues []string

	if len(content.Steps) == 0 {
		issues = append(issues, "pipeline has no steps (at least one step is required)")
	}

	for index, step := range content.Steps {
		prefix := fmt.Sprintf("steps[%d]", index)

		if step.Name == "" {
			issues = append(issues, fmt.Sprintf("%s: name is required", prefix))
		} else {
			prefix = fmt.Sprintf("steps[%d] %q", index, step.Name)
		}

		hasRun := step.Run != ""
		hasPublish := step.Publish != nil

		switch {
		case hasRun && hasPublish:
			issues = append(issues, fmt.Sprintf("%s: run and publish are mutually exclusive (set exactly one)", prefix))
		case !hasRun && !hasPublish:
			issues = append(issues, fmt.Sprintf("%s: must set either run or publish", prefix))
		}

		// Fields that are only meaningful for Run steps.
		if !hasRun {
			if step.Check != "" {
				issues = append(issues, fmt.Sprintf("%s: check is only valid on run steps", prefix))
			}
			if step.When != "" {
				issues = append(issues, fmt.Sprintf("%s: when is only valid on run steps", prefix))
			}
			if step.Interactive {
				issues = append(issues, fmt.Sprintf("%s: interactive is only valid on run steps", prefix))
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

		// Timeout must be parseable when present.
		if step.Timeout != "" {
			if _, err := time.ParseDuration(step.Timeout); err != nil {
				issues = append(issues, fmt.Sprintf("%s: invalid timeout %q: %v", prefix, step.Timeout, err))
			}
		}
	}

	// Log room must be non-empty when Log is configured.
	if content.Log != nil && content.Log.Room == "" {
		issues = append(issues, "log.room is required when log is configured")
	}

	return issues
}
