// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/cli/doctor"
	"github.com/bureau-foundation/bureau/lib/principal"
)

// uninstallParams holds the parameters for the machine uninstall command.
type uninstallParams struct {
	cli.JSONOutput
	DryRun         bool   `json:"dry_run"     flag:"dry-run"     desc:"preview what would be removed without executing"`
	RemoveUser     bool   `json:"remove_user" flag:"remove-user"  desc:"also remove the bureau user and operators group"`
	SystemUser     string `json:"system_user,omitempty"     flag:"system-user"     desc:"Unix user for Bureau processes (default: bureau)"`
	OperatorsGroup string `json:"operators_group,omitempty" flag:"operators-group"  desc:"Unix group for operator socket access (default: bureau-operators)"`
}

func uninstallCommand() *cli.Command {
	var params uninstallParams

	return &cli.Command{
		Name:    "uninstall",
		Summary: "Remove Bureau installation from this machine",
		Description: `Remove Bureau from the local machine: stop and disable systemd services,
remove unit files, remove binaries, remove configuration, and remove state
directories.

WARNING: This removes /var/lib/bureau/ which contains the machine's session
and keypair. This is irreversible. Decommission the machine from the fleet
first with "bureau machine decommission" so other fleet members know it
is gone.

Use --dry-run to preview what would be removed without making changes.

Use --remove-user to also delete the bureau system user and operators group.
Without this flag, the user and group are left in place (harmless, and
allows re-installation without re-adding operators to the group).

This command requires root privileges (sudo).`,
		Usage: "bureau machine uninstall [flags]",
		Examples: []cli.Example{
			{
				Description: "Preview what would be removed",
				Command:     "sudo bureau machine uninstall --dry-run",
			},
			{
				Description: "Remove Bureau completely",
				Command:     "sudo bureau machine uninstall --remove-user",
			},
		},
		Annotations:    cli.Destructive(),
		Output:         func() any { return &doctor.JSONOutput{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/machine/uninstall"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}

			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()

			return runMachineUninstall(ctx, params, logger)
		},
	}
}

// runMachineUninstall generates uninstall checks and executes them.
// Unlike doctor's multi-iteration fix loop, uninstall is single-pass:
// each removal is idempotent and there are no cascading dependencies
// between uninstall steps.
func runMachineUninstall(ctx context.Context, params uninstallParams, logger *slog.Logger) error {
	systemUser := params.SystemUser
	if systemUser == "" {
		systemUser = principal.SystemUserName
	}
	operatorsGroup := params.OperatorsGroup
	if operatorsGroup == "" {
		operatorsGroup = principal.OperatorsGroupName
	}

	results := uninstallChecks(systemUser, operatorsGroup, params.RemoveUser)

	aggregateOutcome := doctor.Outcome{
		ElevatedHint: "sudo bureau machine uninstall",
	}

	isOSPermissionDenied := func(err error) bool {
		return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
	}

	outcome := doctor.ExecuteFixes(ctx, results, params.DryRun, isOSPermissionDenied)
	if outcome.PermissionDenied {
		aggregateOutcome.PermissionDenied = true
	}
	aggregateOutcome.ElevatedSkipped += outcome.ElevatedSkipped
	aggregateOutcome.FixedCount += outcome.FixedCount

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

	// Pass fixMode=true so PrintChecklist shows fix hints and dry-run
	// previews. Uninstall always intends to act (or preview acting).
	return doctor.PrintChecklist(results, true, params.DryRun, aggregateOutcome)
}

// uninstallChecks generates the ordered list of removal checks. Each
// item that exists produces a FailElevated result with a removal
// action; items already gone produce Pass. The order matters: services
// must stop before their units are removed, units before directories.
func uninstallChecks(systemUser, operatorsGroup string, removeUser bool) []doctor.Result {
	var results []doctor.Result

	// Step 1: Stop running services. Stop in reverse dependency order
	// (daemon before launcher) so the daemon gets a clean shutdown
	// signal rather than losing its launcher mid-operation.
	for index := len(expectedUnits) - 1; index >= 0; index-- {
		unit := expectedUnits[index]
		if isUnitActive(unit.name) {
			results = append(results, doctor.FailElevated(
				"stop "+unit.name,
				unit.name+" is running",
				"systemctl stop "+unit.name,
				func(ctx context.Context) error {
					return runCommand("systemctl", "stop", unit.name)
				},
			))
		} else {
			results = append(results, doctor.Pass("stop "+unit.name, "not running"))
		}
	}

	// Step 2: Disable services.
	for _, unit := range expectedUnits {
		unit := unit
		enabledOutput, _ := exec.Command("systemctl", "is-enabled", unit.name).Output()
		enabledStatus := strings.TrimSpace(string(enabledOutput))
		if enabledStatus == "enabled" {
			results = append(results, doctor.FailElevated(
				"disable "+unit.name,
				unit.name+" is enabled",
				"systemctl disable "+unit.name,
				func(ctx context.Context) error {
					return runCommand("systemctl", "disable", unit.name)
				},
			))
		} else {
			results = append(results, doctor.Pass("disable "+unit.name, "not enabled"))
		}
	}

	// Step 3: Remove systemd unit files. A single daemon-reload at the
	// end covers all removals.
	var removedAnyUnit bool
	for _, unit := range expectedUnits {
		unit := unit
		if _, err := os.Stat(unit.installPath); err == nil {
			removedAnyUnit = true
			results = append(results, doctor.FailElevated(
				"remove "+unit.name,
				unit.installPath+" exists",
				"rm "+unit.installPath,
				func(ctx context.Context) error {
					return os.Remove(unit.installPath)
				},
			))
		} else {
			results = append(results, doctor.Pass("remove "+unit.name, "already removed"))
		}
	}
	if removedAnyUnit {
		results = append(results, doctor.FailElevated(
			"systemd daemon-reload",
			"unit files were removed",
			"systemctl daemon-reload",
			func(ctx context.Context) error {
				return runCommand("systemctl", "daemon-reload")
			},
		))
	}

	// Step 4: Remove PATH profile.
	if _, err := os.Stat(profileDPath); err == nil {
		results = append(results, doctor.FailElevated(
			"remove "+profileDPath,
			profileDPath+" exists",
			"rm "+profileDPath,
			func(ctx context.Context) error {
				return os.Remove(profileDPath)
			},
		))
	} else {
		results = append(results, doctor.Pass("remove "+profileDPath, "already removed"))
	}

	// Step 5: Remove binary symlinks from the install directory.
	for _, binaryName := range expectedHostBinaries {
		binaryName := binaryName
		installPath := filepath.Join(binaryInstallDir, binaryName)
		if _, err := os.Lstat(installPath); err == nil {
			results = append(results, doctor.FailElevated(
				"remove "+binaryName,
				installPath+" exists",
				"rm "+installPath,
				func(ctx context.Context) error {
					return os.Remove(installPath)
				},
			))
		} else {
			results = append(results, doctor.Pass("remove "+binaryName, "already removed"))
		}
	}

	// Step 6: Remove directories. Iterate in reverse order (deepest
	// first) so children are removed before parents.
	directories := buildExpectedDirectories(systemUser, operatorsGroup)
	for index := len(directories) - 1; index >= 0; index-- {
		directory := directories[index]
		if _, err := os.Stat(directory.path); err == nil {
			message := directory.path + " exists"
			if directory.path == principal.DefaultStateDir {
				message += " (contains session and keypair)"
			}
			results = append(results, doctor.FailElevated(
				"remove "+directory.path,
				message,
				"rm -rf "+directory.path,
				func(ctx context.Context) error {
					return os.RemoveAll(directory.path)
				},
			))
		} else {
			results = append(results, doctor.Pass("remove "+directory.path, "already removed"))
		}
	}

	// The /var/bureau/ parent directory is not in buildExpectedDirectories
	// but contains bin/, workspace/, and cache/. Remove it if it exists
	// after the children are gone.
	if _, err := os.Stat("/var/bureau"); err == nil {
		results = append(results, doctor.FailElevated(
			"remove /var/bureau",
			"/var/bureau exists",
			"rm -rf /var/bureau",
			func(ctx context.Context) error {
				return os.RemoveAll("/var/bureau")
			},
		))
	} else {
		results = append(results, doctor.Pass("remove /var/bureau", "already removed"))
	}

	// Step 7: Remove machine.conf. This lives inside /etc/bureau/ which
	// was handled above, but if directory removal failed (permissions,
	// non-empty), removing the config file individually still cleans up.
	confPath := cli.MachineConfPath()
	if _, err := os.Stat(confPath); err == nil {
		results = append(results, doctor.FailElevated(
			"remove machine.conf",
			confPath+" exists",
			"rm "+confPath,
			func(ctx context.Context) error {
				return os.Remove(confPath)
			},
		))
	} else {
		results = append(results, doctor.Pass("remove machine.conf", "already removed"))
	}

	// Step 8: Remove user and group (only with --remove-user).
	if removeUser {
		if _, err := user.Lookup(systemUser); err == nil {
			results = append(results, doctor.FailElevated(
				"remove "+systemUser+" user",
				fmt.Sprintf("user %q exists", systemUser),
				"userdel "+systemUser,
				func(ctx context.Context) error {
					return runCommand("userdel", systemUser)
				},
			))
		} else {
			results = append(results, doctor.Pass("remove "+systemUser+" user", "does not exist"))
		}

		if _, err := user.LookupGroup(operatorsGroup); err == nil {
			results = append(results, doctor.FailElevated(
				"remove "+operatorsGroup+" group",
				fmt.Sprintf("group %q exists", operatorsGroup),
				"groupdel "+operatorsGroup,
				func(ctx context.Context) error {
					return runCommand("groupdel", operatorsGroup)
				},
			))
		} else {
			results = append(results, doctor.Pass("remove "+operatorsGroup+" group", "does not exist"))
		}
	}

	return results
}
