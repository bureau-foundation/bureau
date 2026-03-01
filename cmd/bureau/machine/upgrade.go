// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// upgradeParams holds the CLI-specific parameters for the machine upgrade command.
type upgradeParams struct {
	cli.SessionConfig
	cli.JSONOutput
	HostEnvPath string `json:"host_env_path" flag:"host-env" desc:"path to bureau-host-env Nix derivation"`
	Local       bool   `json:"-" flag:"local" desc:"upgrade the local machine (reads launcher session)"`
}

// upgradeResult is the JSON output of the machine upgrade command.
type upgradeResult struct {
	Machine       ref.Machine          `json:"machine"         desc:"machine that was upgraded"`
	ConfigRoomID  ref.RoomID           `json:"config_room_id"  desc:"config room where the version was published"`
	ConfigEventID ref.EventID          `json:"config_event_id" desc:"event ID of the updated MachineConfig"`
	BureauVersion schema.BureauVersion `json:"bureau_version"  desc:"published binary versions"`
}

func upgradeCommand() *cli.Command {
	var params upgradeParams

	return &cli.Command{
		Name:    "upgrade",
		Summary: "Publish a BureauVersion to trigger binary self-update",
		Description: `Publish a BureauVersion state event to a machine's config room.

The positional argument is a machine reference — either a bare localpart
(e.g., "bureau/fleet/prod/machine/workstation") or a full Matrix user ID
(e.g., "@bureau/fleet/prod/machine/workstation:remote.server"). The @
sigil distinguishes the two forms. When a bare localpart is given, the
server name is derived from the connected admin session.

The --local flag reads the launcher's session file to determine the local
machine's identity. Set BUREAU_LAUNCHER_SESSION to point to a non-default
session file.

The --host-env flag specifies a bureau-host-env Nix derivation. The command
follows the bin/bureau-daemon, bin/bureau-launcher, and bin/bureau-proxy
symlinks to resolve the actual Nix store paths.

Once published, the daemon detects the new BureauVersion via Matrix /sync,
prefetches the Nix store paths, compares binary content hashes against the
running versions, and orchestrates atomic exec() transitions for any that
differ. The update ordering is: proxy (IPC to launcher), daemon (exec),
launcher (IPC then exec).`,
		Usage: "bureau machine upgrade <machine-ref> --host-env <path> [flags]",
		Examples: []cli.Example{
			{
				Description: "Upgrade the local machine from a Nix build",
				Command:     "bureau machine upgrade --local --host-env $(nix build .#bureau-host-env --print-out-paths --no-link) --credential-file ./creds",
			},
			{
				Description: "Upgrade a specific machine (bare localpart)",
				Command:     "bureau machine upgrade bureau/fleet/prod/machine/workstation --host-env /nix/store/...-bureau-host-env --credential-file ./creds",
			},
			{
				Description: "Upgrade a machine on a remote homeserver",
				Command:     "bureau machine upgrade @bureau/fleet/prod/machine/workstation:remote.server --host-env /nix/store/...-bureau-host-env --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &upgradeResult{} },
		RequiredGrants: []string{"command/machine/upgrade"},
		Annotations:    cli.Idempotent(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			return runUpgrade(ctx, args, &params, logger)
		},
	}
}

func runUpgrade(ctx context.Context, args []string, params *upgradeParams, logger *slog.Logger) error {
	// Validate args: exactly one positional arg XOR --local.
	if params.Local && len(args) > 0 {
		return cli.Validation("--local and a positional machine argument are mutually exclusive")
	}
	if !params.Local && len(args) == 0 {
		return cli.Validation("either a machine reference or --local is required").
			WithHint("Usage: bureau machine upgrade <machine-ref> --host-env <path> [flags]")
	}
	if len(args) > 1 {
		return cli.Validation("expected at most one argument (machine reference), got %d", len(args))
	}
	if params.HostEnvPath == "" {
		return cli.Validation("--host-env is required").
			WithHint("Pass --host-env with the path to a bureau-host-env Nix derivation.\n" +
				"Build one with: nix build .#bureau-host-env --print-out-paths --no-link")
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Connect as admin for state event publishing.
	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	// Derive default server from the admin session's identity.
	defaultServer, err := ref.ServerFromUserID(session.UserID().String())
	if err != nil {
		return cli.Internal("cannot determine server name from session: %w", err)
	}

	// Resolve the machine reference.
	var machine ref.Machine
	if params.Local {
		localpart, resolveErr := cli.ResolveLocalMachine()
		if resolveErr != nil {
			return resolveErr
		}
		machine, err = ref.ParseMachine(localpart, defaultServer)
		if err != nil {
			return cli.Internal("parsing local machine identity: %w", err)
		}
		logger.Info("resolved --local", "machine", machine.Localpart())
	} else {
		machine, err = resolveMachineArg(args[0], defaultServer)
		if err != nil {
			return cli.Validation("invalid machine reference: %v", err)
		}
	}

	logger = logger.With(
		"machine", machine.Localpart(),
		"server", machine.Server().String(),
	)

	// Resolve the host-env to a BureauVersion.
	version, err := resolveHostEnvBinaries(params.HostEnvPath)
	if err != nil {
		return cli.Validation("resolving host-env binaries: %v", err)
	}

	logger.Info("resolved host-env binaries",
		"daemon", version.DaemonStorePath,
		"launcher", version.LauncherStorePath,
		"proxy", version.ProxyStorePath,
		"log_relay", version.LogRelayStorePath,
		"host_env", version.HostEnvironmentPath,
	)

	// Resolve the machine's config room.
	configAlias := machine.RoomAlias()
	configRoomID, err := session.ResolveAlias(ctx, configAlias)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return cli.NotFound("config room %s not found", configAlias).
				WithHint("Is the machine provisioned? Run 'bureau machine provision' to register it.")
		}
		return cli.Transient("resolving config room %s: %w", configAlias, err).
			WithHint("Check that the homeserver is running. Run 'bureau matrix doctor' to diagnose.")
	}

	// Read-modify-write MachineConfig. Read the existing config (404
	// means no config yet), set BureauVersion, and publish. This
	// preserves existing Principals and DefaultPolicy.
	machineLocalpart := machine.Localpart()
	config, err := messaging.GetState[schema.MachineConfig](ctx, session, configRoomID, schema.EventTypeMachineConfig, machineLocalpart)
	if err != nil && !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return cli.Transient("reading machine config: %w", err).
			WithHint("Check that the homeserver is running. Run 'bureau matrix doctor' to diagnose.")
	}

	config.BureauVersion = version

	eventID, err := session.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineLocalpart, config)
	if err != nil {
		return cli.Transient("publishing machine config: %w", err).
			WithHint("Check that the homeserver is running and you have write access to the config room.\n" +
				"Run 'bureau matrix doctor' to diagnose.")
	}

	result := upgradeResult{
		Machine:       machine,
		ConfigRoomID:  configRoomID,
		ConfigEventID: eventID,
		BureauVersion: *version,
	}

	if done, emitErr := params.EmitJSON(result); done {
		return emitErr
	}

	logger.Info("BureauVersion published",
		"config_room", configRoomID.String(),
		"event_id", eventID.String(),
	)

	return nil
}

