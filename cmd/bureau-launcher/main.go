// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/bureau-foundation/bureau/lib/binhash"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/tmux"
	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/sandbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		homeserverURL         string
		registrationTokenFile string
		bootstrapFile         string
		firstBootOnly         bool
		machineName           string
		serverName            string
		fleetPrefix           string
		runDir                string
		stateDir              string
		proxyBinaryPath       string
		logRelayBinaryPath    string
		workspaceRoot         string
		cacheRoot             string
		operatorsGroup        string
		showVersion           bool
	)

	flag.StringVar(&homeserverURL, "homeserver", "http://localhost:6167", "Matrix homeserver URL")
	flag.StringVar(&registrationTokenFile, "registration-token-file", "", "path to file containing registration token, or - for stdin (first boot only)")
	flag.StringVar(&bootstrapFile, "bootstrap-file", "", "path to bootstrap config from 'bureau machine provision' (first boot only, mutually exclusive with --registration-token-file)")
	flag.BoolVar(&firstBootOnly, "first-boot-only", false, "exit after first boot setup instead of entering the main loop")
	flag.StringVar(&machineName, "machine-name", "", "machine localpart (e.g., bureau/fleet/prod/machine/workstation) (required)")
	flag.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
	flag.StringVar(&fleetPrefix, "fleet", "", "fleet prefix (e.g., bureau/fleet/prod) (required)")
	flag.StringVar(&runDir, "run-dir", principal.DefaultRunDir, "runtime directory for sockets and ephemeral state (all socket paths derive from this)")
	flag.StringVar(&stateDir, "state-dir", principal.DefaultStateDir, "directory for persistent state (keypair, session)")
	flag.StringVar(&proxyBinaryPath, "proxy-binary", "", "path to bureau-proxy binary (auto-detected if empty)")
	flag.StringVar(&logRelayBinaryPath, "log-relay-binary", "", "path to bureau-log-relay binary (auto-detected if empty)")
	flag.StringVar(&workspaceRoot, "workspace-root", principal.DefaultWorkspaceRoot, "root directory for project workspaces")
	flag.StringVar(&cacheRoot, "cache-root", principal.DefaultCacheRoot, "root directory for machine-level tool and model cache")
	flag.StringVar(&operatorsGroup, "operators-group", principal.OperatorsGroupName, "Unix group for operator socket access (empty to disable)")
	flag.BoolVar(&showVersion, "version", false, "print version information and exit")
	flag.Parse()

	if registrationTokenFile != "" && bootstrapFile != "" {
		return fmt.Errorf("--registration-token-file and --bootstrap-file are mutually exclusive")
	}

	if showVersion {
		fmt.Printf("bureau-launcher %s\n", version.Info())
		return nil
	}

	if machineName == "" {
		return fmt.Errorf("--machine-name is required")
	}
	if fleetPrefix == "" {
		return fmt.Errorf("--fleet is required")
	}

	// Construct typed identity refs from the string flags.
	parsedServerName, err := ref.ParseServerName(serverName)
	if err != nil {
		return fmt.Errorf("invalid --server-name: %w", err)
	}
	fleet, err := ref.ParseFleet(fleetPrefix, parsedServerName)
	if err != nil {
		return fmt.Errorf("invalid fleet: %w", err)
	}
	_, bareMachineName, err := ref.ExtractEntityName(machineName)
	if err != nil {
		return fmt.Errorf("invalid machine name: %w", err)
	}
	machine, err := ref.NewMachine(fleet, bareMachineName)
	if err != nil {
		return fmt.Errorf("invalid machine ref: %w", err)
	}

	if err := principal.ValidateRunDir(runDir); err != nil {
		return fmt.Errorf("run directory validation: %w", err)
	}

	// Derive all runtime socket paths from run-dir.
	socketPath := principal.LauncherSocketPath(runDir)

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if maxLocalpart := principal.MaxLocalpartAvailable(runDir); maxLocalpart < principal.MaxLocalpartLength {
		logger.Warn("--run-dir limits maximum localpart length",
			"run_dir", runDir,
			"max_localpart", maxLocalpart,
			"default_max", principal.MaxLocalpartLength,
		)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Load or generate the machine keypair.
	keypair, firstBoot, err := loadOrGenerateKeypair(stateDir, logger)
	if err != nil {
		return fmt.Errorf("keypair: %w", err)
	}
	defer keypair.Close()
	logger.Info("machine keypair loaded",
		"public_key", keypair.PublicKey,
		"first_boot", firstBoot,
	)

	// On first boot, register the machine with Matrix and publish its key.
	// Two bootstrap paths:
	//   --registration-token-file: direct registration (dev/test)
	//   --bootstrap-file: pre-provisioned login + password rotation (production)
	if firstBoot {
		if registrationTokenFile == "" && bootstrapFile == "" {
			return fmt.Errorf("first boot requires --registration-token-file or --bootstrap-file")
		}
		if bootstrapFile != "" {
			if err := firstBootFromBootstrapConfig(ctx, bootstrapFile, machine, keypair, stateDir, logger); err != nil {
				return fmt.Errorf("bootstrap first boot: %w", err)
			}
		} else {
			if err := firstBootSetup(ctx, homeserverURL, registrationTokenFile, machine, keypair, stateDir, logger); err != nil {
				return fmt.Errorf("first boot setup: %w", err)
			}
		}
	} else if firstBootOnly {
		return fmt.Errorf("--first-boot-only is set but this machine has already completed first boot (state-dir %s has existing keypair)", stateDir)
	}

	if firstBootOnly {
		logger.Info("first boot complete, exiting (--first-boot-only)")
		return nil
	}

	// Load the saved Matrix session.
	_, session, err := service.LoadSession(stateDir, homeserverURL, logger)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}
	logger.Info("matrix session loaded", "user_id", session.UserID())

	// Find and validate the proxy binary for sandbox spawning. Every
	// sandbox needs bureau-proxy, so if it's missing the launcher cannot
	// do useful work. Fail immediately with a precise error rather than
	// accepting IPC connections and failing each create-sandbox request.
	if proxyBinaryPath == "" {
		proxyBinaryPath = findProxyBinary(logger)
	}
	if err := validateBinary(proxyBinaryPath, "bureau-proxy"); err != nil {
		return fmt.Errorf("bureau-proxy: %w\n  Install bureau-proxy or set --proxy-binary to its path", err)
	}
	logger.Info("proxy binary validated", "path", proxyBinaryPath)

	// Find and validate the log relay binary. The log relay wraps sandbox
	// commands, holding the outer PTY open until the child exits. This
	// eliminates the tmux 3.4+ race between PTY EOF and SIGCHLD that
	// causes exit codes to be lost.
	if logRelayBinaryPath == "" {
		logRelayBinaryPath = findSiblingBinary("bureau-log-relay", logger)
	}
	if err := validateBinary(logRelayBinaryPath, "bureau-log-relay"); err != nil {
		return fmt.Errorf("bureau-log-relay: %w\n  Install bureau-log-relay or set --log-relay-binary to its path", err)
	}
	logger.Info("log relay binary validated", "path", logRelayBinaryPath)

	// Validate bwrap is available. Every sandboxed principal requires it.
	bwrapPath, err := sandbox.BwrapPath()
	if err != nil {
		return fmt.Errorf("bwrap: %w\n  Install bubblewrap (bwrap) to create sandboxed principals", err)
	}
	logger.Info("bwrap validated", "path", bwrapPath)

	// Ensure the workspace root directory exists. This is the top-level
	// directory where all project workspaces live. The .cache/ subdirectory
	// holds cross-project shared caches (pip, huggingface, etc.).
	if err := os.MkdirAll(workspaceRoot, 0755); err != nil {
		return fmt.Errorf("creating workspace root %s: %w", workspaceRoot, err)
	}
	if err := os.MkdirAll(filepath.Join(workspaceRoot, ".cache"), 0755); err != nil {
		return fmt.Errorf("creating workspace cache %s/.cache: %w", workspaceRoot, err)
	}
	logger.Info("workspace root ready", "path", workspaceRoot)

	// Ensure the machine-level cache root exists. This is separate from
	// workspace/.cache/ (which is workspace-adjacent and backed up with
	// workspace data). The cache root holds machine infrastructure:
	// installed tools, model weights, package caches. The sysadmin
	// principal has read-write access; other agents get read-only mounts
	// to specific subdirectories via template configuration.
	if err := os.MkdirAll(cacheRoot, 0755); err != nil {
		return fmt.Errorf("creating cache root %s: %w", cacheRoot, err)
	}
	logger.Info("cache root ready", "path", cacheRoot)

	// Look up the operators group GID for setting socket group ownership
	// on operator-facing sockets (service CBOR endpoints). In production,
	// the group is created by "bureau machine doctor --fix". Pass an empty
	// --operators-group to disable group ownership (integration tests,
	// development environments without the system group).
	operatorsGID := principal.LookupOperatorsGID(operatorsGroup)
	if operatorsGroup == "" {
		logger.Info("operators group disabled (--operators-group is empty)")
	} else if operatorsGID < 0 {
		logger.Warn("operators group not found (service socket group ownership will not be set)",
			"group", operatorsGroup)
	} else {
		logger.Info("operators group found", "group", operatorsGroup, "gid", operatorsGID)
	}

	// Start the IPC socket for daemon communication.
	launcher := &Launcher{
		session:            session,
		keypair:            keypair,
		machine:            machine,
		homeserverURL:      homeserverURL,
		runDir:             runDir,
		fleetRunDir:        machine.Fleet().RunDir(runDir),
		stateDir:           stateDir,
		proxyBinaryPath:    proxyBinaryPath,
		logRelayBinaryPath: logRelayBinaryPath,
		workspaceRoot:      workspaceRoot,
		cacheRoot:          cacheRoot,
		tmuxServer:         tmux.NewServer(principal.TmuxSocketPath(runDir), writeTmuxConfig(runDir)),
		operatorsGID:       operatorsGID,
		sandboxes:          make(map[string]*managedSandbox),
		failedExecPaths:    make(map[string]bool),
		logger:             logger,
	}

	// Resolve the launcher's own binary path and hash. The path is
	// needed for the exec() watchdog (PreviousBinary field). The hash
	// is queried by the daemon via "status" IPC to determine whether
	// the launcher binary needs updating.
	if executablePath, execErr := os.Executable(); execErr == nil {
		launcher.launcherBinaryPath = executablePath
		if digest, hashErr := binhash.HashFile(executablePath); hashErr == nil {
			launcher.launcherBinaryHash = binhash.FormatDigest(digest)
			logger.Info("launcher binary identity resolved",
				"path", launcher.launcherBinaryPath,
				"hash", launcher.launcherBinaryHash)
		} else {
			logger.Warn("failed to hash launcher binary", "error", hashErr)
		}
	} else {
		logger.Warn("failed to resolve launcher executable path", "error", execErr)
	}

	// Reconnect sandboxes from a previous exec() transition. If a
	// state file exists, the launcher was recently exec()'d and the
	// proxy processes from the previous incarnation are still running.
	if err := launcher.reconnectSandboxes(); err != nil {
		logger.Error("reconnecting sandboxes after exec", "error", err)
		// Non-fatal: the daemon will recreate any missing sandboxes
		// on its next reconcile cycle.
	}

	// Check the watchdog to determine if a previous exec() transition
	// succeeded or failed. Seeds failedExecPaths to prevent retrying a
	// broken binary that the system reverted from.
	if failedPath := checkLauncherWatchdog(
		launcher.launcherWatchdogPath(),
		launcher.launcherBinaryPath,
		logger,
	); failedPath != "" {
		launcher.failedExecPaths[failedPath] = true
	}

	listener, err := listenSocket(socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	defer listener.Close()
	logger.Info("launcher listening", "socket", socketPath)

	// Handle connections in the background.
	go launcher.serve(ctx, listener)

	// Wait for shutdown.
	<-ctx.Done()
	logger.Info("shutting down")

	// Terminate all running proxy processes.
	launcher.shutdownAllSandboxes()

	return nil
}

