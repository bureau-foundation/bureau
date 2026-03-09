// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/cmd/bureau/environment"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestBatchFlakePublishTemplates verifies that PublishFlakeTemplates correctly
// evaluates a flake's bureauTemplates.<system> (plural) output and publishes
// each template as an m.bureau.template state event with correct content and
// origin metadata.
//
// This is the production code path used by "bureau environment compose" to
// atomically publish templates declared by environment flakes.
//
// Skipped when nix is not installed.
func TestBatchFlakePublishTemplates(t *testing.T) {
	t.Parallel()
	nixTestEnv(t)

	ns := setupTestNamespace(t)
	context := t.Context()
	logger := testLogger(t)

	// Create a flake that exports bureauTemplates (plural) with two
	// templates. Each template has distinct content to verify they're
	// published independently.
	flakeDirectory := createBatchTestFlakeRepo(t, batchFlakeOptions{
		templates: map[string]batchTemplateEntry{
			"agent-alpha": {
				description: "Batch test template alpha",
				command:     []string{"/bin/alpha", "--mode", "test"},
				envVars:     map[string]string{"ROLE": "alpha"},
			},
			"agent-beta": {
				description: "Batch test template beta",
				command:     []string{"/bin/beta"},
				envVars:     map[string]string{"ROLE": "beta", "EXTRA": "value"},
			},
		},
	})
	flakeRef := "path:" + flakeDirectory

	system := nixSystem(t)

	count, err := environment.PublishFlakeTemplates(context, ns.Admin, flakeRef, system, ns.Namespace, logger)
	if err != nil {
		t.Fatalf("PublishFlakeTemplates: %v", err)
	}
	if count != 2 {
		t.Fatalf("published %d templates, want 2", count)
	}

	// Read back each template from Matrix and verify content + origin.
	verifyPublishedTemplate(t, ns.Admin, ns, "agent-alpha",
		"Batch test template alpha",
		[]string{"/bin/alpha", "--mode", "test"},
		map[string]string{"ROLE": "alpha"},
		flakeRef,
	)
	verifyPublishedTemplate(t, ns.Admin, ns, "agent-beta",
		"Batch test template beta",
		[]string{"/bin/beta"},
		map[string]string{"ROLE": "beta", "EXTRA": "value"},
		flakeRef,
	)
}

// TestBatchFlakePublishPipelines verifies that PublishFlakePipelines correctly
// evaluates a flake's bureauPipelines (plural) output and publishes each
// pipeline as an m.bureau.pipeline state event.
//
// Skipped when nix is not installed.
func TestBatchFlakePublishPipelines(t *testing.T) {
	t.Parallel()
	nixTestEnv(t)

	ns := setupTestNamespace(t)
	context := t.Context()
	logger := testLogger(t)

	flakeDirectory := createBatchTestFlakeRepo(t, batchFlakeOptions{
		pipelines: map[string]batchPipelineEntry{
			"setup-env": {
				steps: []batchPipelineStep{
					{name: "build", run: "echo building"},
					{name: "push", run: "echo pushing"},
				},
			},
		},
	})
	flakeRef := "path:" + flakeDirectory

	count, err := environment.PublishFlakePipelines(context, ns.Admin, flakeRef, ns.Namespace, logger)
	if err != nil {
		t.Fatalf("PublishFlakePipelines: %v", err)
	}
	if count != 1 {
		t.Fatalf("published %d pipelines, want 1", count)
	}

	// Read back and verify.
	stored, err := messaging.GetState[pipeline.PipelineContent](
		context, ns.Admin, ns.PipelineRoomID,
		schema.EventTypePipeline, "setup-env",
	)
	if err != nil {
		t.Fatalf("read back pipeline: %v", err)
	}
	if length := len(stored.Steps); length != 2 {
		t.Fatalf("pipeline has %d steps, want 2", length)
	}
	if stored.Steps[0].Name != "build" {
		t.Errorf("step[0].name = %q, want %q", stored.Steps[0].Name, "build")
	}
	if stored.Steps[1].Name != "push" {
		t.Errorf("step[1].name = %q, want %q", stored.Steps[1].Name, "push")
	}
	if stored.Origin == nil {
		t.Fatal("stored pipeline should have origin metadata")
	}
	if stored.Origin.FlakeRef != flakeRef {
		t.Errorf("origin.flake_ref = %q, want %q", stored.Origin.FlakeRef, flakeRef)
	}
	if stored.Origin.ContentHash == "" {
		t.Error("origin.content_hash is empty")
	}
}

