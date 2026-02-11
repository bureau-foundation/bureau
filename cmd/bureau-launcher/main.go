// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/binhash"
	"github.com/bureau-foundation/bureau/lib/bootstrap"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/messaging"
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
		runDir                string
		stateDir              string
		proxyBinaryPath       string
		workspaceRoot         string
		showVersion           bool
	)

	flag.StringVar(&homeserverURL, "homeserver", "http://localhost:6167", "Matrix homeserver URL")
	flag.StringVar(&registrationTokenFile, "registration-token-file", "", "path to file containing registration token, or - for stdin (first boot only)")
	flag.StringVar(&bootstrapFile, "bootstrap-file", "", "path to bootstrap config from 'bureau machine provision' (first boot only, mutually exclusive with --registration-token-file)")
	flag.BoolVar(&firstBootOnly, "first-boot-only", false, "exit after first boot setup instead of entering the main loop")
	flag.StringVar(&machineName, "machine-name", "", "machine localpart (e.g., machine/workstation) (required)")
	flag.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
	flag.StringVar(&runDir, "run-dir", principal.DefaultRunDir, "runtime directory for sockets and ephemeral state (all socket paths derive from this)")
	flag.StringVar(&stateDir, "state-dir", principal.DefaultStateDir, "directory for persistent state (keypair, session)")
	flag.StringVar(&proxyBinaryPath, "proxy-binary", "", "path to bureau-proxy binary (auto-detected if empty)")
	flag.StringVar(&workspaceRoot, "workspace-root", principal.DefaultWorkspaceRoot, "root directory for project workspaces")
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
	if err := principal.ValidateLocalpart(machineName); err != nil {
		return fmt.Errorf("invalid machine name: %w", err)
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
			if err := firstBootFromBootstrapConfig(ctx, bootstrapFile, machineName, serverName, keypair, stateDir, logger); err != nil {
				return fmt.Errorf("bootstrap first boot: %w", err)
			}
		} else {
			if err := firstBootSetup(ctx, homeserverURL, registrationTokenFile, machineName, serverName, keypair, stateDir, logger); err != nil {
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
	session, err := loadSession(stateDir, homeserverURL, logger)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}
	logger.Info("matrix session loaded", "user_id", session.UserID())

	// Find the proxy binary for sandbox spawning.
	if proxyBinaryPath == "" {
		proxyBinaryPath = findProxyBinary(logger)
	}

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

	// Start the IPC socket for daemon communication.
	launcher := &Launcher{
		session:         session,
		keypair:         keypair,
		machineName:     machineName,
		serverName:      serverName,
		homeserverURL:   homeserverURL,
		runDir:          runDir,
		stateDir:        stateDir,
		proxyBinaryPath: proxyBinaryPath,
		workspaceRoot:   workspaceRoot,
		sandboxes:       make(map[string]*managedSandbox),
		failedExecPaths: make(map[string]bool),
		logger:          logger,
	}

	// Resolve the launcher's own binary path and hash. The path is
	// needed for the exec() watchdog (PreviousBinary field). The hash
	// is queried by the daemon via "status" IPC to determine whether
	// the launcher binary needs updating.
	if executablePath, execErr := os.Executable(); execErr == nil {
		launcher.binaryPath = executablePath
		if digest, hashErr := binhash.HashFile(executablePath); hashErr == nil {
			launcher.binaryHash = binhash.FormatDigest(digest)
			logger.Info("launcher binary identity resolved",
				"path", launcher.binaryPath,
				"hash", launcher.binaryHash)
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
		launcher.binaryPath,
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
			for i := range privateKeyData {
				privateKeyData[i] = 0
			}
			return nil, false, fmt.Errorf("private key exists but public key missing at %s: %w", publicKeyPath, err)
		}

		// Move the private key into mmap-backed memory. bytes.TrimSpace
		// returns a sub-slice of privateKeyData; NewFromBytes copies it
		// into mmap and zeros the sub-slice. We then zero all of
		// privateKeyData to catch any leading/trailing whitespace bytes
		// that NewFromBytes didn't reach.
		trimmedKey := bytes.TrimSpace(privateKeyData)
		privateKeyBuffer, bufferError := secret.NewFromBytes(trimmedKey)
		for i := range privateKeyData {
			privateKeyData[i] = 0
		}
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

// firstBootSetup registers the machine with Matrix and publishes its key.
func firstBootSetup(ctx context.Context, homeserverURL, registrationTokenFile, machineName, serverName string, keypair *sealed.Keypair, stateDir string, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	registrationToken, err := readSecret(registrationTokenFile)
	if err != nil {
		return fmt.Errorf("reading registration token: %w", err)
	}
	defer registrationToken.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
		Logger:        logger,
	})
	if err != nil {
		return fmt.Errorf("creating matrix client: %w", err)
	}

	// Register the machine account. Use a password derived from the
	// registration token (deterministic, so re-running is idempotent).
	password, err := derivePassword(registrationToken.String(), machineName)
	if err != nil {
		return fmt.Errorf("derive machine password: %w", err)
	}
	defer password.Close()
	session, err := registerOrLogin(ctx, client, machineName, password.String(), registrationToken.String())
	if err != nil {
		return fmt.Errorf("registering machine account: %w", err)
	}
	logger.Info("machine account ready", "user_id", session.UserID())

	// Join the machines room and publish the public key.
	machinesAlias := principal.RoomAlias("bureau/machines", serverName)
	machinesRoomID, err := session.ResolveAlias(ctx, machinesAlias)
	if err != nil {
		return fmt.Errorf("resolving machines room alias %q: %w", machinesAlias, err)
	}

	if _, err := session.JoinRoom(ctx, machinesRoomID); err != nil {
		// If we're already joined, JoinRoom may return an error on some
		// homeservers. Try to continue regardless.
		logger.Warn("join machines room returned error (may already be joined)", "error", err)
	}

	machineKeyContent := schema.MachineKey{
		Algorithm: "age-x25519",
		PublicKey: keypair.PublicKey,
	}
	_, err = session.SendStateEvent(ctx, machinesRoomID, schema.EventTypeMachineKey, machineName, machineKeyContent)
	if err != nil {
		return fmt.Errorf("publishing machine key: %w", err)
	}
	logger.Info("machine public key published",
		"room", machinesRoomID,
		"public_key", keypair.PublicKey,
	)

	// Join the services room so the daemon can read service directory state
	// events via /sync and GetRoomState. Like machines, this requires an
	// admin invitation (the room is invite-only). Non-fatal because the
	// daemon will retry on every startup.
	servicesAlias := principal.RoomAlias("bureau/services", serverName)
	servicesRoomID, err := session.ResolveAlias(ctx, servicesAlias)
	if err != nil {
		logger.Warn("could not resolve services room (daemon will retry on startup)",
			"alias", servicesAlias, "error", err)
	} else {
		if _, err := session.JoinRoom(ctx, servicesRoomID); err != nil {
			logger.Warn("join services room failed (may need admin invitation)",
				"room_id", servicesRoomID, "error", err)
		} else {
			logger.Info("joined services room", "room_id", servicesRoomID)
		}
	}

	// Save the session for subsequent boots.
	if err := saveSession(stateDir, homeserverURL, session); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}

	return nil
}

