// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/watchdog"
)

// watchdogMaxAge is the maximum age of a watchdog file that will be
// acted upon during startup. A watchdog older than this is treated as
// stale (from an unrelated restart) and silently cleared. Five minutes
// is generous — normal launcher startup completes in under a second.
const watchdogMaxAge = 5 * time.Minute

// launcherState is written to the state file before exec(). It captures
// the minimal information needed to reconnect to surviving proxy
// processes after the new binary starts.
type launcherState struct {
	// Sandboxes maps principal localpart to the state needed for
	// reconnection. Each entry corresponds to a running proxy process
	// that will survive the exec() transition.
	Sandboxes map[string]*sandboxEntry `cbor:"sandboxes"`

	// ProxyBinaryPath is the proxy binary the launcher was using at
	// the time of exec(). Preserved because the daemon may have updated
	// it via "update-proxy-binary" IPC since the launcher started —
	// os.Args has the stale flag value.
	ProxyBinaryPath string `cbor:"proxy_binary_path"`
}

// sandboxEntry captures the per-sandbox state needed to reconnect after
// exec(). ProxyPID is the credential injection proxy process; configDir is
// the temp directory containing its config file; roles are from the
// SandboxSpec.
type sandboxEntry struct {
	ProxyPID  int                 `cbor:"proxy_pid"`
	ConfigDir string              `cbor:"config_dir"`
	Roles     map[string][]string `cbor:"roles,omitempty"`
}

// launcherWatchdogPath returns the path to the launcher watchdog file.
// Lives in stateDir (persistent) because the watchdog must survive both
// exec() and unclean restarts (the whole point is detecting whether the
// new binary crashed and the old one was restarted by systemd).
func (l *Launcher) launcherWatchdogPath() string {
	return filepath.Join(l.stateDir, "launcher-watchdog.cbor")
}

// launcherStatePath returns the path to the launcher state file. Lives
// in runDir (tmpfs) because reboots kill all child processes — there is
// nothing to reconnect to after a reboot, so a stale state file on
// persistent storage would cause incorrect reconnection attempts.
func (l *Launcher) launcherStatePath() string {
	return filepath.Join(l.runDir, "launcher-state.cbor")
}

// writeStateFile serializes the current sandboxes map to the state file.
// Uses atomic write (temp file + rename) so readers never see a partial
// write. Called immediately before exec().
func (l *Launcher) writeStateFile() error {
	state := launcherState{
		Sandboxes:       make(map[string]*sandboxEntry, len(l.sandboxes)),
		ProxyBinaryPath: l.proxyBinaryPath,
	}

	for localpart, sandbox := range l.sandboxes {
		// Skip sandboxes whose proxy process has already exited —
		// there is nothing to reconnect to.
		select {
		case <-sandbox.done:
			continue
		default:
		}

		state.Sandboxes[localpart] = &sandboxEntry{
			ProxyPID:  sandbox.proxyProcess.Pid,
			ConfigDir: sandbox.configDir,
			Roles:     sandbox.roles,
		}
	}

	data, err := codec.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshaling launcher state: %w", err)
	}

	statePath := l.launcherStatePath()
	temporaryPath := statePath + ".tmp"

	file, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("creating temporary state file: %w", err)
	}

	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(temporaryPath)
		return fmt.Errorf("writing temporary state file: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(temporaryPath)
		return fmt.Errorf("syncing temporary state file: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(temporaryPath)
		return fmt.Errorf("closing temporary state file: %w", err)
	}

	if err := os.Rename(temporaryPath, statePath); err != nil {
		os.Remove(temporaryPath)
		return fmt.Errorf("renaming state file into place: %w", err)
	}

	return nil
}

// clearStateFile removes the state file. Idempotent — returns no error
// if the file does not exist.
func (l *Launcher) clearStateFile() {
	os.Remove(l.launcherStatePath())
}

