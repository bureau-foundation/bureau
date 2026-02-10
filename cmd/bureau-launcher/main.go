// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-launcher is the privileged Bureau process. It manages machine
// identity, credential decryption, and sandbox lifecycle. It has no network
// access — all external communication flows through bureau-daemon, which
// communicates with the launcher via a unix socket.
//
// On first boot, the launcher generates an age keypair, registers a Matrix
// account for the machine, and publishes the machine's public key. On every
// boot, it loads the keypair and listens for lifecycle requests from the
// daemon.
//
// See CREDENTIALS.md for the full security model and privilege separation
// design.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/messaging"
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
		machineName           string
		serverName            string
		stateDir              string
		socketPath            string
		proxyBinaryPath       string
		socketBasePath        string
		adminBasePath         string
		showVersion           bool
	)

	flag.StringVar(&homeserverURL, "homeserver", "http://localhost:6167", "Matrix homeserver URL")
	flag.StringVar(&registrationTokenFile, "registration-token-file", "", "path to file containing registration token, or - for stdin (first boot only)")
	flag.StringVar(&machineName, "machine-name", "", "machine localpart (e.g., machine/workstation) (required)")
	flag.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
	flag.StringVar(&stateDir, "state-dir", "/var/lib/bureau", "directory for persistent state (keypair, etc.)")
	flag.StringVar(&socketPath, "socket", "/run/bureau/launcher.sock", "path for the launcher IPC socket")
	flag.StringVar(&proxyBinaryPath, "proxy-binary", "", "path to bureau-proxy binary (auto-detected if empty)")
	flag.StringVar(&socketBasePath, "socket-base-path", principal.SocketBasePath, "base directory for principal agent sockets")
	flag.StringVar(&adminBasePath, "admin-base-path", principal.AdminSocketBasePath, "base directory for principal admin sockets")
	flag.BoolVar(&showVersion, "version", false, "print version information and exit")
	flag.Parse()

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

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Load or generate the machine keypair.
	keypair, firstBoot, err := loadOrGenerateKeypair(stateDir, logger)
	if err != nil {
		return fmt.Errorf("keypair: %w", err)
	}
	logger.Info("machine keypair loaded",
		"public_key", keypair.PublicKey,
		"first_boot", firstBoot,
	)

	// On first boot, register the machine with Matrix and publish its key.
	if firstBoot {
		if registrationTokenFile == "" {
			return fmt.Errorf("--registration-token-file is required on first boot")
		}
		if err := firstBootSetup(ctx, homeserverURL, registrationTokenFile, machineName, serverName, keypair, stateDir, logger); err != nil {
			return fmt.Errorf("first boot setup: %w", err)
		}
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

	// Start the IPC socket for daemon communication.
	launcher := &Launcher{
		session:         session,
		keypair:         keypair,
		machineName:     machineName,
		serverName:      serverName,
		homeserverURL:   homeserverURL,
		stateDir:        stateDir,
		proxyBinaryPath: proxyBinaryPath,
		socketBasePath:  socketBasePath,
		adminBasePath:   adminBasePath,
		sandboxes:       make(map[string]*managedSandbox),
		logger:          logger,
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
			return nil, false, fmt.Errorf("private key exists but public key missing at %s: %w", publicKeyPath, err)
		}

		privateKey := strings.TrimSpace(string(privateKeyData))
		publicKey := strings.TrimSpace(string(publicKeyData))

		if err := sealed.ParsePrivateKey(privateKey); err != nil {
			return nil, false, fmt.Errorf("stored private key is invalid: %w", err)
		}
		if err := sealed.ParsePublicKey(publicKey); err != nil {
			return nil, false, fmt.Errorf("stored public key is invalid: %w", err)
		}

		return &sealed.Keypair{PrivateKey: privateKey, PublicKey: publicKey}, false, nil
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
		return nil, false, fmt.Errorf("creating state directory %s: %w", stateDir, err)
	}

	// Write the private key (0600 — owner-only read/write).
	if err := os.WriteFile(privateKeyPath, []byte(keypair.PrivateKey), 0600); err != nil {
		return nil, false, fmt.Errorf("writing private key to %s: %w", privateKeyPath, err)
	}

	// Write the public key (0644 — readable by all, this is public data).
	if err := os.WriteFile(publicKeyPath, []byte(keypair.PublicKey), 0644); err != nil {
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
	if registrationToken == "" {
		return fmt.Errorf("registration token is empty")
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
		Logger:        logger,
	})
	if err != nil {
		return fmt.Errorf("creating matrix client: %w", err)
	}

	// Register the machine account. Use the registration token as the password
	// (deterministic, so re-running with the same token is idempotent).
	password := derivePassword(registrationToken, machineName)
	session, err := registerOrLogin(ctx, client, machineName, password, registrationToken)
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

	// Save the session for subsequent boots.
	if err := saveSession(stateDir, homeserverURL, session); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}

	return nil
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
		return nil, fmt.Errorf("parsing session from %s: %w", sessionPath, err)
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

	return client.SessionFromToken(data.UserID, data.AccessToken), nil
}

