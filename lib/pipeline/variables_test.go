// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestResolveVariables(t *testing.T) {
	t.Parallel()

	t.Run("defaults only", func(t *testing.T) {
		t.Parallel()

		declarations := map[string]schema.PipelineVariable{
			"BRANCH": {Default: "main"},
			"MODE":   {Default: "development"},
		}

		resolved, err := ResolveVariables(declarations, nil, nil)
		if err != nil {
			t.Fatalf("ResolveVariables: %v", err)
		}
		if resolved["BRANCH"] != "main" {
			t.Errorf("BRANCH = %q, want %q", resolved["BRANCH"], "main")
		}
		if resolved["MODE"] != "development" {
			t.Errorf("MODE = %q, want %q", resolved["MODE"], "development")
		}
	})

	t.Run("payload overrides defaults", func(t *testing.T) {
		t.Parallel()

		declarations := map[string]schema.PipelineVariable{
			"BRANCH": {Default: "main"},
		}
		payload := map[string]string{"BRANCH": "feature/test"}

		resolved, err := ResolveVariables(declarations, payload, nil)
		if err != nil {
			t.Fatalf("ResolveVariables: %v", err)
		}
		if resolved["BRANCH"] != "feature/test" {
			t.Errorf("BRANCH = %q, want %q", resolved["BRANCH"], "feature/test")
		}
	})

	t.Run("environ overrides payload", func(t *testing.T) {
		t.Parallel()

		declarations := map[string]schema.PipelineVariable{
			"BRANCH": {Default: "main"},
		}
		payload := map[string]string{"BRANCH": "feature/test"}
		environ := func(name string) string {
			if name == "BRANCH" {
				return "env-branch"
			}
			return ""
		}

		resolved, err := ResolveVariables(declarations, payload, environ)
		if err != nil {
			t.Fatalf("ResolveVariables: %v", err)
		}
		if resolved["BRANCH"] != "env-branch" {
			t.Errorf("BRANCH = %q, want %q", resolved["BRANCH"], "env-branch")
		}
	})

	t.Run("environ only checks declared variables", func(t *testing.T) {
		t.Parallel()

		declarations := map[string]schema.PipelineVariable{
			"DECLARED": {},
		}
		environ := func(name string) string {
			if name == "DECLARED" {
				return "from-env"
			}
			if name == "UNDECLARED" {
				return "should-not-appear"
			}
			return ""
		}

		resolved, err := ResolveVariables(declarations, nil, environ)
		if err != nil {
			t.Fatalf("ResolveVariables: %v", err)
		}
		if resolved["DECLARED"] != "from-env" {
			t.Errorf("DECLARED = %q, want %q", resolved["DECLARED"], "from-env")
		}
		if _, exists := resolved["UNDECLARED"]; exists {
			t.Error("UNDECLARED should not be in resolved map")
		}
	})

	t.Run("payload includes undeclared variables", func(t *testing.T) {
		t.Parallel()

		declarations := map[string]schema.PipelineVariable{}
		payload := map[string]string{"EXTRA": "bonus"}

		resolved, err := ResolveVariables(declarations, payload, nil)
		if err != nil {
			t.Fatalf("ResolveVariables: %v", err)
		}
		if resolved["EXTRA"] != "bonus" {
			t.Errorf("EXTRA = %q, want %q", resolved["EXTRA"], "bonus")
		}
	})

	t.Run("required variable satisfied by default", func(t *testing.T) {
		t.Parallel()

		declarations := map[string]schema.PipelineVariable{
			"REPO": {Required: true, Default: "https://github.com/example/repo"},
		}

		resolved, err := ResolveVariables(declarations, nil, nil)
		if err != nil {
			t.Fatalf("ResolveVariables: %v", err)
		}
		if resolved["REPO"] != "https://github.com/example/repo" {
			t.Errorf("REPO = %q", resolved["REPO"])
		}
	})

	t.Run("required variable satisfied by payload", func(t *testing.T) {
		t.Parallel()

		declarations := map[string]schema.PipelineVariable{
			"REPO": {Required: true},
		}
		payload := map[string]string{"REPO": "https://github.com/example/repo"}

		resolved, err := ResolveVariables(declarations, payload, nil)
		if err != nil {
			t.Fatalf("ResolveVariables: %v", err)
		}
		if resolved["REPO"] != "https://github.com/example/repo" {
			t.Errorf("REPO = %q", resolved["REPO"])
		}
	})

	t.Run("required variable missing", func(t *testing.T) {
		t.Parallel()

		declarations := map[string]schema.PipelineVariable{
			"REPO": {Required: true},
		}

		_, err := ResolveVariables(declarations, nil, nil)
		if err == nil {
			t.Fatal("expected error for missing required variable")
		}
		if !strings.Contains(err.Error(), "REPO") {
			t.Errorf("error should mention REPO: %v", err)
		}
	})

	t.Run("multiple required variables missing", func(t *testing.T) {
		t.Parallel()

		declarations := map[string]schema.PipelineVariable{
			"ALPHA": {Required: true},
			"BRAVO": {Required: true},
		}

		_, err := ResolveVariables(declarations, nil, nil)
		if err == nil {
			t.Fatal("expected error for missing required variables")
		}
		// Error message lists them alphabetically.
		if !strings.Contains(err.Error(), "ALPHA") || !strings.Contains(err.Error(), "BRAVO") {
			t.Errorf("error should mention both variables: %v", err)
		}
	})

	t.Run("empty declarations and payload", func(t *testing.T) {
		t.Parallel()

		resolved, err := ResolveVariables(nil, nil, nil)
		if err != nil {
			t.Fatalf("ResolveVariables: %v", err)
		}
		if len(resolved) != 0 {
			t.Errorf("expected empty map, got %v", resolved)
		}
	})
}

