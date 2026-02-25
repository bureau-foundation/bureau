// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ipc"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/tmux"
	"github.com/bureau-foundation/bureau/messaging"
)

// managedSandbox tracks a running sandbox for a principal. A "sandbox" is the
// aggregate of proxy + tmux session + isolation envelope (bwrap) + command.
// The tmux session is the lifecycle boundary: when it ends (for any reason —
// command exits, user types exit, session killed), cleanup fires: the proxy
// is killed, sockets are removed, and the done channel closes.
type managedSandbox struct {
	localpart        string
	proxyProcess     *os.Process         // credential injection proxy (killed on sandbox exit)
	configDir        string              // temp directory for proxy config, scripts, payload
	exitCodeFilePath string              // path to exit-code file written by bureau-log-relay; empty for bare shells and reconnected sandboxes
	done             chan struct{}       // closed when the sandbox exits (tmux session ends)
	doneOnce         sync.Once           // protects close(done) from concurrent callers
	exitCode         int                 // command exit code (set before done is closed)
	exitError        error               // descriptive exit error (set before done is closed)
	exitOutput       string              // captured terminal output from tmux pane (set before done is closed)
	roles            map[string][]string // role name → command, from SandboxSpec.Roles
	proxyDone        chan struct{}       // closed when the proxy process exits
	proxyExitCode    int                 // proxy exit code (set before proxyDone is closed)
}