// firstBootFromBootstrapConfig performs first boot using a pre-provisioned
// bootstrap config file from "bureau machine provision". Instead of
// registering a new account (which requires the registration token), it
// logs in with the one-time password from the bootstrap config, then
// immediately rotates the password to a value derived from the machine's
// private key material. After rotation, the one-time password is useless
// even if the bootstrap config file is captured.
func firstBootFromBootstrapConfig(ctx context.Context, bootstrapFilePath, machineName, serverName string, keypair *sealed.Keypair, stateDir string, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	config, err := bootstrap.ReadConfig(bootstrapFilePath)
	if err != nil {
		return fmt.Errorf("reading bootstrap config: %w", err)
	}

	// Validate that the bootstrap config matches the --machine-name flag.
	if config.MachineName != machineName {
		return fmt.Errorf("bootstrap config machine_name %q does not match --machine-name %q", config.MachineName, machineName)
	}
	if config.ServerName != serverName {
		return fmt.Errorf("bootstrap config server_name %q does not match --server-name %q", config.ServerName, serverName)
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: config.HomeserverURL,
		Logger:        logger,
	})
	if err != nil {
		return fmt.Errorf("creating matrix client: %w", err)
	}

	// Log in with the one-time password (not register — the account was
	// already created by "bureau machine provision").
	session, err := client.Login(ctx, machineName, config.Password)
	if err != nil {
		return fmt.Errorf("logging in with bootstrap password: %w", err)
	}
	logger.Info("logged in with bootstrap password", "user_id", session.UserID())

	// Derive a permanent password from the machine's private key material.
	// This is deterministic: if the launcher re-runs with the same keypair,
	// it produces the same password. The private key never leaves the
	// machine, so this password can't be derived by anyone who captured
	// the bootstrap config.
	permanentPassword, err := derivePermanentPassword(machineName, keypair)
	if err != nil {
		return fmt.Errorf("deriving permanent password: %w", err)
	}
	defer permanentPassword.Close()

	// Rotate the password immediately. After this, the one-time password
	// from the bootstrap config is invalidated.
	if err := session.ChangePassword(ctx, config.Password, permanentPassword.String()); err != nil {
		return fmt.Errorf("rotating password: %w", err)
	}
	logger.Info("rotated one-time password to permanent password")

	// Join the machines room and publish the public key — same as the
	// registration-token first boot path.
	machinesAlias := principal.RoomAlias("bureau/machines", serverName)
	machinesRoomID, err := session.ResolveAlias(ctx, machinesAlias)
	if err != nil {
		return fmt.Errorf("resolving machines room alias %q: %w", machinesAlias, err)
	}

	if _, err := session.JoinRoom(ctx, machinesRoomID); err != nil {
		logger.Warn("join machines room returned error (may already be joined)", "error", err)
	}

	machineKeyContent := schema.MachineKey{
		Algorithm: "age-x25519",
		PublicKey: keypair.PublicKey,
	}
	_, err = session.SendStateEvent(ctx, machinesRoomID, schema.EventTypeMachineKey, machineName, machineKeyContent)
	if err != nil {
		return fmt.Errorf("publishing machine key: %w", err)
	}
	logger.Info("machine public key published",
		"room", machinesRoomID,
		"public_key", keypair.PublicKey,
	)

	// Join the services room (non-fatal, daemon retries on startup).
	servicesAlias := principal.RoomAlias("bureau/services", serverName)
	servicesRoomID, err := session.ResolveAlias(ctx, servicesAlias)
	if err != nil {
		logger.Warn("could not resolve services room (daemon will retry on startup)",
			"alias", servicesAlias, "error", err)
	} else {
		if _, err := session.JoinRoom(ctx, servicesRoomID); err != nil {
			logger.Warn("join services room failed (may need admin invitation)",
				"room_id", servicesRoomID, "error", err)
		} else {
			logger.Info("joined services room", "room_id", servicesRoomID)
		}
	}

	// Save the session.
	if err := saveSession(stateDir, config.HomeserverURL, session); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}

	// Attempt to delete the bootstrap config file. The one-time password
	// has been rotated, so the file is no longer sensitive — but cleaning
	// it up avoids confusion.
	if err := os.Remove(bootstrapFilePath); err != nil {
		logger.Warn("could not delete bootstrap config file (password already rotated, file is no longer sensitive)",
			"path", bootstrapFilePath, "error", err)
	} else {
		logger.Info("deleted bootstrap config file", "path", bootstrapFilePath)
	}

	return nil
}

// derivePermanentPassword derives a password from the machine name and private
// key material. The result is deterministic: the same keypair always produces
// the same password. This is used after bootstrap to replace the one-time
// password, ensuring that only the machine itself (which holds the private key)
// can derive its own password.
func derivePermanentPassword(machineName string, keypair *sealed.Keypair) (*secret.Buffer, error) {
	hash := sha256.Sum256([]byte("bureau-machine-permanent:" + machineName + ":" + keypair.PrivateKey.String()))
	hexBytes := []byte(hex.EncodeToString(hash[:]))
	return secret.NewFromBytes(hexBytes)
}

// sessionData is the JSON structure stored on disk for the Matrix session.
type sessionData struct {
	HomeserverURL string `json:"homeserver_url"`
	UserID        string `json:"user_id"`
	AccessToken   string `json:"access_token"`
}

// saveSession writes the Matrix session to disk for use on subsequent boots.
func saveSession(stateDir string, homeserverURL string, session *messaging.Session) error {
	data := sessionData{
		HomeserverURL: homeserverURL,
		UserID:        session.UserID(),
		AccessToken:   session.AccessToken(),
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshaling session: %w", err)
	}

	sessionPath := filepath.Join(stateDir, "session.json")
	if err := os.WriteFile(sessionPath, jsonData, 0600); err != nil {
		return fmt.Errorf("writing session to %s: %w", sessionPath, err)
	}

	return nil
}

// loadSession reads the saved Matrix session from disk and creates a Session.
func loadSession(stateDir string, homeserverURL string, logger *slog.Logger) (*messaging.Session, error) {
	sessionPath := filepath.Join(stateDir, "session.json")

	jsonData, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, fmt.Errorf("reading session from %s: %w", sessionPath, err)
	}

	var data sessionData
	if err := json.Unmarshal(jsonData, &data); err != nil {
		for index := range jsonData {
			jsonData[index] = 0
		}
		return nil, fmt.Errorf("parsing session from %s: %w", sessionPath, err)
	}
	// Zero the raw JSON which contains the access token in plaintext.
	for index := range jsonData {
		jsonData[index] = 0
	}

	if data.AccessToken == "" {
		return nil, fmt.Errorf("session file %s has empty access token", sessionPath)
	}

	// Use the homeserver URL from the flag if provided, falling back to
	// the saved URL. This allows changing the URL without re-registering.
	serverURL := homeserverURL
	if serverURL == "" {
		serverURL = data.HomeserverURL
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: serverURL,
		Logger:        logger,
	})
	if err != nil {
		return nil, fmt.Errorf("creating matrix client: %w", err)
	}

	return client.SessionFromToken(data.UserID, data.AccessToken)
}

// managedSandbox tracks a running sandbox for a principal. A "sandbox" is the
// aggregate of proxy + tmux session + isolation envelope (bwrap) + command.
// The tmux session is the lifecycle boundary: when it ends (for any reason —
// command exits, user types exit, session killed), cleanup fires: the proxy
// is killed, sockets are removed, and the done channel closes.
type managedSandbox struct {
	localpart    string
	proxyProcess *os.Process         // credential injection proxy (killed on sandbox exit)
	configDir    string              // temp directory for proxy config, scripts, payload
	done         chan struct{}       // closed when the sandbox exits (tmux session ends)
	doneOnce     sync.Once           // protects close(done) from concurrent callers
	exitCode     int                 // command exit code (set before done is closed)
	exitError    error               // descriptive exit error (set before done is closed)
	roles        map[string][]string // role name → command, from SandboxSpec.Roles
}