// TestBatchFlakeMissingAttribute verifies that PublishFlakeTemplates returns
// 0 (not an error) when the flake does not export the requested attribute.
// This is the expected behavior for environment flakes that declare only
// templates or only pipelines.
//
// Skipped when nix is not installed.
func TestBatchFlakeMissingAttribute(t *testing.T) {
	t.Parallel()
	nixTestEnv(t)

	ns := setupTestNamespace(t)
	context := t.Context()
	logger := testLogger(t)

	// Create a flake that only exports bureauTemplates, not bureauPipelines.
	flakeDirectory := createBatchTestFlakeRepo(t, batchFlakeOptions{
		templates: map[string]batchTemplateEntry{
			"only-template": {
				description: "Template without pipelines",
				command:     []string{"/bin/test"},
			},
		},
	})
	flakeRef := "path:" + flakeDirectory

	// Publishing pipelines from a flake that doesn't export them
	// should return 0, nil — not an error.
	count, err := environment.PublishFlakePipelines(context, ns.Admin, flakeRef, ns.Namespace, logger)
	if err != nil {
		t.Fatalf("PublishFlakePipelines should not error on missing attribute: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 pipelines for missing attribute, got %d", count)
	}
}

// --- Test helpers ---

// nixSystem returns the Nix system triple for the current architecture.
func nixSystem(t *testing.T) string {
	t.Helper()
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64-linux"
	case "arm64":
		return "aarch64-linux"
	default:
		t.Fatalf("unsupported architecture: %s", runtime.GOARCH)
		return ""
	}
}

// verifyPublishedTemplate reads a template back from Matrix and verifies
// its content and origin metadata match expectations.
func verifyPublishedTemplate(
	t *testing.T,
	admin *messaging.DirectSession,
	ns *testNamespaceResult,
	name string,
	wantDescription string,
	wantCommand []string,
	wantEnvVars map[string]string,
	wantFlakeRef string,
) {
	t.Helper()

	context := t.Context()

	stored, err := messaging.GetState[schema.TemplateContent](
		context, admin, ns.TemplateRoomID,
		schema.EventTypeTemplate, name,
	)
	if err != nil {
		t.Fatalf("read back template %q: %v", name, err)
	}

	if stored.Description != wantDescription {
		t.Errorf("template %q description = %q, want %q", name, stored.Description, wantDescription)
	}

	if len(stored.Command) != len(wantCommand) {
		t.Errorf("template %q command = %v, want %v", name, stored.Command, wantCommand)
	} else {
		for index, argument := range wantCommand {
			if stored.Command[index] != argument {
				t.Errorf("template %q command[%d] = %q, want %q", name, index, stored.Command[index], argument)
			}
		}
	}

	for key, wantValue := range wantEnvVars {
		if stored.EnvironmentVariables[key] != wantValue {
			t.Errorf("template %q env[%s] = %q, want %q", name, key, stored.EnvironmentVariables[key], wantValue)
		}
	}

	if stored.Origin == nil {
		t.Fatalf("template %q: stored origin is nil", name)
	}
	if stored.Origin.FlakeRef != wantFlakeRef {
		t.Errorf("template %q origin.flake_ref = %q, want %q", name, stored.Origin.FlakeRef, wantFlakeRef)
	}
	if stored.Origin.ContentHash == "" {
		t.Errorf("template %q origin.content_hash is empty", name)
	}
	if !strings.HasPrefix(stored.Origin.ContentHash, "sha256:") {
		t.Errorf("template %q origin.content_hash = %q, want sha256: prefix", name, stored.Origin.ContentHash)
	}
}

// --- Batch test flake creation ---

type batchFlakeOptions struct {
	templates map[string]batchTemplateEntry
	pipelines map[string]batchPipelineEntry
}

type batchTemplateEntry struct {
	description string
	command     []string
	envVars     map[string]string
}

type batchPipelineStep struct {
	name string
	run  string
}

type batchPipelineEntry struct {
	steps []batchPipelineStep
}

