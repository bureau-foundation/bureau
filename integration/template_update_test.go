// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// nixTestEnv checks that nix is installed and the current architecture is
// supported, skipping the test otherwise. Returns the nix binary directory
// for PATH injection.
func nixTestEnv(t *testing.T) string {
	t.Helper()

	nixBinary := "/nix/var/nix/profiles/default/bin/nix"
	if _, err := os.Stat(nixBinary); err != nil {
		t.Skip("nix not installed: flake tests require the Nix package manager")
	}

	switch runtime.GOARCH {
	case "amd64", "arm64":
		// supported
	default:
		t.Skipf("unsupported architecture %s for flake test", runtime.GOARCH)
	}

	return filepath.Dir(nixBinary)
}

// createTestFlakeRepo creates a minimal git repository with a flake.nix that
// exports bureauTemplate for the current system. Returns the directory path.
// The caller can modify flake.nix and call updateTestFlakeRepo to commit
// changes.
func createTestFlakeRepo(t *testing.T, description string, envVars map[string]string) string {
	t.Helper()

	directory := filepath.Join(t.TempDir(), "test-flake")
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatalf("create flake directory: %v", err)
	}

	writeTestFlakeNix(t, directory, description, envVars)

	context := t.Context()
	for _, step := range []struct {
		args        []string
		description string
	}{
		{[]string{"init", "-b", "main", directory}, "git init"},
		{[]string{"-C", directory, "config", "user.name", "Test"}, "git config user.name"},
		{[]string{"-C", directory, "config", "user.email", "test@test"}, "git config user.email"},
		{[]string{"-C", directory, "add", "flake.nix"}, "git add"},
		{[]string{"-C", directory, "commit", "-m", "Initial flake"}, "git commit"},
	} {
		cmd := exec.CommandContext(context, "git", step.args...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v", step.description, err)
		}
	}

	return directory
}

// writeTestFlakeNix writes a flake.nix to the given directory with the
// specified description and environment variables.
func writeTestFlakeNix(t *testing.T, directory string, description string, envVars map[string]string) {
	t.Helper()

	var envLines strings.Builder
	for key, value := range envVars {
		fmt.Fprintf(&envLines, "        %s = \"%s\";\n", key, value)
	}

	flakeNix := fmt.Sprintf(`{
  outputs = { self }: {
    bureauTemplate.x86_64-linux = {
      description = "%s";
      command = [ "/bin/test-agent" "--mode" "flake-test" ];
      environment_variables = {
%s      };
    };
    bureauTemplate.aarch64-linux = {
      description = "%s";
      command = [ "/bin/test-agent" "--mode" "flake-test" ];
      environment_variables = {
%s      };
    };
  };
}
`, description, envLines.String(), description, envLines.String())

	if err := os.WriteFile(filepath.Join(directory, "flake.nix"), []byte(flakeNix), 0644); err != nil {
		t.Fatalf("write flake.nix: %v", err)
	}
}

// commitTestFlakeRepo stages and commits all changes in the test flake repo.
func commitTestFlakeRepo(t *testing.T, directory string, message string) {
	t.Helper()

	context := t.Context()
	for _, step := range []struct {
		args        []string
		description string
	}{
		{[]string{"-C", directory, "add", "flake.nix"}, "git add"},
		{[]string{"-C", directory, "commit", "-m", message}, "git commit"},
	} {
		cmd := exec.CommandContext(context, "git", step.args...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v", step.description, err)
		}
	}
}

// templateUpdateResult mirrors the CLI's JSON output for template update.
// Defined here so integration tests can unmarshal the structured output
// without importing the CLI package.
type templateUpdateResult struct {
	Name        string   `json:"name"`
	Status      string   `json:"status"`
	Source      string   `json:"source,omitempty"`
	OldRevision string   `json:"old_revision,omitempty"`
	NewRevision string   `json:"new_revision,omitempty"`
	OldHash     string   `json:"old_hash,omitempty"`
	NewHash     string   `json:"new_hash,omitempty"`
	Change      string   `json:"change,omitempty"`
	Fields      []string `json:"fields,omitempty"`
	EventID     string   `json:"event_id,omitempty"`
	Error       string   `json:"error,omitempty"`
	DryRun      bool     `json:"dry_run"`
}

