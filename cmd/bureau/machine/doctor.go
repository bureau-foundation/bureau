// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/cli/doctor"
	"github.com/bureau-foundation/bureau/lib/content"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

// machineDoctorParams holds the parameters for the machine doctor command.
type machineDoctorParams struct {
	cli.JSONOutput
	Fix       bool   `json:"fix"         flag:"fix"        desc:"automatically repair fixable issues"`
	DryRun    bool   `json:"dry_run"     flag:"dry-run"    desc:"preview repairs without executing (requires --fix)"`
	BinaryDir string `json:"binary_dir,omitempty" flag:"binary-dir" desc:"directory containing Bureau binaries to install (default: auto-detect from running binary)"`

	// Bootstrap flags — allow running doctor before machine.conf exists.
	// When provided, these override the values in machine.conf.
	Homeserver  string `json:"homeserver,omitempty"    flag:"homeserver"    desc:"Matrix homeserver URL (overrides machine.conf)"`
	MachineName string `json:"machine_name,omitempty"  flag:"machine-name"  desc:"machine localpart (overrides machine.conf)"`
	ServerName  string `json:"server_name,omitempty"   flag:"server-name"   desc:"Matrix server name (overrides machine.conf)"`
	Fleet       string `json:"fleet,omitempty"         flag:"fleet"         desc:"fleet localpart (overrides machine.conf)"`

	// System identity flags — configurable user and group names for
	// multi-instance deployments (e.g., separate test and prod fleets
	// on the same machine with different system users).
	SystemUser     string `json:"system_user,omitempty"     flag:"system-user"     desc:"Unix user for Bureau processes (default: bureau)"`
	OperatorsGroup string `json:"operators_group,omitempty" flag:"operators-group"  desc:"Unix group for operator socket access (default: bureau-operators)"`
}

// machineConfig holds validated configuration from /etc/bureau/machine.conf.
type machineConfig struct {
	HomeserverURL string
	MachineName   string
	ServerName    string
	Fleet         string
}

// machineConfPath is the canonical location for machine configuration.
// Variable rather than constant to allow test overrides.
var machineConfPath = "/etc/bureau/machine.conf"

// machineConfRequiredKeys are the keys that must be present in machine.conf.
var machineConfRequiredKeys = []string{
	"BUREAU_HOMESERVER_URL",
	"BUREAU_MACHINE_NAME",
	"BUREAU_SERVER_NAME",
	"BUREAU_FLEET",
}

// directorySpec describes a directory that should exist with specific ownership
// and permissions.
type directorySpec struct {
	path  string
	owner string // "user:group"
	mode  fs.FileMode
}

// buildExpectedDirectories returns the directory specs for the given
// system user and operators group names. Separating this from a
// package-level variable allows multi-instance deployments with
// different system identities (e.g., "bureau-test" user for a test
// fleet alongside "bureau" for production).
func buildExpectedDirectories(systemUser, operatorsGroup string) []directorySpec {
	return []directorySpec{
		{"/etc/bureau", "root:root", 0755},
		{"/run/bureau", systemUser + ":" + operatorsGroup, 0750},
		{"/var/lib/bureau", systemUser + ":" + systemUser, 0700},
		{"/var/bureau/workspace", systemUser + ":" + systemUser, 0755},
		{"/var/bureau/cache", systemUser + ":" + systemUser, 0755},
	}
}

// systemdUnitSpec describes a systemd unit file to check.
type systemdUnitSpec struct {
	name            string // e.g., "bureau-launcher.service"
	expectedContent func() string
	installPath     string // e.g., "/etc/systemd/system/bureau-launcher.service"
}

var expectedUnits = []systemdUnitSpec{
	{
		name:            "bureau-launcher.service",
		expectedContent: content.LauncherServiceUnit,
		installPath:     "/etc/systemd/system/bureau-launcher.service",
	},
	{
		name:            "bureau-daemon.service",
		expectedContent: content.DaemonServiceUnit,
		installPath:     "/etc/systemd/system/bureau-daemon.service",
	},
}

// binaryInstallDir is where Bureau host binaries are installed.
// Variable rather than constant to allow test overrides.
var binaryInstallDir = "/usr/local/bin"

// expectedHostBinaries lists Bureau binaries that should be installed on the
// host machine. This should match the hostBinaries list in flake.nix.
var expectedHostBinaries = []string{
	"bureau",
	"bureau-launcher",
	"bureau-daemon",
	"bureau-proxy",
	"bureau-log-relay",
	"bureau-observe-relay",
	"bureau-bridge",
	"bureau-sandbox",
	"bureau-credentials",
	"bureau-agent-service",
	"bureau-artifact-service",
	"bureau-ticket-service",
}