// Launcher handles IPC requests from the daemon. The serve loop accepts
// connections concurrently (each handleConnection runs in its own goroutine),
// so all mutable state is protected by mu. Immutable-after-startup fields
// (session, keypair, machine, homeserverURL, runDir, stateDir, workspaceRoot,
// cacheRoot, launcherBinaryHash, launcherBinaryPath) are set before the
// listener starts and are safe to read without the lock.
type Launcher struct {
	session            *messaging.DirectSession
	keypair            *sealed.Keypair
	machine            ref.Machine
	homeserverURL      string
	runDir             string       // base runtime directory (e.g., /run/bureau)
	fleetRunDir        string       // fleet-scoped runtime directory (e.g., /run/bureau/fleet/prod), computed from machine.Fleet().RunDir(runDir)
	stateDir           string       // persistent state directory (e.g., /var/lib/bureau)
	workspaceRoot      string       // root directory for project workspaces; the launcher ensures this and its .cache/ subdirectory exist
	cacheRoot          string       // root directory for machine-level tool/model cache; sysadmin has rw, agents get ro subdirectory mounts
	launcherBinaryHash string       // SHA256 hex digest of the launcher binary, computed at startup for version management
	launcherBinaryPath string       // absolute filesystem path of the running launcher binary (for watchdog PreviousBinary)
	logRelayBinaryPath string       // path to bureau-log-relay binary; wraps sandbox commands to hold the outer PTY open until exit code is collected
	tmuxServer         *tmux.Server // Bureau's dedicated tmux server (socket at <runDir>/tmux.sock, -f /dev/null)
	operatorsGID       int          // GID of bureau-operators group (-1 if not found); used to chown service listen sockets for operator access
	logger             *slog.Logger

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

// Type aliases for the shared IPC types. The canonical definitions live
// in lib/ipc; these aliases keep the rest of this file unchanged.
type (
	IPCRequest       = ipc.Request
	IPCResponse      = ipc.Response
	ServiceMount     = ipc.ServiceMount
	SandboxListEntry = ipc.SandboxListEntry
)

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

// handleConnection processes a single IPC request/response cycle.
func (l *Launcher) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Set a deadline for the entire request/response cycle.
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	decoder := codec.NewDecoder(conn)
	encoder := codec.NewEncoder(conn)

	var request IPCRequest
	if err := decoder.Decode(&request); err != nil {
		l.logger.Error("decoding IPC request", "error", err)
		if err := encoder.Encode(IPCResponse{OK: false, Error: "invalid request"}); err != nil {
			l.logger.Error("encoding IPC error response", "error", err)
		}
		return
	}

	l.logger.Info("IPC request", "action", request.Action, "principal", request.Principal)

	// wait-sandbox and wait-proxy block until a sandbox or proxy process
	// exits, potentially for hours. Handle them before acquiring the
	// mutex so that other IPC requests (create-sandbox, destroy-sandbox,
	// status, etc.) are not blocked while a wait is pending.
	if request.Action == ipc.ActionWaitSandbox {
		l.handleWaitSandbox(ctx, conn, encoder, &request)
		return
	}
	if request.Action == ipc.ActionWaitProxy {
		l.handleWaitProxy(ctx, conn, encoder, &request)
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	var response IPCResponse
	switch request.Action {
	case ipc.ActionStatus:
		response = IPCResponse{
			OK:              true,
			BinaryHash:      l.launcherBinaryHash,
			ProxyBinaryPath: l.proxyBinaryPath,
		}

	case ipc.ActionListSandboxes:
		response = l.handleListSandboxes()

	case ipc.ActionCreateSandbox:
		response = l.handleCreateSandbox(ctx, &request)

	case ipc.ActionDestroySandbox:
		response = l.handleDestroySandbox(ctx, &request)

	case ipc.ActionSignalSandbox:
		response = l.handleSignalSandbox(&request)

	case ipc.ActionUpdatePayload:
		response = l.handleUpdatePayload(ctx, &request)

	case ipc.ActionUpdateProxyBinary:
		response = l.handleUpdateProxyBinary(ctx, &request)

	case ipc.ActionProvisionCredential:
		response = l.handleProvisionCredential(&request)

	case ipc.ActionExecUpdate:
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
// agent-facing socket (<run-dir>/<localpart>.sock) and admin socket
// (<run-dir>/<localpart>.admin.sock) mirror the principal's localpart
// hierarchy under the run directory.
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

	pid, err := l.spawnProxy(request.Principal, credentials, request.Grants)
	if err != nil {
		return IPCResponse{OK: false, Error: err.Error()}
	}

	// Build the sandbox command if a SandboxSpec was provided. When no
	// spec is present, the tmux session gets a bare shell (interactive
	// principal without a bwrap sandbox).
	var sandboxCommand []string
	if request.SandboxSpec != nil {
		sandboxCmd, setupErr := l.buildSandboxCommand(request.Principal, request.SandboxSpec, request.TriggerContent, request.ServiceMounts, request.TokenDirectory)
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

// handleListSandboxes returns all currently running sandboxes. The daemon
// uses this on startup to discover principals that survived a daemon restart
// while the launcher continued running. Only sandboxes whose process is still
// alive (done channel not closed) are included.
func (l *Launcher) handleListSandboxes() IPCResponse {
	var entries []SandboxListEntry
	for localpart, sandbox := range l.sandboxes {
		select {
		case <-sandbox.done:
			continue // Already exited — not reported as running.
		default:
		}
		entries = append(entries, SandboxListEntry{
			Localpart: localpart,
			ProxyPID:  sandbox.proxyProcess.Pid,
		})
	}
	return IPCResponse{OK: true, Sandboxes: entries}
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
	// watcher fires concurrently. No output capture — the session was
	// killed before we could capture, and explicit destruction is not
	// an error path that needs diagnostics.
	l.finishSandbox(sb, -1, fmt.Errorf("destroyed by IPC request"), "")

	// Wait for done to ensure everything has settled before cleanup.
	<-sb.done

	l.cleanupSandbox(request.Principal)

	l.logger.Info("sandbox destroyed", "principal", request.Principal)
	return IPCResponse{OK: true}
}

// handleSignalSandbox sends a signal to a running sandbox's process. The
// daemon uses this for graceful drain: send SIGTERM to give the agent time
// to post agent-complete and write checkpoints before force-killing via
// destroy-sandbox after the grace period.
//
// The signal is delivered to the pane PID (which is the bwrap process,
// thanks to the exec in sandbox.sh). Bwrap forwards SIGTERM to the
// sandboxed process via its PID-1 signal helper.
func (l *Launcher) handleSignalSandbox(request *IPCRequest) IPCResponse {
	if request.Principal == "" {
		return IPCResponse{OK: false, Error: "principal is required"}
	}

	if err := principal.ValidateLocalpart(request.Principal); err != nil {
		return IPCResponse{OK: false, Error: fmt.Sprintf("invalid principal: %v", err)}
	}

	if request.Signal == 0 {
		return IPCResponse{OK: false, Error: "signal is required (e.g., 15 for SIGTERM)"}
	}

	sb, exists := l.sandboxes[request.Principal]
	if !exists {
		return IPCResponse{OK: false, Error: fmt.Sprintf("no sandbox running for principal %q", request.Principal)}
	}

	// Check if the sandbox has already exited.
	select {
	case <-sb.done:
		return IPCResponse{OK: false, Error: fmt.Sprintf("sandbox for %q has already exited", request.Principal)}
	default:
	}

	sessionName := tmuxSessionName(request.Principal)
	signal := syscall.Signal(request.Signal)

	if err := l.tmuxServer.SignalPane(sessionName, signal); err != nil {
		return IPCResponse{OK: false, Error: fmt.Sprintf("signaling sandbox %q: %v", request.Principal, err)}
	}

	l.logger.Info("signal sent to sandbox",
		"principal", request.Principal,
		"signal", request.Signal,
		"session", sessionName,
	)

	return IPCResponse{OK: true}
}

// watchDisconnect monitors an IPC connection for closure in a background
// goroutine. Returns a channel that is closed when the peer disconnects
// (the Read returns any error, including EOF). Used by the wait-sandbox
// and wait-proxy handlers to detect daemon crashes during long-running waits.
func watchDisconnect(conn net.Conn) <-chan struct{} {
	closed := make(chan struct{})
	go func() {
		buffer := make([]byte, 1)
		conn.Read(buffer)
		close(closed)
	}()
	return closed
}

// handleWaitSandbox blocks until the named sandbox exits (tmux session
// ends), then responds with the exit code. This runs outside the launcher
// mutex so that other IPC requests are not blocked during the (potentially
// long) wait.
//
// The handler monitors both the sandbox's done channel and the IPC
// connection. If the daemon disconnects (crashes, restarts, context
// cancelled), the handler returns without sending a response.
func (l *Launcher) handleWaitSandbox(ctx context.Context, conn net.Conn, encoder *codec.Encoder, request *IPCRequest) {
	if request.Principal == "" {
		if err := encoder.Encode(IPCResponse{OK: false, Error: "principal is required"}); err != nil {
			l.logger.Error("encoding IPC response", "error", err, "action", "wait-sandbox")
		}
		return
	}

	if err := principal.ValidateLocalpart(request.Principal); err != nil {
		if encodeErr := encoder.Encode(IPCResponse{OK: false, Error: fmt.Sprintf("invalid principal: %v", err)}); encodeErr != nil {
			l.logger.Error("encoding IPC response", "error", encodeErr, "action", "wait-sandbox")
		}
		return
	}

	// Look up the sandbox under the lock, then release immediately.
	l.mu.Lock()
	sandbox, exists := l.sandboxes[request.Principal]
	l.mu.Unlock()

	if !exists {
		if err := encoder.Encode(IPCResponse{OK: false, Error: fmt.Sprintf("no sandbox running for principal %q", request.Principal)}); err != nil {
			l.logger.Error("encoding IPC response", "error", err, "action", "wait-sandbox", "principal", request.Principal)
		}
		return
	}

	// Clear the read/write deadline — wait-sandbox can block for hours
	// (pipeline execution time). The 30-second deadline set by
	// handleConnection is only appropriate for quick request-response
	// actions.
	conn.SetDeadline(time.Time{})

	connClosed := watchDisconnect(conn)

	select {
	case <-sandbox.done:
		exitCode := sandbox.exitCode
		response := IPCResponse{OK: true, ExitCode: &exitCode}
		if sandbox.exitError != nil {
			response.Error = sandbox.exitError.Error()
		}
		if sandbox.exitOutput != "" {
			response.Output = sandbox.exitOutput
		}
		if err := encoder.Encode(response); err != nil {
			l.logger.Error("encoding wait-sandbox result", "error", err,
				"principal", request.Principal, "exit_code", exitCode)
		}

	case <-connClosed:
		l.logger.Info("wait-sandbox: daemon disconnected",
			"principal", request.Principal)

	case <-ctx.Done():
		l.logger.Info("wait-sandbox: launcher shutting down",
			"principal", request.Principal)
	}
}

// handleWaitProxy blocks until the named sandbox's proxy process exits,
// then responds with the exit code. This mirrors handleWaitSandbox but
// watches the proxy process instead of the tmux session, enabling the
// daemon to detect proxy crashes even when the sandbox is still running.
//
// Runs outside the launcher mutex so that other IPC requests are not
// blocked during the (potentially long) wait.
func (l *Launcher) handleWaitProxy(ctx context.Context, conn net.Conn, encoder *codec.Encoder, request *IPCRequest) {
	if request.Principal == "" {
		if err := encoder.Encode(IPCResponse{OK: false, Error: "principal is required"}); err != nil {
			l.logger.Error("encoding IPC response", "error", err, "action", "wait-proxy")
		}
		return
	}

	if err := principal.ValidateLocalpart(request.Principal); err != nil {
		if encodeErr := encoder.Encode(IPCResponse{OK: false, Error: fmt.Sprintf("invalid principal: %v", err)}); encodeErr != nil {
			l.logger.Error("encoding IPC response", "error", encodeErr, "action", "wait-proxy")
		}
		return
	}

	l.mu.Lock()
	sandbox, exists := l.sandboxes[request.Principal]
	l.mu.Unlock()

	if !exists {
		if err := encoder.Encode(IPCResponse{OK: false, Error: fmt.Sprintf("no sandbox running for principal %q", request.Principal)}); err != nil {
			l.logger.Error("encoding IPC response", "error", err, "action", "wait-proxy", "principal", request.Principal)
		}
		return
	}

	conn.SetDeadline(time.Time{})

	connClosed := watchDisconnect(conn)

	select {
	case <-sandbox.proxyDone:
		exitCode := sandbox.proxyExitCode
		if err := encoder.Encode(IPCResponse{OK: true, ExitCode: &exitCode}); err != nil {
			l.logger.Error("encoding wait-proxy result", "error", err,
				"principal", request.Principal, "exit_code", exitCode)
		}

	case <-connClosed:
		l.logger.Info("wait-proxy: daemon disconnected",
			"principal", request.Principal)

	case <-ctx.Done():
		l.logger.Info("wait-proxy: launcher shutting down",
			"principal", request.Principal)
	}
}

// handleUpdatePayload rewrites the payload file for a running sandbox.
//
// The file is written in-place (truncate + write) rather than via atomic
// rename. Rename creates a new inode, and bwrap bind mounts reference the
// source dentry — after a rename the sandbox process still sees the old
// content through the original inode, making the update invisible.
//
// In-place write is not atomic: the file is briefly empty between truncate
// and write completion. This is safe because there is no concurrent reader
// during the truncate window — the IPC response does not return until
// write+close completes, the daemon sends its config room notification
// only after the IPC response, and agents read the file only after
// receiving that notification. Unlike atomic rename (which guarantees
// old-or-new content on crash, never partial), in-place write is not
// crash-safe — a launcher crash between truncate and write leaves an empty
// file. This is acceptable because payloads are ephemeral: the sandbox is
// gone if the launcher crashes, and the payload is recreated on restart.
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

	payloadJSON, err := json.Marshal(request.Payload)
	if err != nil {
		return IPCResponse{OK: false, Error: fmt.Sprintf("marshaling payload: %v", err)}
	}

	// Write in-place to preserve the inode. O_CREATE is defensive — the
	// file should exist from initial sandbox creation, but we handle the
	// edge case of unexpected removal gracefully.
	payloadPath := filepath.Join(sandbox.configDir, "payload.json")
	file, err := os.OpenFile(payloadPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0644)
	if err != nil {
		return IPCResponse{OK: false, Error: fmt.Sprintf("opening payload file: %v", err)}
	}
	if _, err := file.Write(payloadJSON); err != nil {
		file.Close()
		return IPCResponse{OK: false, Error: fmt.Sprintf("writing payload: %v", err)}
	}
	if err := file.Close(); err != nil {
		return IPCResponse{OK: false, Error: fmt.Sprintf("closing payload file: %v", err)}
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

// handleProvisionCredential decrypts an existing credential bundle, merges
// a new key-value pair into it, and re-encrypts to the specified recipients.
// This enables connector services to inject per-principal credentials (e.g.,
// Forgejo tokens, GitHub PATs) without ever seeing the full plaintext bundle.
//
// The daemon is responsible for authorization and recipient key resolution
// before sending this IPC request. The launcher only does cryptography.
func (l *Launcher) handleProvisionCredential(request *IPCRequest) IPCResponse {
	if request.KeyName == "" {
		return IPCResponse{OK: false, Error: "key_name is required for provision-credential"}
	}
	if request.KeyValue == "" {
		return IPCResponse{OK: false, Error: "key_value is required for provision-credential"}
	}
	if len(request.RecipientKeys) == 0 {
		return IPCResponse{OK: false, Error: "recipient_keys is required for provision-credential"}
	}

	// Start from existing credentials or an empty bundle.
	credentials := make(map[string]string)
	if request.EncryptedCredentials != "" {
		decrypted, err := sealed.Decrypt(request.EncryptedCredentials, l.keypair.PrivateKey)
		if err != nil {
			return IPCResponse{OK: false, Error: fmt.Sprintf("decrypting credentials: %v", err)}
		}
		defer decrypted.Close()

		if err := json.Unmarshal(decrypted.Bytes(), &credentials); err != nil {
			return IPCResponse{OK: false, Error: fmt.Sprintf("parsing decrypted credentials: %v", err)}
		}
	}

	// Upsert the new credential.
	credentials[request.KeyName] = request.KeyValue

	// Marshal back to JSON for re-encryption.
	updatedJSON, err := json.Marshal(credentials)
	if err != nil {
		return IPCResponse{OK: false, Error: fmt.Sprintf("marshaling updated credentials: %v", err)}
	}
	defer secret.Zero(updatedJSON)

	// Re-encrypt to the daemon-provided recipient list.
	updatedCiphertext, err := sealed.Encrypt(updatedJSON, request.RecipientKeys)
	if err != nil {
		return IPCResponse{OK: false, Error: fmt.Sprintf("encrypting updated credentials: %v", err)}
	}

	// Build sorted key list for the Credentials event's Keys field.
	updatedKeys := make([]string, 0, len(credentials))
	for key := range credentials {
		updatedKeys = append(updatedKeys, key)
	}
	sort.Strings(updatedKeys)

	l.logger.Info("provisioned credential",
		"key", request.KeyName,
		"total_keys", len(updatedKeys),
	)

	return IPCResponse{
		OK:                true,
		UpdatedCiphertext: updatedCiphertext,
		UpdatedKeys:       updatedKeys,
	}
}

// finishSandbox sets the exit code, error, and captured output on a sandbox
// and closes its done channel. Safe to call from multiple goroutines (session
// watcher, handleDestroySandbox, shutdownAllSandboxes) — doneOnce ensures the
// channel is closed exactly once and the exit state is set atomically with the
// close.
//
// output is the captured terminal content from the tmux pane. It is only
// available when the session watcher detects a normal process exit (the
// pane is still alive due to remain-on-exit). For forced destruction or
// launcher shutdown, output is empty — this is expected and not an error.
//
// Also kills the proxy process if it is still running. The proxy is
// infrastructure for the sandbox; when the sandbox ends, the proxy has
// no purpose.
func (l *Launcher) finishSandbox(sb *managedSandbox, exitCode int, exitError error, output string) {
	sb.doneOnce.Do(func() {
		sb.exitCode = exitCode
		sb.exitError = exitError
		sb.exitOutput = output
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

	// Remove socket files (the proxy may have already removed them
	// during graceful shutdown, so ignore errors).
	principalRef, parseErr := ref.NewEntityFromAccountLocalpart(l.machine.Fleet(), principalLocalpart)
	if parseErr != nil {
		l.logger.Error("cannot parse principal for socket cleanup",
			"principal", principalLocalpart, "error", parseErr)
	} else {
		os.Remove(principalRef.ProxySocketPath(l.fleetRunDir))
		os.Remove(principalRef.ProxyAdminSocketPath(l.fleetRunDir))
		// For service principals, also remove the symlink from
		// ServiceSocketPath to the configDir listen directory.
		// The symlink target directory is cleaned up with configDir.
		if principalRef.EntityType() == "service" {
			os.Remove(principalRef.ServiceSocketPath(l.fleetRunDir))
		}
	}

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
	if err := l.tmuxServer.KillServer(); err != nil {
		l.logger.Debug("killing tmux server (may already be stopped)",
			"socket", l.tmuxServer.SocketPath(), "error", err)
	} else {
		l.logger.Info("tmux server stopped", "socket", l.tmuxServer.SocketPath())
	}

	// Finish each sandbox: close done channels and kill proxy processes.
	for localpart, sb := range l.sandboxes {
		l.logger.Info("shutting down sandbox", "principal", localpart, "proxy_pid", sb.proxyProcess.Pid)
		l.finishSandbox(sb, -1, fmt.Errorf("launcher shutdown"), "")

		// Wait for the done channel (should be immediate since we just
		// called finishSandbox, but the session watcher goroutine may
		// have fired concurrently).
		<-sb.done
		l.cleanupSandbox(localpart)
	}
}