// TestTemplateUpdateFlake verifies the full flake update flow: publish a
// template from a Nix flake, modify the flake source, then run
// `bureau template update --yes` to re-evaluate and publish the new version.
// Asserts that the update detects the content change, classifies it, and
// publishes a new Matrix state event.
func TestTemplateUpdateFlake(t *testing.T) {
	t.Parallel()

	nixBinDirectory := nixTestEnv(t)
	op := setupOperatorEnv(t)

	flakeEnv := append(append([]string(nil), op.Env...),
		"PATH="+nixBinDirectory+":"+os.Getenv("PATH"))

	// Create a flake repo with the initial template content.
	flakeDirectory := createTestFlakeRepo(t,
		"update-flake-test: initial",
		map[string]string{"VERSION": "1"})
	flakeRef := "path:" + flakeDirectory

	templateRef := op.Namespace.TemplateRoomAliasLocalpart() + ":update-flake-test"

	// Publish the initial template from the flake.
	runBureauWithEnvOrFail(t, flakeEnv,
		"template", "publish",
		"--flake", flakeRef,
		templateRef,
		"--json")

	// Verify the initial template has origin metadata.
	showOutput := runBureauWithEnvOrFail(t, op.Env,
		"template", "show", "--raw", templateRef, "--json")

	var initialContent schema.TemplateContent
	if err := json.Unmarshal([]byte(showOutput), &initialContent); err != nil {
		t.Fatalf("parse initial show output: %v\noutput:\n%s", err, showOutput)
	}
	if initialContent.Origin == nil {
		t.Fatal("initial template should have origin metadata")
	}
	initialHash := initialContent.Origin.ContentHash

	// Modify the flake: change the description and add an environment variable.
	// This produces a structural change (environment_variables is structural).
	writeTestFlakeNix(t, flakeDirectory,
		"update-flake-test: updated",
		map[string]string{"VERSION": "2", "NEW_VAR": "added"})
	commitTestFlakeRepo(t, flakeDirectory, "Update template content")

	// Run the update command with --yes to publish without prompting.
	updateOutput := runBureauWithEnvOrFail(t, flakeEnv,
		"template", "update",
		"--yes",
		templateRef,
		"--json")

	var results []templateUpdateResult
	if err := json.Unmarshal([]byte(updateOutput), &results); err != nil {
		t.Fatalf("parse update output: %v\noutput:\n%s", err, updateOutput)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %s", len(results), updateOutput)
	}

	result := results[0]

	if result.Name != "update-flake-test" {
		t.Errorf("name = %q, want %q", result.Name, "update-flake-test")
	}
	if result.Status != "updated" {
		t.Errorf("status = %q, want %q", result.Status, "updated")
	}
	if result.Source != "flake" {
		t.Errorf("source = %q, want %q", result.Source, "flake")
	}
	if result.OldHash == "" {
		t.Error("old_hash should not be empty")
	}
	if result.NewHash == "" {
		t.Error("new_hash should not be empty")
	}
	if result.OldHash == result.NewHash {
		t.Error("old_hash and new_hash should differ after modification")
	}
	// Path flakes may or may not report a resolved git revision depending
	// on the nix version. When both are present, they must differ.
	if result.OldRevision != "" && result.NewRevision != "" {
		if result.OldRevision == result.NewRevision {
			t.Errorf("old_revision %q == new_revision %q, should differ after git commit",
				result.OldRevision, result.NewRevision)
		}
	}
	if result.EventID == "" {
		t.Error("event_id should be set after successful publish")
	}
	if result.DryRun {
		t.Error("dry_run should be false with --yes")
	}
	if result.Change == "" {
		t.Error("change classification should be set")
	}
	if len(result.Fields) == 0 {
		t.Error("fields should list changed field names")
	}

	// Read the template back from Matrix and verify the new content.
	showOutput = runBureauWithEnvOrFail(t, op.Env,
		"template", "show", "--raw", templateRef, "--json")

	var updatedContent schema.TemplateContent
	if err := json.Unmarshal([]byte(showOutput), &updatedContent); err != nil {
		t.Fatalf("parse updated show output: %v\noutput:\n%s", err, showOutput)
	}

	if updatedContent.Description != "update-flake-test: updated" {
		t.Errorf("description = %q, want %q", updatedContent.Description, "update-flake-test: updated")
	}
	if updatedContent.EnvironmentVariables == nil {
		t.Fatal("environment_variables should not be nil after update")
	}
	if updatedContent.EnvironmentVariables["VERSION"] != "2" {
		t.Errorf("VERSION = %q, want %q", updatedContent.EnvironmentVariables["VERSION"], "2")
	}
	if updatedContent.EnvironmentVariables["NEW_VAR"] != "added" {
		t.Errorf("NEW_VAR = %q, want %q", updatedContent.EnvironmentVariables["NEW_VAR"], "added")
	}

	// Origin should be updated with the new revision and content hash.
	if updatedContent.Origin == nil {
		t.Fatal("updated template should have origin metadata")
	}
	if updatedContent.Origin.ContentHash == initialHash {
		t.Error("origin content_hash should change after update")
	}
	if !strings.HasPrefix(updatedContent.Origin.ContentHash, "sha256:") {
		t.Errorf("origin content_hash = %q, want sha256: prefix", updatedContent.Origin.ContentHash)
	}
}

