// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/bureau-foundation/bureau/lib/nix"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestEnvironmentUpdateDryRunDetectsStale verifies that "bureau environment
// update --dry-run" correctly detects when a profile's source flake has a
// newer revision than the last compose. This is the core staleness detection
// path that drives fleet environment updates.
//
// Steps:
//  1. Create a test flake repo and record its initial git revision
//  2. Publish a fake m.bureau.environment_build with that revision
//  3. Make a new commit to the flake
//  4. Run "bureau environment update --dry-run" — expect status "updated"
//  5. Verify the old and new revisions are reported correctly
//
// Skipped when nix is not installed.
func TestEnvironmentUpdateDryRunDetectsStale(t *testing.T) {
	t.Parallel()
	nixTestEnv(t)

	op := setupOperatorEnv(t)

	// Set up fleet cache config so the update command can resolve
	// system and template defaults.
	publishFleetCacheConfig(t, op)

	// Create a test flake and get its initial revision.
	flakeDirectory := createMinimalFlakeRepo(t)
	flakeRef := "git+file://" + flakeDirectory

	initialRevision := getFlakeRevision(t, flakeDirectory)

	// Publish a fake build record with the initial revision.
	profileName := "test-env-stale"
	publishBuildRecord(t, op, profileName, flakeRef, initialRevision)

	// Make a new commit to advance the flake's HEAD.
	writeMinimalFlakeNix(t, flakeDirectory, "v2")
	commitFlakeRepo(t, flakeDirectory, "Advance to v2")

	newRevision := getFlakeRevision(t, flakeDirectory)
	if newRevision == initialRevision {
		t.Fatal("flake revision should change after new commit")
	}

	// Run update --dry-run and parse the JSON output.
	updateOutput := runBureauWithEnvOrFail(t, op.Env,
		"environment", "update",
		"--dry-run",
		"--machine", op.Fleet.Prefix+"/machine/builder",
		"--flake", flakeRef,
		"--system", nixSystem(t),
		"--template", "bureau/template:nix-builder",
		"--json",
		profileName,
	)

	var result environmentUpdateResult
	if err := json.Unmarshal([]byte(updateOutput), &result); err != nil {
		t.Fatalf("parse update output: %v\noutput:\n%s", err, updateOutput)
	}

	if result.Profile != profileName {
		t.Errorf("profile = %q, want %q", result.Profile, profileName)
	}
	if result.Status != "updated" {
		t.Errorf("status = %q, want %q (error: %s)", result.Status, "updated", result.Error)
	}
	if !result.DryRun {
		t.Error("dry_run should be true")
	}
	if result.BuildRevision == "" {
		t.Error("build_revision should be set")
	}
	if result.CurrentRevision == "" {
		t.Error("current_revision should be set")
	}
	if result.BuildRevision == result.CurrentRevision {
		t.Error("build_revision and current_revision should differ")
	}

	// Ticket fields should be empty in dry-run mode.
	if result.TicketID != "" {
		t.Errorf("ticket_id = %q, should be empty in dry-run", result.TicketID)
	}
}

// TestEnvironmentUpdateCurrent verifies that "bureau environment update
// --dry-run" reports status "current" when the flake revision matches
// the last build.
//
// Skipped when nix is not installed.
func TestEnvironmentUpdateCurrent(t *testing.T) {
	t.Parallel()
	nixTestEnv(t)

	op := setupOperatorEnv(t)

	publishFleetCacheConfig(t, op)

	flakeDirectory := createMinimalFlakeRepo(t)
	flakeRef := "git+file://" + flakeDirectory
	revision := getFlakeRevision(t, flakeDirectory)

	// Publish a build record with the current revision.
	profileName := "test-env-current"
	publishBuildRecord(t, op, profileName, flakeRef, revision)

	updateOutput := runBureauWithEnvOrFail(t, op.Env,
		"environment", "update",
		"--dry-run",
		"--machine", op.Fleet.Prefix+"/machine/builder",
		"--flake", flakeRef,
		"--system", nixSystem(t),
		"--template", "bureau/template:nix-builder",
		"--json",
		profileName,
	)

	var result environmentUpdateResult
	if err := json.Unmarshal([]byte(updateOutput), &result); err != nil {
		t.Fatalf("parse update output: %v\noutput:\n%s", err, updateOutput)
	}

	if result.Status != "current" {
		t.Errorf("status = %q, want %q (error: %s)", result.Status, "current", result.Error)
	}
	if result.BuildRevision == "" {
		t.Error("build_revision should be set")
	}
	if result.CurrentRevision == "" {
		t.Error("current_revision should be set")
	}
	if result.BuildRevision != result.CurrentRevision {
		t.Errorf("revisions should match: build=%q current=%q",
			result.BuildRevision, result.CurrentRevision)
	}
}