// proxyCredentialPayload is the JSON structure piped to the proxy's stdin.
// This mirrors proxy.pipeCredentialPayload but lives here to avoid importing
// the proxy package — the launcher should not depend on the proxy.
type proxyCredentialPayload struct {
	MatrixHomeserverURL string               `json:"matrix_homeserver_url"`
	MatrixToken         string               `json:"matrix_token"`
	MatrixUserID        string               `json:"matrix_user_id"`
	Credentials         map[string]string    `json:"credentials"`
	MatrixPolicy        *schema.MatrixPolicy `json:"matrix_policy,omitempty"`
}

// Launcher handles IPC requests from the daemon. The serve loop accepts
// connections concurrently (each handleConnection runs in its own goroutine),
// so all mutable state is protected by mu. Immutable-after-startup fields
// (session, keypair, machineName, serverName, homeserverURL, runDir, stateDir,
// workspaceRoot, binaryHash, binaryPath) are set before the listener starts
// and are safe to read without the lock.
type Launcher struct {
	session       *messaging.Session
	keypair       *sealed.Keypair
	machineName   string
	serverName    string
	homeserverURL string
	runDir        string // runtime directory for sockets (e.g., /run/bureau)
	stateDir      string // persistent state directory (e.g., /var/lib/bureau)
	workspaceRoot string // root directory for project workspaces; the launcher ensures this and its .cache/ subdirectory exist
	binaryHash    string // SHA256 hex digest of the launcher binary, computed at startup
	binaryPath    string // absolute filesystem path of the running binary (for watchdog PreviousBinary)
	logger        *slog.Logger

	// mu serializes access to mutable state: sandboxes, failedExecPaths,
	// and proxyBinaryPath. Acquired at the top of handleConnection (after
	// decoding the request) so all IPC handler methods run under the lock.
	// Also acquired by shutdownAllSandboxes during graceful shutdown.
	mu              sync.Mutex
	proxyBinaryPath string
	sandboxes       map[string]*managedSandbox

	// failedExecPaths tracks binary paths where exec() has been attempted
	// and failed during this process lifetime. Prevents retry loops where
	// the daemon repeatedly sends exec-update for a broken binary.
	failedExecPaths map[string]bool

	// execFunc is the function called to replace the process image. Nil
	// means use syscall.Exec. Injectable for testing — the test can
	// capture the arguments without actually exec'ing.
	execFunc func(argv0 string, argv []string, envv []string) error
}

// serve accepts connections on the IPC socket and handles requests.
func (l *Launcher) serve(ctx context.Context, listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			// Check if the context was cancelled (shutdown).
			select {
			case <-ctx.Done():
				return
			default:
			}
			l.logger.Error("accept error", "error", err)
			continue
		}
		go l.handleConnection(ctx, conn)
	}
}

// IPCRequest is the JSON structure of a request from the daemon.
type IPCRequest struct {
	// Action is the request type: "create-sandbox", "destroy-sandbox",
	// "update-payload", "update-proxy-binary", or "status".
	Action string `json:"action"`

	// Principal is the localpart of the principal to operate on (for
	// create-sandbox and destroy-sandbox).
	Principal string `json:"principal,omitempty"`

	// EncryptedCredentials is the base64-encoded age ciphertext from
	// the m.bureau.credentials state event (for create-sandbox). The
	// launcher decrypts this using its keypair.
	//
	// Mutually exclusive with DirectCredentials — set one or neither.
	EncryptedCredentials string `json:"encrypted_credentials,omitempty"`

	// DirectCredentials provides plaintext credentials for the proxy
	// (for create-sandbox). Used when the daemon spawns ephemeral
	// sandboxes using its own Matrix session, bypassing age encryption.
	// Safe because the IPC socket is local-only and the daemon is a
	// trusted process on the same machine.
	//
	// Expected keys: MATRIX_TOKEN, MATRIX_USER_ID. Additional keys
	// (OPENAI_API_KEY, etc.) are forwarded to the proxy as external
	// credentials.
	//
	// Mutually exclusive with EncryptedCredentials — set one or neither.
	DirectCredentials map[string]string `json:"direct_credentials,omitempty"`

	// MatrixPolicy is the Matrix access policy for this principal's proxy.
	// Forwarded from the PrincipalAssignment in MachineConfig. The launcher
	// includes this in the credential payload piped to the proxy subprocess.
	MatrixPolicy *schema.MatrixPolicy `json:"matrix_policy,omitempty"`

	// SandboxSpec is the fully-resolved sandbox configuration produced by
	// the daemon's template resolution pipeline. When set, the launcher
	// uses this to build the bwrap command line and configure the sandbox
	// environment. When nil (current behavior), the launcher spawns only
	// the proxy process without a bwrap sandbox.
	SandboxSpec *schema.SandboxSpec `json:"sandbox_spec,omitempty"`

	// Payload is the new payload data for update-payload requests. The
	// launcher atomically rewrites the payload file that is bind-mounted
	// into the sandbox at /run/bureau/payload.json.
	Payload map[string]any `json:"payload,omitempty"`

	// TriggerContent is the raw JSON content of the state event that
	// satisfied the principal's StartCondition. Written to
	// /run/bureau/trigger.json inside the sandbox as a read-only bind
	// mount. Enables event-triggered principals to read context from
	// the event that caused their launch.
	//
	// nil when the principal has no StartCondition or when the condition
	// has no associated event content.
	TriggerContent json.RawMessage `json:"trigger_content,omitempty"`

	// BinaryPath is a filesystem path used by the "update-proxy-binary"
	// action. The launcher validates the path exists and is executable,
	// then switches to it for future sandbox creation. Existing proxy
	// processes continue running their current binary.
	BinaryPath string `json:"binary_path,omitempty"`
}

// IPCResponse is the JSON structure of a response to the daemon.
type IPCResponse struct {
	// OK indicates whether the request succeeded.
	OK bool `json:"ok"`

	// Error contains the error message if OK is false.
	Error string `json:"error,omitempty"`

	// ProxyPID is the PID of the spawned proxy process (for create-sandbox).
	ProxyPID int `json:"proxy_pid,omitempty"`

	// BinaryHash is the SHA256 hex digest of the launcher's own binary.
	// Returned by the "status" action so the daemon can compare against
	// the desired BureauVersion without restarting the launcher.
	BinaryHash string `json:"binary_hash,omitempty"`

	// ProxyBinaryPath is the filesystem path of the proxy binary the
	// launcher is currently using for new sandbox creation. Returned by
	// the "status" action so the daemon can hash-compare the current
	// proxy against the desired version.
	ProxyBinaryPath string `json:"proxy_binary_path,omitempty"`

	// ExitCode is the process exit code returned by "wait-sandbox".
	// Uses a pointer to distinguish "exit code 0" (success) from
	// "field not present" (non-wait-sandbox responses).
	ExitCode *int `json:"exit_code,omitempty"`
}