// TestTemplateUpdateAlreadyCurrent verifies that updating a template whose
// flake source has not changed reports status "current" with no publish.
func TestTemplateUpdateAlreadyCurrent(t *testing.T) {
	t.Parallel()

	nixBinDirectory := nixTestEnv(t)
	op := setupOperatorEnv(t)

	flakeEnv := append(append([]string(nil), op.Env...),
		"PATH="+nixBinDirectory+":"+os.Getenv("PATH"))

	flakeDirectory := createTestFlakeRepo(t,
		"update-current-test",
		map[string]string{"STABLE": "true"})
	flakeRef := "path:" + flakeDirectory

	templateRef := op.Namespace.TemplateRoomAliasLocalpart() + ":update-current-test"

	// Publish the template.
	runBureauWithEnvOrFail(t, flakeEnv,
		"template", "publish",
		"--flake", flakeRef,
		templateRef,
		"--json")

	// Immediately check for updates — the source is unchanged.
	updateOutput := runBureauWithEnvOrFail(t, flakeEnv,
		"template", "update",
		"--yes",
		templateRef,
		"--json")

	var results []templateUpdateResult
	if err := json.Unmarshal([]byte(updateOutput), &results); err != nil {
		t.Fatalf("parse update output: %v\noutput:\n%s", err, updateOutput)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %s", len(results), updateOutput)
	}

	result := results[0]

	if result.Name != "update-current-test" {
		t.Errorf("name = %q, want %q", result.Name, "update-current-test")
	}
	if result.Status != "current" {
		t.Errorf("status = %q, want %q", result.Status, "current")
	}
	if result.EventID != "" {
		t.Errorf("event_id = %q, should be empty for current template", result.EventID)
	}
	if result.OldHash == "" {
		t.Error("old_hash should be set")
	}
	if result.NewHash == "" {
		t.Error("new_hash should be set")
	}
	if result.OldHash != result.NewHash {
		t.Errorf("old_hash %q != new_hash %q, but template should be current", result.OldHash, result.NewHash)
	}
}

