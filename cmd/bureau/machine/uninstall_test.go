// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli/doctor"
)

func TestUninstallChecks_StopOrderReversed(t *testing.T) {
	// Verify services are stopped in reverse order (daemon before
	// launcher). On a test machine without systemd, isUnitActive
	// returns false, so all stop checks produce Pass — but the
	// ordering is still visible in the result names.
	savedUnits := expectedUnits
	defer func() { expectedUnits = savedUnits }()

	expectedUnits = []systemdUnitSpec{
		{name: "first.service", installPath: "/nonexistent/first.service"},
		{name: "second.service", installPath: "/nonexistent/second.service"},
	}

	results := uninstallChecks("nobody", "nogroup", false)

	// First two results should be "stop" in reverse order.
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}
	if results[0].Name != "stop second.service" {
		t.Errorf("first stop should be second.service (reverse order), got %q", results[0].Name)
	}
	if results[1].Name != "stop first.service" {
		t.Errorf("second stop should be first.service (reverse order), got %q", results[1].Name)
	}
}

func TestUninstallChecks_UnitFileExists_ProducesFailElevated(t *testing.T) {
	savedUnits := expectedUnits
	defer func() { expectedUnits = savedUnits }()

	unitPath := filepath.Join(t.TempDir(), "test-unit.service")
	if err := os.WriteFile(unitPath, []byte("[Unit]\nDescription=Test\n"), 0644); err != nil {
		t.Fatalf("write unit: %v", err)
	}

	expectedUnits = []systemdUnitSpec{
		{
			name:            "test-unit.service",
			expectedContent: func() string { return "[Unit]\nDescription=Test\n" },
			installPath:     unitPath,
		},
	}

	results := uninstallChecks("nobody", "nogroup", false)

	// Find the "remove test-unit.service" result.
	var removeResult *doctor.Result
	for index := range results {
		if results[index].Name == "remove test-unit.service" {
			removeResult = &results[index]
			break
		}
	}

	if removeResult == nil {
		names := resultNames(results)
		t.Fatalf("no 'remove test-unit.service' result found; results: %v", names)
	}

	if removeResult.Status != doctor.StatusFail {
		t.Errorf("expected FAIL for existing unit file, got %s", removeResult.Status)
	}
	if !removeResult.HasFix() {
		t.Error("existing unit file should have a removal fix")
	}
	if !removeResult.Elevated {
		t.Error("unit file removal should be elevated")
	}
	if !strings.Contains(removeResult.Message, unitPath) {
		t.Errorf("message should contain unit path %q, got: %s", unitPath, removeResult.Message)
	}
}

func TestUninstallChecks_UnitFileExists_TriggersDaemonReload(t *testing.T) {
	savedUnits := expectedUnits
	defer func() { expectedUnits = savedUnits }()

	unitPath := filepath.Join(t.TempDir(), "test-unit.service")
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0644); err != nil {
		t.Fatalf("write unit: %v", err)
	}

	expectedUnits = []systemdUnitSpec{
		{name: "test-unit.service", installPath: unitPath},
	}

	results := uninstallChecks("nobody", "nogroup", false)

	var hasDaemonReload bool
	for _, result := range results {
		if result.Name == "systemd daemon-reload" {
			hasDaemonReload = true
			if result.Status != doctor.StatusFail {
				t.Errorf("daemon-reload should be FAIL (action needed), got %s", result.Status)
			}
			break
		}
	}

	if !hasDaemonReload {
		t.Error("expected daemon-reload result when unit files exist")
	}
}

func TestUninstallChecks_UnitFileGone_NoDaemonReload(t *testing.T) {
	savedUnits := expectedUnits
	defer func() { expectedUnits = savedUnits }()

	expectedUnits = []systemdUnitSpec{
		{name: "test-unit.service", installPath: "/nonexistent/test-unit.service"},
	}

	results := uninstallChecks("nobody", "nogroup", false)

	for _, result := range results {
		if result.Name == "systemd daemon-reload" {
			t.Error("should not produce daemon-reload when no unit files exist")
		}
	}
}

func TestUninstallChecks_BinaryExists_ProducesFailElevated(t *testing.T) {
	savedBinaries := expectedHostBinaries
	savedBinaryDir := binaryInstallDir
	defer func() {
		expectedHostBinaries = savedBinaries
		binaryInstallDir = savedBinaryDir
	}()

	tempDir := t.TempDir()
	binaryInstallDir = tempDir
	expectedHostBinaries = []string{"test-binary"}

	binaryPath := filepath.Join(tempDir, "test-binary")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	results := uninstallChecks("nobody", "nogroup", false)

	var removeResult *doctor.Result
	for index := range results {
		if results[index].Name == "remove test-binary" {
			removeResult = &results[index]
			break
		}
	}

	if removeResult == nil {
		t.Fatalf("no 'remove test-binary' result; results: %v", resultNames(results))
	}
	if removeResult.Status != doctor.StatusFail {
		t.Errorf("expected FAIL for existing binary, got %s", removeResult.Status)
	}
	if !removeResult.Elevated {
		t.Error("binary removal should be elevated")
	}
}

