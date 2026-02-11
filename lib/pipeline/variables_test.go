// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
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
				EventType: "m.bureau.workspace.ready",
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
}