// TestTemplateUpdateNoOrigin verifies that file-published templates (which
// carry no origin metadata) are skipped by the update command.
func TestTemplateUpdateNoOrigin(t *testing.T) {
	t.Parallel()

	op := setupOperatorEnv(t)

	// Publish a template from a local file (no origin tracking).
	templateContent := `{
  "description": "update-no-origin-test: file-sourced",
  "command": ["/bin/test-agent"]
}`
	templateFile := filepath.Join(t.TempDir(), "no-origin.jsonc")
	if err := os.WriteFile(templateFile, []byte(templateContent), 0644); err != nil {
		t.Fatalf("write template file: %v", err)
	}

	templateRef := op.Namespace.TemplateRoomAliasLocalpart() + ":update-no-origin-test"

	op.run(t, "template", "publish",
		"--file", templateFile,
		templateRef,
		"--json")

	// Run update on the file-published template.
	updateOutput := op.run(t, "template", "update",
		"--yes",
		templateRef,
		"--json")

	var results []templateUpdateResult
	if err := json.Unmarshal([]byte(updateOutput), &results); err != nil {
		t.Fatalf("parse update output: %v\noutput:\n%s", err, updateOutput)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %s", len(results), updateOutput)
	}

	result := results[0]

	if result.Name != "update-no-origin-test" {
		t.Errorf("name = %q, want %q", result.Name, "update-no-origin-test")
	}
	if result.Status != "skipped" {
		t.Errorf("status = %q, want %q", result.Status, "skipped")
	}
	if result.EventID != "" {
		t.Errorf("event_id = %q, should be empty for skipped template", result.EventID)
	}
}

// TestTemplateUpdateRoom verifies room-scoped update: checks all templates
// in a room, updating flake-sourced ones and skipping file-sourced ones.
func TestTemplateUpdateRoom(t *testing.T) {
	t.Parallel()

	nixBinDirectory := nixTestEnv(t)
	op := setupOperatorEnv(t)

	flakeEnv := append(append([]string(nil), op.Env...),
		"PATH="+nixBinDirectory+":"+os.Getenv("PATH"))

	// Publish a flake-sourced template.
	flakeDirectory := createTestFlakeRepo(t,
		"update-room-flake",
		map[string]string{"FLAVOR": "flake"})
	flakeRef := "path:" + flakeDirectory

	roomLocalpart := op.Namespace.TemplateRoomAliasLocalpart()
	flakeTemplateRef := roomLocalpart + ":update-room-flake"

	runBureauWithEnvOrFail(t, flakeEnv,
		"template", "publish",
		"--flake", flakeRef,
		flakeTemplateRef,
		"--json")

	// Publish a file-sourced template (no origin).
	fileContent := `{
  "description": "update-room-file: file-sourced",
  "command": ["/bin/test-agent"]
}`
	templateFile := filepath.Join(t.TempDir(), "file-template.jsonc")
	if err := os.WriteFile(templateFile, []byte(fileContent), 0644); err != nil {
		t.Fatalf("write template file: %v", err)
	}

	fileTemplateRef := roomLocalpart + ":update-room-file"
	op.run(t, "template", "publish",
		"--file", templateFile,
		fileTemplateRef,
		"--json")

	// Modify the flake source and commit.
	writeTestFlakeNix(t, flakeDirectory,
		"update-room-flake: modified",
		map[string]string{"FLAVOR": "flake-v2"})
	commitTestFlakeRepo(t, flakeDirectory, "Update for room-scoped test")

	// Run room-scoped update.
	updateOutput := runBureauWithEnvOrFail(t, flakeEnv,
		"template", "update",
		"--yes",
		roomLocalpart,
		"--json")

	var results []templateUpdateResult
	if err := json.Unmarshal([]byte(updateOutput), &results); err != nil {
		t.Fatalf("parse update output: %v\noutput:\n%s", err, updateOutput)
	}

	// Find results for our two templates. The room may contain other
	// templates from the namespace setup, so filter by name.
	var flakeResult, fileResult *templateUpdateResult
	for index := range results {
		switch results[index].Name {
		case "update-room-flake":
			flakeResult = &results[index]
		case "update-room-file":
			fileResult = &results[index]
		}
	}

	if flakeResult == nil {
		t.Fatalf("no result for update-room-flake in output: %s", updateOutput)
	}
	if flakeResult.Status != "updated" {
		t.Errorf("flake template status = %q, want %q", flakeResult.Status, "updated")
	}
	if flakeResult.EventID == "" {
		t.Error("flake template event_id should be set after publish")
	}
	if flakeResult.Source != "flake" {
		t.Errorf("flake template source = %q, want %q", flakeResult.Source, "flake")
	}
	if flakeResult.OldHash == flakeResult.NewHash {
		t.Error("flake template old_hash and new_hash should differ")
	}

	if fileResult == nil {
		t.Fatalf("no result for update-room-file in output: %s", updateOutput)
	}
	if fileResult.Status != "skipped" {
		t.Errorf("file template status = %q, want %q", fileResult.Status, "skipped")
	}
	if fileResult.EventID != "" {
		t.Errorf("file template event_id = %q, should be empty", fileResult.EventID)
	}
}

