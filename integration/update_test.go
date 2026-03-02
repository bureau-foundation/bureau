// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/binhash"
	"github.com/bureau-foundation/bureau/lib/bootstrap"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// updateContainerName includes PID to avoid collisions with parallel test runs.
var updateContainerName = fmt.Sprintf("bureau-update-test-%d", os.Getpid())

// v2Marker is the trailing bytes appended to v2 binaries to change their
// SHA256 hash without affecting ELF execution. ELF loading reads segment
// headers to determine memory layout; trailing data past the last segment
// is ignored by the kernel's loader.
const v2Marker = "BUREAU_TEST_V2_MARKER"

// TestUpdateLifecycle exercises the full binary update path inside a Docker
// container: publish a BureauVersion state event, verify the daemon detects
// the hash mismatch via Matrix /sync, exec()'s to the new binary, the new
// daemon triggers a launcher exec via IPC, and proxy sockets survive both
// transitions. Then rolls back by publishing the original version and
// verifying the same flow in reverse.
//
// This test validates the production code path from end to end:
//
//   - reconcileBureauVersion detects version changes
//   - prefetchBureauVersion fast path (files already exist, no Nix needed)
//   - version.Compare hashes binaries and produces a Diff
//   - execDaemon writes watchdog, calls syscall.Exec
//   - checkDaemonWatchdog detects success, posts to Matrix
//   - Launcher exec via IPC, reconnectSandboxes preserves proxy processes
//   - os.Args[1:] preserved across exec (daemon starts with same flags)
func TestUpdateLifecycle(t *testing.T) {
	t.Parallel()

	hostBinaries := []struct {
		envVar     string
		binaryName string
	}{
		{"BUREAU_BINARY", "bureau"},
		{"LAUNCHER_BINARY", "bureau-launcher"},
		{"DAEMON_BINARY", "bureau-daemon"},
		{"PROXY_BINARY", "bureau-proxy"},
		{"LOG_RELAY_BINARY", "bureau-log-relay"},
		{"OBSERVE_RELAY_BINARY", "bureau-observe-relay"},
		{"BRIDGE_BINARY", "bureau-bridge"},
		{"SANDBOX_BINARY", "bureau-sandbox"},
		{"CREDENTIALS_BINARY", "bureau-credentials"},
		{"AGENT_SERVICE_BINARY", "bureau-agent-service"},
		{"ARTIFACT_SERVICE_BINARY", "bureau-artifact-service"},
		{"TICKET_SERVICE_BINARY", "bureau-ticket-service"},
	}

	resolvedHostBinaries := make(map[string]string)
	for _, binary := range hostBinaries {
		resolvedHostBinaries[binary.binaryName] = resolvedBinary(t, binary.envVar)
	}

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	updateFleet := createTestFleet(t, admin)

	// --- Provision the machine ---
	machineRef, err := ref.NewMachine(updateFleet.Ref, "update-lifecycle")
	if err != nil {
		t.Fatalf("create machine ref: %v", err)
	}
	machineName := machineRef.Localpart()

	bootstrapPath := filepath.Join(t.TempDir(), "bootstrap.json")
	bootstrapClient := adminClient(t)
	hsAdmin := homeserverAdmin(t)
	provisionMachine(t, bootstrapClient, admin, hsAdmin, machineRef, bootstrapPath)

	bootstrapConfig, err := bootstrap.ReadConfig(bootstrapPath)
	if err != nil {
		t.Fatalf("read bootstrap config: %v", err)
	}

	// --- Prepare binaries and precompute expected hashes ---
	binaryDir := t.TempDir()
	for _, binary := range hostBinaries {
		copyBinary(t, resolvedHostBinaries[binary.binaryName], filepath.Join(binaryDir, binary.binaryName))
	}

	// Compute the v1 daemon hash from the host-side binary. The v1
	// binary inside the container is an exact copy, so the hashes match.
	v1DaemonHash := hashBinaryFile(t, filepath.Join(binaryDir, "bureau-daemon"))

	// Create the v2 daemon binary on the host side (copy + trailing
	// marker) and hash it. This produces the same content that the
	// container's v2 binary will have.
	v2DaemonPath := filepath.Join(t.TempDir(), "bureau-daemon-v2")
	createV2Binary(t, filepath.Join(binaryDir, "bureau-daemon"), v2DaemonPath)
	v2DaemonHash := hashBinaryFile(t, v2DaemonPath)

	if v1DaemonHash == v2DaemonHash {
		t.Fatal("v1 and v2 daemon hashes are identical — marker append did not change the hash")
	}

	// --- Build Docker image and start container ---
	dockerfilePath := filepath.Join(workspaceRoot, "deploy", "test", "Dockerfile.machine")
	buildImage(t, bootstrapImageName, dockerfilePath)

	containerID := startContainer(t, bootstrapImageName, updateContainerName,
		"-v", binaryDir+":/bureau-bin:ro",
		"-v", workspaceRoot+":/bureau-src:ro",
	)

	dockerCpOrFail(t, bootstrapPath, containerID+":/tmp/bootstrap.json")

	// --- Bootstrap ---
	dockerExecOrFail(t, containerID,
		"/bureau-src/script/bootstrap-machine",
		"--skip-nix",
		"--binary-dir", "/bureau-bin",
		"--no-start",
		"/tmp/bootstrap.json",
	)
	t.Log("bootstrap complete")

	if !containerHasBwrap(t, containerID) {
		t.Skip("bwrap not available in container (needs --privileged); skipping update lifecycle test")
	}

	// --- Start launcher and daemon with log capture ---
	// Use shell wrappers so stderr is captured to log files inside the
	// container. After `exec`, the shell is replaced by the actual binary
	// so `pkill -f bureau-launcher` and `pkill -f bureau-daemon` still
	// find the right process.
	launcherScript := fmt.Sprintf(
		"exec /var/bureau/bin/bureau-launcher --homeserver %s --machine-name %s --server-name %s --fleet %s >>/tmp/launcher.log 2>&1",
		testHomeserverURL, bootstrapConfig.MachineName, testServerName, bootstrapConfig.FleetPrefix,
	)
	dockerExecOrFail(t, containerID, "sh", "-c",
		fmt.Sprintf("nohup sh -c '%s' &", launcherScript))
	t.Cleanup(func() {
		_ = exec.Command("docker", "exec", containerID,
			"pkill", "-f", "bureau-launcher").Run()
	})
	waitForFileInContainer(t, containerID, "/run/bureau/launcher.sock", 15*time.Second)
	t.Log("launcher started")

	// Dump daemon and launcher logs on failure for diagnostics.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, logFile := range []string{"/tmp/daemon.log", "/tmp/launcher.log"} {
			output := dockerExecOutput(t, containerID, "cat", logFile)
			t.Logf("=== %s ===\n%s", logFile, output)
		}
	})

	// --- Start daemon, wait for heartbeat ---
	statusWatch := watchRoom(t, admin, updateFleet.MachineRoomID)

	daemonScript := fmt.Sprintf(
		"exec /var/bureau/bin/bureau-daemon --homeserver %s --machine-name %s --server-name %s --admin-user %s --status-interval 2s --fleet %s >>/tmp/daemon.log 2>&1",
		testHomeserverURL, machineName, testServerName, admin.UserID().Localpart(), updateFleet.Prefix,
	)
	dockerExecOrFail(t, containerID, "sh", "-c",
		fmt.Sprintf("nohup sh -c '%s' &", daemonScript))
	t.Cleanup(func() {
		_ = exec.Command("docker", "exec", containerID,
			"pkill", "-f", "bureau-daemon").Run()
	})

	statusWatch.WaitForStateEvent(t, schema.EventTypeMachineStatus, machineName)
	t.Log("daemon started and publishing status")

	// --- Resolve config room and deploy a principal ---
	configAlias := machineRef.RoomAlias()
	configRoomID, err := admin.ResolveAlias(ctx, configAlias)
	if err != nil {
		t.Fatalf("config room not created: %v", err)
	}
	if _, err := admin.JoinRoom(ctx, configRoomID); err != nil {
		t.Fatalf("admin join config room: %v", err)
	}

	// Read machine public key for credential encryption.
	machineKeyJSON, err := admin.GetStateEvent(ctx, updateFleet.MachineRoomID,
		schema.EventTypeMachineKey, machineName)
	if err != nil {
		t.Fatalf("get machine key: %v", err)
	}
	var machineKey struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(machineKeyJSON, &machineKey); err != nil {
		t.Fatalf("unmarshal machine key: %v", err)
	}

	containerMachine := &testMachine{
		Ref:           machineRef,
		Name:          machineName,
		PublicKey:     machineKey.PublicKey,
		ConfigRoomID:  configRoomID,
		MachineRoomID: updateFleet.MachineRoomID,
	}

	principalAccount := registerFleetPrincipal(t, updateFleet, "agent/update-test", "test-password")
	pushCredentials(t, admin, containerMachine, principalAccount)
	pushMachineConfig(t, admin, containerMachine, deploymentConfig{
		Principals: []principalSpec{{Localpart: principalAccount.Localpart}},
	})

	proxySocketPath := principalProxySocketPath(t, machineRef, "/run/bureau", principalAccount.Localpart)
	waitForFileInContainer(t, containerID, proxySocketPath, 30*time.Second)
	t.Log("principal deployed, proxy socket appeared")

	// --- Create fake Nix store inside container ---
	v1Version, v2Version := createFakeNixStore(t, containerID)

	// --- Forward update: publish v2, verify exec lifecycle ---
	t.Run("ForwardUpdate", func(t *testing.T) {
		configWatch := watchRoom(t, admin, configRoomID)

		publishBureauVersion(t, admin, configRoomID, machineName, v2Version)
		t.Log("published v2 BureauVersion")

		// Wait for the v2 daemon to post self-update success. The v1
		// daemon exec()'s to v2; the v2 daemon reads the watchdog on
		// startup and posts "succeeded" to the config room.
		waitForNotification[schema.DaemonSelfUpdateMessage](
			t, &configWatch, schema.MsgTypeDaemonSelfUpdate, machineRef.UserID(),
			func(message schema.DaemonSelfUpdateMessage) bool {
				return message.Status == schema.SelfUpdateSucceeded
			}, "daemon self-update succeeded (v1→v2)")
		t.Log("daemon exec succeeded (v1→v2)")

		// The v2 daemon's first reconcile detects the launcher binary
		// mismatch (launcher is still v1, desired is v2) and sends
		// exec-update IPC. The BureauVersionUpdateMessage with
		// LauncherChanged=true confirms the IPC was sent and accepted.
		waitForNotification[schema.BureauVersionUpdateMessage](
			t, &configWatch, schema.MsgTypeBureauVersionUpdate, machineRef.UserID(),
			func(message schema.BureauVersionUpdateMessage) bool {
				return message.Status == schema.VersionUpdateReconciled && message.LauncherChanged
			}, "bureau version reconciled with launcher changed")
		t.Log("launcher exec triggered (v1→v2)")

		verifyDaemonStatusHash(t, containerID, v2DaemonHash, machineRef)

		if dockerExecExitCode(t, containerID, "test", "-S", proxySocketPath) != 0 {
			t.Fatalf("proxy socket %s disappeared after forward update", proxySocketPath)
		}
		t.Log("proxy socket survived forward update")
	})

	// --- Rollback: publish v1, verify exec lifecycle ---
	t.Run("Rollback", func(t *testing.T) {
		configWatch := watchRoom(t, admin, configRoomID)

		publishBureauVersion(t, admin, configRoomID, machineName, v1Version)
		t.Log("published v1 BureauVersion (rollback)")

		waitForNotification[schema.DaemonSelfUpdateMessage](
			t, &configWatch, schema.MsgTypeDaemonSelfUpdate, machineRef.UserID(),
			func(message schema.DaemonSelfUpdateMessage) bool {
				return message.Status == schema.SelfUpdateSucceeded
			}, "daemon self-update succeeded (v2→v1)")
		t.Log("daemon exec succeeded (v2→v1)")

		waitForNotification[schema.BureauVersionUpdateMessage](
			t, &configWatch, schema.MsgTypeBureauVersionUpdate, machineRef.UserID(),
			func(message schema.BureauVersionUpdateMessage) bool {
				return message.Status == schema.VersionUpdateReconciled && message.LauncherChanged
			}, "bureau version reconciled with launcher changed (rollback)")
		t.Log("launcher exec triggered (v2→v1)")

		verifyDaemonStatusHash(t, containerID, v1DaemonHash, machineRef)

		if dockerExecExitCode(t, containerID, "test", "-S", proxySocketPath) != 0 {
			t.Fatalf("proxy socket %s disappeared after rollback", proxySocketPath)
		}
		t.Log("proxy socket survived rollback")
	})

	decommissionMachine(t, admin, machineRef)
	t.Log("update lifecycle test complete")
}

