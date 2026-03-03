// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// TestTemplatePublishFile verifies the bureau template publish --file path:
// writes a JSONC template to disk, publishes it via the CLI, and reads it
// back with bureau template show --raw to confirm a faithful round-trip
// through Matrix state events. File-sourced templates carry no origin
// metadata.
func TestTemplatePublishFile(t *testing.T) {
	t.Parallel()

	op := setupOperatorEnv(t)

	// Write a JSONC template file — comments and trailing commas exercise
	// the JSONC parser (jsonc.ToJSON strips these before json.Unmarshal).
	templateContent := `{
  // Published from a local file — JSONC comment that should be stripped
  "description": "Integration test: file-published template",
  "command": ["/bin/test-agent", "--mode", "file-test"],
  "environment_variables": {
    "TEST_VAR": "hello",
  },
}`
	templateFile := filepath.Join(t.TempDir(), "test-template.jsonc")
	if err := os.WriteFile(templateFile, []byte(templateContent), 0644); err != nil {
		t.Fatalf("write template file: %v", err)
	}

	templateRef := op.Namespace.TemplateRoomAliasLocalpart() + ":publish-file-test"

	// Publish and capture the structured result.
	publishOutput := op.run(t, "template", "publish",
		"--file", templateFile,
		templateRef,
		"--json")

	var publishResult struct {
		Ref       string                 `json:"ref"`
		Source    string                 `json:"source"`
		SourceRef string                 `json:"source_ref"`
		EventID   string                 `json:"event_id"`
		Origin    *schema.TemplateOrigin `json:"origin"`
		DryRun    bool                   `json:"dry_run"`
	}
	if err := json.Unmarshal([]byte(publishOutput), &publishResult); err != nil {
		t.Fatalf("parse publish result: %v\noutput:\n%s", err, publishOutput)
	}

	if publishResult.Source != "file" {
		t.Errorf("source = %q, want %q", publishResult.Source, "file")
	}
	if publishResult.SourceRef != templateFile {
		t.Errorf("source_ref = %q, want %q", publishResult.SourceRef, templateFile)
	}
	if publishResult.EventID == "" {
		t.Error("event_id is empty — publish did not return a Matrix event ID")
	}
	if publishResult.DryRun {
		t.Error("dry_run should be false for actual publish")
	}
	if publishResult.Origin != nil {
		t.Error("origin should be nil for file-sourced templates")
	}

	// Read the template back from Matrix via show --raw. The --raw flag
	// returns the state event content directly (no inheritance resolution),
	// which preserves the Origin field as stored.
	showOutput := op.run(t, "template", "show", "--raw", templateRef, "--json")

	var shown schema.TemplateContent
	if err := json.Unmarshal([]byte(showOutput), &shown); err != nil {
		t.Fatalf("parse show output: %v\noutput:\n%s", err, showOutput)
	}

	if shown.Description != "Integration test: file-published template" {
		t.Errorf("description = %q, want %q", shown.Description, "Integration test: file-published template")
	}
	wantCommand := []string{"/bin/test-agent", "--mode", "file-test"}
	if len(shown.Command) != len(wantCommand) {
		t.Errorf("command = %v, want %v", shown.Command, wantCommand)
	} else {
		for index, arg := range wantCommand {
			if shown.Command[index] != arg {
				t.Errorf("command[%d] = %q, want %q", index, shown.Command[index], arg)
			}
		}
	}
	if shown.EnvironmentVariables == nil || shown.EnvironmentVariables["TEST_VAR"] != "hello" {
		t.Errorf("environment_variables = %v, want map with TEST_VAR=hello", shown.EnvironmentVariables)
	}
	if shown.Origin != nil {
		t.Error("stored origin should be nil — file sources do not set origin")
	}
}