func doctorCommand() *cli.Command {
	var params machineDoctorParams

	return &cli.Command{
		Name:    "doctor",
		Summary: "Check and repair Bureau machine infrastructure",
		Description: `Validate the local machine's Bureau infrastructure: system user and group,
directory ownership, configuration, binary installation, systemd units, socket
connectivity, and Matrix session health.

Runs a series of checks and reports pass/fail/warn for each. Exits with
code 1 if any check fails.

Use --fix to automatically repair fixable issues. Most fixes require root
privileges; the doctor groups these and suggests re-running with sudo.

Use --fix --dry-run to preview what would be repaired without making changes.

Bootstrap flags (--homeserver, --machine-name, --server-name, --fleet) allow
running doctor on a fresh machine before machine.conf exists. With --fix,
doctor creates the configuration file from these values.

Use --json for machine-readable output suitable for monitoring or CI.`,
		Usage: "bureau machine doctor [flags]",
		Examples: []cli.Example{
			{
				Description: "Check local machine health",
				Command:     "bureau machine doctor",
			},
			{
				Description: "Bootstrap and repair a fresh machine",
				Command:     "sudo bureau machine doctor --fix --homeserver http://matrix:6167 --machine-name bureau/fleet/prod/machine/worker-01 --server-name bureau.local --fleet bureau/fleet/prod",
			},
			{
				Description: "Preview repairs without executing",
				Command:     "sudo bureau machine doctor --fix --dry-run",
			},
			{
				Description: "Machine-readable output",
				Command:     "bureau machine doctor --json",
			},
		},
		Annotations:    cli.ReadOnly(),
		Output:         func() any { return &doctor.JSONOutput{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/machine/doctor"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}
			if params.DryRun && !params.Fix {
				return cli.Validation("--dry-run requires --fix")
			}

			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()

			return runMachineDoctor(ctx, params, logger)
		},
	}
}

func runMachineDoctor(ctx context.Context, params machineDoctorParams, logger *slog.Logger) error {
	const maxFixIterations = 5
	repairedNames := make(map[string]bool)
	var aggregateOutcome doctor.Outcome
	var results []doctor.Result

	isOSPermissionDenied := func(err error) bool {
		return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
	}

	for range maxFixIterations {
		results = checkMachine(ctx, params, logger)

		if !params.Fix {
			break
		}

		for _, result := range results {
			if result.Status == doctor.StatusFail {
				repairedNames[result.Name] = true
			}
		}

		outcome := doctor.ExecuteFixes(ctx, results, params.DryRun, isOSPermissionDenied)
		if outcome.PermissionDenied {
			aggregateOutcome.PermissionDenied = true
		}
		aggregateOutcome.ElevatedSkipped += outcome.ElevatedSkipped
		if outcome.FixedCount == 0 || params.DryRun {
			break
		}

		// Allow services to finish starting before re-checking. Fixes
		// like "systemctl start" return immediately while the service
		// initializes in the background; without this delay, the next
		// iteration would see "activating" or missing sockets.
		settleTimer := time.NewTimer(3 * time.Second)
		select {
		case <-settleTimer.C:
		case <-ctx.Done():
			settleTimer.Stop()
			return ctx.Err()
		}
	}

	doctor.MarkRepaired(results, repairedNames)

	if done, err := params.EmitJSON(doctor.BuildJSON(results, params.DryRun, aggregateOutcome)); done {
		if err != nil {
			return err
		}
		for _, result := range results {
			if result.Status == doctor.StatusFail {
				return &cli.ExitError{Code: 1}
			}
		}
		return nil
	}
	return doctor.PrintChecklist(results, params.Fix, params.DryRun, aggregateOutcome)
}

// checkMachine runs all machine health checks and returns results.
func checkMachine(ctx context.Context, params machineDoctorParams, logger *slog.Logger) []doctor.Result {
	var results []doctor.Result

	// Resolve system identity: CLI flags take priority, then machine.conf,
	// then the defaults. This lets multi-instance deployments persist
	// their identity in machine.conf without repeating flags every time.
	systemUser := params.SystemUser
	operatorsGroup := params.OperatorsGroup
	if systemUser == "" || operatorsGroup == "" {
		if credentials, err := cli.ReadCredentialFile(machineConfPath); err == nil {
			if systemUser == "" {
				systemUser = credentials["BUREAU_SYSTEM_USER"]
			}
			if operatorsGroup == "" {
				operatorsGroup = credentials["BUREAU_OPERATORS_GROUP"]
			}
		}
	}
	if systemUser == "" {
		systemUser = principal.SystemUserName
	}
	if operatorsGroup == "" {
		operatorsGroup = principal.OperatorsGroupName
	}

	// Section 1: System prerequisites.
	results = append(results, checkSystemUser(systemUser, operatorsGroup)...)
	bureauUserExists := results[0].Status == doctor.StatusPass

	// Section 2: Directories (skip if system user doesn't exist — ownership
	// checks need the UID/GID).
	directories := buildExpectedDirectories(systemUser, operatorsGroup)
	if bureauUserExists {
		results = append(results, checkDirectories(directories)...)
	} else {
		for _, directory := range directories {
			results = append(results, doctor.Skip(directory.path, fmt.Sprintf("skipped: %s user does not exist", systemUser)))
		}
	}

	// Section 3: Configuration.
	config := readMachineConfig(params)
	results = append(results, checkMachineConfig(params, config)...)

	// Section 4: Binary installation. Must run before unit/service checks
	// so that any service restarts triggered by unit updates use the new
	// binaries, not stale ones.
	results = append(results, checkBinaries(params)...)

	// Section 5: Systemd units.
	results = append(results, checkSystemdUnits()...)

	// Section 6: Sockets (skip if services are not running).
	servicesRunning := areServicesRunning()
	if servicesRunning {
		results = append(results, checkSockets(operatorsGroup)...)
	} else {
		results = append(results, doctor.Skip("launcher.sock", "skipped: services not running"))
		results = append(results, doctor.Skip("observe.sock", "skipped: services not running"))
	}

	// Section 7: Matrix connectivity (skip if no homeserver URL).
	if config != nil && config.HomeserverURL != "" {
		results = append(results, checkMatrixConnectivity(ctx, config, logger)...)
	} else {
		results = append(results, doctor.Skip("homeserver reachable", "skipped: no homeserver URL configured"))
		results = append(results, doctor.Skip("machine session", "skipped: no homeserver URL configured"))
		results = append(results, doctor.Skip("session valid", "skipped: no homeserver URL configured"))
	}

	return results
}

// --- Section 1: System prerequisites ---

func checkSystemUser(systemUser, operatorsGroupName string) []doctor.Result {
	var results []doctor.Result

	// Check system user.
	bureauUser, err := user.Lookup(systemUser)
	if err != nil {
		results = append(results, doctor.FailElevated(
			systemUser+" user",
			fmt.Sprintf("user %q does not exist", systemUser),
			fmt.Sprintf("useradd --system --no-create-home --shell /usr/sbin/nologin %s", systemUser),
			func(ctx context.Context) error {
				return runCommand("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", systemUser)
			},
		))
	} else {
		results = append(results, doctor.Pass(systemUser+" user", fmt.Sprintf("uid=%s", bureauUser.Uid)))
	}

	// Check operators group.
	operatorsGroup, err := user.LookupGroup(operatorsGroupName)
	if err != nil {
		results = append(results, doctor.FailElevated(
			operatorsGroupName+" group",
			fmt.Sprintf("group %q does not exist", operatorsGroupName),
			fmt.Sprintf("groupadd --system %s && usermod -aG %s %s", operatorsGroupName, operatorsGroupName, systemUser),
			func(ctx context.Context) error {
				if err := runCommand("groupadd", "--system", operatorsGroupName); err != nil {
					return err
				}
				return runCommand("usermod", "-aG", operatorsGroupName, systemUser)
			},
		))
	} else {
		results = append(results, doctor.Pass(operatorsGroupName+" group", fmt.Sprintf("gid=%s", operatorsGroup.Gid)))
	}

	// Check operator group membership. When running as root via sudo, check
	// the invoking user (SUDO_USER) rather than root itself — root can do
	// anything but the point is to ensure the operator can work without sudo.
	operatorUsername := ""
	if doctor.IsRoot() {
		operatorUsername = os.Getenv("SUDO_USER")
	}
	if operatorUsername == "" && !doctor.IsRoot() {
		if currentUser, err := user.Current(); err == nil {
			operatorUsername = currentUser.Username
		} else {
			results = append(results, doctor.Fail("operator group membership", fmt.Sprintf("cannot determine current user: %v", err)))
		}
	}

	if operatorUsername != "" {
		operatorUser, err := user.Lookup(operatorUsername)
		if err != nil {
			results = append(results, doctor.Fail("operator group membership", fmt.Sprintf("cannot look up user %s: %v", operatorUsername, err)))
		} else {
			groupIDs, err := operatorUser.GroupIds()
			if err != nil {
				results = append(results, doctor.Fail("operator group membership", fmt.Sprintf("cannot read group membership: %v", err)))
			} else {
				inGroup := false
				if resolvedGroup, lookupErr := user.LookupGroup(operatorsGroupName); lookupErr == nil {
					for _, groupID := range groupIDs {
						if groupID == resolvedGroup.Gid {
							inGroup = true
							break
						}
					}
				}
				if inGroup {
					results = append(results, doctor.Pass("operator group membership", fmt.Sprintf("%s is in %s", operatorUsername, operatorsGroupName)))
				} else {
					results = append(results, doctor.FailElevated(
						"operator group membership",
						fmt.Sprintf("%s is not in %s group", operatorUsername, operatorsGroupName),
						fmt.Sprintf("usermod -aG %s %s", operatorsGroupName, operatorUsername),
						func(ctx context.Context) error {
							return runCommand("usermod", "-aG", operatorsGroupName, operatorUsername)
						},
					))
				}
			}
		}
	} else if doctor.IsRoot() {
		// Running as root without SUDO_USER (e.g., root shell). No operator
		// to check, but the check itself is fine.
		results = append(results, doctor.Pass("operator group membership", "running as root"))
	}

	return results
}

// --- Section 2: Directories ---

func checkDirectories(directories []directorySpec) []doctor.Result {
	var results []doctor.Result

	for _, directory := range directories {
		directory := directory // capture for closure
		info, err := os.Stat(directory.path)
		if err != nil {
			if os.IsNotExist(err) {
				results = append(results, doctor.FailElevated(
					directory.path,
					"does not exist",
					fmt.Sprintf("mkdir -p %s && chown %s %s && chmod %04o %s",
						directory.path, directory.owner, directory.path, directory.mode, directory.path),
					func(ctx context.Context) error {
						return createDirectory(directory)
					},
				))
				continue
			}
			results = append(results, doctor.Fail(directory.path, fmt.Sprintf("cannot stat: %v", err)))
			continue
		}

		if !info.IsDir() {
			results = append(results, doctor.Fail(directory.path, "exists but is not a directory"))
			continue
		}

		// Check mode.
		actualMode := info.Mode().Perm()
		modeOK := actualMode == directory.mode

		// Check ownership via syscall.Stat_t.
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			results = append(results, doctor.Fail(directory.path, "cannot read ownership (non-Linux?)"))
			continue
		}

		expectedUID, expectedGID, ownerParseErr := resolveOwner(directory.owner)
		if ownerParseErr != nil {
			results = append(results, doctor.Fail(directory.path, fmt.Sprintf("cannot resolve owner %q: %v", directory.owner, ownerParseErr)))
			continue
		}

		ownerOK := stat.Uid == expectedUID && stat.Gid == expectedGID

		if modeOK && ownerOK {
			results = append(results, doctor.Pass(directory.path, fmt.Sprintf("%s %04o", directory.owner, directory.mode)))
		} else {
			var issues []string
			if !modeOK {
				issues = append(issues, fmt.Sprintf("mode %04o, expected %04o", actualMode, directory.mode))
			}
			if !ownerOK {
				issues = append(issues, fmt.Sprintf("owner %d:%d, expected %s (%d:%d)",
					stat.Uid, stat.Gid, directory.owner, expectedUID, expectedGID))
			}
			results = append(results, doctor.FailElevated(
				directory.path,
				strings.Join(issues, "; "),
				fmt.Sprintf("chown -R %s %s && chmod %04o %s", directory.owner, directory.path, directory.mode, directory.path),
				func(ctx context.Context) error {
					return fixDirectoryOwnership(directory, expectedUID, expectedGID)
				},
			))
		}
	}

	return results
}