// loadOrGenerateKeypair loads the machine keypair from the state directory,
// or generates a new one if this is the first boot. Returns the keypair and
// whether this was a first boot (keypair was just generated).
func loadOrGenerateKeypair(stateDir string, logger *slog.Logger) (*sealed.Keypair, bool, error) {
	privateKeyPath := filepath.Join(stateDir, "machine-key.txt")
	publicKeyPath := filepath.Join(stateDir, "machine-key.pub")

	// Check if the keypair already exists.
	privateKeyData, err := os.ReadFile(privateKeyPath)
	if err == nil {
		publicKeyData, err := os.ReadFile(publicKeyPath)
		if err != nil {
			secret.Zero(privateKeyData)
			return nil, false, fmt.Errorf("private key exists but public key missing at %s: %w", publicKeyPath, err)
		}

		// Move the private key into mmap-backed memory. bytes.TrimSpace
		// returns a sub-slice of privateKeyData; NewFromBytes copies it
		// into mmap and zeros the sub-slice. We then zero all of
		// privateKeyData to catch any leading/trailing whitespace bytes
		// that NewFromBytes didn't reach.
		trimmedKey := bytes.TrimSpace(privateKeyData)
		privateKeyBuffer, bufferError := secret.NewFromBytes(trimmedKey)
		secret.Zero(privateKeyData)
		if bufferError != nil {
			return nil, false, fmt.Errorf("protecting private key: %w", bufferError)
		}

		publicKey := strings.TrimSpace(string(publicKeyData))

		if err := sealed.ParsePrivateKey(privateKeyBuffer); err != nil {
			privateKeyBuffer.Close()
			return nil, false, fmt.Errorf("stored private key is invalid: %w", err)
		}
		if err := sealed.ParsePublicKey(publicKey); err != nil {
			privateKeyBuffer.Close()
			return nil, false, fmt.Errorf("stored public key is invalid: %w", err)
		}

		return &sealed.Keypair{PrivateKey: privateKeyBuffer, PublicKey: publicKey}, false, nil
	}

	if !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("reading private key from %s: %w", privateKeyPath, err)
	}

	// First boot: generate a new keypair.
	logger.Info("generating new machine keypair (first boot)")

	keypair, err := sealed.GenerateKeypair()
	if err != nil {
		return nil, false, fmt.Errorf("generating keypair: %w", err)
	}

	// Ensure the state directory exists.
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		keypair.Close()
		return nil, false, fmt.Errorf("creating state directory %s: %w", stateDir, err)
	}

	// Write the private key (0600 — owner-only read/write).
	if err := os.WriteFile(privateKeyPath, keypair.PrivateKey.Bytes(), 0600); err != nil {
		keypair.Close()
		return nil, false, fmt.Errorf("writing private key to %s: %w", privateKeyPath, err)
	}

	// Write the public key (0644 — readable by all, this is public data).
	if err := os.WriteFile(publicKeyPath, []byte(keypair.PublicKey), 0644); err != nil {
		keypair.Close()
		return nil, false, fmt.Errorf("writing public key to %s: %w", publicKeyPath, err)
	}

	return keypair, true, nil
}