// TestTemplateUpdateDryRun verifies that --dry-run reports available updates
// without publishing them. The Matrix state event should remain unchanged
// after a dry-run.
func TestTemplateUpdateDryRun(t *testing.T) {
	t.Parallel()

	nixBinDirectory := nixTestEnv(t)
	op := setupOperatorEnv(t)

	flakeEnv := append(append([]string(nil), op.Env...),
		"PATH="+nixBinDirectory+":"+os.Getenv("PATH"))

	flakeDirectory := createTestFlakeRepo(t,
		"update-dryrun-test: initial",
		map[string]string{"VERSION": "1"})
	flakeRef := "path:" + flakeDirectory

	templateRef := op.Namespace.TemplateRoomAliasLocalpart() + ":update-dryrun-test"

	// Publish the initial template.
	runBureauWithEnvOrFail(t, flakeEnv,
		"template", "publish",
		"--flake", flakeRef,
		templateRef,
		"--json")

	// Read the initial content for later comparison.
	showBefore := runBureauWithEnvOrFail(t, op.Env,
		"template", "show", "--raw", templateRef, "--json")

	var contentBefore schema.TemplateContent
	if err := json.Unmarshal([]byte(showBefore), &contentBefore); err != nil {
		t.Fatalf("parse initial show output: %v\noutput:\n%s", err, showBefore)
	}

	// Modify the flake and commit.
	writeTestFlakeNix(t, flakeDirectory,
		"update-dryrun-test: modified",
		map[string]string{"VERSION": "2"})
	commitTestFlakeRepo(t, flakeDirectory, "Update for dry-run test")

	// Run update with --dry-run.
	updateOutput := runBureauWithEnvOrFail(t, flakeEnv,
		"template", "update",
		"--dry-run",
		"--yes",
		templateRef,
		"--json")

	var results []templateUpdateResult
	if err := json.Unmarshal([]byte(updateOutput), &results); err != nil {
		t.Fatalf("parse update output: %v\noutput:\n%s", err, updateOutput)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %s", len(results), updateOutput)
	}

	result := results[0]

	if result.Status != "updated" {
		t.Errorf("status = %q, want %q (dry-run still detects the change)", result.Status, "updated")
	}
	if !result.DryRun {
		t.Error("dry_run should be true with --dry-run flag")
	}
	if result.EventID != "" {
		t.Errorf("event_id = %q, should be empty for dry-run", result.EventID)
	}
	if result.OldHash == result.NewHash {
		t.Error("old_hash and new_hash should differ (change was detected)")
	}
	if result.Change == "" {
		t.Error("change classification should be set")
	}
	if len(result.Fields) == 0 {
		t.Error("fields should list changed field names")
	}

	// Verify Matrix state was NOT modified by the dry-run.
	showAfter := runBureauWithEnvOrFail(t, op.Env,
		"template", "show", "--raw", templateRef, "--json")

	var contentAfter schema.TemplateContent
	if err := json.Unmarshal([]byte(showAfter), &contentAfter); err != nil {
		t.Fatalf("parse post-dryrun show output: %v\noutput:\n%s", err, showAfter)
	}

	if contentAfter.Description != contentBefore.Description {
		t.Errorf("description changed after dry-run: %q → %q",
			contentBefore.Description, contentAfter.Description)
	}
	if contentAfter.Origin == nil {
		t.Fatal("origin should still be present after dry-run")
	}
	if contentAfter.Origin.ContentHash != contentBefore.Origin.ContentHash {
		t.Errorf("content_hash changed after dry-run: %q → %q",
			contentBefore.Origin.ContentHash, contentAfter.Origin.ContentHash)
	}
}
