// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// variablePattern matches ${NAME} references in strings. Only the
// braced form is recognized — bare $NAME is left for shell
// interpretation. Variable names must start with a letter or
// underscore and contain only letters, digits, and underscores.
var variablePattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ResolveVariables merges variable sources according to pipeline
// resolution order (lowest to highest priority):
//
//  1. Declared defaults from pipeline variable definitions
//  2. Payload values from the pipeline execution request
//  3. Environment lookup via the environ function
//
// Returns the merged variable map. Returns an error if any required
// variable (per its declaration) has no value from any source.
//
// The environ function is typically os.Getenv for production use, or
// a stub for testing. It is only consulted for variables that are
// declared in the pipeline — undeclared environment variables are not
// included in the result.
func ResolveVariables(declarations map[string]schema.PipelineVariable, payload map[string]string, environ func(string) string) (map[string]string, error) {
	resolved := make(map[string]string, len(declarations)+len(payload))

	// Start with declared defaults (lowest priority).
	for name, declaration := range declarations {
		if declaration.Default != "" {
			resolved[name] = declaration.Default
		}
	}

	// Overlay payload values (medium priority).
	for name, value := range payload {
		resolved[name] = value
	}

	// Overlay environment values for declared variables (highest priority).
	// Only declared variables are looked up — we don't pull in the entire
	// process environment.
	if environ != nil {
		for name := range declarations {
			if value := environ(name); value != "" {
				resolved[name] = value
			}
		}
	}

	// Check that all required variables have a value.
	var missing []string
	for name, declaration := range declarations {
		if declaration.Required {
			if _, exists := resolved[name]; !exists {
				missing = append(missing, name)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("required pipeline variables not set: %s", strings.Join(missing, ", "))
	}

	return resolved, nil
}

// Expand replaces ${NAME} references in input with values from the
// variables map. Only the ${NAME} form is recognized (braces required);
// bare $NAME is left for shell interpretation.
//
// Returns an error listing all referenced variables that have no value
// in the map. This ensures pipeline definitions fail fast on
// unresolvable references rather than producing broken commands.
func Expand(input string, variables map[string]string) (string, error) {
	var unresolved []string

	result := variablePattern.ReplaceAllStringFunc(input, func(match string) string {
		// Extract the variable name from ${NAME}.
		name := match[2 : len(match)-1]
		if value, exists := variables[name]; exists {
			return value
		}
		unresolved = append(unresolved, name)
		return match
	})

	if len(unresolved) > 0 {
		return "", fmt.Errorf("unresolved pipeline variables: %s", strings.Join(unresolved, ", "))
	}

	return result, nil
}

// ExpandStep returns a copy of step with all string fields expanded
// using Expand. Step-level Env values are expanded first (against
// pipeline variables only), then merged into the variable map for
// expanding other fields. This means a run command can reference
// step env variables with ${NAME}, and those values will already
// have their own ${REFERENCES} resolved.
//
// The original step and variables map are not modified.
func ExpandStep(step schema.PipelineStep, variables map[string]string) (schema.PipelineStep, error) {
	// First pass: expand step-level env values against pipeline
	// variables only (not against other step env values — no
	// cross-referencing between env entries).
	var expandedEnv map[string]string
	if len(step.Env) > 0 {
		expandedEnv = make(map[string]string, len(step.Env))
		for name, value := range step.Env {
			expandedValue, err := Expand(value, variables)
			if err != nil {
				return schema.PipelineStep{}, fmt.Errorf("step %q env[%s]: %w", step.Name, name, err)
			}
			expandedEnv[name] = expandedValue
		}
	}

	// Build the merged variable map: pipeline variables as base,
	// expanded step env on top. Step env takes precedence.
	merged := make(map[string]string, len(variables)+len(expandedEnv))
	for name, value := range variables {
		merged[name] = value
	}
	for name, value := range expandedEnv {
		merged[name] = value
	}

	var err error

	// Expand string fields on the step.
	if step.Run, err = Expand(step.Run, merged); err != nil {
		return schema.PipelineStep{}, fmt.Errorf("step %q run: %w", step.Name, err)
	}
	if step.Check, err = Expand(step.Check, merged); err != nil {
		return schema.PipelineStep{}, fmt.Errorf("step %q check: %w", step.Name, err)
	}
	if step.When, err = Expand(step.When, merged); err != nil {
		return schema.PipelineStep{}, fmt.Errorf("step %q when: %w", step.Name, err)
	}

	// Expand publish fields if present.
	if step.Publish != nil {
		expanded := *step.Publish
		if expanded.EventType, err = Expand(expanded.EventType, merged); err != nil {
			return schema.PipelineStep{}, fmt.Errorf("step %q publish.event_type: %w", step.Name, err)
		}
		if expanded.Room, err = Expand(expanded.Room, merged); err != nil {
			return schema.PipelineStep{}, fmt.Errorf("step %q publish.room: %w", step.Name, err)
		}
		if expanded.StateKey, err = Expand(expanded.StateKey, merged); err != nil {
			return schema.PipelineStep{}, fmt.Errorf("step %q publish.state_key: %w", step.Name, err)
		}
		if expanded.Content, err = expandMap(expanded.Content, merged); err != nil {
			return schema.PipelineStep{}, fmt.Errorf("step %q publish.content: %w", step.Name, err)
		}
		step.Publish = &expanded
	}

	step.Env = expandedEnv
	return step, nil
}

// expandMap recursively expands ${NAME} references in string values
// within a map[string]any. Non-string values are passed through
// unchanged. Nested maps are expanded recursively.
func expandMap(input map[string]any, variables map[string]string) (map[string]any, error) {
	if input == nil {
		return nil, nil
	}

	result := make(map[string]any, len(input))
	for key, value := range input {
		switch typed := value.(type) {
		case string:
			expanded, err := Expand(typed, variables)
			if err != nil {
				return nil, fmt.Errorf("key %q: %w", key, err)
			}
			result[key] = expanded
		case map[string]any:
			expanded, err := expandMap(typed, variables)
			if err != nil {
				return nil, fmt.Errorf("key %q: %w", key, err)
			}
			result[key] = expanded
		default:
			result[key] = value
		}
	}
	return result, nil
}
