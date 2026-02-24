// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/bootstrap"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

const bootstrapImageName = "bureau-test-machine"

// bootstrapContainerName includes the PID so parallel test runs
// don't collide on Docker container names.
var bootstrapContainerName = fmt.Sprintf("bureau-bootstrap-test-%d", os.Getpid())

// TestBootstrapScript exercises the full bootstrap-machine shell script inside
// a Docker container. This validates the actual production bootstrap path: the
// script parses the bootstrap config, installs binaries (from --binary-dir),
// runs first boot (launcher registers, rotates password, publishes key),
// writes machine.conf, and installs systemd units.
//
// The test runs in two phases:
//
//   - Phase 1: Run bootstrap-machine inside a Docker container with
//     --skip-nix --binary-dir --no-start. Verify all installation
//     artifacts (symlinks, config, systemd units) and that first boot
//     published the machine key to Matrix.
//
//   - Phase 2: Start launcher + daemon inside the same container and
//     deploy a principal. This proves bwrap works in Docker (requires
//     --privileged). Skipped if bwrap is not available in the container.
func TestBootstrapScript(t *testing.T) {
	t.Parallel()

	launcherBinary := resolvedBinary(t, "LAUNCHER_BINARY")
	daemonBinary := resolvedBinary(t, "DAEMON_BINARY")
	proxyBinary := resolvedBinary(t, "PROXY_BINARY")
	logRelayBinary := resolvedBinary(t, "LOG_RELAY_BINARY")

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	bootstrapFleet := createTestFleet(t, admin)

	// --- Provision the machine account on Matrix ---
	machineRef, err := ref.NewMachine(bootstrapFleet.Ref, "bootstrap-script")
	if err != nil {
		t.Fatalf("create machine ref: %v", err)
	}
	machineName := machineRef.Localpart()

	bootstrapPath := filepath.Join(t.TempDir(), "bootstrap.json")
	bootstrapClient := adminClient(t)
	provisionMachine(t, bootstrapClient, admin, machineRef, bootstrapPath)

	bootstrapConfig, err := bootstrap.ReadConfig(bootstrapPath)
	if err != nil {
		t.Fatalf("read bootstrap config: %v", err)
	}
	t.Logf("machine provisioned: %s (password length %d)", bootstrapConfig.MachineName, len(bootstrapConfig.Password))

	// --- Build the Docker image ---
	dockerfilePath := filepath.Join(workspaceRoot, "deploy", "test", "Dockerfile.machine")
	buildImage(t, bootstrapImageName, dockerfilePath)

	// --- Prepare a binary directory ---
	// The bootstrap script expects bureau-launcher, bureau-daemon, and
	// bureau-proxy in the --binary-dir. Create a directory with copies
	// (not symlinks — symlinks to Bazel runfiles may not resolve inside
	// the container).
	binaryDir := t.TempDir()
	copyBinary(t, launcherBinary, filepath.Join(binaryDir, "bureau-launcher"))
	copyBinary(t, daemonBinary, filepath.Join(binaryDir, "bureau-daemon"))
	copyBinary(t, proxyBinary, filepath.Join(binaryDir, "bureau-proxy"))
	copyBinary(t, logRelayBinary, filepath.Join(binaryDir, "bureau-log-relay"))

	// --- Start the container ---
	containerID := startContainer(t, bootstrapImageName, bootstrapContainerName,
		"-v", binaryDir+":/bureau-bin:ro",
		"-v", workspaceRoot+":/bureau-src:ro",
	)
	t.Logf("container started: %s", containerID[:12])

	// Copy the bootstrap config INTO the container (not bind-mount).
	// Bind-mounting a single file makes it a mount point that os.Remove
	// cannot delete. The launcher deletes the bootstrap file after first
	// boot (security: one-time password cleanup), so it must be a regular
	// file inside the container's filesystem.
	dockerCpOrFail(t, bootstrapPath, containerID+":/tmp/bootstrap.json")

	// --- Phase 1: Run bootstrap-machine ---
	dockerExecOrFail(t, containerID,
		"/bureau-src/script/bootstrap-machine",
		"--skip-nix",
		"--binary-dir", "/bureau-bin",
		"--no-start",
		"/tmp/bootstrap.json",
	)
	t.Log("bootstrap-machine completed successfully")

	// Verify: symlinks installed at /usr/local/bin.
	for _, binary := range []string{"bureau-launcher", "bureau-daemon", "bureau-proxy"} {
		dockerExecOrFail(t, containerID, "test", "-L", "/usr/local/bin/"+binary)
		// Verify the symlink target is readable (binary exists).
		dockerExecOrFail(t, containerID, "test", "-x", "/usr/local/bin/"+binary)
	}
	t.Log("binary symlinks verified")

	// Verify: machine.conf written with correct content. The bootstrap
	// config stores the fleet-scoped machine name and fleet prefix, which
	// the bootstrap-machine script writes to machine.conf as-is.
	confOutput := dockerExecOutput(t, containerID, "cat", "/etc/bureau/machine.conf")
	assertContains(t, confOutput, "BUREAU_HOMESERVER_URL="+testHomeserverURL, "machine.conf homeserver URL")
	assertContains(t, confOutput, "BUREAU_MACHINE_NAME="+bootstrapConfig.MachineName, "machine.conf machine name")
	assertContains(t, confOutput, "BUREAU_SERVER_NAME="+testServerName, "machine.conf server name")
	assertContains(t, confOutput, "BUREAU_FLEET="+bootstrapConfig.FleetPrefix, "machine.conf fleet prefix")
	t.Log("machine.conf verified")

	// Verify: systemd units installed.
	dockerExecOrFail(t, containerID, "test", "-f", "/etc/systemd/system/bureau-launcher.service")
	dockerExecOrFail(t, containerID, "test", "-f", "/etc/systemd/system/bureau-daemon.service")
	t.Log("systemd units installed")

	// Verify: bootstrap config was deleted by the launcher (security).
	exitCode := dockerExecExitCode(t, containerID, "test", "-f", "/tmp/bootstrap.json")
	if exitCode == 0 {
		t.Error("bootstrap config should have been deleted after first boot, but still exists")
	}
	t.Log("bootstrap config deleted (security check passed)")

	// Verify: first boot published the machine key to Matrix.
	machineRoomID := bootstrapFleet.MachineRoomID
	// First boot already completed, so the key exists in room state.
	machineKeyJSON, err := admin.GetStateEvent(ctx, machineRoomID,
		schema.EventTypeMachineKey, machineName)
	if err != nil {
		t.Fatalf("get machine key: %v", err)
	}
	var machineKey struct {
		Algorithm string `json:"algorithm"`
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(machineKeyJSON, &machineKey); err != nil {
		t.Fatalf("unmarshal machine key: %v", err)
	}
	if machineKey.Algorithm != "age-x25519" {
		t.Errorf("machine key algorithm = %q, want age-x25519", machineKey.Algorithm)
	}
	if machineKey.PublicKey == "" {
		t.Fatal("machine key has empty public key")
	}
	t.Logf("machine key published: algorithm=%s key=%s...", machineKey.Algorithm, machineKey.PublicKey[:20])

	// Verify: session.json and keypair files exist in the container.
	dockerExecOrFail(t, containerID, "test", "-f", "/var/lib/bureau/session.json")
	dockerExecOrFail(t, containerID, "test", "-f", "/var/lib/bureau/machine-key.pub")
	dockerExecOrFail(t, containerID, "test", "-f", "/var/lib/bureau/machine-key.txt")
	t.Log("session and keypair files verified")

	// Verify: directories created.
	for _, directory := range []string{"/var/lib/bureau", "/run/bureau", "/var/bureau/workspace", "/var/bureau/cache", "/etc/bureau"} {
		dockerExecOrFail(t, containerID, "test", "-d", directory)
	}
	t.Log("directories verified")

	// --- Phase 2: Start launcher + daemon inside container ---
	// This proves the bootstrapped machine can actually run services
	// and create sandboxes (bwrap-in-Docker).
	t.Run("Services", func(t *testing.T) {
		// Check if bwrap works in this container before starting services.
		// If bwrap is not available (unprivileged container), skip Phase 2.
		if !containerHasBwrap(t, containerID) {
			t.Skip("bwrap not available in container (needs --privileged); skipping service test")
		}

		// Start launcher in the background inside the container.
		// The launcher reads its session from /var/lib/bureau/session.json
		// (written during first boot) so no registration token is needed.
		// The machine.conf BUREAU_MACHINE_NAME has the fleet-scoped name;
		// we pass the same value here so the launcher can reconstruct the ref.
		dockerExecBackground(t, containerID, "launcher",
			"/usr/local/bin/bureau-launcher",
			"--homeserver", testHomeserverURL,
			"--machine-name", bootstrapConfig.MachineName,
			"--server-name", testServerName,
			"--fleet", bootstrapConfig.FleetPrefix,
		)

		// Wait for the launcher socket to appear.
		waitForFileInContainer(t, containerID, "/run/bureau/launcher.sock", 15*time.Second)
		t.Log("launcher started inside container")

		// Set up a watch before starting the daemon to detect its heartbeat.
		statusWatch := watchRoom(t, admin, bootstrapFleet.MachineRoomID)

		// Start daemon in the background.
		dockerExecBackground(t, containerID, "daemon",
			"/usr/local/bin/bureau-daemon",
			"--homeserver", testHomeserverURL,
			"--machine-name", machineName,
			"--server-name", testServerName,
			"--admin-user", admin.UserID().Localpart(),
			"--status-interval", "2s",
			"--fleet", bootstrapFleet.Prefix,
		)

		// Wait for daemon heartbeat in Matrix.
		statusWatch.WaitForStateEvent(t,
			schema.EventTypeMachineStatus, machineName)
		t.Log("daemon started and publishing status")

		// Deploy a principal to verify sandbox creation (bwrap).
		configAlias := machineRef.RoomAlias()
		configRoomID, err := admin.ResolveAlias(t.Context(), configAlias)
		if err != nil {
			t.Fatalf("config room not created: %v", err)
		}
		if _, err := admin.JoinRoom(t.Context(), configRoomID); err != nil {
			t.Fatalf("admin join config room: %v", err)
		}

		// Register and deploy a minimal principal (no template, just proxy).
		principalAccount := registerFleetPrincipal(t, bootstrapFleet, "agent/bootstrap-test", "test-password")
		containerMachine := &testMachine{
			Ref:           machineRef,
			Name:          machineName,
			PublicKey:     machineKey.PublicKey,
			ConfigRoomID:  configRoomID,
			MachineRoomID: bootstrapFleet.MachineRoomID,
		}
		pushCredentials(t, admin, containerMachine, principalAccount)
		pushMachineConfig(t, admin, containerMachine, deploymentConfig{
			Principals: []principalSpec{{Localpart: principalAccount.Localpart}},
		})

		// Wait for the proxy socket inside the container. The socket
		// path matches the container's /run/bureau layout.
		proxySocketPath := principalProxySocketPath(t, machineRef, "/run/bureau", principalAccount.Localpart)
		waitForFileInContainer(t, containerID, proxySocketPath, 30*time.Second)
		t.Log("proxy socket appeared — sandbox created inside Docker container")
	})

	// --- Cleanup ---
	// Decommission the machine from Matrix.
	decommissionMachine(t, admin, machineRef)
	t.Log("bootstrap script test complete")
}

// --- Docker Helpers ---

// buildImage builds a Docker image from the given Dockerfile.
func buildImage(t *testing.T, imageName, dockerfilePath string) {
	t.Helper()

	contextDir := filepath.Dir(dockerfilePath)
	cmd := exec.Command("docker", "build",
		"-t", imageName,
		"-f", dockerfilePath,
		contextDir,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker build failed: %v", err)
	}
}

// startContainer starts a long-lived Docker container and registers cleanup.
// Returns the container ID.
func startContainer(t *testing.T, imageName, containerName string, extraArgs ...string) string {
	t.Helper()

	// Remove any leftover container from a previous interrupted run.
	_ = exec.Command("docker", "rm", "-f", containerName).Run()

	args := []string{
		"run", "-d",
		"--privileged",
		"--network", "host",
		"--name", containerName,
	}
	args = append(args, extraArgs...)
	args = append(args, imageName, "sleep", "infinity")

	cmd := exec.Command("docker", args...)
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("docker run failed: %v\nstderr: %s", err, exitErr.Stderr)
		}
		t.Fatalf("docker run failed: %v", err)
	}

	containerID := strings.TrimSpace(string(output))

	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", containerID).Run()
	})

	return containerID
}

