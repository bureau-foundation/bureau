// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/cli/doctor"
	"github.com/bureau-foundation/bureau/lib/bootstrap"
	"github.com/bureau-foundation/bureau/lib/principal"
)

// deployLocalParams holds the parameters for the machine deploy local command.
type deployLocalParams struct {
	cli.JSONOutput
	BootstrapFile  string `json:"bootstrap_file"              flag:"bootstrap-file"  desc:"path to bootstrap config from 'bureau machine provision' (required)"`
	BinaryDir      string `json:"binary_dir,omitempty"        flag:"binary-dir"      desc:"directory containing Bureau binaries to install (default: auto-detect from running binary)"`
	SystemUser     string `json:"system_user,omitempty"       flag:"system-user"     desc:"Unix user for Bureau processes (default: bureau)"`
	OperatorsGroup string `json:"operators_group,omitempty"   flag:"operators-group" desc:"Unix group for operator socket access (default: bureau-operators)"`
	DryRun         bool   `json:"dry_run"                     flag:"dry-run"         desc:"preview what would happen without executing"`
}

func deployCommand() *cli.Command {
	return &cli.Command{
		Name:    "deploy",
		Summary: "Deploy Bureau to a machine",
		Description: `Deploy Bureau to a machine.

Use "bureau machine deploy local" to set up the local machine from a
bootstrap config file produced by "bureau machine provision".`,
		Subcommands: []*cli.Command{
			deployLocalCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Deploy Bureau locally from a bootstrap config",
				Command:     "sudo bureau machine deploy local --bootstrap-file bootstrap.json",
			},
			{
				Description: "Preview what deploy would do",
				Command:     "sudo bureau machine deploy local --bootstrap-file bootstrap.json --dry-run",
			},
		},
	}
}