// resolveMachineArg parses a positional argument as either a bare machine
// localpart (defaulting to defaultServer) or a full Matrix user ID
// (@localpart:server) with an explicit server. The @ sigil distinguishes
// the two forms, matching Matrix convention.
func resolveMachineArg(arg string, defaultServer ref.ServerName) (ref.Machine, error) {
	if strings.HasPrefix(arg, "@") {
		return ref.ParseMachineUserID(arg)
	}
	return ref.ParseMachine(arg, defaultServer)
}

// resolveHostEnvBinaries resolves the bin/ symlinks in a bureau-host-env
// Nix derivation to produce a BureauVersion with full Nix store paths.
// Each core binary is resolved via filepath.EvalSymlinks and validated to
// be under /nix/store/. The host-env path itself is included so the
// daemon can resolve service binaries from the Nix closure without
// relying on /usr/local/bin symlinks.
func resolveHostEnvBinaries(hostEnvPath string) (*schema.BureauVersion, error) {
	binaries := []struct {
		name  string
		label string
	}{
		{"bureau-daemon", "daemon"},
		{"bureau-launcher", "launcher"},
		{"bureau-proxy", "proxy"},
		{"bureau-log-relay", "log-relay"},
	}

	paths := make([]string, len(binaries))
	for index, binary := range binaries {
		symlink := filepath.Join(hostEnvPath, "bin", binary.name)
		resolved, err := filepath.EvalSymlinks(symlink)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("%s binary not found at %s", binary.label, symlink)
			}
			return nil, fmt.Errorf("resolving %s symlink %s: %w", binary.label, symlink, err)
		}

		if !strings.HasPrefix(resolved, "/nix/store/") {
			return nil, fmt.Errorf("%s binary %s resolves to %s which is not under /nix/store/", binary.label, symlink, resolved)
		}

		paths[index] = resolved
	}

	// Validate that the host-env path itself is under /nix/store/.
	resolvedHostEnv, err := filepath.EvalSymlinks(hostEnvPath)
	if err != nil {
		return nil, fmt.Errorf("resolving host-env path %s: %w", hostEnvPath, err)
	}
	if !strings.HasPrefix(resolvedHostEnv, "/nix/store/") {
		return nil, fmt.Errorf("host-env path %s resolves to %s which is not under /nix/store/", hostEnvPath, resolvedHostEnv)
	}

	return &schema.BureauVersion{
		DaemonStorePath:     paths[0],
		LauncherStorePath:   paths[1],
		ProxyStorePath:      paths[2],
		LogRelayStorePath:   paths[3],
		HostEnvironmentPath: resolvedHostEnv,
	}, nil
}
