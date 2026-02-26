// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli/doctor"
	"github.com/bureau-foundation/bureau/lib/principal"
)

func TestCheckSystemUser_BureauUserExists(t *testing.T) {
	// The bureau user may or may not exist on the test machine.
	// This test verifies the check produces a result with the correct
	// name and status type. It can't assert a specific status without
	// controlling the OS state.
	results := checkSystemUser(principal.SystemUserName, principal.OperatorsGroupName)

	// Should always produce at least 3 results: bureau user, group, membership.
	if len(results) < 3 {
		t.Fatalf("checkSystemUser() returned %d results, expected at least 3", len(results))
	}

	if results[0].Name != "bureau user" {
		t.Errorf("first check name = %q, want %q", results[0].Name, "bureau user")
	}
	if results[1].Name != "bureau-operators group" {
		t.Errorf("second check name = %q, want %q", results[1].Name, "bureau-operators group")
	}
	if results[2].Name != "operator group membership" {
		t.Errorf("third check name = %q, want %q", results[2].Name, "operator group membership")
	}

	// If bureau user doesn't exist, the first check should be a fixable
	// elevated failure.
	_, err := user.Lookup(principal.SystemUserName)
	if err != nil {
		if results[0].Status != doctor.StatusFail {
			t.Errorf("bureau user check should FAIL when user doesn't exist, got %s", results[0].Status)
		}
		if !results[0].HasFix() {
			t.Error("bureau user check should have a fix when user doesn't exist")
		}
		if !results[0].Elevated {
			t.Error("bureau user fix should be elevated")
		}
	} else {
		if results[0].Status != doctor.StatusPass {
			t.Errorf("bureau user check should PASS when user exists, got %s: %s", results[0].Status, results[0].Message)
		}
	}
}

func TestCheckDirectories_NonexistentPath(t *testing.T) {
	nonexistent := t.TempDir() + "/nonexistent"
	directories := []directorySpec{
		{nonexistent, "root:root", 0755},
	}

	results := checkDirectories(directories)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	if results[0].Status != doctor.StatusFail {
		t.Errorf("expected FAIL for nonexistent directory, got %s: %s", results[0].Status, results[0].Message)
	}
	if !results[0].HasFix() {
		t.Error("nonexistent directory should have a fix")
	}
	if !results[0].Elevated {
		t.Error("directory fix should be elevated")
	}
}

func TestCheckDirectories_CorrectOwnership(t *testing.T) {
	// Create a temp directory owned by current user.
	directory := t.TempDir() + "/testdir"
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Stat the actual directory to get its real UID/GID, then look up
	// the user and group names from those IDs. This avoids assuming the
	// user's primary group matches a group-by-name lookup (which can
	// differ when the user's primary GID maps to a differently-named group).
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	actualUser, err := user.LookupId(fmt.Sprintf("%d", stat.Uid))
	if err != nil {
		t.Fatalf("lookup uid %d: %v", stat.Uid, err)
	}
	actualGroup, err := user.LookupGroupId(fmt.Sprintf("%d", stat.Gid))
	if err != nil {
		t.Fatalf("lookup gid %d: %v", stat.Gid, err)
	}

	directories := []directorySpec{
		{directory, actualUser.Username + ":" + actualGroup.Name, 0755},
	}

	results := checkDirectories(directories)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	if results[0].Status != doctor.StatusPass {
		t.Errorf("expected PASS for correctly owned directory, got %s: %s", results[0].Status, results[0].Message)
	}
}

func TestCheckDirectories_WrongMode(t *testing.T) {
	directory := t.TempDir() + "/testdir"
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	currentUser, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	currentGroup, err := user.LookupGroupId(currentUser.Gid)
	if err != nil {
		t.Fatalf("lookup group: %v", err)
	}

	directories := []directorySpec{
		{directory, currentUser.Username + ":" + currentGroup.Name, 0755},
	}

	results := checkDirectories(directories)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	if results[0].Status != doctor.StatusFail {
		t.Errorf("expected FAIL for wrong mode, got %s: %s", results[0].Status, results[0].Message)
	}
	if !strings.Contains(results[0].Message, "mode") {
		t.Errorf("expected 'mode' in message, got: %s", results[0].Message)
	}
}