// --- Section 3: Configuration ---

// readMachineConfig reads machine.conf and merges with flag overrides.
// Returns nil if machine.conf doesn't exist and no flags are provided.
func readMachineConfig(params machineDoctorParams) *machineConfig {
	config := &machineConfig{
		HomeserverURL: params.Homeserver,
		MachineName:   params.MachineName,
		ServerName:    params.ServerName,
		Fleet:         params.Fleet,
	}

	credentials, err := cli.ReadCredentialFile(machineConfPath)
	if err == nil {
		if config.HomeserverURL == "" {
			config.HomeserverURL = credentials["BUREAU_HOMESERVER_URL"]
		}
		if config.MachineName == "" {
			config.MachineName = credentials["BUREAU_MACHINE_NAME"]
		}
		if config.ServerName == "" {
			config.ServerName = credentials["BUREAU_SERVER_NAME"]
		}
		if config.Fleet == "" {
			config.Fleet = credentials["BUREAU_FLEET"]
		}
	}

	// If we have no values at all, return nil.
	if config.HomeserverURL == "" && config.MachineName == "" &&
		config.ServerName == "" && config.Fleet == "" {
		return nil
	}

	return config
}

func checkMachineConfig(params machineDoctorParams, config *machineConfig) []doctor.Result {
	// Check machine.conf file exists.
	_, err := os.Stat(machineConfPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Can we fix it? Only if all bootstrap flags are provided.
			if params.Homeserver != "" && params.MachineName != "" &&
				params.ServerName != "" && params.Fleet != "" {
				return []doctor.Result{doctor.FailElevated(
					"machine.conf",
					fmt.Sprintf("%s does not exist", machineConfPath),
					fmt.Sprintf("write %s with bootstrap flags", machineConfPath),
					func(ctx context.Context) error {
						return writeMachineConf(params)
					},
				)}
			}
			return []doctor.Result{doctor.Fail(
				"machine.conf",
				fmt.Sprintf("%s does not exist (use --homeserver, --machine-name, --server-name, --fleet with --fix to create)", machineConfPath),
			)}
		}
		return []doctor.Result{doctor.Fail("machine.conf", fmt.Sprintf("cannot stat: %v", err))}
	}

	// File exists — check it has all required keys.
	credentials, err := cli.ReadCredentialFile(machineConfPath)
	if err != nil {
		return []doctor.Result{doctor.Fail("machine.conf", fmt.Sprintf("cannot read: %v", err))}
	}

	var missingKeys []string
	for _, key := range machineConfRequiredKeys {
		if credentials[key] == "" {
			missingKeys = append(missingKeys, key)
		}
	}

	if len(missingKeys) > 0 {
		// Can fix if bootstrap flags provide the missing values.
		canFix := true
		for _, key := range missingKeys {
			switch key {
			case "BUREAU_HOMESERVER_URL":
				if params.Homeserver == "" {
					canFix = false
				}
			case "BUREAU_MACHINE_NAME":
				if params.MachineName == "" {
					canFix = false
				}
			case "BUREAU_SERVER_NAME":
				if params.ServerName == "" {
					canFix = false
				}
			case "BUREAU_FLEET":
				if params.Fleet == "" {
					canFix = false
				}
			}
		}

		message := fmt.Sprintf("missing keys: %s", strings.Join(missingKeys, ", "))
		if canFix {
			return []doctor.Result{doctor.FailElevated(
				"machine.conf",
				message,
				fmt.Sprintf("update %s with bootstrap flags", machineConfPath),
				func(ctx context.Context) error {
					return writeMachineConf(params)
				},
			)}
		}
		return []doctor.Result{doctor.Fail(
			"machine.conf",
			message+" (provide values via --homeserver, --machine-name, --server-name, --fleet)",
		)}
	}

	return []doctor.Result{doctor.Pass("machine.conf", fmt.Sprintf("%s has all required keys", machineConfPath))}
}

