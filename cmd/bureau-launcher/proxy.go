// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ipc"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/sandbox"
)

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
func (l *Launcher) spawnProxy(principalLocalpart string, credentials map[string]string, grants []schema.Grant) (int, error) {
	if l.proxyBinaryPath == "" {
		return 0, fmt.Errorf("proxy binary path not configured (set --proxy-binary or install bureau-proxy on PATH)")
	}

	// Construct a typed ref from the account localpart for socket path derivation.
	principalRef, err := ref.NewEntityFromAccountLocalpart(l.machine.Fleet(), principalLocalpart)
	if err != nil {
		return 0, fmt.Errorf("parsing principal %q: %w", principalLocalpart, err)
	}
	proxySocketPath := principalRef.ProxySocketPath(l.fleetRunDir)
	adminSocketPath := principalRef.ProxyAdminSocketPath(l.fleetRunDir)

	// Create a config directory for proxy state (config file, service
	// listen sockets, payload, trigger). Located under the fleet run
	// directory so everything stays within /run/bureau/ — no /tmp
	// symlinks, no PrivateTmp visibility issues. Fleet-scoped to avoid
	// collisions when multiple fleets share a run directory.
	sanitizedName := strings.ReplaceAll(principalLocalpart, "/", "-")
	configDir := filepath.Join(l.fleetRunDir, "sandbox", sanitizedName)
	// Remove any stale directory from a previous run that wasn't
	// cleaned up (e.g., launcher killed before destroy).
	os.RemoveAll(configDir)
	if err := os.MkdirAll(configDir, 0750); err != nil {
		return 0, fmt.Errorf("creating config directory: %w", err)
	}

	// Write the proxy config. The services map is empty because the daemon
	// registers services dynamically via the admin socket.
	configContent := fmt.Sprintf("socket_path: %s\nadmin_socket_path: %s\nservices: {}\n",
		proxySocketPath, adminSocketPath)
	configPath := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(configContent), 0600); err != nil {
		os.RemoveAll(configDir)
		return 0, fmt.Errorf("writing proxy config: %w", err)
	}

	// Ensure socket parent directories exist. Both the proxy socket
	// and admin socket live under the fleet run dir in entity-type
	// subdirectories (e.g., /run/bureau/fleet/prod/agent/).
	if err := os.MkdirAll(filepath.Dir(proxySocketPath), 0755); err != nil {
		os.RemoveAll(configDir)
		return 0, fmt.Errorf("creating proxy socket directory for %s: %w", proxySocketPath, err)
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
		payload, err := l.buildCredentialPayload(principalRef, credentials, grants)
		if err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			os.RemoveAll(configDir)
			return 0, fmt.Errorf("building credential payload: %w", err)
		}

		payloadBytes, err := codec.Marshal(payload)
		if err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			os.RemoveAll(configDir)
			return 0, fmt.Errorf("marshaling credential payload: %w", err)
		}

		_, writeError := stdinPipe.Write(payloadBytes)
		secret.Zero(payloadBytes)

		if writeError != nil {
			cmd.Process.Kill()
			cmd.Wait()
			os.RemoveAll(configDir)
			return 0, fmt.Errorf("writing credentials to proxy stdin: %w", writeError)
		}
		stdinPipe.Close()
	}

	// Track the sandbox. The done channel is NOT closed when the proxy
	// exits — it is closed by the session watcher when the tmux session
	// ends, or by handleDestroySandbox. The proxyDone channel is closed
	// by the reap goroutine when the proxy process exits, enabling the
	// daemon's wait-proxy IPC to detect proxy crashes independently of
	// the sandbox lifecycle.
	sb := &managedSandbox{
		localpart:    principalLocalpart,
		proxyProcess: cmd.Process,
		configDir:    configDir,
		done:         make(chan struct{}),
		proxyDone:    make(chan struct{}),
	}

	// Reap the proxy process in the background to avoid zombies. Sets
	// the proxy exit code and closes proxyDone so that wait-proxy IPC
	// callers and waitForSocket are unblocked. Does NOT close
	// sandbox.done — that is the session watcher's responsibility.
	go func() {
		waitError := cmd.Wait()
		exitCode := 0
		if waitError != nil {
			var exitErr *exec.ExitError
			if errors.As(waitError, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
		}
		sb.proxyExitCode = exitCode
		close(sb.proxyDone)
		l.logger.Info("proxy process exited",
			"principal", principalLocalpart,
			"pid", cmd.Process.Pid,
			"exit_code", exitCode,
			"error", waitError,
		)
	}()

	// Wait for the proxy to become ready (agent socket file appears).
	// Uses sb.proxyDone to detect early proxy death and fail fast
	// rather than waiting the full 10-second timeout.
	if err := waitForSocket(proxySocketPath, sb.proxyDone, 10*time.Second); err != nil {
		cmd.Process.Kill()
		<-sb.proxyDone
		os.RemoveAll(configDir)
		return 0, fmt.Errorf("proxy for %q: %w", principalLocalpart, err)
	}

	l.sandboxes[principalLocalpart] = sb

	l.logger.Info("proxy started",
		"principal", principalLocalpart,
		"pid", cmd.Process.Pid,
		"socket", proxySocketPath,
		"admin_socket", adminSocketPath,
	)

	return cmd.Process.Pid, nil
}