// TestTemplatePublishFlake verifies the bureau template publish --flake path:
// evaluates a Nix flake's bureauTemplate output, publishes the result with
// origin tracking, and reads it back. The origin includes the flake reference,
// resolved git revision, and a content hash for change detection.
//
// Skipped when nix is not installed.
func TestTemplatePublishFlake(t *testing.T) {
	t.Parallel()

	// Nix lives at a well-known path outside the default bazel test PATH.
	// Inject its directory into PATH for the bureau subprocess, which
	// uses exec.LookPath("nix") internally.
	nixBinary := "/nix/var/nix/profiles/default/bin/nix"
	if _, err := os.Stat(nixBinary); err != nil {
		t.Skip("nix not installed: flake publish requires the Nix package manager")
	}
	nixBinDirectory := filepath.Dir(nixBinary)

	// Verify we're on a supported architecture. The publish command
	// auto-detects the Nix system from runtime.GOARCH.
	switch runtime.GOARCH {
	case "amd64", "arm64":
		// supported
	default:
		t.Skipf("unsupported architecture %s for flake test", runtime.GOARCH)
	}

	op := setupOperatorEnv(t)

	// Extend the operator env with nix on PATH so the bureau subprocess
	// can find it via exec.LookPath.
	flakeEnv := append(append([]string(nil), op.Env...),
		"PATH="+nixBinDirectory+":"+os.Getenv("PATH"))

	// Create a minimal git repository with a flake.nix that exports
	// bureauTemplate.<system>. The attribute set uses snake_case field
	// names matching the TemplateContent JSON wire format, so nix eval
	// --json produces directly parseable TemplateContent JSON.
	flakeDirectory := filepath.Join(t.TempDir(), "test-flake")
	if err := os.MkdirAll(flakeDirectory, 0755); err != nil {
		t.Fatalf("create flake directory: %v", err)
	}

	flakeNix := `{
  outputs = { self }: {
    bureauTemplate.x86_64-linux = {
      description = "Integration test: flake-published template";
      command = [ "/bin/test-agent" "--mode" "flake-test" ];
      environment_variables = {
        FLAKE_VAR = "world";
      };
    };
    bureauTemplate.aarch64-linux = {
      description = "Integration test: flake-published template";
      command = [ "/bin/test-agent" "--mode" "flake-test" ];
      environment_variables = {
        FLAKE_VAR = "world";
      };
    };
  };
}
`
	if err := os.WriteFile(filepath.Join(flakeDirectory, "flake.nix"), []byte(flakeNix), 0644); err != nil {
		t.Fatalf("write flake.nix: %v", err)
	}

	// Initialize a git repo so nix can evaluate the flake. Nix requires
	// files to be tracked by git for flake evaluation.
	context := t.Context()
	for _, step := range []struct {
		args        []string
		description string
	}{
		{[]string{"init", "-b", "main", flakeDirectory}, "git init"},
		{[]string{"-C", flakeDirectory, "config", "user.name", "Test"}, "git config user.name"},
		{[]string{"-C", flakeDirectory, "config", "user.email", "test@test"}, "git config user.email"},
		{[]string{"-C", flakeDirectory, "add", "flake.nix"}, "git add"},
		{[]string{"-C", flakeDirectory, "commit", "-m", "Initial flake"}, "git commit"},
	} {
		cmd := exec.CommandContext(context, "git", step.args...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v", step.description, err)
		}
	}

	templateRef := op.Namespace.TemplateRoomAliasLocalpart() + ":publish-flake-test"
	flakeRef := "path:" + flakeDirectory

	// Publish from the flake. Uses flakeEnv (not op.Env) so the bureau
	// subprocess can find nix via exec.LookPath.
	publishOutput := runBureauWithEnvOrFail(t, flakeEnv,
		"template", "publish",
		"--flake", flakeRef,
		templateRef,
		"--json")

	var publishResult struct {
		Ref       string                 `json:"ref"`
		Source    string                 `json:"source"`
		SourceRef string                 `json:"source_ref"`
		EventID   string                 `json:"event_id"`
		Origin    *schema.TemplateOrigin `json:"origin"`
		DryRun    bool                   `json:"dry_run"`
	}
	if err := json.Unmarshal([]byte(publishOutput), &publishResult); err != nil {
		t.Fatalf("parse publish result: %v\noutput:\n%s", err, publishOutput)
	}

	if publishResult.Source != "flake" {
		t.Errorf("source = %q, want %q", publishResult.Source, "flake")
	}
	if publishResult.SourceRef != flakeRef {
		t.Errorf("source_ref = %q, want %q", publishResult.SourceRef, flakeRef)
	}
	if publishResult.EventID == "" {
		t.Error("event_id is empty — publish did not return a Matrix event ID")
	}
	if publishResult.DryRun {
		t.Error("dry_run should be false for actual publish")
	}

	// Flake-sourced templates must have origin metadata for update tracking.
	if publishResult.Origin == nil {
		t.Fatal("origin should be set for flake-sourced templates")
	}
	if publishResult.Origin.FlakeRef != flakeRef {
		t.Errorf("origin.flake_ref = %q, want %q", publishResult.Origin.FlakeRef, flakeRef)
	}
	if publishResult.Origin.ContentHash == "" {
		t.Error("origin.content_hash is empty")
	}
	if !strings.HasPrefix(publishResult.Origin.ContentHash, "sha256:") {
		t.Errorf("origin.content_hash = %q, want sha256: prefix", publishResult.Origin.ContentHash)
	}

	// Read the template back from Matrix via show --raw. The --raw flag
	// returns the state event content without inheritance resolution,
	// preserving the Origin field as stored.
	showOutput := op.run(t, "template", "show", "--raw", templateRef, "--json")

	var shown schema.TemplateContent
	if err := json.Unmarshal([]byte(showOutput), &shown); err != nil {
		t.Fatalf("parse show output: %v\noutput:\n%s", err, showOutput)
	}

	if shown.Description != "Integration test: flake-published template" {
		t.Errorf("description = %q, want %q", shown.Description, "Integration test: flake-published template")
	}
	wantCommand := []string{"/bin/test-agent", "--mode", "flake-test"}
	if len(shown.Command) != len(wantCommand) {
		t.Errorf("command = %v, want %v", shown.Command, wantCommand)
	} else {
		for index, arg := range wantCommand {
			if shown.Command[index] != arg {
				t.Errorf("command[%d] = %q, want %q", index, shown.Command[index], arg)
			}
		}
	}
	if shown.EnvironmentVariables == nil || shown.EnvironmentVariables["FLAKE_VAR"] != "world" {
		t.Errorf("environment_variables = %v, want map with FLAKE_VAR=world", shown.EnvironmentVariables)
	}

	// Verify origin metadata was stored with the template.
	if shown.Origin == nil {
		t.Fatal("stored origin should be set for flake-sourced templates")
	}
	if shown.Origin.FlakeRef != flakeRef {
		t.Errorf("stored origin.flake_ref = %q, want %q", shown.Origin.FlakeRef, flakeRef)
	}
	if shown.Origin.ContentHash == "" {
		t.Error("stored origin.content_hash is empty")
	}

	// The resolved revision should be a 40-character hex git commit hash
	// from the test repository. Path flakes with committed files report
	// the HEAD revision.
	if shown.Origin.ResolvedRev != "" && len(shown.Origin.ResolvedRev) != 40 {
		t.Errorf("stored origin.resolved_rev = %q, expected 40-char hex commit hash or empty", shown.Origin.ResolvedRev)
	}

	// Content hash must match between the publish result and stored template.
	if shown.Origin.ContentHash != publishResult.Origin.ContentHash {
		t.Errorf("content hash mismatch: publish=%q stored=%q",
			publishResult.Origin.ContentHash, shown.Origin.ContentHash)
	}
}