// handleExecUpdate validates an exec-update IPC request and prepares
// for exec(). On success, the caller should send the response and then
// call performExec(). This separation ensures the daemon receives the
// OK response before the process image is replaced.
func (l *Launcher) handleExecUpdate(ctx context.Context, request *IPCRequest) IPCResponse {
	if request.BinaryPath == "" {
		return IPCResponse{OK: false, Error: "binary_path is required for exec-update"}
	}

	// Retry protection: don't attempt exec for a path that already
	// failed during this process lifetime.
	if l.failedExecPaths[request.BinaryPath] {
		return IPCResponse{OK: false, Error: fmt.Sprintf(
			"exec previously failed for %s during this process lifetime", request.BinaryPath)}
	}

	// Validate the binary exists and is executable.
	info, err := os.Stat(request.BinaryPath)
	if err != nil {
		return IPCResponse{OK: false, Error: fmt.Sprintf("binary not found: %v", err)}
	}
	if info.Mode()&0111 == 0 {
		return IPCResponse{OK: false, Error: fmt.Sprintf("binary not executable: %s", request.BinaryPath)}
	}

	// Guard: we need our own path for the watchdog's PreviousBinary
	// field. Without it, the startup watchdog check cannot determine
	// whether the exec succeeded or the old binary was restarted.
	if l.binaryPath == "" {
		return IPCResponse{OK: false, Error: "launcher binary path unknown, cannot write watchdog for exec"}
	}

	// Write the state file (sandboxes map) so the new process can
	// reconnect to surviving proxy processes.
	if err := l.writeStateFile(); err != nil {
		return IPCResponse{OK: false, Error: fmt.Sprintf("writing state file: %v", err)}
	}

	// Write the watchdog so the new process can detect whether the
	// exec transition succeeded or failed.
	watchdogState := watchdog.State{
		Component:      "launcher",
		PreviousBinary: l.binaryPath,
		NewBinary:      request.BinaryPath,
		Timestamp:      time.Now(),
	}
	if err := watchdog.Write(l.launcherWatchdogPath(), watchdogState); err != nil {
		l.clearStateFile()
		return IPCResponse{OK: false, Error: fmt.Sprintf("writing watchdog: %v", err)}
	}

	l.logger.Info("exec-update prepared",
		"previous", l.binaryPath,
		"new", request.BinaryPath,
		"sandboxes", len(l.sandboxes),
	)

	return IPCResponse{OK: true}
}

// performExec replaces the current process image with the new binary.
// On success, this function never returns. On failure, it cleans up the
// state file and watchdog, records the failed path, and returns so the
// caller can continue serving.
func (l *Launcher) performExec(newBinaryPath string) {
	l.logger.Info("exec()'ing new launcher binary",
		"previous", l.binaryPath,
		"new", newBinaryPath,
	)

	execFunction := l.execFunc
	if execFunction == nil {
		execFunction = syscall.Exec
	}

	argv := append([]string{newBinaryPath}, os.Args[1:]...)
	err := execFunction(newBinaryPath, argv, os.Environ())

	// If we reach here, exec() failed. The process was NOT replaced.
	l.logger.Error("launcher exec() failed, continuing with current binary",
		"new_binary", newBinaryPath,
		"error", err,
	)

	// Clean up the state file and watchdog — we are not in a
	// transition state.
	l.clearStateFile()
	if clearErr := watchdog.Clear(l.launcherWatchdogPath()); clearErr != nil {
		l.logger.Error("clearing watchdog after exec failure",
			"path", l.launcherWatchdogPath(),
			"error", clearErr,
		)
	}

	// Record this path as failed to prevent retry loops.
	l.failedExecPaths[newBinaryPath] = true
}