// buildCredentialPayload restructures a flat credential map into the JSON
// structure expected by the proxy's PipeCredentialSource. The Matrix-specific
// keys (MATRIX_HOMESERVER_URL, MATRIX_TOKEN, MATRIX_USER_ID) are extracted
// into top-level fields; everything else goes under "credentials".
func (l *Launcher) buildCredentialPayload(principalEntity ref.Entity, credentials map[string]string, grants []schema.Grant) (*ipc.ProxyCredentialPayload, error) {
	homeserverURL := credentials["MATRIX_HOMESERVER_URL"]
	if homeserverURL == "" {
		// Fall back to the launcher's homeserver URL (the principal is
		// typically on the same homeserver as the machine).
		homeserverURL = l.homeserverURL
	}

	matrixToken := credentials["MATRIX_TOKEN"]
	if matrixToken == "" {
		return nil, fmt.Errorf("credential bundle missing MATRIX_TOKEN for principal %q", principalEntity.Localpart())
	}

	matrixUserID := credentials["MATRIX_USER_ID"]
	if matrixUserID == "" {
		// Default to the principal's canonical Matrix user ID.
		matrixUserID = principalEntity.UserID().String()
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

	return &ipc.ProxyCredentialPayload{
		MatrixHomeserverURL: homeserverURL,
		MatrixToken:         matrixToken,
		MatrixUserID:        matrixUserID,
		Credentials:         remaining,
		Grants:              grants,
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

// buildSandboxCommand converts a SandboxSpec into a shell script that exec's
// the bwrap sandbox. Returns the command to pass to tmux new-session (the
// script path). The bwrap arguments are built from the SandboxSpec's profile
// conversion, and a payload file is written if the spec includes a Payload.
// When triggerContent is non-nil, it is written to trigger.json and bind-mounted
// read-only at /run/bureau/trigger.json inside the sandbox. Service mounts
// are bind-mounted read-write at /run/bureau/service/<role>.sock, giving
// the sandboxed process direct access to Bureau services. When tokenDirectory
// is non-empty, it is bind-mounted read-only at /run/bureau/service/token/,
// providing <role>.token files for service authentication.
//
// The returned command is a single-element slice containing the script path.
// The script handles all bwrap argument quoting internally, avoiding shell
// escaping issues when tmux invokes the command.
func (l *Launcher) buildSandboxCommand(principalLocalpart string, spec *schema.SandboxSpec, triggerContent []byte, serviceMounts []ServiceMount, tokenDirectory string) ([]string, error) {
	// Find the sandbox's config directory (created by spawnProxy).
	sb, exists := l.sandboxes[principalLocalpart]
	if !exists {
		return nil, fmt.Errorf("no sandbox entry for %q (proxy must be spawned first)", principalLocalpart)
	}

	// Derive the proxy socket path from the entity ref. The proxy
	// creates this socket in the fleet run dir; the sandbox bind-mounts
	// it at /run/bureau/proxy.sock for the sandboxed process to use.
	principalRef, err := ref.NewEntityFromAccountLocalpart(l.machine.Fleet(), principalLocalpart)
	if err != nil {
		return nil, fmt.Errorf("parsing principal %q for sandbox command: %w", principalLocalpart, err)
	}
	proxySocketPath := principalRef.ProxySocketPath(l.fleetRunDir)

	// Convert the SandboxSpec to a sandbox.Profile.
	profile := specToProfile(spec, proxySocketPath)

	// Expand template variables in the profile. The SandboxSpec carries
	// unexpanded ${VARIABLE} references in EnvironmentVariables, Filesystem
	// Source paths, and Command entries; the launcher resolves them here at
	// launch time when concrete values are known. Values must reflect
	// IN-SANDBOX paths (what the agent process sees), not host paths — the
	// bwrap bind mounts translate host paths to sandbox paths.
	vars := sandbox.Variables{
		"WORKSPACE_ROOT": l.workspaceRoot,
		"CACHE_ROOT":     l.cacheRoot,
		"PROXY_SOCKET":   "/run/bureau/proxy.sock",
		"TERM":           os.Getenv("TERM"),
		"MACHINE_NAME":   l.machine.Localpart(),
		"SERVER_NAME":    l.machine.Server().String(),
		"PRINCIPAL_NAME": principalLocalpart,
		"FLEET":          l.machine.Fleet().Localpart(),
	}
	// Extract workspace variables from the payload. The daemon populates
	// these for workspace principals via PrincipalAssignment.Payload;
	// non-workspace principals have no PROJECT in their payload and
	// template variables referencing ${PROJECT} will remain unexpanded
	// (causing a mount error, which is correct — only workspace principals
	// should use workspace templates).
	if spec.Payload != nil {
		if project, ok := spec.Payload["PROJECT"].(string); ok && project != "" {
			vars["PROJECT"] = project
		}
		if worktreePath, ok := spec.Payload["WORKTREE_PATH"].(string); ok && worktreePath != "" {
			vars["WORKTREE_PATH"] = worktreePath
		}
	}
	profile = vars.ExpandProfile(profile)

	// Always create the payload file and bind mount, even when the
	// initial deployment has no payload. This ensures that
	// handleUpdatePayload's in-place write is always visible to the
	// sandbox through the pre-existing bind mount. Without this, a
	// later config update that adds a payload would write the file on
	// the host, but the sandbox would have no mount point to see it
	// (bwrap does not support adding bind mounts to a running namespace).
	payloadContent := spec.Payload
	if payloadContent == nil {
		payloadContent = map[string]any{}
	}
	payloadPath, err := writePayloadFile(sb.configDir, payloadContent)
	if err != nil {
		return nil, fmt.Errorf("writing payload: %w", err)
	}
	profile.Filesystem = append(profile.Filesystem, sandbox.Mount{
		Source: payloadPath,
		Dest:   "/run/bureau/payload.json",
		Mode:   sandbox.MountModeRO,
	})

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

	// Bind-mount service sockets into the sandbox. Each required service
	// gets a socket at /run/bureau/service/<role>.sock, giving the agent
	// direct access to Bureau services without routing through the proxy.
	for _, mount := range serviceMounts {
		profile.Filesystem = append(profile.Filesystem, sandbox.Mount{
			Source: mount.SocketPath,
			Dest:   "/run/bureau/service/" + mount.Role + ".sock",
			Mode:   sandbox.MountModeRW,
		})
	}

	// Bind-mount the token directory into the sandbox at
	// /run/bureau/service/token/. This directory contains <role>.token
	// files written by the daemon. The directory mount (not individual
	// file mounts) ensures atomic token refresh (write+rename on host)
	// is visible inside the sandbox via VFS path traversal. Read-only:
	// the daemon owns token lifecycle (minting, refresh, revocation).
	if tokenDirectory != "" {
		profile.Filesystem = append(profile.Filesystem, sandbox.Mount{
			Source: tokenDirectory,
			Dest:   "/run/bureau/service/token",
			Mode:   sandbox.MountModeRO,
		})
	}

	// For service principals, bind-mount a per-service directory so the
	// service's CBOR listener socket is visible on the host. The service
	// creates its socket at /run/bureau/listen/service.sock inside the
	// sandbox; the bind mount maps /run/bureau/listen/ to a directory
	// under the service's configDir on the host, making the socket file
	// accessible from outside the namespace.
	//
	// Each service gets its own bind-mounted directory (not the shared
	// <runDir>/service/ directory), preventing sandboxed services from
	// seeing each other's CBOR sockets.
	//
	// A symlink from ServiceSocketPath() to the actual host path lets
	// the daemon find the socket at the canonical location without
	// knowing about the configDir layout.
	if principalRef.EntityType() == "service" {
		listenDir := filepath.Join(sb.configDir, "listen")
		if err := os.MkdirAll(listenDir, 0755); err != nil {
			return nil, fmt.Errorf("creating service listen directory: %w", err)
		}
		sandboxSocketPath := "/run/bureau/listen/service.sock"
		profile.Filesystem = append(profile.Filesystem, sandbox.Mount{
			Source: listenDir,
			Dest:   "/run/bureau/listen",
			Mode:   sandbox.MountModeRW,
		})
		// Set the environment variables that BootstrapViaProxy reads.
		// BUREAU_PROXY_SOCKET, BUREAU_MACHINE_NAME, and BUREAU_SERVER_NAME
		// are already available via template variable expansion
		// (${PROXY_SOCKET}, ${MACHINE_NAME}, ${SERVER_NAME}), but
		// BUREAU_PRINCIPAL_NAME, BUREAU_FLEET, and BUREAU_SERVICE_SOCKET
		// are service-specific and injected directly by the launcher.
		if profile.Environment == nil {
			profile.Environment = make(map[string]string)
		}
		profile.Environment["BUREAU_SERVICE_SOCKET"] = sandboxSocketPath
		profile.Environment["BUREAU_PRINCIPAL_NAME"] = principalLocalpart
		profile.Environment["BUREAU_FLEET"] = l.machine.Fleet().Localpart()

		// Create a symlink from the canonical ServiceSocketPath to the
		// actual socket location inside the config directory. The daemon
		// dials ServiceSocketPath; the symlink redirects to the real
		// socket exposed via the bind mount.
		hostSocketPath := principalRef.ServiceSocketPath(l.fleetRunDir)
		hostSocketDir := filepath.Dir(hostSocketPath)
		if err := os.MkdirAll(hostSocketDir, 0755); err != nil {
			return nil, fmt.Errorf("creating service socket symlink directory: %w", err)
		}
		actualSocketPath := filepath.Join(listenDir, "service.sock")
		// Remove any stale symlink from a previous run.
		os.Remove(hostSocketPath)
		if err := os.Symlink(actualSocketPath, hostSocketPath); err != nil {
			return nil, fmt.Errorf("creating service socket symlink %s → %s: %w", hostSocketPath, actualSocketPath, err)
		}
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

	// Compute the exit-code file path. The relay writes the child's
	// exit code here after waitpid, and the session watcher reads it
	// via inotify — bypassing tmux's #{pane_dead_status} race.
	exitCodeFilePath := filepath.Join(sb.configDir, "exit-code")
	sb.exitCodeFilePath = exitCodeFilePath

	// Write the sandbox script. The script exec's bureau-log-relay
	// wrapping bwrap. The log relay holds the outer PTY open until it
	// collects the child's exit code, eliminating the tmux 3.4+ race
	// between PTY EOF and SIGCHLD that causes exit codes to be lost.
	scriptPath, err := writeSandboxScript(sb.configDir, l.logRelayBinaryPath, exitCodeFilePath, bwrapPath, bwrapArgs)
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