// createFakeNixStore creates v2 binaries inside the container at fake Nix
// store paths. v1 uses the original binaries at /bureau-bin/ (already
// present from the bind mount) — no copies needed. v2 daemon and launcher
// are full copies with a trailing marker appended to change the SHA256
// hash without affecting ELF execution. v2 proxy is a small placeholder
// (it is never exec'd, only hashed and path-updated via IPC).
//
// Using /bureau-bin/ for v1 avoids ~180MB of unnecessary file copies
// through Docker's overlay2 filesystem (3 binaries × ~60MB each). This
// keeps the test well within the 10-second budget even under concurrent
// test invocations.
func createFakeNixStore(t *testing.T, containerID string) (v1 *schema.BureauVersion, v2 *schema.BureauVersion) {
	t.Helper()

	// Only create v2 binaries. Daemon and launcher need full copies
	// (they get exec'd). Proxy only needs to be stat-able, hashable,
	// and pass the launcher's executability check — a small placeholder
	// with different content from the v1 proxy is sufficient.
	script := fmt.Sprintf(`
set -e
for component in bureau-daemon bureau-launcher; do
	mkdir -p /nix/store/test-v2-${component}/bin
	cp /bureau-bin/${component} /nix/store/test-v2-${component}/bin/${component}
	printf '%s' >> /nix/store/test-v2-${component}/bin/${component}
done
mkdir -p /nix/store/test-v2-bureau-proxy/bin
printf '%s' > /nix/store/test-v2-bureau-proxy/bin/bureau-proxy
chmod +x /nix/store/test-v2-bureau-proxy/bin/bureau-proxy
`, v2Marker, v2Marker)
	dockerExecOrFail(t, containerID, "sh", "-c", script)
	t.Log("fake Nix store created with v2 binaries")

	v1 = &schema.BureauVersion{
		DaemonStorePath:   "/bureau-bin/bureau-daemon",
		LauncherStorePath: "/bureau-bin/bureau-launcher",
		ProxyStorePath:    "/bureau-bin/bureau-proxy",
	}
	v2 = &schema.BureauVersion{
		DaemonStorePath:   "/nix/store/test-v2-bureau-daemon/bin/bureau-daemon",
		LauncherStorePath: "/nix/store/test-v2-bureau-launcher/bin/bureau-launcher",
		ProxyStorePath:    "/nix/store/test-v2-bureau-proxy/bin/bureau-proxy",
	}
	return v1, v2
}