func TestUninstallChecks_BinaryGone_ProducesPass(t *testing.T) {
	savedBinaries := expectedHostBinaries
	savedBinaryDir := binaryInstallDir
	defer func() {
		expectedHostBinaries = savedBinaries
		binaryInstallDir = savedBinaryDir
	}()

	binaryInstallDir = t.TempDir()
	expectedHostBinaries = []string{"missing-binary"}

	results := uninstallChecks("nobody", "nogroup", false)

	var removeResult *doctor.Result
	for index := range results {
		if results[index].Name == "remove missing-binary" {
			removeResult = &results[index]
			break
		}
	}

	if removeResult == nil {
		t.Fatalf("no 'remove missing-binary' result; results: %v", resultNames(results))
	}
	if removeResult.Status != doctor.StatusPass {
		t.Errorf("expected PASS for missing binary, got %s: %s", removeResult.Status, removeResult.Message)
	}
}

func TestUninstallChecks_WithoutRemoveUser_NoUserChecks(t *testing.T) {
	savedUnits := expectedUnits
	savedBinaries := expectedHostBinaries
	defer func() {
		expectedUnits = savedUnits
		expectedHostBinaries = savedBinaries
	}()

	expectedUnits = nil
	expectedHostBinaries = nil

	results := uninstallChecks("bureau", "bureau-operators", false)

	for _, result := range results {
		if strings.Contains(result.Name, "user") || strings.Contains(result.Name, "group") {
			t.Errorf("removeUser=false should not produce user/group checks, got %q", result.Name)
		}
	}
}

func TestUninstallChecks_WithRemoveUser_HasUserAndGroupChecks(t *testing.T) {
	savedUnits := expectedUnits
	savedBinaries := expectedHostBinaries
	defer func() {
		expectedUnits = savedUnits
		expectedHostBinaries = savedBinaries
	}()

	expectedUnits = nil
	expectedHostBinaries = nil

	results := uninstallChecks("bureau", "bureau-operators", true)

	var hasUserCheck, hasGroupCheck bool
	for _, result := range results {
		if result.Name == "remove bureau user" {
			hasUserCheck = true
		}
		if result.Name == "remove bureau-operators group" {
			hasGroupCheck = true
		}
	}

	if !hasUserCheck {
		t.Error("removeUser=true should produce a user removal check")
	}
	if !hasGroupCheck {
		t.Error("removeUser=true should produce a group removal check")
	}
}

func TestUninstallChecks_DirectoriesReversed(t *testing.T) {
	// Verify directories are removed deepest-first. On a test machine,
	// system directories won't exist, so all produce Pass, but the
	// ordering is still correct.
	savedUnits := expectedUnits
	savedBinaries := expectedHostBinaries
	defer func() {
		expectedUnits = savedUnits
		expectedHostBinaries = savedBinaries
	}()

	expectedUnits = nil
	expectedHostBinaries = nil

	results := uninstallChecks("bureau", "bureau-operators", false)

	// Collect the directory removal results in order.
	var directoryNames []string
	for _, result := range results {
		if strings.HasPrefix(result.Name, "remove /") {
			directoryNames = append(directoryNames, result.Name)
		}
	}

	// The directory list from buildExpectedDirectories is:
	//   /etc/bureau, /run/bureau, /var/lib/bureau,
	//   /var/bureau/bin, /var/bureau/workspace, /var/bureau/cache
	// Reversed gives deepest-first. Then /var/bureau parent last.
	// Verify the parent /var/bureau comes after all /var/bureau/* children.
	var sawVarBureauParent bool
	for _, name := range directoryNames {
		if name == "remove /var/bureau" {
			sawVarBureauParent = true
			continue
		}
		if sawVarBureauParent && strings.HasPrefix(name, "remove /var/bureau/") {
			t.Errorf("/var/bureau parent removed before child %q", name)
		}
	}

	if !sawVarBureauParent {
		t.Error("expected /var/bureau parent in directory removal list")
	}
}

func TestUninstallCommand_UnexpectedArg(t *testing.T) {
	command := uninstallCommand()
	err := command.Execute([]string{"extra"})
	if err == nil {
		t.Fatal("expected error for unexpected argument")
	}
}

// resultNames extracts the names from a slice of results for
// diagnostic messages.
func resultNames(results []doctor.Result) []string {
	names := make([]string, len(results))
	for index, result := range results {
		names[index] = result.Name
	}
	return names
}