// dockerCpOrFail copies a file into a Docker container.
func dockerCpOrFail(t *testing.T, source, destination string) {
	t.Helper()

	cmd := exec.Command("docker", "cp", source, destination)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker cp %s %s failed: %v\noutput: %s", source, destination, err, output)
	}
}

// dockerExecOrFail runs a command inside a Docker container and fails on error.
func dockerExecOrFail(t *testing.T, containerID string, command ...string) {
	t.Helper()

	args := append([]string{"exec", containerID}, command...)
	cmd := exec.Command("docker", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec %v failed: %v\noutput: %s", command, err, output)
	}
}

// dockerExecOutput runs a command inside a Docker container and returns stdout.
func dockerExecOutput(t *testing.T, containerID string, command ...string) string {
	t.Helper()

	args := append([]string{"exec", containerID}, command...)
	cmd := exec.Command("docker", args...)
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("docker exec %v failed: %v\nstderr: %s", command, err, exitErr.Stderr)
		}
		t.Fatalf("docker exec %v failed: %v", command, err)
	}
	return string(output)
}

// dockerExecExitCode runs a command and returns just the exit code (no failure).
func dockerExecExitCode(t *testing.T, containerID string, command ...string) int {
	t.Helper()

	args := append([]string{"exec", containerID}, command...)
	cmd := exec.Command("docker", args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	err := cmd.Run()
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	t.Fatalf("docker exec %v unexpected error: %v", command, err)
	return -1
}

// dockerExecBackground starts a long-running command inside the container.
// Registers cleanup to kill the process via docker exec kill.
func dockerExecBackground(t *testing.T, containerID, name string, command ...string) {
	t.Helper()

	args := append([]string{"exec", "-d", containerID}, command...)
	cmd := exec.Command("docker", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec -d %s failed: %v\noutput: %s", name, err, output)
	}
	t.Logf("started %s in container (detached)", name)

	t.Cleanup(func() {
		// Kill the process by name inside the container.
		_ = exec.Command("docker", "exec", containerID,
			"pkill", "-f", command[0]).Run()
	})
}

// containerHasBwrap checks if bwrap is usable inside the container by
// attempting a minimal bwrap invocation with user namespaces.
func containerHasBwrap(t *testing.T, containerID string) bool {
	t.Helper()

	// Check if bwrap binary is available.
	if dockerExecExitCode(t, containerID, "which", "bwrap") != 0 {
		t.Log("bwrap not found in container")
		return false
	}

	// Try a minimal bwrap invocation. If user namespaces are not
	// available (container not --privileged), this will fail.
	code := dockerExecExitCode(t, containerID,
		"bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "true")
	if code != 0 {
		t.Log("bwrap test invocation failed (user namespaces not available?)")
		return false
	}
	return true
}

// waitForFileInContainer blocks until a file exists inside the container.
// Uses inotifywait to watch the parent directory for creation events
// instead of busy-spinning docker exec calls.
func waitForFileInContainer(t *testing.T, containerID, path string, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	directory := filepath.Dir(path)
	timeoutSeconds := strconv.Itoa(max(1, int(timeout.Seconds())))

	for {
		if dockerExecExitCode(t, containerID, "test", "-e", path) == 0 {
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("timed out after %s waiting for file in container: %s", timeout, path)
		}

		// Watch for create events in the parent directory. If the
		// parent doesn't exist yet, watch its parent instead (e.g.,
		// watch /run for the creation of bureau/).
		watchDirectory := directory
		if dockerExecExitCode(t, containerID, "test", "-d", directory) != 0 {
			watchDirectory = filepath.Dir(directory)
		}

		cmd := exec.CommandContext(ctx, "docker", "exec", containerID,
			"inotifywait", "-t", timeoutSeconds, "-e", "create", watchDirectory)
		cmd.Stdout = nil
		cmd.Stderr = nil
		_ = cmd.Run()
	}
}

// copyBinary copies a binary file, preserving executable permissions.
func copyBinary(t *testing.T, source, destination string) {
	t.Helper()

	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read binary %s: %v", source, err)
	}
	if err := os.WriteFile(destination, data, 0755); err != nil {
		t.Fatalf("write binary %s: %v", destination, err)
	}
}

// assertContains checks that haystack contains needle, failing with a
// descriptive message if not.
func assertContains(t *testing.T, haystack, needle, description string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s: expected to contain %q, got:\n%s", description, needle, haystack)
	}
}