func deployLocalCommand() *cli.Command {
	var params deployLocalParams

	return &cli.Command{
		Name:    "local",
		Summary: "Deploy Bureau on the local machine",
		Description: `Set up Bureau on the local machine from a bootstrap config file.

This command combines three operations into a single invocation:

  1. Run "bureau machine doctor --fix" to ensure system infrastructure
     is in place (user, group, directories, binaries, systemd units).

  2. Run the launcher in first-boot mode to register the machine with
     the homeserver (login, password rotation, keypair generation,
     room joins).

  3. Start Bureau services via systemd.

The bootstrap config file is produced by "bureau machine provision" on
the hub machine and transferred to the target. It contains a one-time
password that the launcher rotates on first boot.

This command requires root privileges. The launcher runs as the bureau
system user (via sudo -u) since it writes to /var/lib/bureau/.

If the machine has already completed first boot (keypair exists in
/var/lib/bureau/), the first-boot step is skipped and services are
started directly.`,
		Usage: "bureau machine deploy local --bootstrap-file <path> [flags]",
		Examples: []cli.Example{
			{
				Description: "Deploy from a bootstrap config",
				Command:     "sudo bureau machine deploy local --bootstrap-file bootstrap.json",
			},
			{
				Description: "Deploy with a specific binary directory",
				Command:     "sudo bureau machine deploy local --bootstrap-file bootstrap.json --binary-dir /nix/store/...-bureau-host-env/bin",
			},
		},
		Annotations:    cli.Idempotent(),
		Output:         func() any { return &doctor.JSONOutput{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/machine/deploy"},
		Run: func(ctx context.Context, arguments []string, logger *slog.Logger) error {
			if len(arguments) > 0 {
				return cli.Validation("unexpected argument: %s", arguments[0])
			}
			if params.BootstrapFile == "" {
				return cli.Validation("--bootstrap-file is required")
			}

			ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
			defer cancel()

			return runDeployLocal(ctx, params, logger)
		},
	}
}

// runDeployLocal orchestrates the three phases of local deployment:
// infrastructure setup (doctor --fix), first boot (launcher), and
// service start (systemctl).
func runDeployLocal(ctx context.Context, params deployLocalParams, logger *slog.Logger) error {
	// Phase 0: Read and validate bootstrap config.
	bootstrapConfig, err := bootstrap.ReadConfig(params.BootstrapFile)
	if err != nil {
		return err
	}
	logger.Info("bootstrap config loaded",
		"homeserver", bootstrapConfig.HomeserverURL,
		"machine", bootstrapConfig.MachineName,
		"fleet", bootstrapConfig.FleetPrefix,
	)

	systemUser := params.SystemUser
	if systemUser == "" {
		systemUser = principal.SystemUserName
	}

	// Phase 1: Run doctor --fix to set up infrastructure.
	doctorParams := machineDoctorParams{
		Fix:            true,
		DryRun:         params.DryRun,
		BinaryDir:      params.BinaryDir,
		Homeserver:     bootstrapConfig.HomeserverURL,
		MachineName:    bootstrapConfig.MachineName,
		ServerName:     bootstrapConfig.ServerName,
		SystemUser:     params.SystemUser,
		OperatorsGroup: params.OperatorsGroup,
	}

	results, aggregateOutcome, err := runDoctorChecks(ctx, doctorParams, logger)
	if err != nil {
		return fmt.Errorf("infrastructure setup failed: %w", err)
	}

	// Check if any infrastructure checks still fail after fixing.
	var infrastructureFailure bool
	for _, result := range results {
		if result.Status == doctor.StatusFail {
			infrastructureFailure = true
			break
		}
	}

	if params.DryRun {
		return emitDeployResults(params, results, aggregateOutcome, infrastructureFailure, true)
	}

	if infrastructureFailure {
		// Output what failed so the operator can see what went wrong,
		// then return an error.
		if outputErr := emitDeployResults(params, results, aggregateOutcome, true, false); outputErr != nil {
			return outputErr
		}
		return fmt.Errorf("infrastructure setup incomplete: some checks still fail after repair")
	}

	// Phase 2: Run launcher first boot (if not already completed).
	// The launcher detects existing state and refuses --first-boot-only
	// if a keypair already exists, so we check first.
	stateDir := principal.DefaultStateDir
	if firstBootNeeded(stateDir) {
		logger.Info("running first boot registration")
		if err := runFirstBoot(ctx, params, bootstrapConfig, systemUser, logger); err != nil {
			return fmt.Errorf("first boot failed: %w", err)
		}
		logger.Info("first boot complete")
	} else {
		logger.Info("first boot already completed, skipping",
			"state_dir", stateDir,
		)
	}

	// Phase 3: Start services if not already running.
	if !areServicesRunning() {
		logger.Info("starting Bureau services")
		if err := runCommand("systemctl", "start", "bureau-launcher"); err != nil {
			return fmt.Errorf("failed to start services: %w", err)
		}
		logger.Info("services started")
	} else {
		logger.Info("services already running")
	}

	return emitDeployResults(params, results, aggregateOutcome, false, false)
}

// firstBootNeeded returns true if the launcher has not yet completed
// first boot. The launcher generates a keypair on first boot and
// stores it in the state directory; its absence means first boot
// hasn't run.
func firstBootNeeded(stateDir string) bool {
	keypairPath := filepath.Join(stateDir, "keypair.json")
	return !fileExists(keypairPath)
}

// runFirstBoot executes the launcher in first-boot-only mode as the
// bureau system user. The launcher logs in with the one-time password,
// rotates it, generates a keypair, and joins the fleet rooms.
func runFirstBoot(ctx context.Context, params deployLocalParams, config *bootstrap.Config, systemUser string, logger *slog.Logger) error {
	launcherPath := filepath.Join(binaryInstallDir, "bureau-launcher")

	launcherArgs := []string{
		"--bootstrap-file", params.BootstrapFile,
		"--first-boot-only",
		"--machine-name", config.MachineName,
		"--server-name", config.ServerName,
		"--fleet", config.FleetPrefix,
		"--homeserver", config.HomeserverURL,
	}

	// Run as the bureau system user since the launcher writes to
	// /var/lib/bureau/ which is owned by that user.
	command := exec.CommandContext(ctx,
		"sudo", append([]string{"-u", systemUser, launcherPath}, launcherArgs...)...,
	)

	output, err := command.CombinedOutput()
	if err != nil {
		logger.Error("launcher first boot failed",
			"output", string(output),
			"error", err,
		)
		return fmt.Errorf("launcher: %w\noutput: %s", err, output)
	}
	return nil
}

// emitDeployResults outputs the deploy results in JSON or checklist
// format depending on the params.
func emitDeployResults(params deployLocalParams, results []doctor.Result, outcome doctor.Outcome, hasFailures bool, dryRun bool) error {
	if done, err := params.EmitJSON(doctor.BuildJSON(results, dryRun, outcome)); done {
		if err != nil {
			return err
		}
		if hasFailures {
			return &cli.ExitError{Code: 1}
		}
		return nil
	}

	// fixMode=true because deploy always intends to act (or preview acting).
	return doctor.PrintChecklist(results, true, dryRun, outcome)
}

// fileExists returns whether a path exists and is not a directory.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