// publishBureauVersion does a read-modify-write of MachineConfig to set
// BureauVersion while preserving Principals and DefaultPolicy. Matches
// the production path in cmd/bureau/machine/upgrade.go.
func publishBureauVersion(
	t *testing.T,
	admin *messaging.DirectSession,
	configRoomID ref.RoomID,
	machineStateKey string,
	version *schema.BureauVersion,
) {
	t.Helper()
	ctx := t.Context()

	config, err := messaging.GetState[schema.MachineConfig](
		ctx, admin, configRoomID, schema.EventTypeMachineConfig, machineStateKey)
	if err != nil && !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		t.Fatalf("read machine config for BureauVersion update: %v", err)
	}

	config.BureauVersion = version

	_, err = admin.SendStateEvent(ctx, configRoomID,
		schema.EventTypeMachineConfig, machineStateKey, config)
	if err != nil {
		t.Fatalf("publish BureauVersion: %v", err)
	}
}

// verifyDaemonStatusHash reads daemon-status.json from the container and
// verifies the daemon binary hash matches expectedDaemonDigest. Also
// checks that the machine identity is correct.
func verifyDaemonStatusHash(t *testing.T, containerID string, expectedDaemonDigest string, machineRef ref.Machine) {
	t.Helper()

	raw := dockerExecOutput(t, containerID, "cat", "/run/bureau/"+schema.DaemonStatusFilename)

	var status schema.DaemonStatus
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		t.Fatalf("daemon status file is not valid JSON: %v\nraw: %s", err, raw)
	}

	if status.DaemonBinaryPath == "" {
		t.Fatal("daemon status: daemon_binary_path is empty")
	}
	if status.DaemonBinaryHash == "" {
		t.Fatal("daemon status: daemon_binary_hash is empty")
	}
	if status.DaemonBinaryHash != expectedDaemonDigest {
		t.Errorf("daemon binary hash mismatch:\n  status file: %s\n  expected:    %s",
			status.DaemonBinaryHash, expectedDaemonDigest)
	}

	expectedUserID := machineRef.UserID().String()
	if status.MachineUserID != expectedUserID {
		t.Errorf("machine_user_id mismatch:\n  status file: %s\n  expected:    %s",
			status.MachineUserID, expectedUserID)
	}

	t.Logf("daemon status verified: path=%s hash=%s...%s",
		status.DaemonBinaryPath,
		status.DaemonBinaryHash[:8],
		status.DaemonBinaryHash[len(status.DaemonBinaryHash)-4:])
}

// hashBinaryFile computes the SHA256 hex digest of the binary at path.
func hashBinaryFile(t *testing.T, path string) string {
	t.Helper()
	digest, err := binhash.HashFile(path)
	if err != nil {
		t.Fatalf("hashing %s: %v", path, err)
	}
	return binhash.FormatDigest(digest)
}

// createV2Binary copies the binary at sourcePath to destPath and appends
// the v2 marker. The resulting file has different SHA256 content from the
// source but is an identical ELF binary at runtime (the kernel's loader
// ignores trailing data past the last ELF segment).
func createV2Binary(t *testing.T, sourcePath, destPath string) {
	t.Helper()
	content, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read binary %s: %v", sourcePath, err)
	}
	content = append(content, []byte(v2Marker)...)
	if err := os.WriteFile(destPath, content, 0755); err != nil {
		t.Fatalf("write v2 binary %s: %v", destPath, err)
	}
}