func TestCheckMachineConfig_FileExists(t *testing.T) {
	// Create a valid machine.conf in a temp directory.
	confDir := t.TempDir()
	confPath := confDir + "/machine.conf"
	confContent := "BUREAU_HOMESERVER_URL=http://matrix:6167\n" +
		"BUREAU_MACHINE_NAME=bureau/fleet/prod/machine/worker-01\n" +
		"BUREAU_SERVER_NAME=bureau.local\n" +
		"BUREAU_FLEET=bureau/fleet/prod\n"
	if err := os.WriteFile(confPath, []byte(confContent), 0644); err != nil {
		t.Fatalf("write machine.conf: %v", err)
	}

	t.Setenv("BUREAU_MACHINE_CONF", confPath)

	params := machineDoctorParams{}
	config := readMachineConfig(params)
	results := checkMachineConfig(params, config)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != doctor.StatusPass {
		t.Errorf("expected PASS, got %s: %s", results[0].Status, results[0].Message)
	}
}

func TestCheckMachineConfig_MissingKeys(t *testing.T) {
	confDir := t.TempDir()
	confPath := confDir + "/machine.conf"
	confContent := "BUREAU_HOMESERVER_URL=http://matrix:6167\n"
	if err := os.WriteFile(confPath, []byte(confContent), 0644); err != nil {
		t.Fatalf("write machine.conf: %v", err)
	}

	t.Setenv("BUREAU_MACHINE_CONF", confPath)

	params := machineDoctorParams{}
	config := readMachineConfig(params)
	results := checkMachineConfig(params, config)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != doctor.StatusFail {
		t.Errorf("expected FAIL, got %s: %s", results[0].Status, results[0].Message)
	}
	if !strings.Contains(results[0].Message, "missing keys") {
		t.Errorf("expected 'missing keys' in message, got: %s", results[0].Message)
	}
}

func TestCheckMachineConfig_MissingFile_WithFlags(t *testing.T) {
	t.Setenv("BUREAU_MACHINE_CONF", t.TempDir()+"/nonexistent/machine.conf")

	params := machineDoctorParams{
		Homeserver:  "http://matrix:6167",
		MachineName: "bureau/fleet/prod/machine/worker-01",
		ServerName:  "bureau.local",
		Fleet:       "bureau/fleet/prod",
	}
	config := readMachineConfig(params)
	results := checkMachineConfig(params, config)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != doctor.StatusFail {
		t.Errorf("expected FAIL, got %s: %s", results[0].Status, results[0].Message)
	}
	if !results[0].HasFix() {
		t.Error("missing file with flags should have a fix")
	}
	if !results[0].Elevated {
		t.Error("config write should be elevated")
	}
}

func TestCheckMachineConfig_MissingFile_NoFlags(t *testing.T) {
	t.Setenv("BUREAU_MACHINE_CONF", t.TempDir()+"/nonexistent/machine.conf")

	params := machineDoctorParams{}
	config := readMachineConfig(params)
	results := checkMachineConfig(params, config)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != doctor.StatusFail {
		t.Errorf("expected FAIL, got %s: %s", results[0].Status, results[0].Message)
	}
	if results[0].HasFix() {
		t.Error("missing file without flags should NOT have a fix")
	}
}

func TestCheckSystemdUnits_MissingUnit(t *testing.T) {
	saved := expectedUnits
	defer func() { expectedUnits = saved }()

	expectedUnits = []systemdUnitSpec{
		{
			name:            "test-unit.service",
			expectedContent: func() string { return "[Unit]\nDescription=Test\n" },
			installPath:     t.TempDir() + "/test-unit.service",
		},
	}

	results := checkSystemdUnits()

	// Should have 3 results: installed, enabled, active.
	if len(results) < 3 {
		t.Fatalf("expected at least 3 results, got %d", len(results))
	}

	// Installed check should fail with fix.
	if results[0].Name != "test-unit.service installed" {
		t.Errorf("first check name = %q", results[0].Name)
	}
	if results[0].Status != doctor.StatusFail {
		t.Errorf("expected FAIL for missing unit file, got %s: %s", results[0].Status, results[0].Message)
	}
	if !results[0].HasFix() {
		t.Error("missing unit should have a fix")
	}
	if !results[0].Elevated {
		t.Error("unit install should be elevated")
	}
}