// --- Section 4: Binary installation ---

func checkBinaries(params machineDoctorParams) []doctor.Result {
	var results []doctor.Result

	// Determine the source directory for Bureau binaries. When --binary-dir
	// is set, use that directly. Otherwise, resolve the invocation path's
	// DIRECTORY through symlinks. We must NOT resolve the binary file itself:
	// in Nix buildEnv packages, the bin/ directory contains symlinks to
	// individual package store paths. Resolving the file follows those
	// symlinks into a single-binary package directory; resolving only the
	// directory stays at the buildEnv level where all siblings are visible.
	sourceDir := params.BinaryDir
	if sourceDir == "" {
		invocationPath := os.Args[0]
		if !filepath.IsAbs(invocationPath) {
			if strings.Contains(invocationPath, string(filepath.Separator)) {
				// Relative path with directory (e.g., "result/bin/bureau").
				absPath, err := filepath.Abs(invocationPath)
				if err != nil {
					results = append(results, doctor.Fail("binary source",
						fmt.Sprintf("cannot resolve %s: %v", invocationPath, err)))
					return results
				}
				invocationPath = absPath
			} else {
				// Bare binary name (e.g., "bureau"). Resolve via PATH.
				lookedUp, err := exec.LookPath(invocationPath)
				if err != nil {
					results = append(results, doctor.Fail("binary source",
						fmt.Sprintf("cannot find %s on PATH: %v", invocationPath, err)))
					return results
				}
				invocationPath = lookedUp
			}
		}

		invocationDir := filepath.Dir(invocationPath)
		resolved, err := filepath.EvalSymlinks(invocationDir)
		if err != nil {
			results = append(results, doctor.Fail("binary source",
				fmt.Sprintf("cannot resolve directory %s: %v", invocationDir, err)))
			return results
		}
		sourceDir = resolved
	}

	for _, binaryName := range expectedHostBinaries {
		binaryName := binaryName // capture for closure
		sourcePath := filepath.Join(sourceDir, binaryName)
		installPath := filepath.Join(binaryInstallDir, binaryName)

		// Verify source binary exists.
		sourceInfo, sourceErr := os.Stat(sourcePath)
		if sourceErr != nil {
			// Source not found. This happens when running from a partial
			// build (e.g., bazel-built CLI without the full host env).
			results = append(results, doctor.Skip(binaryName+" installed",
				fmt.Sprintf("source not found at %s", sourcePath)))
			continue
		}

		// Compare installed binary against source.
		installInfo, installErr := os.Stat(installPath)
		if installErr != nil {
			results = append(results, doctor.FailElevated(
				binaryName+" installed",
				fmt.Sprintf("%s not found", installPath),
				fmt.Sprintf("ln -sf %s %s", sourcePath, installPath),
				func(ctx context.Context) error {
					return atomicSymlink(sourcePath, installPath)
				},
			))
			continue
		}

		// Both exist. Check if they refer to the same underlying file.
		if os.SameFile(sourceInfo, installInfo) {
			results = append(results, doctor.Pass(binaryName+" installed",
				fmt.Sprintf("%s current", installPath)))
		} else {
			results = append(results, doctor.FailElevated(
				binaryName+" installed",
				fmt.Sprintf("%s does not match source at %s", installPath, sourcePath),
				fmt.Sprintf("ln -sf %s %s", sourcePath, installPath),
				func(ctx context.Context) error {
					return atomicSymlink(sourcePath, installPath)
				},
			))
		}
	}

	// Check if running services use the currently-installed binaries.
	// A service may be running an old binary even after symlinks are
	// updated — it needs a restart to pick up the new version.
	results = append(results, checkServicesCurrentBinary()...)

	return results
}