// handleConnection processes a single IPC request/response cycle.
func (l *Launcher) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Set a deadline for the entire request/response cycle.
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	var request IPCRequest
	if err := decoder.Decode(&request); err != nil {
		l.logger.Error("decoding IPC request", "error", err)
		encoder.Encode(IPCResponse{OK: false, Error: "invalid request"})
		return
	}

	l.logger.Info("IPC request", "action", request.Action, "principal", request.Principal)

	// wait-sandbox blocks until a sandbox process exits, potentially for
	// hours. Handle it before acquiring the mutex so that other IPC
	// requests (create-sandbox, destroy-sandbox, status, etc.) are not
	// blocked while a wait is pending.
	if request.Action == "wait-sandbox" {
		l.handleWaitSandbox(ctx, conn, encoder, &request)
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	var response IPCResponse
	switch request.Action {
	case "status":
		response = IPCResponse{
			OK:              true,
			BinaryHash:      l.binaryHash,
			ProxyBinaryPath: l.proxyBinaryPath,
		}

	case "create-sandbox":
		response = l.handleCreateSandbox(ctx, &request)

	case "destroy-sandbox":
		response = l.handleDestroySandbox(ctx, &request)

	case "update-payload":
		response = l.handleUpdatePayload(ctx, &request)

	case "update-proxy-binary":
		response = l.handleUpdateProxyBinary(ctx, &request)

	case "exec-update":
		response = l.handleExecUpdate(ctx, &request)
		// Send the response before a potential exec() — the daemon
		// needs to know the request was accepted before the process
		// image is replaced. On exec() success, this function never
		// returns. On exec() failure, we return normally and the
		// launcher continues serving.
		if err := encoder.Encode(response); err != nil {
			l.logger.Error("encoding IPC response", "error", err)
			return
		}
		if response.OK {
			// Explicitly close the connection to flush the response
			// before exec() replaces the process. The deferred
			// conn.Close() will see an already-closed connection and
			// return a harmless error.
			conn.Close()
			l.performExec(request.BinaryPath)
			// exec() failed if we reach here. performExec already
			// cleaned up state file, watchdog, and recorded the
			// failure. Continue serving.
		}
		return

	default:
		response = IPCResponse{OK: false, Error: fmt.Sprintf("unknown action: %q", request.Action)}
	}

	if err := encoder.Encode(response); err != nil {
		l.logger.Error("encoding IPC response", "error", err)
	}
}

// handleCreateSandbox validates a create-sandbox request, spawns a bureau-proxy
// process for the principal, and waits for it to become ready. The proxy's
// agent-facing socket and admin socket are placed under
// <run-dir>/principal/ and <run-dir>/admin/ respectively, mirroring the
// principal's localpart hierarchy.
func (l *Launcher) handleCreateSandbox(ctx context.Context, request *IPCRequest) IPCResponse {
	if request.Principal == "" {
		return IPCResponse{OK: false, Error: "principal is required"}
	}

	if err := principal.ValidateLocalpart(request.Principal); err != nil {
		return IPCResponse{OK: false, Error: fmt.Sprintf("invalid principal: %v", err)}
	}

	// Check if a sandbox is already running for this principal.
	if existing, exists := l.sandboxes[request.Principal]; exists {
		select {
		case <-existing.done:
			// Sandbox already finished — clean it up and allow recreation.
			l.cleanupSandbox(request.Principal)
		default:
			return IPCResponse{OK: false, Error: fmt.Sprintf("principal %q already has a running sandbox (proxy pid %d)", request.Principal, existing.proxyProcess.Pid)}
		}
	}

	// Resolve credentials: either decrypt an age ciphertext bundle or
	// use plaintext direct credentials from the daemon. At most one
	// source should be set — if both are set, EncryptedCredentials wins
	// (it's the normal path; DirectCredentials is the daemon-spawned
	// ephemeral path).
	var credentials map[string]string
	if request.EncryptedCredentials != "" {
		decrypted, err := sealed.Decrypt(request.EncryptedCredentials, l.keypair.PrivateKey)
		if err != nil {
			return IPCResponse{OK: false, Error: fmt.Sprintf("decrypting credentials: %v", err)}
		}
		defer decrypted.Close()

		if err := json.Unmarshal(decrypted.Bytes(), &credentials); err != nil {
			return IPCResponse{OK: false, Error: fmt.Sprintf("parsing decrypted credentials: %v", err)}
		}
	} else if len(request.DirectCredentials) > 0 {
		credentials = request.DirectCredentials
	}

	l.logger.Info("spawning proxy for principal",
		"principal", request.Principal,
		"credential_keys", credentialKeys(credentials),
		"has_sandbox_spec", request.SandboxSpec != nil,
	)

	pid, err := l.spawnProxy(request.Principal, credentials, request.MatrixPolicy)
	if err != nil {
		return IPCResponse{OK: false, Error: err.Error()}
	}

	// Build the sandbox command if a SandboxSpec was provided. When no
	// spec is present, the tmux session gets a bare shell (interactive
	// principal without a bwrap sandbox).
	var sandboxCommand []string
	if request.SandboxSpec != nil {
		sandboxCmd, setupErr := l.buildSandboxCommand(request.Principal, request.SandboxSpec, request.TriggerContent)
		if setupErr != nil {
			l.logger.Error("sandbox setup failed, rolling back proxy",
				"principal", request.Principal, "error", setupErr)
			if sb, exists := l.sandboxes[request.Principal]; exists {
				sb.proxyProcess.Kill()
				l.cleanupSandbox(request.Principal)
			}
			return IPCResponse{OK: false, Error: fmt.Sprintf("sandbox setup: %v", setupErr)}
		}
		sandboxCommand = sandboxCmd

		// Store roles for layout resolution.
		if sb, exists := l.sandboxes[request.Principal]; exists {
			sb.roles = request.SandboxSpec.Roles
		}
	}

	// Create a tmux session for observation of this principal.
	if err := l.createTmuxSession(request.Principal, sandboxCommand...); err != nil {
		l.logger.Error("tmux session creation failed, rolling back proxy",
			"principal", request.Principal, "error", err)
		if sb, exists := l.sandboxes[request.Principal]; exists {
			sb.proxyProcess.Kill()
			l.cleanupSandbox(request.Principal)
		}
		return IPCResponse{OK: false, Error: err.Error()}
	}

	// Start the session watcher that drives the sandbox lifecycle.
	// When the tmux session ends (for any reason), the watcher kills
	// the proxy and closes the sandbox's done channel.
	if sb, exists := l.sandboxes[request.Principal]; exists {
		l.startSessionWatcher(request.Principal, sb)
	}

	return IPCResponse{OK: true, ProxyPID: pid}
}

// handleDestroySandbox terminates a sandbox by killing its tmux session and
// proxy process, then cleans up config files and socket files.
func (l *Launcher) handleDestroySandbox(ctx context.Context, request *IPCRequest) IPCResponse {
	if request.Principal == "" {
		return IPCResponse{OK: false, Error: "principal is required"}
	}

	if err := principal.ValidateLocalpart(request.Principal); err != nil {
		return IPCResponse{OK: false, Error: fmt.Sprintf("invalid principal: %v", err)}
	}

	sb, exists := l.sandboxes[request.Principal]
	if !exists {
		return IPCResponse{OK: false, Error: fmt.Sprintf("no sandbox running for principal %q", request.Principal)}
	}

	l.logger.Info("destroying sandbox", "principal", request.Principal, "proxy_pid", sb.proxyProcess.Pid)

	// Kill the tmux session first. This stops the bwrap/command inside it.
	// The session watcher will also detect this and call finishSandbox,
	// but we call it explicitly below so the IPC response doesn't have to
	// wait for the next poll interval.
	l.destroyTmuxSession(request.Principal)

	// Close the done channel and kill the proxy. finishSandbox is
	// idempotent (via doneOnce), so it's safe even if the session
	// watcher fires concurrently.
	l.finishSandbox(sb, -1, fmt.Errorf("destroyed by IPC request"))

	// Wait for done to ensure everything has settled before cleanup.
	<-sb.done

	l.cleanupSandbox(request.Principal)

	l.logger.Info("sandbox destroyed", "principal", request.Principal)
	return IPCResponse{OK: true}
}