func TestCheckSystemdUnits_ContentMismatch(t *testing.T) {
	saved := expectedUnits
	defer func() { expectedUnits = saved }()

	unitPath := t.TempDir() + "/test-unit.service"
	if err := os.WriteFile(unitPath, []byte("wrong content"), 0644); err != nil {
		t.Fatalf("write unit: %v", err)
	}

	expectedUnits = []systemdUnitSpec{
		{
			name:            "test-unit.service",
			expectedContent: func() string { return "[Unit]\nDescription=Test\n" },
			installPath:     unitPath,
		},
	}

	results := checkSystemdUnits()

	if results[0].Status != doctor.StatusFail {
		t.Errorf("expected FAIL for content mismatch, got %s: %s", results[0].Status, results[0].Message)
	}
	if !strings.Contains(results[0].Message, "differs") {
		t.Errorf("expected 'differs' in message, got: %s", results[0].Message)
	}
}

func TestCheckSystemdUnits_ContentMatch(t *testing.T) {
	saved := expectedUnits
	defer func() { expectedUnits = saved }()

	expectedContent := "[Unit]\nDescription=Test\n"
	unitPath := t.TempDir() + "/test-unit.service"
	if err := os.WriteFile(unitPath, []byte(expectedContent), 0644); err != nil {
		t.Fatalf("write unit: %v", err)
	}

	expectedUnits = []systemdUnitSpec{
		{
			name:            "test-unit.service",
			expectedContent: func() string { return expectedContent },
			installPath:     unitPath,
		},
	}

	results := checkSystemdUnits()

	if results[0].Status != doctor.StatusPass {
		t.Errorf("expected PASS for matching content, got %s: %s", results[0].Status, results[0].Message)
	}
}

func TestResolveOwner(t *testing.T) {
	// Test root:root resolution.
	uid, gid, err := resolveOwner("root:root")
	if err != nil {
		t.Fatalf("resolveOwner(root:root): %v", err)
	}
	if uid != 0 || gid != 0 {
		t.Errorf("root:root = %d:%d, want 0:0", uid, gid)
	}

	// Test current user resolution.
	currentUser, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	currentGroup, err := user.LookupGroupId(currentUser.Gid)
	if err != nil {
		t.Fatalf("lookup group: %v", err)
	}

	uid, gid, err = resolveOwner(currentUser.Username + ":" + currentGroup.Name)
	if err != nil {
		t.Fatalf("resolveOwner(%s:%s): %v", currentUser.Username, currentGroup.Name, err)
	}
	if uid == 0 && currentUser.Uid != "0" {
		t.Errorf("expected non-root uid for %s", currentUser.Username)
	}

	// Test invalid format.
	_, _, err = resolveOwner("nogroup")
	if err == nil {
		t.Error("expected error for invalid owner format")
	}

	// Test nonexistent user.
	_, _, err = resolveOwner("nonexistent-user-zzzz:root")
	if err == nil {
		t.Error("expected error for nonexistent user")
	}
}

func TestReadMachineConfig_FlagsOnly(t *testing.T) {
	// With no machine.conf and flags provided, config should come from flags.
	t.Setenv("BUREAU_MACHINE_CONF", t.TempDir()+"/nonexistent")

	params := machineDoctorParams{
		Homeserver:  "http://matrix:6167",
		MachineName: "bureau/fleet/prod/machine/test",
		ServerName:  "bureau.local",
		Fleet:       "bureau/fleet/prod",
	}

	config := readMachineConfig(params)
	if config == nil {
		t.Fatal("expected non-nil config from flags")
	}
	if config.HomeserverURL != "http://matrix:6167" {
		t.Errorf("HomeserverURL = %q, want %q", config.HomeserverURL, "http://matrix:6167")
	}
	if config.MachineName != "bureau/fleet/prod/machine/test" {
		t.Errorf("MachineName = %q", config.MachineName)
	}
	if config.ServerName != "bureau.local" {
		t.Errorf("ServerName = %q", config.ServerName)
	}
	if config.Fleet != "bureau/fleet/prod" {
		t.Errorf("Fleet = %q", config.Fleet)
	}
}