// TestEnvironmentUpdateNoRevision verifies that when a build record has
// no resolved_revision (from a compose before revision tracking was
// added), the update command treats it as stale.
//
// Skipped when nix is not installed.
func TestEnvironmentUpdateNoRevision(t *testing.T) {
	t.Parallel()
	nixTestEnv(t)

	op := setupOperatorEnv(t)

	publishFleetCacheConfig(t, op)

	flakeDirectory := createMinimalFlakeRepo(t)
	flakeRef := "git+file://" + flakeDirectory

	// Publish a build record with NO resolved revision.
	profileName := "test-env-no-rev"
	publishBuildRecord(t, op, profileName, flakeRef, "")

	updateOutput := runBureauWithEnvOrFail(t, op.Env,
		"environment", "update",
		"--dry-run",
		"--machine", op.Fleet.Prefix+"/machine/builder",
		"--flake", flakeRef,
		"--system", nixSystem(t),
		"--template", "bureau/template:nix-builder",
		"--json",
		profileName,
	)

	var result environmentUpdateResult
	if err := json.Unmarshal([]byte(updateOutput), &result); err != nil {
		t.Fatalf("parse update output: %v\noutput:\n%s", err, updateOutput)
	}

	if result.Status != "updated" {
		t.Errorf("status = %q, want %q (no-revision builds should be treated as stale; error: %s)",
			result.Status, "updated", result.Error)
	}
	if result.BuildRevision != "" {
		t.Errorf("build_revision = %q, should be empty for pre-tracking builds",
			result.BuildRevision)
	}
	if result.CurrentRevision == "" {
		t.Error("current_revision should be set from the flake")
	}
}

// TestEnvironmentUpdateNoBuild verifies that the update command returns
// a clear error when no previous build exists for the profile.
//
// Skipped when nix is not installed.
func TestEnvironmentUpdateNoBuild(t *testing.T) {
	t.Parallel()
	nixTestEnv(t)

	op := setupOperatorEnv(t)

	publishFleetCacheConfig(t, op)

	flakeDirectory := createMinimalFlakeRepo(t)
	flakeRef := "git+file://" + flakeDirectory

	// Run update for a profile that was never composed.
	updateOutput := runBureauWithEnvOrFail(t, op.Env,
		"environment", "update",
		"--dry-run",
		"--machine", op.Fleet.Prefix+"/machine/builder",
		"--flake", flakeRef,
		"--system", nixSystem(t),
		"--template", "bureau/template:nix-builder",
		"--json",
		"nonexistent-profile",
	)

	var result environmentUpdateResult
	if err := json.Unmarshal([]byte(updateOutput), &result); err != nil {
		t.Fatalf("parse update output: %v\noutput:\n%s", err, updateOutput)
	}

	if result.Status != "error" {
		t.Errorf("status = %q, want %q", result.Status, "error")
	}
	if result.Error == "" {
		t.Error("error message should explain that no previous build was found")
	}
}

// --- Test result type ---

// environmentUpdateResult mirrors the CLI's JSON output for environment update.
type environmentUpdateResult struct {
	Profile         string `json:"profile"`
	Status          string `json:"status"`
	FlakeRef        string `json:"flake_ref"`
	BuildRevision   string `json:"build_revision,omitempty"`
	CurrentRevision string `json:"current_revision,omitempty"`
	StorePath       string `json:"store_path,omitempty"`
	TicketID        string `json:"ticket_id,omitempty"`
	TicketRoom      string `json:"ticket_room,omitempty"`
	Conclusion      string `json:"conclusion,omitempty"`
	Error           string `json:"error,omitempty"`
	DryRun          bool   `json:"dry_run"`
}

// --- Helpers ---