// checkServicesCurrentBinary verifies that running Bureau services are
// executing the same binary that is currently installed. This catches the
// case where symlinks were updated but services weren't restarted.
func checkServicesCurrentBinary() []doctor.Result {
	if !isUnitActive("bureau-launcher.service") {
		return nil
	}

	pid := getServiceMainPID("bureau-launcher.service")
	if pid <= 0 {
		return nil
	}

	exePath, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		if errors.Is(err, syscall.EACCES) {
			return []doctor.Result{doctor.Skip("services running current binaries",
				"cannot read process binary (requires root)")}
		}
		return []doctor.Result{doctor.Warn("services running current binaries",
			fmt.Sprintf("cannot read /proc/%d/exe: %v", pid, err))}
	}

	installedPath, err := filepath.EvalSymlinks(filepath.Join(binaryInstallDir, "bureau-launcher"))
	if err != nil {
		return []doctor.Result{doctor.Fail("services running current binaries",
			fmt.Sprintf("cannot resolve installed binary: %v", err))}
	}

	// /proc/PID/exe may have " (deleted)" suffix if the binary file was
	// replaced or the store path was garbage-collected.
	exePath = strings.TrimSuffix(exePath, " (deleted)")

	if exePath == installedPath {
		return []doctor.Result{doctor.Pass("services running current binaries",
			fmt.Sprintf("launcher PID %d matches installed binary", pid))}
	}

	return []doctor.Result{doctor.FailElevated(
		"services running current binaries",
		fmt.Sprintf("launcher running %s, installed %s", filepath.Base(exePath), filepath.Base(installedPath)),
		"systemctl restart bureau-launcher",
		func(ctx context.Context) error {
			return runCommand("systemctl", "restart", "bureau-launcher.service")
		},
	)}
}

// --- Section 5: Systemd units ---

func checkSystemdUnits() []doctor.Result {
	var results []doctor.Result

	for _, unit := range expectedUnits {
		unit := unit // capture for closure

		// Check unit file installed with correct content.
		installedContent, err := os.ReadFile(unit.installPath)
		if err != nil {
			if os.IsNotExist(err) {
				results = append(results, doctor.FailElevated(
					unit.name+" installed",
					fmt.Sprintf("%s not found", unit.installPath),
					fmt.Sprintf("write %s", unit.installPath),
					func(ctx context.Context) error {
						if err := os.WriteFile(unit.installPath, []byte(unit.expectedContent()), 0644); err != nil {
							return err
						}
						return runCommand("systemctl", "daemon-reload")
					},
				))
			} else {
				results = append(results, doctor.Fail(unit.name+" installed", fmt.Sprintf("cannot read %s: %v", unit.installPath, err)))
			}
		} else {
			expected := unit.expectedContent()
			if string(installedContent) == expected {
				results = append(results, doctor.Pass(unit.name+" installed", "content matches"))
			} else {
				// Unit content changed. Write the new file, reload systemd,
				// and restart the service if it's already running — a running
				// service with a stale unit file won't pick up changes until
				// restarted.
				wasRunning := isUnitActive(unit.name)
				results = append(results, doctor.FailElevated(
					unit.name+" installed",
					"installed content differs from expected",
					fmt.Sprintf("update %s and restart if running", unit.installPath),
					func(ctx context.Context) error {
						if err := os.WriteFile(unit.installPath, []byte(unit.expectedContent()), 0644); err != nil {
							return err
						}
						if err := runCommand("systemctl", "daemon-reload"); err != nil {
							return err
						}
						if wasRunning {
							return runCommand("systemctl", "restart", unit.name)
						}
						return nil
					},
				))
			}
		}

		// Check unit enabled.
		enabledOutput, enabledErr := exec.Command("systemctl", "is-enabled", unit.name).Output()
		enabledStatus := strings.TrimSpace(string(enabledOutput))
		if enabledErr != nil || enabledStatus != "enabled" {
			results = append(results, doctor.FailElevated(
				unit.name+" enabled",
				fmt.Sprintf("status: %s", enabledStatus),
				fmt.Sprintf("systemctl enable %s", unit.name),
				func(ctx context.Context) error {
					return runCommand("systemctl", "enable", unit.name)
				},
			))
		} else {
			results = append(results, doctor.Pass(unit.name+" enabled", "enabled"))
		}

		// Check unit active.
		activeOutput, activeErr := exec.Command("systemctl", "is-active", unit.name).Output()
		activeStatus := strings.TrimSpace(string(activeOutput))
		if activeErr != nil || (activeStatus != "active" && activeStatus != "activating") {
			// Starting the launcher starts the daemon via Requires=.
			results = append(results, doctor.FailElevated(
				unit.name+" active",
				fmt.Sprintf("status: %s", activeStatus),
				"systemctl start bureau-launcher",
				func(ctx context.Context) error {
					return runCommand("systemctl", "start", "bureau-launcher")
				},
			))
		} else {
			results = append(results, doctor.Pass(unit.name+" active", "running"))
		}
	}

	return results
}

// --- Section 6: Sockets ---

// isUnitActive returns whether a specific systemd unit is running.
func isUnitActive(unitName string) bool {
	output, err := exec.Command("systemctl", "is-active", unitName).Output()
	if err != nil {
		return false
	}
	status := strings.TrimSpace(string(output))
	return status == "active" || status == "activating"
}

// areServicesRunning checks if the launcher is active without producing a
// doctor result. Used to decide whether to run socket checks.
func areServicesRunning() bool {
	output, err := exec.Command("systemctl", "is-active", "bureau-launcher").Output()
	if err != nil {
		return false
	}
	status := strings.TrimSpace(string(output))
	return status == "active" || status == "activating"
}