func TestExpand(t *testing.T) {
	t.Parallel()

	t.Run("simple substitution", func(t *testing.T) {
		t.Parallel()

		result, err := Expand("git clone ${REPO}", map[string]string{"REPO": "https://github.com/org/repo"})
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		if result != "git clone https://github.com/org/repo" {
			t.Errorf("result = %q", result)
		}
	})

	t.Run("multiple references", func(t *testing.T) {
		t.Parallel()

		variables := map[string]string{"USER": "alice", "HOST": "example.com"}
		result, err := Expand("ssh ${USER}@${HOST}", variables)
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		if result != "ssh alice@example.com" {
			t.Errorf("result = %q", result)
		}
	})

	t.Run("repeated reference", func(t *testing.T) {
		t.Parallel()

		result, err := Expand("${X} and ${X}", map[string]string{"X": "value"})
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		if result != "value and value" {
			t.Errorf("result = %q", result)
		}
	})

	t.Run("no references", func(t *testing.T) {
		t.Parallel()

		result, err := Expand("no variables here", map[string]string{"UNUSED": "value"})
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		if result != "no variables here" {
			t.Errorf("result = %q", result)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		t.Parallel()

		result, err := Expand("", map[string]string{"X": "value"})
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		if result != "" {
			t.Errorf("result = %q, want empty", result)
		}
	})

	t.Run("bare dollar left for shell", func(t *testing.T) {
		t.Parallel()

		result, err := Expand("for f in $files; do echo $f; done", nil)
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		if result != "for f in $files; do echo $f; done" {
			t.Errorf("result = %q (should be unchanged)", result)
		}
	})

	t.Run("unresolved variable", func(t *testing.T) {
		t.Parallel()

		_, err := Expand("git clone ${REPO}", map[string]string{})
		if err == nil {
			t.Fatal("expected error for unresolved variable")
		}
		if !strings.Contains(err.Error(), "REPO") {
			t.Errorf("error should mention REPO: %v", err)
		}
	})

	t.Run("multiple unresolved variables", func(t *testing.T) {
		t.Parallel()

		_, err := Expand("${ALPHA} and ${BRAVO}", map[string]string{})
		if err == nil {
			t.Fatal("expected error for unresolved variables")
		}
		if !strings.Contains(err.Error(), "ALPHA") || !strings.Contains(err.Error(), "BRAVO") {
			t.Errorf("error should mention both: %v", err)
		}
	})

	t.Run("variable with underscore and digits", func(t *testing.T) {
		t.Parallel()

		result, err := Expand("${MY_VAR_2}", map[string]string{"MY_VAR_2": "value"})
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		if result != "value" {
			t.Errorf("result = %q", result)
		}
	})

	t.Run("adjacent to braces", func(t *testing.T) {
		t.Parallel()

		result, err := Expand("{${NAME}}", map[string]string{"NAME": "test"})
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		if result != "{test}" {
			t.Errorf("result = %q", result)
		}
	})

	t.Run("empty value is valid", func(t *testing.T) {
		t.Parallel()

		result, err := Expand("prefix-${EMPTY}-suffix", map[string]string{"EMPTY": ""})
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		if result != "prefix--suffix" {
			t.Errorf("result = %q", result)
		}
	})
}

func TestExpandStep(t *testing.T) {
	t.Parallel()

	t.Run("run step", func(t *testing.T) {
		t.Parallel()

		step := schema.PipelineStep{
			Name:  "clone",
			Run:   "git clone ${REPO} ${WORKSPACE}/src",
			When:  "test -n '${REPO}'",
			Check: "test -d ${WORKSPACE}/src/.git",
		}
		variables := map[string]string{
			"REPO":      "https://github.com/org/repo",
			"WORKSPACE": "/var/bureau/workspace/test",
		}

		expanded, err := ExpandStep(step, variables)
		if err != nil {
			t.Fatalf("ExpandStep: %v", err)
		}
		if expanded.Run != "git clone https://github.com/org/repo /var/bureau/workspace/test/src" {
			t.Errorf("Run = %q", expanded.Run)
		}
		if expanded.When != "test -n 'https://github.com/org/repo'" {
			t.Errorf("When = %q", expanded.When)
		}
		if expanded.Check != "test -d /var/bureau/workspace/test/src/.git" {
			t.Errorf("Check = %q", expanded.Check)
		}
	})

	t.Run("step env overrides pipeline variables", func(t *testing.T) {
		t.Parallel()

		step := schema.PipelineStep{
			Name: "build",
			Run:  "CC=${CC} make",
			Env:  map[string]string{"CC": "clang"},
		}
		variables := map[string]string{"CC": "gcc"}

		expanded, err := ExpandStep(step, variables)
		if err != nil {
			t.Fatalf("ExpandStep: %v", err)
		}
		if expanded.Run != "CC=clang make" {
			t.Errorf("Run = %q (step env should override)", expanded.Run)
		}
	})

	t.Run("step env values are expanded", func(t *testing.T) {
		t.Parallel()

		step := schema.PipelineStep{
			Name: "setup",
			Run:  "cat ${CONFIG}",
			Env:  map[string]string{"CONFIG": "${WORKSPACE}/config.json"},
		}
		variables := map[string]string{"WORKSPACE": "/var/bureau/workspace/test"}

		expanded, err := ExpandStep(step, variables)
		if err != nil {
			t.Fatalf("ExpandStep: %v", err)
		}
		// Step env values should be expanded against pipeline variables.
		if expanded.Env["CONFIG"] != "/var/bureau/workspace/test/config.json" {
			t.Errorf("Env[CONFIG] = %q", expanded.Env["CONFIG"])
		}
		// Run referencing a step env variable should get the fully expanded
		// value, not the raw template string.
		if expanded.Run != "cat /var/bureau/workspace/test/config.json" {
			t.Errorf("Run = %q, want %q", expanded.Run, "cat /var/bureau/workspace/test/config.json")
		}
	})

	t.Run("publish step", func(t *testing.T) {
		t.Parallel()

		step := schema.PipelineStep{
			Name: "publish-ready",
			Publish: &schema.PipelinePublish{
				EventType: "m.bureau.workspace",
				Room:      "${WORKSPACE_ROOM}",
				StateKey:  "${MACHINE}",
				Content: map[string]any{
					"status":    "ready",
					"workspace": "${WORKSPACE}",
				},
			},
		}
		variables := map[string]string{
			"WORKSPACE_ROOM": "!abc:bureau.local",
			"MACHINE":        "@machine/test:bureau.local",
			"WORKSPACE":      "/var/bureau/workspace/test",
		}

		expanded, err := ExpandStep(step, variables)
		if err != nil {
			t.Fatalf("ExpandStep: %v", err)
		}
		if expanded.Publish.Room != "!abc:bureau.local" {
			t.Errorf("Publish.Room = %q", expanded.Publish.Room)
		}
		if expanded.Publish.StateKey != "@machine/test:bureau.local" {
			t.Errorf("Publish.StateKey = %q", expanded.Publish.StateKey)
		}
		if expanded.Publish.Content["workspace"] != "/var/bureau/workspace/test" {
			t.Errorf("Publish.Content[workspace] = %q", expanded.Publish.Content["workspace"])
		}
	})

	t.Run("publish with nested content map", func(t *testing.T) {
		t.Parallel()

		step := schema.PipelineStep{
			Name: "publish-config",
			Publish: &schema.PipelinePublish{
				EventType: "m.bureau.config",
				Room:      "!room:test",
				Content: map[string]any{
					"nested": map[string]any{
						"path": "${WORKSPACE}/data",
					},
					"count": 42,
				},
			},
		}
		variables := map[string]string{"WORKSPACE": "/var/workspace"}

		expanded, err := ExpandStep(step, variables)
		if err != nil {
			t.Fatalf("ExpandStep: %v", err)
		}
		nested := expanded.Publish.Content["nested"].(map[string]any)
		if nested["path"] != "/var/workspace/data" {
			t.Errorf("nested.path = %q", nested["path"])
		}
		// Non-string values passed through unchanged.
		if expanded.Publish.Content["count"] != 42 {
			t.Errorf("count = %v", expanded.Publish.Content["count"])
		}
	})

	t.Run("unresolved variable in run", func(t *testing.T) {
		t.Parallel()

		step := schema.PipelineStep{
			Name: "broken",
			Run:  "echo ${MISSING}",
		}

		_, err := ExpandStep(step, map[string]string{})
		if err == nil {
			t.Fatal("expected error for unresolved variable")
		}
		if !strings.Contains(err.Error(), "MISSING") {
			t.Errorf("error should mention MISSING: %v", err)
		}
		if !strings.Contains(err.Error(), `step "broken"`) {
			t.Errorf("error should identify the step: %v", err)
		}
	})

	t.Run("does not modify original variables map", func(t *testing.T) {
		t.Parallel()

		variables := map[string]string{"X": "original"}
		step := schema.PipelineStep{
			Name: "test",
			Run:  "echo ${X}",
			Env:  map[string]string{"X": "overridden"},
		}

		_, err := ExpandStep(step, variables)
		if err != nil {
			t.Fatalf("ExpandStep: %v", err)
		}
		if variables["X"] != "original" {
			t.Errorf("original variables map was modified: X = %q", variables["X"])
		}
	})

	t.Run("nil publish passes through", func(t *testing.T) {
		t.Parallel()

		step := schema.PipelineStep{
			Name: "simple",
			Run:  "echo hello",
		}

		expanded, err := ExpandStep(step, map[string]string{})
		if err != nil {
			t.Fatalf("ExpandStep: %v", err)
		}
		if expanded.Publish != nil {
			t.Error("Publish should remain nil")
		}
	})

	t.Run("assert_state step", func(t *testing.T) {
		t.Parallel()

		step := schema.PipelineStep{
			Name: "check-status",
			AssertState: &schema.PipelineAssertState{
				Room:      "${WORKSPACE_ROOM_ID}",
				EventType: "m.bureau.worktree",
				StateKey:  "${WORKTREE_PATH}",
				Field:     "status",
				Equals:    "${EXPECTED_STATUS}",
				Message:   "expected ${EXPECTED_STATUS} but got something else",
			},
		}
		variables := map[string]string{
			"WORKSPACE_ROOM_ID": "!abc:bureau.local",
			"WORKTREE_PATH":     "feature/amdgpu",
			"EXPECTED_STATUS":   "removing",
		}

		expanded, err := ExpandStep(step, variables)
		if err != nil {
			t.Fatalf("ExpandStep: %v", err)
		}
		if expanded.AssertState.Room != "!abc:bureau.local" {
			t.Errorf("AssertState.Room = %q", expanded.AssertState.Room)
		}
		if expanded.AssertState.StateKey != "feature/amdgpu" {
			t.Errorf("AssertState.StateKey = %q", expanded.AssertState.StateKey)
		}
		if expanded.AssertState.Equals != "removing" {
			t.Errorf("AssertState.Equals = %q", expanded.AssertState.Equals)
		}
		if expanded.AssertState.Message != "expected removing but got something else" {
			t.Errorf("AssertState.Message = %q", expanded.AssertState.Message)
		}
	})

	t.Run("assert_state with in list", func(t *testing.T) {
		t.Parallel()

		step := schema.PipelineStep{
			Name: "check-in-set",
			AssertState: &schema.PipelineAssertState{
				Room:      "!room:bureau.local",
				EventType: "m.bureau.workspace",
				Field:     "status",
				In:        []string{"${STATUS_A}", "${STATUS_B}"},
			},
		}
		variables := map[string]string{
			"STATUS_A": "active",
			"STATUS_B": "teardown",
		}

		expanded, err := ExpandStep(step, variables)
		if err != nil {
			t.Fatalf("ExpandStep: %v", err)
		}
		if len(expanded.AssertState.In) != 2 {
			t.Fatalf("AssertState.In length = %d, want 2", len(expanded.AssertState.In))
		}
		if expanded.AssertState.In[0] != "active" {
			t.Errorf("AssertState.In[0] = %q", expanded.AssertState.In[0])
		}
		if expanded.AssertState.In[1] != "teardown" {
			t.Errorf("AssertState.In[1] = %q", expanded.AssertState.In[1])
		}
	})

	t.Run("assert_state with not_in list", func(t *testing.T) {
		t.Parallel()

		step := schema.PipelineStep{
			Name: "check-not-in-set",
			AssertState: &schema.PipelineAssertState{
				Room:      "!room:bureau.local",
				EventType: "m.bureau.workspace",
				Field:     "status",
				NotIn:     []string{"${TERMINAL_A}", "${TERMINAL_B}"},
			},
		}
		variables := map[string]string{
			"TERMINAL_A": "archived",
			"TERMINAL_B": "removed",
		}

		expanded, err := ExpandStep(step, variables)
		if err != nil {
			t.Fatalf("ExpandStep: %v", err)
		}
		if len(expanded.AssertState.NotIn) != 2 {
			t.Fatalf("AssertState.NotIn length = %d, want 2", len(expanded.AssertState.NotIn))
		}
		if expanded.AssertState.NotIn[0] != "archived" {
			t.Errorf("AssertState.NotIn[0] = %q", expanded.AssertState.NotIn[0])
		}
		if expanded.AssertState.NotIn[1] != "removed" {
			t.Errorf("AssertState.NotIn[1] = %q", expanded.AssertState.NotIn[1])
		}
	})

	t.Run("assert_state with not_equals", func(t *testing.T) {
		t.Parallel()

		step := schema.PipelineStep{
			Name: "check-not-removing",
			AssertState: &schema.PipelineAssertState{
				Room:      "!room:bureau.local",
				EventType: "m.bureau.worktree",
				StateKey:  "${WORKTREE_PATH}",
				Field:     "status",
				NotEquals: "${FORBIDDEN_STATUS}",
			},
		}
		variables := map[string]string{
			"WORKTREE_PATH":    "feature/test",
			"FORBIDDEN_STATUS": "removing",
		}

		expanded, err := ExpandStep(step, variables)
		if err != nil {
			t.Fatalf("ExpandStep: %v", err)
		}
		if expanded.AssertState.NotEquals != "removing" {
			t.Errorf("AssertState.NotEquals = %q", expanded.AssertState.NotEquals)
		}
	})

	t.Run("nil assert_state passes through", func(t *testing.T) {
		t.Parallel()

		step := schema.PipelineStep{
			Name: "simple",
			Run:  "echo hello",
		}

		expanded, err := ExpandStep(step, map[string]string{})
		if err != nil {
			t.Fatalf("ExpandStep: %v", err)
		}
		if expanded.AssertState != nil {
			t.Error("AssertState should remain nil")
		}
	})

	t.Run("unresolved variable in assert_state", func(t *testing.T) {
		t.Parallel()

		step := schema.PipelineStep{
			Name: "broken-assert",
			AssertState: &schema.PipelineAssertState{
				Room:      "${MISSING_ROOM}",
				EventType: "m.bureau.workspace",
				Field:     "status",
				Equals:    "active",
			},
		}

		_, err := ExpandStep(step, map[string]string{})
		if err == nil {
			t.Fatal("expected error for unresolved variable")
		}
		if !strings.Contains(err.Error(), "MISSING_ROOM") {
			t.Errorf("error should mention MISSING_ROOM: %v", err)
		}
		if !strings.Contains(err.Error(), "assert_state.room") {
			t.Errorf("error should identify the field: %v", err)
		}
	})

	t.Run("assert_state does not modify original", func(t *testing.T) {
		t.Parallel()

		original := &schema.PipelineAssertState{
			Room:      "${ROOM}",
			EventType: "m.bureau.worktree",
			Field:     "status",
			Equals:    "active",
			In:        []string{"${A}", "${B}"},
		}
		step := schema.PipelineStep{
			Name:        "check",
			AssertState: original,
		}
		variables := map[string]string{
			"ROOM": "!expanded:bureau.local",
			"A":    "one",
			"B":    "two",
		}

		expanded, err := ExpandStep(step, variables)
		if err != nil {
			t.Fatalf("ExpandStep: %v", err)
		}
		// Expanded step should have new values.
		if expanded.AssertState.Room != "!expanded:bureau.local" {
			t.Errorf("expanded Room = %q", expanded.AssertState.Room)
		}
		// Original should be untouched.
		if original.Room != "${ROOM}" {
			t.Errorf("original Room was modified to %q", original.Room)
		}
		if original.In[0] != "${A}" {
			t.Errorf("original In[0] was modified to %q", original.In[0])
		}
	})

	t.Run("output paths are expanded", func(t *testing.T) {
		t.Parallel()

		step := schema.PipelineStep{
			Name: "build",
			Run:  "make build",
			Outputs: map[string]json.RawMessage{
				"binary": json.RawMessage(`"/tmp/outputs/${PROJECT}_bin"`),
			},
		}
		variables := map[string]string{"PROJECT": "myapp"}

		expanded, err := ExpandStep(step, variables)
		if err != nil {
			t.Fatalf("ExpandStep: %v", err)
		}

		// Parse the expanded output to verify the path was expanded.
		parsed, err := schema.ParseStepOutputs(expanded.Outputs)
		if err != nil {
			t.Fatalf("ParseStepOutputs: %v", err)
		}
		if parsed["binary"].Path != "/tmp/outputs/myapp_bin" {
			t.Errorf("output path = %q, want %q", parsed["binary"].Path, "/tmp/outputs/myapp_bin")
		}
	})
}