func TestReadMachineConfig_FlagsOverrideFile(t *testing.T) {
	confDir := t.TempDir()
	confPath := confDir + "/machine.conf"
	confContent := "BUREAU_HOMESERVER_URL=http://file-server:6167\n" +
		"BUREAU_MACHINE_NAME=bureau/fleet/prod/machine/from-file\n" +
		"BUREAU_SERVER_NAME=file.local\n" +
		"BUREAU_FLEET=bureau/fleet/file\n"
	if err := os.WriteFile(confPath, []byte(confContent), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv("BUREAU_MACHINE_CONF", confPath)

	params := machineDoctorParams{
		Homeserver: "http://flag-server:6167",
	}

	config := readMachineConfig(params)
	if config == nil {
		t.Fatal("expected non-nil config")
	}

	// Flag should override file.
	if config.HomeserverURL != "http://flag-server:6167" {
		t.Errorf("HomeserverURL = %q, want flag override", config.HomeserverURL)
	}
	// Non-overridden values should come from file.
	if config.MachineName != "bureau/fleet/prod/machine/from-file" {
		t.Errorf("MachineName = %q, want from-file", config.MachineName)
	}
}

func TestReadMachineConfig_NoValues(t *testing.T) {
	t.Setenv("BUREAU_MACHINE_CONF", t.TempDir()+"/nonexistent")

	params := machineDoctorParams{}
	config := readMachineConfig(params)
	if config != nil {
		t.Error("expected nil config when no flags and no file")
	}
}

func TestWriteMachineConf(t *testing.T) {
	confDir := t.TempDir() + "/etc/bureau"
	confPath := confDir + "/machine.conf"

	t.Setenv("BUREAU_MACHINE_CONF", confPath)

	params := machineDoctorParams{
		Homeserver:  "http://matrix:6167",
		MachineName: "bureau/fleet/prod/machine/test",
		ServerName:  "bureau.local",
		Fleet:       "bureau/fleet/prod",
	}

	if err := writeMachineConf(params); err != nil {
		t.Fatalf("writeMachineConf: %v", err)
	}

	// Read it back.
	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "BUREAU_HOMESERVER_URL=http://matrix:6167") {
		t.Errorf("missing BUREAU_HOMESERVER_URL in output:\n%s", content)
	}
	if !strings.Contains(content, "BUREAU_MACHINE_NAME=bureau/fleet/prod/machine/test") {
		t.Errorf("missing BUREAU_MACHINE_NAME in output:\n%s", content)
	}
	if !strings.Contains(content, "BUREAU_SERVER_NAME=bureau.local") {
		t.Errorf("missing BUREAU_SERVER_NAME in output:\n%s", content)
	}
	if !strings.Contains(content, "BUREAU_FLEET=bureau/fleet/prod") {
		t.Errorf("missing BUREAU_FLEET in output:\n%s", content)
	}
}

func TestDoctorCommand_UnexpectedArg(t *testing.T) {
	command := doctorCommand()
	err := command.Execute([]string{"extra"})
	if err == nil {
		t.Fatal("expected error for unexpected argument")
	}
}