// managedSandbox tracks a running proxy process for a principal.
type managedSandbox struct {
	principal string
	process   *os.Process
	configDir string       // temp directory containing proxy config
	done      chan struct{} // closed when process exits
	exitError error        // set when process exits
}

// proxyCredentialPayload is the JSON structure piped to the proxy's stdin.
// This mirrors proxy.pipeCredentialPayload but lives here to avoid importing
// the proxy package — the launcher should not depend on the proxy.
type proxyCredentialPayload struct {
	MatrixHomeserverURL string            `json:"matrix_homeserver_url"`
	MatrixToken         string            `json:"matrix_token"`
	MatrixUserID        string            `json:"matrix_user_id"`
	Credentials         map[string]string `json:"credentials"`
}

// Launcher handles IPC requests from the daemon.
type Launcher struct {
	session         *messaging.Session
	keypair         *sealed.Keypair
	machineName     string
	serverName      string
	homeserverURL   string
	stateDir        string
	proxyBinaryPath string
	socketBasePath  string // base directory for agent sockets (e.g., /run/bureau/principal/)
	adminBasePath   string // base directory for admin sockets (e.g., /run/bureau/admin/)
	sandboxes       map[string]*managedSandbox
	logger          *slog.Logger
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
	// Action is the request type: "create-sandbox", "destroy-sandbox", "status".
	Action string `json:"action"`

	// Principal is the localpart of the principal to operate on (for
	// create-sandbox and destroy-sandbox).
	Principal string `json:"principal,omitempty"`

	// EncryptedCredentials is the base64-encoded age ciphertext from
	// the m.bureau.credentials state event (for create-sandbox).
	EncryptedCredentials string `json:"encrypted_credentials,omitempty"`
}

// IPCResponse is the JSON structure of a response to the daemon.
type IPCResponse struct {
	// OK indicates whether the request succeeded.
	OK bool `json:"ok"`

	// Error contains the error message if OK is false.
	Error string `json:"error,omitempty"`

	// ProxyPID is the PID of the spawned proxy process (for create-sandbox).
	ProxyPID int `json:"proxy_pid,omitempty"`
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

	var response IPCResponse
	switch request.Action {
	case "status":
		response = IPCResponse{OK: true}

	case "create-sandbox":
		response = l.handleCreateSandbox(ctx, &request)

	case "destroy-sandbox":
		response = l.handleDestroySandbox(ctx, &request)

	default:
		response = IPCResponse{OK: false, Error: fmt.Sprintf("unknown action: %q", request.Action)}
	}

	if err := encoder.Encode(response); err != nil {
		l.logger.Error("encoding IPC response", "error", err)
	}
}

// handleCreateSandbox validates a create-sandbox request, spawns a bureau-proxy
// process for the principal, and waits for it to become ready. The proxy's
// agent-facing socket and admin socket are placed under socketBasePath and
// adminBasePath respectively, mirroring the principal's localpart hierarchy.
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
			// Process already exited — clean it up and allow recreation.
			l.cleanupSandbox(request.Principal)
		default:
			return IPCResponse{OK: false, Error: fmt.Sprintf("principal %q already has a running sandbox (pid %d)", request.Principal, existing.process.Pid)}
		}
	}

	// Decrypt the credential bundle if provided.
	var credentials map[string]string
	if request.EncryptedCredentials != "" {
		decrypted, err := sealed.Decrypt(request.EncryptedCredentials, l.keypair.PrivateKey)
		if err != nil {
			return IPCResponse{OK: false, Error: fmt.Sprintf("decrypting credentials: %v", err)}
		}

		if err := json.Unmarshal(decrypted, &credentials); err != nil {
			return IPCResponse{OK: false, Error: fmt.Sprintf("parsing decrypted credentials: %v", err)}
		}

		// Zero the decrypted bytes.
		for i := range decrypted {
			decrypted[i] = 0
		}
	}

	l.logger.Info("spawning proxy for principal",
		"principal", request.Principal,
		"credential_keys", credentialKeys(credentials),
	)

	pid, err := l.spawnProxy(request.Principal, credentials)
	if err != nil {
		return IPCResponse{OK: false, Error: err.Error()}
	}

	return IPCResponse{OK: true, ProxyPID: pid}
}