// handleWaitSandbox blocks until the named sandbox exits (tmux session
// ends), then responds with the exit code. This runs outside the launcher
// mutex so that other IPC requests are not blocked during the (potentially
// long) wait.
//
// The handler monitors both the sandbox's done channel and the IPC
// connection. If the daemon disconnects (crashes, restarts, context
// cancelled), the handler returns without sending a response.
func (l *Launcher) handleWaitSandbox(ctx context.Context, conn net.Conn, encoder *json.Encoder, request *IPCRequest) {
	if request.Principal == "" {
		encoder.Encode(IPCResponse{OK: false, Error: "principal is required"})
		return
	}

	if err := principal.ValidateLocalpart(request.Principal); err != nil {
		encoder.Encode(IPCResponse{OK: false, Error: fmt.Sprintf("invalid principal: %v", err)})
		return
	}

	// Look up the sandbox under the lock, then release immediately.
	l.mu.Lock()
	sandbox, exists := l.sandboxes[request.Principal]
	l.mu.Unlock()

	if !exists {
		encoder.Encode(IPCResponse{OK: false, Error: fmt.Sprintf("no sandbox running for principal %q", request.Principal)})
		return
	}

	// Clear the read/write deadline — wait-sandbox can block for hours
	// (pipeline execution time). The 30-second deadline set by
	// handleConnection is only appropriate for quick request-response
	// actions.
	conn.SetDeadline(time.Time{})

	// Monitor connection closure in a background goroutine. If the
	// daemon disconnects (crash, restart, context cancellation on the
	// daemon side), the Read returns an error and we close connClosed.
	// This prevents the handler from blocking on a dead connection.
	connClosed := make(chan struct{})
	go func() {
		buffer := make([]byte, 1)
		conn.Read(buffer)
		close(connClosed)
	}()

	select {
	case <-sandbox.done:
		exitCode := sandbox.exitCode
		response := IPCResponse{OK: true, ExitCode: &exitCode}
		if sandbox.exitError != nil {
			response.Error = sandbox.exitError.Error()
		}
		encoder.Encode(response)

	case <-connClosed:
		l.logger.Info("wait-sandbox: daemon disconnected",
			"principal", request.Principal)

	case <-ctx.Done():
		l.logger.Info("wait-sandbox: launcher shutting down",
			"principal", request.Principal)
	}
}

// handleUpdatePayload atomically rewrites the payload file for a running
// sandbox. The payload file is bind-mounted into the sandbox at
// /run/bureau/payload.json, so the update is immediately visible to the
// agent process. The agent can detect the change via inotify or periodic
// polling — Bureau does not send a signal to the process.
func (l *Launcher) handleUpdatePayload(ctx context.Context, request *IPCRequest) IPCResponse {
	if request.Principal == "" {
		return IPCResponse{OK: false, Error: "principal is required"}
	}

	if err := principal.ValidateLocalpart(request.Principal); err != nil {
		return IPCResponse{OK: false, Error: fmt.Sprintf("invalid principal: %v", err)}
	}

	sandbox, exists := l.sandboxes[request.Principal]
	if !exists {
		return IPCResponse{OK: false, Error: fmt.Sprintf("no sandbox running for principal %q", request.Principal)}
	}

	// Check if the sandbox has already exited.
	select {
	case <-sandbox.done:
		return IPCResponse{OK: false, Error: fmt.Sprintf("sandbox for %q has already exited", request.Principal)}
	default:
	}

	if request.Payload == nil {
		return IPCResponse{OK: false, Error: "payload is required for update-payload"}
	}

	// Write the payload atomically: write to a temp file in the same
	// directory, then rename. This ensures the agent never sees a
	// partially-written file.
	payloadJSON, err := json.Marshal(request.Payload)
	if err != nil {
		return IPCResponse{OK: false, Error: fmt.Sprintf("marshaling payload: %v", err)}
	}

	payloadPath := filepath.Join(sandbox.configDir, "payload.json")
	tempPath := payloadPath + ".tmp"
	if err := os.WriteFile(tempPath, payloadJSON, 0644); err != nil {
		return IPCResponse{OK: false, Error: fmt.Sprintf("writing temp payload: %v", err)}
	}
	if err := os.Rename(tempPath, payloadPath); err != nil {
		os.Remove(tempPath)
		return IPCResponse{OK: false, Error: fmt.Sprintf("renaming payload: %v", err)}
	}

	l.logger.Info("payload updated",
		"principal", request.Principal,
		"path", payloadPath,
	)

	return IPCResponse{OK: true}
}

// handleUpdateProxyBinary validates and applies a new proxy binary path. The
// daemon sends this when BureauVersion.ProxyStorePath changes. The launcher
// validates that the new path exists and is a regular file, then switches to
// it for future sandbox creation. Existing proxy processes are unaffected —
// they continue running their current binary until their sandbox is recycled.
func (l *Launcher) handleUpdateProxyBinary(ctx context.Context, request *IPCRequest) IPCResponse {
	if request.BinaryPath == "" {
		return IPCResponse{OK: false, Error: "binary_path is required for update-proxy-binary"}
	}

	info, err := os.Stat(request.BinaryPath)
	if err != nil {
		return IPCResponse{OK: false, Error: fmt.Sprintf("proxy binary path %q: %v", request.BinaryPath, err)}
	}
	if !info.Mode().IsRegular() {
		return IPCResponse{OK: false, Error: fmt.Sprintf("proxy binary path %q is not a regular file", request.BinaryPath)}
	}
	if info.Mode()&0111 == 0 {
		return IPCResponse{OK: false, Error: fmt.Sprintf("proxy binary path %q is not executable", request.BinaryPath)}
	}

	previousPath := l.proxyBinaryPath
	l.proxyBinaryPath = request.BinaryPath

	l.logger.Info("proxy binary path updated",
		"previous", previousPath,
		"new", request.BinaryPath,
	)

	return IPCResponse{OK: true}
}