func checkSockets(operatorsGroupName string) []doctor.Result {
	var results []doctor.Result

	operatorsGID := principal.LookupOperatorsGID(operatorsGroupName)

	sockets := []struct {
		name string
		path string
	}{
		{"launcher.sock", "/run/bureau/launcher.sock"},
		{"observe.sock", "/run/bureau/observe.sock"},
	}

	for _, socket := range sockets {
		info, err := os.Stat(socket.path)
		if err != nil {
			results = append(results, doctor.Fail(socket.name, fmt.Sprintf("%s: %v", socket.path, err)))
			continue
		}

		if info.Mode().Type()&os.ModeSocket == 0 {
			results = append(results, doctor.Fail(socket.name, fmt.Sprintf("%s exists but is not a socket", socket.path)))
			continue
		}

		// Check socket group and mode directly. Relying on connect()
		// is insufficient because root can connect to anything — we
		// need to verify that operators in bureau-operators can connect.
		socketPath := socket.path
		if operatorsGID >= 0 {
			stat, statErr := statOwnership(socketPath)
			if statErr != nil {
				results = append(results, doctor.Fail(socket.name, fmt.Sprintf("cannot stat ownership: %v", statErr)))
				continue
			}

			wrongGroup := int(stat.Gid) != operatorsGID
			wrongMode := info.Mode().Perm()&0060 != 0060 // group read+write required
			if wrongGroup || wrongMode {
				gid := operatorsGID
				detail := fmt.Sprintf("group=%d (want %d), mode=%04o (want 0660)",
					stat.Gid, operatorsGID, info.Mode().Perm())
				results = append(results, doctor.FailElevated(
					socket.name,
					detail,
					fmt.Sprintf("chgrp %s %s && chmod 0660 %s", operatorsGroupName, socketPath, socketPath),
					func(ctx context.Context) error {
						if err := principal.SetOperatorGroupOwnership(socketPath, gid); err != nil {
							return fmt.Errorf("chgrp %s: %w", socketPath, err)
						}
						return os.Chmod(socketPath, 0660)
					},
				))
				continue
			}
		}

		// Verify the service is actually listening.
		conn, err := net.DialTimeout("unix", socket.path, 2*time.Second)
		if err != nil {
			results = append(results, doctor.Fail(socket.name, fmt.Sprintf("cannot connect: %v", err)))
			continue
		}
		conn.Close()

		// When running as root via sudo, root can connect to any socket
		// regardless of permissions. Check whether the operator's login
		// session actually has the bureau-operators group. If the user was
		// just added to the group (by this same doctor run), their session
		// won't have it until they re-login. In that case, grant immediate
		// access via POSIX ACL so they can use the system right now.
		if doctor.IsRoot() {
			sudoUser := os.Getenv("SUDO_USER")
			if sudoUser != "" && operatorsGID >= 0 {
				sessionHasGroup, groupCheckErr := callerSessionHasGroup(operatorsGID)
				if groupCheckErr != nil {
					// Can't determine session groups — fall through to
					// basic pass. This shouldn't happen in practice (we're
					// root, /proc is readable), but don't block on it.
					results = append(results, doctor.Pass(socket.name, "connectable"))
					continue
				}

				if !sessionHasGroup {
					// The operator's session lacks bureau-operators. The
					// socket's group ownership is correct (long-term model),
					// but the operator can't connect until re-login. Grant
					// immediate per-user access via POSIX ACL.
					socketPath := socket.path
					results = append(results, doctor.FailElevated(
						socket.name,
						fmt.Sprintf("connectable, but %s session lacks %s group (re-login needed without fix)", sudoUser, operatorsGroupName),
						fmt.Sprintf("setfacl -m u:%s:rw %s", sudoUser, socketPath),
						func(ctx context.Context) error {
							return runCommand("setfacl", "-m", fmt.Sprintf("u:%s:rw", sudoUser), socketPath)
						},
					))
					continue
				}

				results = append(results, doctor.Pass(socket.name,
					fmt.Sprintf("connectable (verified %s has %s)", sudoUser, operatorsGroupName)))
				continue
			}
		}

		results = append(results, doctor.Pass(socket.name, "connectable"))
	}

	return results
}

// --- Section 7: Matrix connectivity ---

func checkMatrixConnectivity(ctx context.Context, config *machineConfig, logger *slog.Logger) []doctor.Result {
	var results []doctor.Result

	// Check homeserver reachable.
	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: config.HomeserverURL,
	})
	if err != nil {
		results = append(results, doctor.Fail("homeserver reachable", fmt.Sprintf("cannot create client: %v", err)))
		results = append(results, doctor.Skip("machine session", "skipped: homeserver unreachable"))
		results = append(results, doctor.Skip("session valid", "skipped: homeserver unreachable"))
		return results
	}

	versions, err := client.ServerVersions(ctx)
	if err != nil {
		results = append(results, doctor.Fail("homeserver reachable", fmt.Sprintf("unreachable: %v", err)))
		results = append(results, doctor.Skip("machine session", "skipped: homeserver unreachable"))
		results = append(results, doctor.Skip("session valid", "skipped: homeserver unreachable"))
		return results
	}
	results = append(results, doctor.Pass("homeserver reachable", fmt.Sprintf("%s versions: %s", config.HomeserverURL, strings.Join(versions.Versions, ", "))))

	// Check machine session file exists. The session file lives in
	// /var/lib/bureau (bureau:bureau 0700), so non-root non-bureau users
	// cannot read it — that's expected, not a failure.
	sessionData, err := readLauncherSession()
	if err != nil {
		if errors.Is(err, syscall.EACCES) {
			results = append(results, doctor.Warn("machine session", "permission denied (only bureau user and root can read session)"))
			results = append(results, doctor.Skip("session valid", "skipped: cannot read session"))
			return results
		}
		results = append(results, doctor.Fail("machine session", fmt.Sprintf("cannot read launcher session: %v", err)))
		results = append(results, doctor.Skip("session valid", "skipped: no session file"))
		return results
	}

	if sessionData.UserID.IsZero() || sessionData.AccessToken == "" {
		results = append(results, doctor.Fail("machine session", "session file missing user_id or access_token"))
		results = append(results, doctor.Skip("session valid", "skipped: incomplete session"))
		return results
	}
	results = append(results, doctor.Pass("machine session", fmt.Sprintf("user_id=%s", sessionData.UserID)))

	// Check session is valid via WhoAmI.
	session, err := client.SessionFromToken(sessionData.UserID, sessionData.AccessToken)
	if err != nil {
		results = append(results, doctor.Fail("session valid", fmt.Sprintf("cannot create session: %v", err)))
		return results
	}
	defer session.Close()

	whoami, err := session.WhoAmI(ctx)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeUnknownToken) {
			results = append(results, doctor.Fail("session valid", "access token is invalid or expired"))
		} else {
			results = append(results, doctor.Fail("session valid", fmt.Sprintf("WhoAmI failed: %v", err)))
		}
		return results
	}

	if whoami != sessionData.UserID {
		results = append(results, doctor.Warn("session valid", fmt.Sprintf("WhoAmI returned %s, session has %s", whoami, sessionData.UserID)))
	} else {
		results = append(results, doctor.Pass("session valid", fmt.Sprintf("authenticated as %s", whoami)))
	}

	return results
}