// createBatchTestFlakeRepo creates a git-initialized flake repo with
// bureauTemplates (plural) and/or bureauPipelines (plural) outputs.
func createBatchTestFlakeRepo(t *testing.T, options batchFlakeOptions) string {
	t.Helper()

	directory := filepath.Join(t.TempDir(), "batch-flake")
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatalf("create flake directory: %v", err)
	}

	writeBatchFlakeNix(t, directory, options)

	context := t.Context()
	for _, step := range []struct {
		args        []string
		description string
	}{
		{[]string{"init", "-b", "main", directory}, "git init"},
		{[]string{"-C", directory, "config", "user.name", "Test"}, "git config user.name"},
		{[]string{"-C", directory, "config", "user.email", "test@test"}, "git config user.email"},
		{[]string{"-C", directory, "add", "flake.nix"}, "git add"},
		{[]string{"-C", directory, "commit", "-m", "Initial batch flake"}, "git commit"},
	} {
		cmd := exec.CommandContext(context, "git", step.args...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v", step.description, err)
		}
	}

	return directory
}

// writeBatchFlakeNix generates a flake.nix with bureauTemplates (plural)
// and/or bureauPipelines (plural) outputs for both x86_64-linux and
// aarch64-linux.
func writeBatchFlakeNix(t *testing.T, directory string, options batchFlakeOptions) {
	t.Helper()

	var builder strings.Builder
	builder.WriteString("{\n  outputs = { self }: {\n")

	// Write bureauTemplates (plural) for both architectures.
	if len(options.templates) > 0 {
		templateNames := make([]string, 0, len(options.templates))
		for name := range options.templates {
			templateNames = append(templateNames, name)
		}
		sort.Strings(templateNames)

		for _, system := range []string{"x86_64-linux", "aarch64-linux"} {
			fmt.Fprintf(&builder, "    bureauTemplates.%s = {\n", system)
			for _, name := range templateNames {
				entry := options.templates[name]
				fmt.Fprintf(&builder, "      %s = {\n", nixIdentifier(name))
				fmt.Fprintf(&builder, "        description = %q;\n", entry.description)

				// Command as nix list.
				builder.WriteString("        command = [")
				for _, argument := range entry.command {
					fmt.Fprintf(&builder, " %q", argument)
				}
				builder.WriteString(" ];\n")

				if len(entry.envVars) > 0 {
					envKeys := make([]string, 0, len(entry.envVars))
					for key := range entry.envVars {
						envKeys = append(envKeys, key)
					}
					sort.Strings(envKeys)

					builder.WriteString("        environment_variables = {\n")
					for _, key := range envKeys {
						fmt.Fprintf(&builder, "          %s = %q;\n", key, entry.envVars[key])
					}
					builder.WriteString("        };\n")
				}

				builder.WriteString("      };\n")
			}
			builder.WriteString("    };\n")
		}
	}

	// Write bureauPipelines (plural, system-agnostic).
	if len(options.pipelines) > 0 {
		pipelineNames := make([]string, 0, len(options.pipelines))
		for name := range options.pipelines {
			pipelineNames = append(pipelineNames, name)
		}
		sort.Strings(pipelineNames)

		builder.WriteString("    bureauPipelines = {\n")
		for _, name := range pipelineNames {
			entry := options.pipelines[name]
			fmt.Fprintf(&builder, "      %s = {\n", nixIdentifier(name))
			builder.WriteString("        steps = [\n")
			for _, step := range entry.steps {
				fmt.Fprintf(&builder, "          { name = %q; run = %q; }\n", step.name, step.run)
			}
			builder.WriteString("        ];\n")
			builder.WriteString("      };\n")
		}
		builder.WriteString("    };\n")
	}

	builder.WriteString("  };\n}\n")

	if err := os.WriteFile(filepath.Join(directory, "flake.nix"), []byte(builder.String()), 0644); err != nil {
		t.Fatalf("write flake.nix: %v", err)
	}
}

// nixIdentifier converts a kebab-case name to a nix-safe identifier by
// quoting it if it contains hyphens. Nix attribute names with hyphens must
// be quoted strings.
func nixIdentifier(name string) string {
	if strings.ContainsAny(name, "-. ") {
		return fmt.Sprintf("%q", name)
	}
	return name
}