// reconnectSandboxes reads the state file written by a previous launcher
// process before exec() and reconnects to the surviving proxy processes.
// For each entry, it verifies the process is alive and the admin socket
// exists, then rebuilds the managedSandbox with a new wait goroutine.
//
// Processes that have died or lost their socket are cleaned up silently —
// the daemon's next reconcile cycle will recreate them.
//
// Returns nil and does nothing when no state file exists (normal startup).
func (l *Launcher) reconnectSandboxes() error {
	statePath := l.launcherStatePath()

	data, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading state file: %w", err)
	}
	// Always clean up the state file, regardless of whether
	// reconnection succeeds or fails. A stale state file from a
	// previous partial reconnection is worse than a missing one.
	defer os.Remove(statePath)

	var state launcherState
	if err := codec.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parsing state file: %w", err)
	}

	// Restore the proxy binary path from the state file. The daemon
	// may have updated it via "update-proxy-binary" IPC after the
	// launcher started, so os.Args has the stale flag value.
	if state.ProxyBinaryPath != "" {
		l.proxyBinaryPath = state.ProxyBinaryPath
	}

	for localpart, entry := range state.Sandboxes {
		// os.FindProcess on Unix always succeeds (just wraps the PID).
		process, err := os.FindProcess(entry.ProxyPID)
		if err != nil {
			l.logger.Warn("cannot find process for reconnected sandbox",
				"principal", localpart, "proxy_pid", entry.ProxyPID, "error", err)
			cleanupConfigDir(entry.ConfigDir)
			continue
		}

		// Signal 0 checks if the process is alive without sending a
		// real signal. ESRCH means the process does not exist.
		if err := process.Signal(syscall.Signal(0)); err != nil {
			l.logger.Warn("reconnected sandbox proxy is dead",
				"principal", localpart, "proxy_pid", entry.ProxyPID, "error", err)
			cleanupConfigDir(entry.ConfigDir)
			continue
		}

		// Verify the admin socket exists. A live process without its
		// socket is wedged — kill it and let the daemon recreate.
		principalRef, parseErr := ref.NewEntityFromAccountLocalpart(l.machine.Fleet(), localpart)
		if parseErr != nil {
			l.logger.Warn("cannot parse principal localpart for reconnection, killing proxy",
				"principal", localpart, "proxy_pid", entry.ProxyPID, "error", parseErr)
			process.Kill()
			var status syscall.WaitStatus
			syscall.Wait4(entry.ProxyPID, &status, 0, nil)
			cleanupConfigDir(entry.ConfigDir)
			continue
		}
		adminSocketPath := principalRef.AdminSocketPath(l.fleetRunDir)
		if _, err := os.Stat(adminSocketPath); err != nil {
			l.logger.Warn("reconnected sandbox admin socket missing, killing wedged proxy",
				"principal", localpart, "proxy_pid", entry.ProxyPID,
				"admin_socket", adminSocketPath, "error", err)
			process.Kill()
			// Wait for the zombie to be collected. Use Wait4 directly
			// because process.Wait() may not work for FindProcess'd
			// PIDs across exec() boundaries in all Go runtime versions.
			var status syscall.WaitStatus
			syscall.Wait4(entry.ProxyPID, &status, 0, nil)
			cleanupConfigDir(entry.ConfigDir)
			continue
		}

		// Reconnect: create managedSandbox with a proxy reaper goroutine.
		// The done channel is NOT closed by the proxy reaper — the session
		// watcher (started below) drives the sandbox lifecycle.
		sb := &managedSandbox{
			localpart:    localpart,
			proxyProcess: process,
			configDir:    entry.ConfigDir,
			done:         make(chan struct{}),
			roles:        entry.Roles,
		}

		// Start a goroutine to reap the proxy process (avoid zombies).
		// Does NOT close sandbox.done — that's the session watcher's job.
		go func(localpart string, pid int) {
			var status syscall.WaitStatus
			_, waitError := syscall.Wait4(pid, &status, 0, nil)
			exitCode := 0
			if waitError != nil {
				exitCode = -1
			} else if status.Exited() {
				exitCode = status.ExitStatus()
			} else if status.Signaled() {
				exitCode = 128 + int(status.Signal())
			}
			l.logger.Info("reconnected proxy process exited",
				"principal", localpart,
				"pid", pid,
				"exit_code", exitCode,
				"error", waitError,
			)
		}(localpart, entry.ProxyPID)

		l.sandboxes[localpart] = sb

		// Start the session watcher that drives the sandbox lifecycle.
		l.startSessionWatcher(localpart, sb)

		l.logger.Info("reconnected sandbox",
			"principal", localpart,
			"proxy_pid", entry.ProxyPID,
			"config_dir", entry.ConfigDir,
		)
	}

	return nil
}

// cleanupConfigDir removes a proxy config temp directory. Used during
// reconnection when the proxy process is dead or wedged.
func cleanupConfigDir(configDir string) {
	if configDir != "" {
		os.RemoveAll(configDir)
	}
}

// checkLauncherWatchdog reads the launcher watchdog file on startup and
// determines whether a previous exec() transition succeeded or failed.
//
// Unlike the daemon's checkDaemonWatchdog, the launcher does not report
// to Matrix (it has no configRoomID). The daemon reports on the
// launcher's behalf when it detects the binary hash change.
//
// Returns the failed binary path (for seeding failedExecPaths) when the
// transition failed, or empty string otherwise.
func checkLauncherWatchdog(
	watchdogPath string,
	launcherBinaryPath string,
	logger *slog.Logger,
) (failedPath string) {
	state, found, err := watchdog.Check(watchdogPath, watchdogMaxAge)
	if err != nil {
		logger.Error("reading launcher watchdog", "path", watchdogPath, "error", err)
		return ""
	}
	if !found {
		return ""
	}

	// A recent watchdog exists — this process started after an exec()
	// transition attempt (or a crash during one).

	switch launcherBinaryPath {
	case state.NewBinary:
		// We are the new binary. The exec() succeeded.
		logger.Info("launcher exec() succeeded",
			"previous", state.PreviousBinary,
			"new", state.NewBinary,
		)

	case state.PreviousBinary:
		// We are the old binary. The new binary crashed or failed to
		// start, and this process was restarted (by systemd, manual
		// intervention, etc.).
		logger.Error("launcher exec() failed: old binary restarted",
			"attempted", state.NewBinary,
			"current", state.PreviousBinary,
		)
		failedPath = state.NewBinary

	default:
		// Neither match — this process is a different binary entirely
		// (e.g., manually deployed a third version). The watchdog is
		// from a stale transition that no longer applies.
		logger.Info("clearing stale launcher watchdog (current binary matches neither previous nor new)",
			"current", launcherBinaryPath,
			"watchdog_previous", state.PreviousBinary,
			"watchdog_new", state.NewBinary,
		)
	}

	if err := watchdog.Clear(watchdogPath); err != nil {
		logger.Error("clearing launcher watchdog", "path", watchdogPath, "error", err)
	}

	return failedPath
}