// --- Helpers ---

// statOwnership returns the syscall.Stat_t for a path, giving access
// to the raw UID and GID.
func statOwnership(path string) (*syscall.Stat_t, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("cannot read ownership of %s (non-Linux?)", path)
	}
	return stat, nil
}

// runCommand executes a command and returns an error if it fails.
func runCommand(name string, args ...string) error {
	command := exec.Command(name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// resolveOwner parses "user:group" and returns the UID and GID.
func resolveOwner(owner string) (uint32, uint32, error) {
	userName, groupName, found := strings.Cut(owner, ":")
	if !found {
		return 0, 0, fmt.Errorf("invalid owner format %q (expected user:group)", owner)
	}

	var uid uint32
	if userName == "root" {
		uid = 0
	} else {
		userInfo, err := user.Lookup(userName)
		if err != nil {
			return 0, 0, fmt.Errorf("lookup user %q: %w", userName, err)
		}
		var uidInt int
		_, err = fmt.Sscanf(userInfo.Uid, "%d", &uidInt)
		if err != nil {
			return 0, 0, fmt.Errorf("parse uid %q: %w", userInfo.Uid, err)
		}
		uid = uint32(uidInt)
	}

	var gid uint32
	if groupName == "root" {
		gid = 0
	} else {
		groupInfo, err := user.LookupGroup(groupName)
		if err != nil {
			return 0, 0, fmt.Errorf("lookup group %q: %w", groupName, err)
		}
		var gidInt int
		_, err = fmt.Sscanf(groupInfo.Gid, "%d", &gidInt)
		if err != nil {
			return 0, 0, fmt.Errorf("parse gid %q: %w", groupInfo.Gid, err)
		}
		gid = uint32(gidInt)
	}

	return uid, gid, nil
}

// createDirectory creates a directory with the specified ownership and mode.
func createDirectory(directory directorySpec) error {
	if err := os.MkdirAll(directory.path, directory.mode); err != nil {
		return fmt.Errorf("mkdir %s: %w", directory.path, err)
	}

	uid, gid, err := resolveOwner(directory.owner)
	if err != nil {
		return err
	}

	if err := os.Chown(directory.path, int(uid), int(gid)); err != nil {
		return fmt.Errorf("chown %s: %w", directory.path, err)
	}
	if err := os.Chmod(directory.path, directory.mode); err != nil {
		return fmt.Errorf("chmod %s: %w", directory.path, err)
	}
	return nil
}

// fixDirectoryOwnership repairs ownership and mode of a directory and
// recursively chowns all contents. Recursive ownership is critical when
// transitioning from a dev setup (services running as the developer's
// user) to a production setup (services running as the bureau user):
// files like session.json and age keypairs inside /var/lib/bureau/ must
// be readable by the new owner or the services crash on restart.
//
// The directory itself gets the specified mode; files and subdirectories
// within it get ownership changes only (their modes are left as-is,
// since individual files may have intentionally restrictive permissions
// like 0600 for secret material).
func fixDirectoryOwnership(directory directorySpec, uid, gid uint32) error {
	// Set the directory's own mode and ownership.
	if err := os.Chmod(directory.path, directory.mode); err != nil {
		return fmt.Errorf("chmod %s: %w", directory.path, err)
	}

	// Recursively chown everything within.
	var walkErrors []error
	err := filepath.WalkDir(directory.path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			walkErrors = append(walkErrors, fmt.Errorf("%s: %w", path, err))
			return nil // continue walking
		}
		if chownErr := os.Lchown(path, int(uid), int(gid)); chownErr != nil {
			walkErrors = append(walkErrors, fmt.Errorf("chown %s: %w", path, chownErr))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walking %s: %w", directory.path, err)
	}
	if len(walkErrors) > 0 {
		var messages []string
		for _, walkError := range walkErrors {
			messages = append(messages, walkError.Error())
		}
		return fmt.Errorf("chown errors in %s: %s", directory.path, strings.Join(messages, "; "))
	}
	return nil
}

// writeMachineConf writes machine.conf from bootstrap flag values.
func writeMachineConf(params machineDoctorParams) error {
	fileContent := fmt.Sprintf(
		"# Bureau machine configuration.\n"+
			"# Written by bureau machine doctor --fix.\n"+
			"\n"+
			"BUREAU_HOMESERVER_URL=%s\n"+
			"BUREAU_MACHINE_NAME=%s\n"+
			"BUREAU_SERVER_NAME=%s\n"+
			"BUREAU_FLEET=%s\n",
		params.Homeserver,
		params.MachineName,
		params.ServerName,
		params.Fleet,
	)

	// Include non-default identity configuration so machine.conf is
	// self-documenting about what user/group this instance uses.
	if params.SystemUser != "" && params.SystemUser != principal.SystemUserName {
		fileContent += fmt.Sprintf("BUREAU_SYSTEM_USER=%s\n", params.SystemUser)
	}
	if params.OperatorsGroup != "" && params.OperatorsGroup != principal.OperatorsGroupName {
		fileContent += fmt.Sprintf("BUREAU_OPERATORS_GROUP=%s\n", params.OperatorsGroup)
	}

	// Ensure parent directory exists.
	parentDirectory := filepath.Dir(machineConfPath)
	if err := os.MkdirAll(parentDirectory, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", parentDirectory, err)
	}

	return os.WriteFile(machineConfPath, []byte(fileContent), 0644)
}

// callerSessionHasGroup checks whether the operator's login session
// includes a specific group by inspecting actual process credentials via
// /proc. When doctor runs via `sudo`, we walk up the process tree until
// we find a process owned by the original user (identified by SUDO_UID).
// That process's supplementary group list reflects what initgroups() set
// at login time — NOT what /etc/group says now. This detects the case
// where the operator was just added to the group (by this doctor run)
// but hasn't re-logged in yet.
//
// Walking up handles both direct invocation (`sudo bureau ...`) and
// indirect chains (`sudo bash -c "bureau ..."`), since we stop at the
// first process that belongs to the calling user rather than counting
// exactly N levels.
func callerSessionHasGroup(targetGID int) (bool, error) {
	sudoUIDStr := os.Getenv("SUDO_UID")
	if sudoUIDStr == "" {
		return false, fmt.Errorf("SUDO_UID not set")
	}
	sudoUID, err := strconv.Atoi(sudoUIDStr)
	if err != nil {
		return false, fmt.Errorf("parse SUDO_UID %q: %w", sudoUIDStr, err)
	}

	// Walk up the process tree to find the first process owned by
	// the original user. Start from our parent (which is typically
	// sudo or a shell under sudo).
	pid := os.Getppid()
	const maxWalk = 32 // guard against infinite loops from PID 1 cycles
	for range maxWalk {
		if pid <= 1 {
			return false, fmt.Errorf("reached init (pid %d) without finding UID %d", pid, sudoUID)
		}

		processUID, uidErr := readProcUID(pid)
		if uidErr != nil {
			return false, fmt.Errorf("read UID of pid %d: %w", pid, uidErr)
		}

		if processUID == sudoUID {
			// Found the operator's process. Read its groups.
			groups, groupErr := readProcGroups(pid)
			if groupErr != nil {
				return false, fmt.Errorf("read groups of pid %d: %w", pid, groupErr)
			}

			for _, gid := range groups {
				if gid == targetGID {
					return true, nil
				}
			}
			return false, nil
		}

		// Not the user's process — go up one more level.
		parentPID, ppidErr := readPPID(pid)
		if ppidErr != nil {
			return false, fmt.Errorf("read PPID of pid %d: %w", pid, ppidErr)
		}
		pid = parentPID
	}

	return false, fmt.Errorf("exceeded max walk depth without finding UID %d", sudoUID)
}

// readPPID reads the parent PID from /proc/<pid>/status.
func readPPID(pid int) (int, error) {
	return readProcIntField(pid, "PPid:")
}

// readProcUID reads the real UID from /proc/<pid>/status. The Uid: line
// has four values (real, effective, saved, filesystem); we return the
// first (real UID).
func readProcUID(pid int) (int, error) {
	return readProcIntField(pid, "Uid:")
}

// readProcGroups reads the supplementary group IDs from /proc/<pid>/status.
func readProcGroups(pid int) ([]int, error) {
	statusPath := fmt.Sprintf("/proc/%d/status", pid)
	file, err := os.Open(statusPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Groups:") {
			fields := strings.Fields(line)
			// fields[0] is "Groups:", rest are GID strings.
			groups := make([]int, 0, len(fields)-1)
			for _, field := range fields[1:] {
				gid, parseErr := strconv.Atoi(field)
				if parseErr != nil {
					return nil, fmt.Errorf("parse GID %q: %w", field, parseErr)
				}
				groups = append(groups, gid)
			}
			return groups, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("no Groups: line in %s", statusPath)
}

// readProcIntField reads a single integer field from /proc/<pid>/status.
func readProcIntField(pid int, fieldPrefix string) (int, error) {
	statusPath := fmt.Sprintf("/proc/%d/status", pid)
	file, err := os.Open(statusPath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, fieldPrefix) {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, fmt.Errorf("malformed %s line: %q", fieldPrefix, line)
			}
			return strconv.Atoi(fields[1])
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("no %s line in %s", fieldPrefix, statusPath)
}

// atomicSymlink creates or replaces a symlink atomically by creating a
// temporary symlink and renaming it over the target.
func atomicSymlink(source, target string) error {
	tempPath := target + ".new"
	os.Remove(tempPath) // remove leftover from previous interrupted operation
	if err := os.Symlink(source, tempPath); err != nil {
		return fmt.Errorf("create symlink %s -> %s: %w", tempPath, source, err)
	}
	if err := os.Rename(tempPath, target); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("rename %s -> %s: %w", tempPath, target, err)
	}
	return nil
}

// getServiceMainPID returns the main PID of a systemd service, or 0 if
// the PID cannot be determined.
func getServiceMainPID(unitName string) int {
	output, err := exec.Command("systemctl", "show", "--property=MainPID", unitName).Output()
	if err != nil {
		return 0
	}
	// Output format: "MainPID=12345\n"
	parts := strings.SplitN(strings.TrimSpace(string(output)), "=", 2)
	if len(parts) != 2 {
		return 0
	}
	pid, err := strconv.Atoi(parts[1])
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// launcherSession holds the minimal fields from the launcher's session.json.
type launcherSession struct {
	UserID      ref.UserID `json:"user_id"`
	AccessToken string     `json:"access_token"`
}

// readLauncherSession reads the launcher's session.json file.
func readLauncherSession() (*launcherSession, error) {
	sessionPath := os.Getenv("BUREAU_LAUNCHER_SESSION")
	if sessionPath == "" {
		sessionPath = cli.DefaultLauncherSessionPath
	}

	data, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, err
	}

	var session launcherSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("parsing session %s: %w", sessionPath, err)
	}

	return &session, nil
}