func TestDoctorCommand_DryRunRequiresFix(t *testing.T) {
	command := doctorCommand()
	err := command.Execute([]string{"--dry-run"})
	if err == nil {
		t.Fatal("expected error for --dry-run without --fix")
	}
	if !strings.Contains(err.Error(), "--dry-run requires --fix") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOSPermissionDeniedClassification(t *testing.T) {
	isOSPermissionDenied := func(err error) bool {
		return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
	}

	if !isOSPermissionDenied(syscall.EPERM) {
		t.Error("EPERM should be classified as permission denied")
	}
	if !isOSPermissionDenied(syscall.EACCES) {
		t.Error("EACCES should be classified as permission denied")
	}
	if isOSPermissionDenied(errors.New("some other error")) {
		t.Error("arbitrary error should not be classified as permission denied")
	}

	// Test wrapped errors.
	wrapped := &os.PathError{Op: "open", Path: "/etc/bureau", Err: syscall.EACCES}
	if !isOSPermissionDenied(wrapped) {
		t.Error("wrapped EACCES should be classified as permission denied")
	}
}

func TestCreateDirectory(t *testing.T) {
	// Get current user info for a directory we can actually create.
	currentUser, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	currentGroup, err := user.LookupGroupId(currentUser.Gid)
	if err != nil {
		t.Fatalf("lookup group: %v", err)
	}

	directory := directorySpec{
		path:  t.TempDir() + "/newdir",
		owner: currentUser.Username + ":" + currentGroup.Name,
		mode:  0750,
	}

	if err := createDirectory(directory); err != nil {
		t.Fatalf("createDirectory: %v", err)
	}

	info, err := os.Stat(directory.path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected directory")
	}
	if info.Mode().Perm() != 0750 {
		t.Errorf("mode = %04o, want 0750", info.Mode().Perm())
	}
}

func TestFixDirectoryOwnership_Recursive(t *testing.T) {
	// Create a directory with nested files and subdirectories, then verify
	// that fixDirectoryOwnership chowns everything recursively.
	currentUser, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	currentGroup, err := user.LookupGroupId(currentUser.Gid)
	if err != nil {
		t.Fatalf("lookup group: %v", err)
	}

	root := t.TempDir() + "/state"
	if err := os.MkdirAll(root+"/subdir", 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(root+"/session.json", []byte(`{"test":true}`), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(root+"/subdir/keypair.age", []byte("secret"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	uid, gid, err := resolveOwner(currentUser.Username + ":" + currentGroup.Name)
	if err != nil {
		t.Fatalf("resolveOwner: %v", err)
	}

	directory := directorySpec{
		path:  root,
		owner: currentUser.Username + ":" + currentGroup.Name,
		mode:  0700,
	}

	if err := fixDirectoryOwnership(directory, uid, gid); err != nil {
		t.Fatalf("fixDirectoryOwnership: %v", err)
	}

	// Verify all files were chowned by checking they're readable.
	// (They should be owned by current user, so always readable, but
	// this at least verifies the walk didn't error.)
	for _, path := range []string{root, root + "/session.json", root + "/subdir", root + "/subdir/keypair.age"} {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat %s after fix: %v", path, err)
			continue
		}
		stat := info.Sys().(*syscall.Stat_t)
		if stat.Uid != uid || stat.Gid != gid {
			t.Errorf("%s: owner %d:%d, expected %d:%d", path, stat.Uid, stat.Gid, uid, gid)
		}
	}

	// Verify file modes were NOT changed (only ownership changes).
	sessionInfo, _ := os.Stat(root + "/session.json")
	if sessionInfo.Mode().Perm() != 0600 {
		t.Errorf("session.json mode = %04o, want 0600 (should not be changed by fixDirectoryOwnership)", sessionInfo.Mode().Perm())
	}
}

func TestCheckSockets_NonexistentPath(t *testing.T) {
	// Socket checks should fail when sockets don't exist.
	// Can't easily test this without controlling /run/bureau, so verify
	// that the check function doesn't panic with missing paths.
	results := checkSockets(principal.OperatorsGroupName)

	for _, result := range results {
		switch result.Status {
		case doctor.StatusPass:
			// Sockets happen to exist on this machine.
		case doctor.StatusFail:
			// Sockets don't exist.
		case doctor.StatusWarn:
			// Parent directory not accessible (permission denied).
		default:
			t.Errorf("socket %q: unexpected status %s", result.Name, result.Status)
		}
	}
}

// --- Binary installation tests ---

func TestCheckBinaries_AllCurrent(t *testing.T) {
	sourceDir := t.TempDir()
	installDir := t.TempDir()

	savedInstallDir := binaryInstallDir
	defer func() { binaryInstallDir = savedInstallDir }()
	binaryInstallDir = installDir

	savedBinaries := expectedHostBinaries
	defer func() { expectedHostBinaries = savedBinaries }()
	expectedHostBinaries = []string{"test-binary"}

	// Create source binary.
	sourcePath := filepath.Join(sourceDir, "test-binary")
	if err := os.WriteFile(sourcePath, []byte("binary-content"), 0755); err != nil {
		t.Fatalf("write source: %v", err)
	}

	// Create install symlink pointing to source.
	installPath := filepath.Join(installDir, "test-binary")
	if err := os.Symlink(sourcePath, installPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	params := machineDoctorParams{BinaryDir: sourceDir}
	results := checkBinaries(params)

	found := false
	for _, result := range results {
		if result.Name == "test-binary installed" {
			found = true
			if result.Status != doctor.StatusPass {
				t.Errorf("expected PASS for matching binary, got %s: %s", result.Status, result.Message)
			}
		}
	}
	if !found {
		t.Error("expected 'test-binary installed' result")
	}
}

func TestCheckBinaries_MismatchedBinary(t *testing.T) {
	sourceDir := t.TempDir()
	installDir := t.TempDir()

	savedInstallDir := binaryInstallDir
	defer func() { binaryInstallDir = savedInstallDir }()
	binaryInstallDir = installDir

	savedBinaries := expectedHostBinaries
	defer func() { expectedHostBinaries = savedBinaries }()
	expectedHostBinaries = []string{"test-binary"}

	// Create source binary.
	sourcePath := filepath.Join(sourceDir, "test-binary")
	if err := os.WriteFile(sourcePath, []byte("new-version"), 0755); err != nil {
		t.Fatalf("write source: %v", err)
	}

	// Create install binary with different content (different file, not symlink).
	installPath := filepath.Join(installDir, "test-binary")
	if err := os.WriteFile(installPath, []byte("old-version"), 0755); err != nil {
		t.Fatalf("write installed: %v", err)
	}

	params := machineDoctorParams{BinaryDir: sourceDir}
	results := checkBinaries(params)

	found := false
	for _, result := range results {
		if result.Name == "test-binary installed" {
			found = true
			if result.Status != doctor.StatusFail {
				t.Errorf("expected FAIL for mismatched binary, got %s: %s", result.Status, result.Message)
			}
			if !result.HasFix() {
				t.Error("mismatched binary should have a fix")
			}
			if !result.Elevated {
				t.Error("binary install should be elevated")
			}
		}
	}
	if !found {
		t.Error("expected 'test-binary installed' result")
	}
}

func TestCheckBinaries_MissingSource(t *testing.T) {
	sourceDir := t.TempDir()
	installDir := t.TempDir()

	savedInstallDir := binaryInstallDir
	defer func() { binaryInstallDir = savedInstallDir }()
	binaryInstallDir = installDir

	savedBinaries := expectedHostBinaries
	defer func() { expectedHostBinaries = savedBinaries }()
	expectedHostBinaries = []string{"test-binary"}

	// Don't create the source binary.
	params := machineDoctorParams{BinaryDir: sourceDir}
	results := checkBinaries(params)

	found := false
	for _, result := range results {
		if result.Name == "test-binary installed" {
			found = true
			if result.Status != doctor.StatusSkip {
				t.Errorf("expected SKIP for missing source, got %s: %s", result.Status, result.Message)
			}
		}
	}
	if !found {
		t.Error("expected 'test-binary installed' result")
	}
}

func TestCheckBinaries_MissingInstalled(t *testing.T) {
	sourceDir := t.TempDir()
	installDir := t.TempDir()

	savedInstallDir := binaryInstallDir
	defer func() { binaryInstallDir = savedInstallDir }()
	binaryInstallDir = installDir

	savedBinaries := expectedHostBinaries
	defer func() { expectedHostBinaries = savedBinaries }()
	expectedHostBinaries = []string{"test-binary"}

	// Create source binary but no install symlink.
	sourcePath := filepath.Join(sourceDir, "test-binary")
	if err := os.WriteFile(sourcePath, []byte("binary-content"), 0755); err != nil {
		t.Fatalf("write source: %v", err)
	}

	params := machineDoctorParams{BinaryDir: sourceDir}
	results := checkBinaries(params)

	found := false
	for _, result := range results {
		if result.Name == "test-binary installed" {
			found = true
			if result.Status != doctor.StatusFail {
				t.Errorf("expected FAIL for missing installed binary, got %s: %s", result.Status, result.Message)
			}
			if !result.HasFix() {
				t.Error("missing installed binary should have a fix")
			}
			if !result.Elevated {
				t.Error("binary install should be elevated")
			}
		}
	}
	if !found {
		t.Error("expected 'test-binary installed' result")
	}
}

// --- /proc group reading tests ---

func TestReadProcGroups_OwnProcess(t *testing.T) {
	// Read our own process's groups — this should always work.
	groups, err := readProcGroups(os.Getpid())
	if err != nil {
		t.Fatalf("readProcGroups(self): %v", err)
	}

	// Every process has at least its primary group.
	if len(groups) == 0 {
		t.Fatal("readProcGroups returned empty group list")
	}

	// Our primary GID should be in the list.
	primaryGID := os.Getgid()
	found := false
	for _, gid := range groups {
		if gid == primaryGID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("primary GID %d not in groups %v", primaryGID, groups)
	}
}

func TestReadPPID_OwnProcess(t *testing.T) {
	// Our parent PID should match os.Getppid().
	ppid, err := readPPID(os.Getpid())
	if err != nil {
		t.Fatalf("readPPID(self): %v", err)
	}

	if ppid != os.Getppid() {
		t.Errorf("readPPID = %d, os.Getppid = %d", ppid, os.Getppid())
	}
}

func TestReadProcGroups_NonexistentPID(t *testing.T) {
	// PID 0 doesn't have a /proc entry on Linux.
	_, err := readProcGroups(0)
	if err == nil {
		t.Fatal("expected error for PID 0")
	}
}

func TestReadProcUID_OwnProcess(t *testing.T) {
	uid, err := readProcUID(os.Getpid())
	if err != nil {
		t.Fatalf("readProcUID(self): %v", err)
	}

	if uid != os.Getuid() {
		t.Errorf("readProcUID = %d, os.Getuid = %d", uid, os.Getuid())
	}
}

func TestCallerSessionHasGroup_FindsOwnGroup(t *testing.T) {
	// Simulate a sudo context: set SUDO_UID to our own UID so that
	// callerSessionHasGroup walks up and finds our parent process
	// (which shares our UID). Then check for our primary group,
	// which must be present.
	t.Setenv("SUDO_UID", strconv.Itoa(os.Getuid()))

	// Our parent should have our primary GID.
	primaryGID := os.Getgid()
	hasGroup, err := callerSessionHasGroup(primaryGID)
	if err != nil {
		// In some test environments (containers, bazel sandboxes), the
		// parent process may be owned by a different UID. Skip gracefully.
		t.Skipf("callerSessionHasGroup: %v", err)
	}

	if !hasGroup {
		t.Errorf("expected parent to have primary GID %d", primaryGID)
	}
}

func TestCallerSessionHasGroup_MissingGroup(t *testing.T) {
	// Simulate a sudo context and check for a GID that definitely
	// doesn't exist in the parent's groups.
	t.Setenv("SUDO_UID", strconv.Itoa(os.Getuid()))

	// GID 2147483647 is extremely unlikely to be a real group.
	hasGroup, err := callerSessionHasGroup(2147483647)
	if err != nil {
		t.Skipf("callerSessionHasGroup: %v", err)
	}

	if hasGroup {
		t.Error("expected false for nonexistent GID")
	}
}

func TestCallerSessionHasGroup_NoSudoUID(t *testing.T) {
	// Without SUDO_UID set, callerSessionHasGroup should error.
	t.Setenv("SUDO_UID", "")
	_, err := callerSessionHasGroup(0)
	if err == nil {
		t.Fatal("expected error without SUDO_UID")
	}
}

func TestAtomicSymlink(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	target := filepath.Join(directory, "target")

	if err := os.WriteFile(source, []byte("content"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Create initial symlink.
	if err := atomicSymlink(source, target); err != nil {
		t.Fatalf("atomicSymlink: %v", err)
	}

	resolved, err := os.Readlink(target)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if resolved != source {
		t.Errorf("target -> %s, want %s", resolved, source)
	}

	// Replace with a different source.
	source2 := filepath.Join(directory, "source2")
	if err := os.WriteFile(source2, []byte("content2"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := atomicSymlink(source2, target); err != nil {
		t.Fatalf("atomicSymlink (replace): %v", err)
	}

	resolved, err = os.Readlink(target)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if resolved != source2 {
		t.Errorf("target -> %s, want %s", resolved, source2)
	}
}