// listenSocket creates a unix socket listener, removing any stale socket file.
func listenSocket(socketPath string) (net.Listener, error) {
	// Ensure the parent directory exists.
	socketDir := filepath.Dir(socketPath)
	if err := os.MkdirAll(socketDir, 0755); err != nil {
		return nil, fmt.Errorf("creating socket directory %s: %w", socketDir, err)
	}

	// Remove stale socket file from a previous run.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing stale socket %s: %w", socketPath, err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}

	// Set socket permissions so the daemon (running as a different user
	// in production) can connect.
	if err := os.Chmod(socketPath, 0660); err != nil {
		listener.Close()
		return nil, fmt.Errorf("setting socket permissions: %w", err)
	}

	return listener, nil
}

// findProxyBinary looks for the bureau-proxy binary next to the launcher
// binary, then on PATH. Returns an empty string if not found; the caller
// validates the result with validateBinary before proceeding.
func findProxyBinary(logger *slog.Logger) string {
	return findSiblingBinary("bureau-proxy", logger)
}

// findSiblingBinary looks for a Bureau binary by name, first next to the
// launcher binary (the standard Nix co-deployment layout), then on PATH.
// Returns an empty string if not found; the caller validates the result
// with validateBinary before proceeding.
func findSiblingBinary(name string, logger *slog.Logger) string {
	// Check next to the launcher binary. In production Nix deployments,
	// all Bureau binaries are co-located in the same store path.
	executable, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(executable), name)
		if _, err := os.Stat(candidate); err == nil {
			logger.Info("found binary next to launcher", "name", name, "path", candidate)
			return candidate
		}
	}

	// Check PATH.
	path, err := exec.LookPath(name)
	if err == nil {
		logger.Info("found binary on PATH", "name", name, "path", path)
		return path
	}

	return ""
}

// validateBinary checks that a binary path points to a regular, executable
// file. Returns a precise error describing what's wrong and where it looked.
func validateBinary(path, name string) error {
	if path == "" {
		return fmt.Errorf("%s not found (checked next to launcher binary and PATH)", name)
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s at %q: %w", name, path, err)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s at %q is not a regular file (mode %s)", name, path, info.Mode())
	}

	if info.Mode()&0111 == 0 {
		return fmt.Errorf("%s at %q is not executable (mode %s)", name, path, info.Mode())
	}

	return nil
}