// buildSandboxCommand converts a SandboxSpec into a shell script that exec's
// the bwrap sandbox. Returns the command to pass to tmux new-session (the
// script path). The bwrap arguments are built from the SandboxSpec's profile
// conversion, and a payload file is written if the spec includes a Payload.
// When triggerContent is non-nil, it is written to trigger.json and bind-mounted
// read-only at /run/bureau/trigger.json inside the sandbox.
//
// The returned command is a single-element slice containing the script path.
// The script handles all bwrap argument quoting internally, avoiding shell
// escaping issues when tmux invokes the command.
func (l *Launcher) buildSandboxCommand(principalLocalpart string, spec *schema.SandboxSpec, triggerContent json.RawMessage) ([]string, error) {
	// Find the sandbox's config directory (created by spawnProxy).
	sb, exists := l.sandboxes[principalLocalpart]
	if !exists {
		return nil, fmt.Errorf("no sandbox entry for %q (proxy must be spawned first)", principalLocalpart)
	}

	// Determine the proxy socket path (same as spawnProxy uses).
	proxySocketPath := principal.RunDirSocketPath(l.runDir, principalLocalpart)

	// Convert the SandboxSpec to a sandbox.Profile.
	profile := specToProfile(spec, proxySocketPath)

	// Expand template variables in the profile. Templates reference
	// workspace paths via ${PROJECT}, ${WORKTREE_PATH}, etc. — these
	// are resolved here at launch time when concrete values are known.
	// The SandboxSpec carries unexpanded variable references (documented
	// in its EnvironmentVariables doc comment); the launcher is where
	// expansion happens because it has the concrete values.
	vars := sandbox.Variables{
		"WORKSPACE_ROOT": l.workspaceRoot,
		"PROXY_SOCKET":   proxySocketPath,
		"TERM":           os.Getenv("TERM"),
	}
	project, worktreePath := workspaceContext(principalLocalpart)
	if project != "" {
		vars["PROJECT"] = project
		vars["WORKTREE_PATH"] = worktreePath
	}
	profile = vars.ExpandProfile(profile)

	// Handle payload: write to file and add a bind mount into the sandbox.
	if len(spec.Payload) > 0 {
		payloadPath, err := writePayloadFile(sb.configDir, spec.Payload)
		if err != nil {
			return nil, fmt.Errorf("writing payload: %w", err)
		}
		profile.Filesystem = append(profile.Filesystem, sandbox.Mount{
			Source: payloadPath,
			Dest:   "/run/bureau/payload.json",
			Mode:   sandbox.MountModeRO,
		})
	}

	// Handle trigger content: when a StartCondition was satisfied, the
	// daemon passes the matched event's content as raw JSON. Write it to
	// trigger.json and bind-mount it read-only at /run/bureau/trigger.json.
	// The pipeline executor reads this to provide EVENT_* variables.
	if len(triggerContent) > 0 {
		triggerPath, err := writeTriggerFile(sb.configDir, triggerContent)
		if err != nil {
			return nil, fmt.Errorf("writing trigger: %w", err)
		}
		profile.Filesystem = append(profile.Filesystem, sandbox.Mount{
			Source: triggerPath,
			Dest:   "/run/bureau/trigger.json",
			Mode:   sandbox.MountModeRO,
		})
	}

	// Find bwrap.
	bwrapPath, err := sandbox.BwrapPath()
	if err != nil {
		return nil, fmt.Errorf("locating bwrap: %w", err)
	}

	// Build bwrap arguments.
	builder := sandbox.NewBwrapBuilder()
	bwrapArgs, err := builder.Build(&sandbox.BwrapOptions{
		Profile:  profile,
		Command:  spec.Command,
		ClearEnv: true,
	})
	if err != nil {
		return nil, fmt.Errorf("building bwrap arguments: %w", err)
	}

	// Optionally wrap with systemd-run for resource limits.
	if profile.Resources.HasLimits() {
		scope := sandbox.NewSystemdScope("bureau-"+strings.ReplaceAll(principalLocalpart, "/", "-"), profile.Resources)
		fullCmd := append([]string{bwrapPath}, bwrapArgs...)
		wrappedCmd := scope.WrapCommand(fullCmd)
		// If systemd wrapped it, the first element is systemd-run.
		if wrappedCmd[0] != bwrapPath {
			bwrapPath = wrappedCmd[0]
			bwrapArgs = wrappedCmd[1:]
		}
	}

	// Write the sandbox script. The script runs bwrap, captures its exit
	// code to a file (read by the session watcher), and exits with the
	// same code.
	exitCodePath := filepath.Join(sb.configDir, "exit-code")
	scriptPath, err := writeSandboxScript(sb.configDir, bwrapPath, bwrapArgs, exitCodePath)
	if err != nil {
		return nil, fmt.Errorf("writing sandbox script: %w", err)
	}

	l.logger.Info("sandbox command built",
		"principal", principalLocalpart,
		"script", scriptPath,
		"bwrap", bwrapPath,
		"command", spec.Command,
	)

	return []string{scriptPath}, nil
}

// credentialKeys returns the key names from a credential map (for logging —
// never log the values).
func credentialKeys(credentials map[string]string) []string {
	keys := make([]string, 0, len(credentials))
	for key := range credentials {
		keys = append(keys, key)
	}
	return keys
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

// registerOrLogin registers a new account, or logs in if it already exists.
func registerOrLogin(ctx context.Context, client *messaging.Client, username, password, registrationToken string) (*messaging.Session, error) {
	session, err := client.Register(ctx, messaging.RegisterRequest{
		Username:          username,
		Password:          password,
		RegistrationToken: registrationToken,
	})
	if err == nil {
		return session, nil
	}

	if messaging.IsMatrixError(err, messaging.ErrCodeUserInUse) {
		slog.Info("account already exists, logging in", "username", username)
		return client.Login(ctx, username, password)
	}

	return nil, err
}

// derivePassword deterministically derives a password from the registration
// token and machine name. The result is returned in an mmap-backed buffer;
// the caller must close it. This makes re-registration idempotent — calling
// register with the same token and machine name always produces the same
// password, so the launcher can safely re-run first-boot against an account
// that already exists.
func derivePassword(registrationToken, machineName string) (*secret.Buffer, error) {
	hash := sha256.Sum256([]byte("bureau-machine-password:" + machineName + ":" + registrationToken))
	hexBytes := []byte(hex.EncodeToString(hash[:]))
	return secret.NewFromBytes(hexBytes)
}

// readSecret reads a secret from a file path, or from stdin if path is "-".
// The returned buffer is mmap-backed (locked into RAM, excluded from core
// dumps) and must be closed by the caller. Leading/trailing whitespace is
// trimmed before storing. Returns an error if the source is empty after
// trimming.
func readSecret(path string) (*secret.Buffer, error) {
	var data []byte

	if path == "-" {
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return nil, fmt.Errorf("reading stdin: %w", err)
			}
			return nil, fmt.Errorf("stdin is empty")
		}
		data = scanner.Bytes()
	} else {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		for index := range data {
			data[index] = 0
		}
		return nil, fmt.Errorf("secret is empty")
	}

	// NewFromBytes copies into mmap-backed memory and zeros trimmed.
	buffer, err := secret.NewFromBytes(trimmed)
	// Zero remaining bytes (whitespace prefix/suffix) not covered by trimmed.
	for index := range data {
		data[index] = 0
	}
	if err != nil {
		return nil, err
	}
	return buffer, nil
}