// handleDestroySandbox terminates the proxy process for a principal, waits for
// it to exit, and cleans up config files and socket files.
func (l *Launcher) handleDestroySandbox(ctx context.Context, request *IPCRequest) IPCResponse {
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

	l.logger.Info("destroying sandbox", "principal", request.Principal, "pid", sandbox.process.Pid)

	// Send SIGTERM for graceful shutdown.
	if err := sandbox.process.Signal(syscall.SIGTERM); err != nil {
		l.logger.Warn("SIGTERM failed (process may have already exited)", "principal", request.Principal, "error", err)
	}

	// Wait for exit with timeout.
	select {
	case <-sandbox.done:
		// Process exited.
	case <-time.After(10 * time.Second):
		l.logger.Warn("proxy did not exit after SIGTERM, sending SIGKILL", "principal", request.Principal)
		sandbox.process.Kill()
		<-sandbox.done
	}

	l.cleanupSandbox(request.Principal)

	l.logger.Info("sandbox destroyed", "principal", request.Principal)
	return IPCResponse{OK: true}
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
// token and machine name. This makes re-registration idempotent — calling
// register with the same token and machine name always produces the same
// password, so the launcher can safely re-run first-boot against an account
// that already exists.
func derivePassword(registrationToken, machineName string) string {
	hash := sha256.Sum256([]byte("bureau-machine-password:" + machineName + ":" + registrationToken))
	return hex.EncodeToString(hash[:])
}

// readSecret reads a secret from a file path, or from stdin if path is "-".
// The returned string has leading/trailing whitespace trimmed (including the
// trailing newline that most files and echo commands produce).
func readSecret(path string) (string, error) {
	if path == "-" {
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", fmt.Errorf("reading stdin: %w", err)
			}
			return "", fmt.Errorf("stdin is empty")
		}
		return strings.TrimSpace(scanner.Text()), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// spawnProxy creates a bureau-proxy subprocess for the given principal.
// It writes a minimal config file, pipes credentials via stdin, and waits
// for the proxy's agent socket to appear before returning. The proxy process
// is tracked in l.sandboxes for later destruction.
func (l *Launcher) spawnProxy(principalLocalpart string, credentials map[string]string) (int, error) {
	if l.proxyBinaryPath == "" {
		return 0, fmt.Errorf("proxy binary path not configured (set --proxy-binary or install bureau-proxy on PATH)")
	}

	// Determine socket paths for this principal.
	socketPath := l.socketBasePath + principalLocalpart + principal.SocketSuffix
	adminSocketPath := l.adminBasePath + principalLocalpart + principal.SocketSuffix

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
		payload, err := l.buildCredentialPayload(principalLocalpart, credentials)
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

	// Track the sandbox.
	sandbox := &managedSandbox{
		principal: principalLocalpart,
		process:   cmd.Process,
		configDir: configDir,
		done:      make(chan struct{}),
	}

	// Wait for the process in the background to collect exit status.
	go func() {
		sandbox.exitError = cmd.Wait()
		close(sandbox.done)
		l.logger.Info("proxy process exited",
			"principal", principalLocalpart,
			"pid", cmd.Process.Pid,
			"error", sandbox.exitError,
		)
	}()

	// Wait for the proxy to become ready (agent socket file appears).
	if err := waitForSocket(socketPath, sandbox.done, 10*time.Second); err != nil {
		cmd.Process.Kill()
		<-sandbox.done
		os.RemoveAll(configDir)
		return 0, fmt.Errorf("proxy for %q: %w", principalLocalpart, err)
	}

	l.sandboxes[principalLocalpart] = sandbox

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
func (l *Launcher) buildCredentialPayload(principalLocalpart string, credentials map[string]string) (*proxyCredentialPayload, error) {
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

// cleanupSandbox removes a sandbox's temp config directory and socket files,
// and deletes it from the sandboxes map. The caller must ensure the process
// has already exited.
func (l *Launcher) cleanupSandbox(principalLocalpart string) {
	sandbox, exists := l.sandboxes[principalLocalpart]
	if !exists {
		return
	}

	if sandbox.configDir != "" {
		os.RemoveAll(sandbox.configDir)
	}

	// Remove socket files (the proxy may have already removed them during
	// graceful shutdown, so ignore errors).
	socketPath := l.socketBasePath + principalLocalpart + principal.SocketSuffix
	adminSocketPath := l.adminBasePath + principalLocalpart + principal.SocketSuffix
	os.Remove(socketPath)
	os.Remove(adminSocketPath)

	delete(l.sandboxes, principalLocalpart)
}

// shutdownAllSandboxes terminates all running proxy processes. Called during
// launcher shutdown.
func (l *Launcher) shutdownAllSandboxes() {
	for principalLocalpart, sandbox := range l.sandboxes {
		l.logger.Info("shutting down sandbox", "principal", principalLocalpart, "pid", sandbox.process.Pid)

		if err := sandbox.process.Signal(syscall.SIGTERM); err != nil {
			l.logger.Warn("SIGTERM failed", "principal", principalLocalpart, "error", err)
		}

		select {
		case <-sandbox.done:
			// Exited cleanly.
		case <-time.After(5 * time.Second):
			l.logger.Warn("killing proxy after timeout", "principal", principalLocalpart)
			sandbox.process.Kill()
			<-sandbox.done
		}

		l.cleanupSandbox(principalLocalpart)
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