// publishFleetCacheConfig publishes a minimal m.bureau.fleet_cache state
// event to the fleet room so that the update command's resolveFleetConfig
// succeeds.
func publishFleetCacheConfig(t *testing.T, op *operatorEnv) {
	t.Helper()

	context := t.Context()
	_, err := op.Admin.SendStateEvent(context, op.Fleet.FleetRoomID,
		schema.EventTypeFleetCache, "",
		schema.FleetCacheContent{
			Name:            "test-cache",
			DefaultSystem:   nixSystem(t),
			ComposeTemplate: "bureau/template:nix-builder",
		},
	)
	if err != nil {
		t.Fatalf("publish fleet cache config: %v", err)
	}
}

// publishBuildRecord publishes an m.bureau.environment_build state event
// simulating a previous compose. If revision is empty, the build record
// has no resolved_revision (simulating pre-tracking builds).
func publishBuildRecord(t *testing.T, op *operatorEnv, profile, flakeRef, revision string) {
	t.Helper()

	context := t.Context()

	// Construct a plausible machine UserID for the build record.
	machineUserID := ref.MustParseUserID("@" + op.Fleet.Prefix + "/machine/builder:" + testServerName)

	_, err := op.Admin.SendStateEvent(context, op.Fleet.FleetRoomID,
		schema.EventTypeEnvironmentBuild, profile,
		schema.EnvironmentBuildContent{
			Profile:          profile,
			FlakeRef:         flakeRef,
			System:           nixSystem(t),
			StorePath:        "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-test-" + profile,
			Machine:          machineUserID,
			ResolvedRevision: revision,
			Timestamp:        "2026-01-01T00:00:00Z",
		},
	)
	if err != nil {
		t.Fatalf("publish build record for %q: %v", profile, err)
	}
}

// createMinimalFlakeRepo creates a git-initialized flake repo with the
// simplest possible flake.nix (no bureau outputs needed — we only use
// it for nix flake metadata to resolve revisions).
func createMinimalFlakeRepo(t *testing.T) string {
	t.Helper()

	directory := t.TempDir()
	writeMinimalFlakeNix(t, directory, "v1")

	context := t.Context()
	for _, step := range []struct {
		args        []string
		description string
	}{
		{[]string{"init", "-b", "main", directory}, "git init"},
		{[]string{"-C", directory, "config", "user.name", "Test"}, "git config user.name"},
		{[]string{"-C", directory, "config", "user.email", "test@test"}, "git config user.email"},
		{[]string{"-C", directory, "add", "flake.nix"}, "git add"},
		{[]string{"-C", directory, "commit", "-m", "Initial minimal flake"}, "git commit"},
	} {
		cmd := exec.CommandContext(context, "git", step.args...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v", step.description, err)
		}
	}

	return directory
}

// writeMinimalFlakeNix writes a trivial flake.nix that produces no
// outputs but has a valid git revision. The version parameter changes
// the file content so that new commits produce distinct revisions.
func writeMinimalFlakeNix(t *testing.T, directory, version string) {
	t.Helper()

	content := `{
  # Minimal flake for revision tracking tests.
  # version: ` + version + `
  outputs = { self }: {
    version = "` + version + `";
  };
}
`
	if err := os.WriteFile(directory+"/flake.nix", []byte(content), 0644); err != nil {
		t.Fatalf("write flake.nix: %v", err)
	}
}

// commitFlakeRepo stages and commits all changes in a test flake repo.
func commitFlakeRepo(t *testing.T, directory, message string) {
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

// getFlakeRevision runs "nix flake metadata --json" and extracts the
// resolved git revision. Uses nix.RunContext which handles binary
// resolution (PATH then /nix/var/nix/profiles/default/bin/nix).
func getFlakeRevision(t *testing.T, directory string) string {
	t.Helper()

	flakeRef := "git+file://" + directory

	output, err := nix.RunContext(t.Context(), "flake", "metadata", "--json", flakeRef)
	if err != nil {
		t.Fatalf("nix flake metadata %s: %v", flakeRef, err)
	}

	var metadata struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal([]byte(output), &metadata); err != nil {
		t.Fatalf("parse flake metadata: %v\noutput: %s", err, output)
	}

	if metadata.Revision == "" {
		t.Fatalf("flake %s has no revision", flakeRef)
	}

	return metadata.Revision
}

// Ensure imports are used.
var (
	_ = messaging.GetState[schema.EnvironmentBuildContent]
	_ ref.UserID
)