// spawnProxy creates a bureau-proxy subprocess for the given principal.
// It writes a minimal config file, pipes credentials via stdin, and waits
// for the proxy's agent socket to appear before returning.
//
// The proxy is infrastructure that exists for the sandbox's lifetime, not
// the other way around. spawnProxy creates the managedSandbox and stores
// it in l.sandboxes, but does NOT close sandbox.done when the proxy exits.
// The session watcher (started later by handleCreateSandbox) owns that.
//
// A background goroutine reaps the proxy process to avoid zombies and logs
// the exit, but the sandbox lifecycle is driven by the tmux session.
func (l *Launcher) spawnProxy(principalLocalpart string, credentials map[string]string, matrixPolicy *schema.MatrixPolicy) (int, error) {
	if l.proxyBinaryPath == "" {
		return 0, fmt.Errorf("proxy binary path not configured (set --proxy-binary or install bureau-proxy on PATH)")
	}

	// Determine socket paths for this principal.
	socketPath := principal.RunDirSocketPath(l.runDir, principalLocalpart)
	adminSocketPath := principal.RunDirAdminSocketPath(l.runDir, principalLocalpart)

	// Create a temp directory for the proxy config.
	sanitizedName := strings.ReplaceAll(principalLocalpart, "/", "-")
	configDir, err := os.MkdirTemp("", "bureau-proxy-"+sanitizedName+"-")
	if err != nil {
		return 0, fmt.Errorf("creating config directory: %w", err)
	}

	// Write the proxy config. The services map is empty because the daemon
	// registers services dynamically via the admin socket.
	configContent := fmt.Sprintf("socket_path: %s\nadmin_socket_path: %s\nservices: {}\n",
		socketPath, adminSocketPath)
	configPath := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(configContent), 0600); err != nil {
		os.RemoveAll(configDir)
		return 0, fmt.Errorf("writing proxy config: %w", err)
	}

	// Ensure socket parent directories exist.
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		os.RemoveAll(configDir)
		return 0, fmt.Errorf("creating socket directory for %s: %w", socketPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(adminSocketPath), 0755); err != nil {
		os.RemoveAll(configDir)
		return 0, fmt.Errorf("creating admin socket directory for %s: %w", adminSocketPath, err)
	}

	// Build command arguments. If credentials are provided, pipe them via stdin.
	args := []string{"-config", configPath}
	if credentials != nil {
		args = append(args, "-credential-stdin")
	}

	cmd := exec.Command(l.proxyBinaryPath, args...)
	cmd.Stderr = os.Stderr // proxy logs to stderr

	var stdinPipe io.WriteCloser
	if credentials != nil {
		stdinPipe, err = cmd.StdinPipe()
		if err != nil {
			os.RemoveAll(configDir)
			return 0, fmt.Errorf("creating stdin pipe: %w", err)
		}
	}

	if err := cmd.Start(); err != nil {
		os.RemoveAll(configDir)
		return 0, fmt.Errorf("starting proxy process: %w", err)
	}

	// Write credential payload to the proxy's stdin and close.
	if credentials != nil {
		payload, err := l.buildCredentialPayload(principalLocalpart, credentials, matrixPolicy)
		if err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			os.RemoveAll(configDir)
			return 0, fmt.Errorf("building credential payload: %w", err)
		}

		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			os.RemoveAll(configDir)
			return 0, fmt.Errorf("marshaling credential payload: %w", err)
		}

		if _, err := stdinPipe.Write(payloadJSON); err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			os.RemoveAll(configDir)
			return 0, fmt.Errorf("writing credentials to proxy stdin: %w", err)
		}
		stdinPipe.Close()
	}

	// Track the sandbox. The done channel is NOT closed when the proxy
	// exits — it is closed by the session watcher when the tmux session
	// ends, or by handleDestroySandbox.
	sb := &managedSandbox{
		localpart:    principalLocalpart,
		proxyProcess: cmd.Process,
		configDir:    configDir,
		done:         make(chan struct{}),
	}

	// proxyDone is a local signal for waitForSocket: if the proxy dies
	// before its socket appears, we need to fail fast rather than wait
	// the full 10 seconds.
	proxyDone := make(chan struct{})

	// Reap the proxy process in the background to avoid zombies. This
	// goroutine does NOT close sandbox.done — that is the session
	// watcher's responsibility.
	go func() {
		waitError := cmd.Wait()
		close(proxyDone)
		exitCode := 0
		if waitError != nil {
			var exitErr *exec.ExitError
			if errors.As(waitError, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
		}
		l.logger.Info("proxy process exited",
			"principal", principalLocalpart,
			"pid", cmd.Process.Pid,
			"exit_code", exitCode,
			"error", waitError,
		)
	}()

	// Wait for the proxy to become ready (agent socket file appears).
	if err := waitForSocket(socketPath, proxyDone, 10*time.Second); err != nil {
		cmd.Process.Kill()
		<-proxyDone
		os.RemoveAll(configDir)
		return 0, fmt.Errorf("proxy for %q: %w", principalLocalpart, err)
	}

	l.sandboxes[principalLocalpart] = sb

	l.logger.Info("proxy started",
		"principal", principalLocalpart,
		"pid", cmd.Process.Pid,
		"socket", socketPath,
		"admin_socket", adminSocketPath,
	)

	return cmd.Process.Pid, nil
}

// buildCredentialPayload restructures a flat credential map into the JSON
// structure expected by the proxy's PipeCredentialSource. The Matrix-specific
// keys (MATRIX_HOMESERVER_URL, MATRIX_TOKEN, MATRIX_USER_ID) are extracted
// into top-level fields; everything else goes under "credentials".
func (l *Launcher) buildCredentialPayload(principalLocalpart string, credentials map[string]string, matrixPolicy *schema.MatrixPolicy) (*proxyCredentialPayload, error) {
	homeserverURL := credentials["MATRIX_HOMESERVER_URL"]
	if homeserverURL == "" {
		// Fall back to the launcher's homeserver URL (the principal is
		// typically on the same homeserver as the machine).
		homeserverURL = l.homeserverURL
	}

	matrixToken := credentials["MATRIX_TOKEN"]
	if matrixToken == "" {
		return nil, fmt.Errorf("credential bundle missing MATRIX_TOKEN for principal %q", principalLocalpart)
	}

	matrixUserID := credentials["MATRIX_USER_ID"]
	if matrixUserID == "" {
		// Default to the principal's canonical Matrix user ID.
		matrixUserID = principal.MatrixUserID(principalLocalpart, l.serverName)
	}

	remaining := make(map[string]string, len(credentials))
	for key, value := range credentials {
		switch key {
		case "MATRIX_HOMESERVER_URL", "MATRIX_TOKEN", "MATRIX_USER_ID":
			continue
		default:
			remaining[key] = value
		}
	}

	return &proxyCredentialPayload{
		MatrixHomeserverURL: homeserverURL,
		MatrixToken:         matrixToken,
		MatrixUserID:        matrixUserID,
		Credentials:         remaining,
		MatrixPolicy:        matrixPolicy,
	}, nil
}

// waitForSocket polls for a unix socket file to appear on disk. Returns nil
// when the file exists, or an error if the process exits first or the timeout
// is reached.
func waitForSocket(socketPath string, processDone <-chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-processDone:
			return fmt.Errorf("process exited before socket %s appeared", socketPath)
		case <-ticker.C:
			if _, err := os.Stat(socketPath); err == nil {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timed out after %v waiting for socket %s", timeout, socketPath)
			}
		}
	}
}

// startSessionWatcher starts a background goroutine that polls the tmux
// session for the given principal. When the session disappears (for any
// reason — command exited, session killed, tmux server crashed), the
// goroutine reads the exit code file, kills the proxy, and closes the
// sandbox's done channel via finishSandbox.
//
// This is the single lifecycle driver for all sandboxes. There are no
// special cases for pipeline executors vs interactive agents vs human
// operators: when the tmux session ends, the sandbox is done.
func (l *Launcher) startSessionWatcher(localpart string, sb *managedSandbox) {
	tmuxSocket := principal.TmuxSocketPath(l.runDir)
	sessionName := tmuxSessionName(localpart)
	exitCodePath := filepath.Join(sb.configDir, "exit-code")

	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-sb.done:
				// Sandbox already finished (e.g., handleDestroySandbox
				// called finishSandbox before we detected the session gone).
				return
			case <-ticker.C:
			}

			cmd := exec.Command("tmux", "-S", tmuxSocket, "has-session", "-t", sessionName)
			if err := cmd.Run(); err != nil {
				// Session is gone. Read the exit code if the sandbox
				// script wrote one.
				exitCode := -1
				var exitError error
				if data, readErr := os.ReadFile(exitCodePath); readErr == nil {
					if code, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil {
						exitCode = code
					}
				}
				if exitCode != 0 {
					exitError = fmt.Errorf("sandbox command exited with code %d", exitCode)
				}

				l.logger.Info("tmux session ended",
					"principal", localpart,
					"session", sessionName,
					"exit_code", exitCode,
				)

				l.finishSandbox(sb, exitCode, exitError)
				return
			}
		}
	}()
}

// finishSandbox sets the exit code and error on a sandbox and closes its
// done channel. Safe to call from multiple goroutines (session watcher,
// handleDestroySandbox, shutdownAllSandboxes) — doneOnce ensures the
// channel is closed exactly once and the exit state is set atomically
// with the close.
//
// Also kills the proxy process if it is still running. The proxy is
// infrastructure for the sandbox; when the sandbox ends, the proxy has
// no purpose.
func (l *Launcher) finishSandbox(sb *managedSandbox, exitCode int, exitError error) {
	sb.doneOnce.Do(func() {
		sb.exitCode = exitCode
		sb.exitError = exitError
		close(sb.done)
	})

	// Kill the proxy. It may already be dead (if the sandbox command
	// outlived it, which shouldn't happen in normal operation, or if
	// the proxy was killed by something else). Signal errors are
	// expected and harmless.
	if sb.proxyProcess != nil {
		sb.proxyProcess.Kill()
	}
}

// cleanupSandbox removes a sandbox's temp config directory, socket files,
// and tmux session, then deletes it from the sandboxes map. The caller
// should ensure finishSandbox has been called (done channel is closed)
// before calling cleanup, but this is not strictly required — cleanup
// is filesystem-only and does not depend on process state.
func (l *Launcher) cleanupSandbox(principalLocalpart string) {
	sb, exists := l.sandboxes[principalLocalpart]
	if !exists {
		return
	}

	if sb.configDir != "" {
		os.RemoveAll(sb.configDir)
	}

	// Remove socket files (the proxy may have already removed them during
	// graceful shutdown, so ignore errors).
	socketPath := principal.RunDirSocketPath(l.runDir, principalLocalpart)
	adminSocketPath := principal.RunDirAdminSocketPath(l.runDir, principalLocalpart)
	os.Remove(socketPath)
	os.Remove(adminSocketPath)

	// Kill the principal's tmux session if it still exists. This is
	// belt-and-suspenders — the session should already be gone (either
	// the command exited naturally, or handleDestroySandbox/
	// shutdownAllSandboxes killed it). destroyTmuxSession logs a debug
	// message and moves on if the session is already gone.
	l.destroyTmuxSession(principalLocalpart)

	delete(l.sandboxes, principalLocalpart)
}

// shutdownAllSandboxes terminates all sandboxes by killing the tmux server
// and all proxy processes. Called during launcher shutdown. Acquires mu to
// serialize with any in-flight IPC handlers.
func (l *Launcher) shutdownAllSandboxes() {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Kill the Bureau tmux server entirely. This stops all tmux sessions
	// (and thus all bwrap commands) in one shot. Since Bureau owns this
	// tmux server (dedicated socket, not the user's tmux), this is safe.
	tmuxSocket := principal.TmuxSocketPath(l.runDir)
	cmd := exec.Command("tmux", "-S", tmuxSocket, "kill-server")
	if output, err := cmd.CombinedOutput(); err != nil {
		l.logger.Debug("killing tmux server (may already be stopped)",
			"socket", tmuxSocket, "error", err, "output", string(output))
	} else {
		l.logger.Info("tmux server stopped", "socket", tmuxSocket)
	}

	// Finish each sandbox: close done channels and kill proxy processes.
	for localpart, sb := range l.sandboxes {
		l.logger.Info("shutting down sandbox", "principal", localpart, "proxy_pid", sb.proxyProcess.Pid)
		l.finishSandbox(sb, -1, fmt.Errorf("launcher shutdown"))

		// Wait for the done channel (should be immediate since we just
		// called finishSandbox, but the session watcher goroutine may
		// have fired concurrently).
		<-sb.done
		l.cleanupSandbox(localpart)
	}
}

// tmuxSessionName returns the tmux session name for a principal.
// Bureau session names follow "bureau/<localpart>" — the slash in the
// localpart creates a natural hierarchy. Tmux treats "/" as a regular
// character in session names (only ":" and "." are delimiters in tmux
// target syntax).
func tmuxSessionName(localpart string) string {
	return "bureau/" + localpart
}

// createTmuxSession creates a detached tmux session for a principal on
// Bureau's dedicated tmux server. The session is configured with Bureau
// defaults: Ctrl-a prefix (distinct from the user's Ctrl-b), mouse support
// for relay pass-through, and generous scrollback for observation history.
//
// When command is provided, the tmux session runs that command instead of a
// bare shell. This is used for bwrap sandbox scripts — the command is the
// path to the script generated by buildSandboxCommand.
func (l *Launcher) createTmuxSession(localpart string, command ...string) error {
	tmuxSocket := principal.TmuxSocketPath(l.runDir)
	sessionName := tmuxSessionName(localpart)

	// Ensure the tmux server socket directory exists.
	if err := os.MkdirAll(filepath.Dir(tmuxSocket), 0755); err != nil {
		return fmt.Errorf("creating tmux socket directory: %w", err)
	}

	// Create a detached tmux session. The first new-session on this socket
	// also starts the tmux server. When a command is provided, tmux runs
	// it instead of the default shell.
	//
	// -f /dev/null prevents tmux from loading the user's ~/.tmux.conf,
	// which can crash the server with incompatible options. Bureau
	// configures its own options via configureTmuxSession after creation.
	args := []string{"-f", "/dev/null", "-S", tmuxSocket, "new-session", "-d", "-s", sessionName}
	args = append(args, command...)
	cmd := exec.Command("tmux", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("creating tmux session %q: %w (%s)",
			sessionName, err, strings.TrimSpace(string(output)))
	}

	l.configureTmuxSession(tmuxSocket, sessionName, localpart)

	l.logger.Info("tmux session created",
		"session", sessionName,
		"server_socket", tmuxSocket,
		"has_command", len(command) > 0,
	)
	return nil
}

// configureTmuxSession sets Bureau-standard tmux options on a session and
// the server. Global options (prefix, mouse, scrollback) are set with -g
// so all sessions on Bureau's dedicated server inherit them. Per-session
// options (status bar content) are set on the specific session.
func (l *Launcher) configureTmuxSession(tmuxSocket, sessionName, localpart string) {
	type tmuxOption struct {
		global bool
		key    string
		value  string
	}

	options := []tmuxOption{
		{global: true, key: "prefix", value: "C-a"},
		{global: true, key: "mouse", value: "on"},
		{global: true, key: "history-limit", value: "50000"},
		{global: false, key: "status-left", value: fmt.Sprintf(" %s ", localpart)},
	}

	for _, option := range options {
		var args []string
		if option.global {
			args = []string{"-S", tmuxSocket, "set-option", "-g", option.key, option.value}
		} else {
			args = []string{"-S", tmuxSocket, "set-option", "-t", sessionName, option.key, option.value}
		}
		cmd := exec.Command("tmux", args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			l.logger.Warn("setting tmux option failed",
				"session", sessionName, "option", option.key,
				"error", err, "output", strings.TrimSpace(string(output)))
		}
	}
}

// destroyTmuxSession kills a principal's tmux session on Bureau's dedicated
// tmux server. Called during sandbox cleanup. Failures are logged as warnings
// because the session may already have been killed (the shell exited, or the
// session was never created due to an earlier failure).
func (l *Launcher) destroyTmuxSession(localpart string) {
	sessionName := tmuxSessionName(localpart)
	cmd := exec.Command("tmux", "-S", principal.TmuxSocketPath(l.runDir),
		"kill-session", "-t", sessionName)
	if output, err := cmd.CombinedOutput(); err != nil {
		l.logger.Debug("killing tmux session (may already be gone)",
			"session", sessionName, "error", err,
			"output", strings.TrimSpace(string(output)))
	} else {
		l.logger.Info("tmux session destroyed", "session", sessionName)
	}
}

// findProxyBinary looks for the bureau-proxy binary next to the launcher
// binary, then on PATH. Returns "bureau-proxy" (bare name) if not found —
// the error will surface at spawn time with a clear message.
func findProxyBinary(logger *slog.Logger) string {
	// Check next to the launcher binary.
	executable, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(executable), "bureau-proxy")
		if _, err := os.Stat(candidate); err == nil {
			logger.Info("found proxy binary next to launcher", "path", candidate)
			return candidate
		}
	}

	// Check PATH.
	path, err := exec.LookPath("bureau-proxy")
	if err == nil {
		logger.Info("found proxy binary on PATH", "path", path)
		return path
	}

	logger.Warn("bureau-proxy not found; sandbox creation will fail until it is installed")
	return "bureau-proxy"
}
