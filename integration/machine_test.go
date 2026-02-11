// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/messaging"
)

// testMachine holds the paths and state for one machine under test. Call
// newTestMachine to create the directory structure, then startMachine to
// boot the launcher and daemon.
type testMachine struct {
	Name     string
	UserID   string
	StateDir string
	RunDir   string // short /tmp path passed as --run-dir to launcher+daemon

	// Socket paths derived from RunDir via the principal package (same
	// derivation the launcher and daemon use internally).
	LauncherSocket string
	TmuxSocket     string
	ObserveSocket  string
	RelaySocket    string
	WorkspaceRoot  string

	// Populated by startMachine:
	PublicKey      string // age public key from the machine_key state event
	ConfigRoomID   string // per-machine config room (#bureau/config/<name>)
	MachinesRoomID string // global #bureau/machines room
}

// PrincipalSocketPath returns the proxy socket path for a principal on
// this machine, using the same derivation as the launcher.
func (m *testMachine) PrincipalSocketPath(localpart string) string {
	return principal.RunDirSocketPath(m.RunDir, localpart)
}

// machineOptions configures process binaries and daemon settings for
// startMachine. LauncherBinary and DaemonBinary are required; the rest
// are optional and conditionally passed as CLI flags.
type machineOptions struct {
	LauncherBinary     string // required
	DaemonBinary       string // required
	ProxyBinary        string // optional: launcher --proxy-binary
	ObserveRelayBinary string // optional: daemon --observe-relay-binary
	StatusInterval     string // optional: daemon --status-interval (default "2s")
}

// principalAccount holds the registration details for a principal Matrix
// account. Created by registerPrincipal.
type principalAccount struct {
	Localpart string
	UserID    string
	Token     string
}

// principalSpec describes a principal to deploy on a machine.
type principalSpec struct {
	Account      principalAccount
	MatrixPolicy map[string]any // optional per-principal policy
}

// deploymentConfig describes what principals to deploy on a machine and
// any machine-level configuration (observe policy, etc.).
type deploymentConfig struct {
	Principals           []principalSpec
	DefaultObservePolicy map[string]any // optional machine-level observe policy
}

// newTestMachine creates the directory structure for a machine. Does not
// start any processes — call startMachine for that. Socket paths are
// derived from RunDir using the principal package, matching the launcher
// and daemon's internal path derivation.
func newTestMachine(t *testing.T, name string) *testMachine {
	t.Helper()

	stateDir := t.TempDir()
	runDir := tempSocketDir(t)

	return &testMachine{
		Name:           name,
		UserID:         "@" + name + ":" + testServerName,
		StateDir:       stateDir,
		RunDir:         runDir,
		LauncherSocket: principal.LauncherSocketPath(runDir),
		TmuxSocket:     principal.TmuxSocketPath(runDir),
		ObserveSocket:  principal.ObserveSocketPath(runDir),
		RelaySocket:    principal.RelaySocketPath(runDir),
		WorkspaceRoot:  filepath.Join(stateDir, "workspace"),
	}
}

// startMachine boots the launcher and daemon for a machine, waits for
// readiness signals (launcher socket, machine key publication, status
// heartbeat, config room creation), and populates the machine's PublicKey,
// ConfigRoomID, and MachinesRoomID fields.
func startMachine(t *testing.T, admin *messaging.Session, machine *testMachine, options machineOptions) {
	t.Helper()

	if options.LauncherBinary == "" {
		t.Fatal("machineOptions.LauncherBinary is required")
	}
	if options.DaemonBinary == "" {
		t.Fatal("machineOptions.DaemonBinary is required")
	}

	ctx := t.Context()

	// Write the registration token to a file for the launcher.
	tokenFile := filepath.Join(machine.StateDir, "reg-token")
	if err := os.WriteFile(tokenFile, []byte(testRegistrationToken), 0600); err != nil {
		t.Fatalf("write registration token: %v", err)
	}

	// Resolve global rooms and invite the machine.
	machinesRoomID, err := admin.ResolveAlias(ctx, "#bureau/machines:"+testServerName)
	if err != nil {
		t.Fatalf("resolve machines room: %v", err)
	}
	servicesRoomID, err := admin.ResolveAlias(ctx, "#bureau/services:"+testServerName)
	if err != nil {
		t.Fatalf("resolve services room: %v", err)
	}
	machine.MachinesRoomID = machinesRoomID

	if err := admin.InviteUser(ctx, machinesRoomID, machine.UserID); err != nil {
		t.Fatalf("invite machine to machines room: %v", err)
	}
	if err := admin.InviteUser(ctx, servicesRoomID, machine.UserID); err != nil {
		t.Fatalf("invite machine to services room: %v", err)
	}

	// Start the launcher.
	launcherArgs := []string{
		"--homeserver", testHomeserverURL,
		"--registration-token-file", tokenFile,
		"--machine-name", machine.Name,
		"--server-name", testServerName,
		"--run-dir", machine.RunDir,
		"--state-dir", machine.StateDir,
		"--workspace-root", machine.WorkspaceRoot,
	}
	if options.ProxyBinary != "" {
		launcherArgs = append(launcherArgs, "--proxy-binary", options.ProxyBinary)
	}

	startProcess(t, machine.Name+"-launcher", options.LauncherBinary, launcherArgs...)
	waitForFile(t, machine.LauncherSocket, 15*time.Second)

	// Retrieve the machine's public key from Matrix.
	machineKeyJSON := waitForStateEvent(t, admin, machinesRoomID,
		"m.bureau.machine_key", machine.Name, 10*time.Second)
	var machineKey struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(machineKeyJSON, &machineKey); err != nil {
		t.Fatalf("unmarshal machine key: %v", err)
	}
	if machineKey.PublicKey == "" {
		t.Fatal("machine key has empty public key")
	}
	machine.PublicKey = machineKey.PublicKey

	// Start the daemon.
	statusInterval := options.StatusInterval
	if statusInterval == "" {
		statusInterval = "2s"
	}
	daemonArgs := []string{
		"--homeserver", testHomeserverURL,
		"--machine-name", machine.Name,
		"--server-name", testServerName,
		"--run-dir", machine.RunDir,
		"--state-dir", machine.StateDir,
		"--admin-user", "bureau-admin",
		"--status-interval", statusInterval,
	}
	if options.ObserveRelayBinary != "" {
		daemonArgs = append(daemonArgs, "--observe-relay-binary", options.ObserveRelayBinary)
	}

	startProcess(t, machine.Name+"-daemon", options.DaemonBinary, daemonArgs...)

	// Wait for daemon readiness (MachineStatus heartbeat).
	waitForStateEvent(t, admin, machinesRoomID,
		"m.bureau.machine_status", machine.Name, 15*time.Second)

	// Resolve the per-machine config room and join as admin.
	configAlias := "#bureau/config/" + machine.Name + ":" + testServerName
	configRoomID, err := admin.ResolveAlias(ctx, configAlias)
	if err != nil {
		t.Fatalf("config room %s not created: %v", configAlias, err)
	}
	if _, err := admin.JoinRoom(ctx, configRoomID); err != nil {
		t.Fatalf("admin join config room: %v", err)
	}
	machine.ConfigRoomID = configRoomID
}

// registerPrincipal creates a Matrix account for a principal on the test
// homeserver and returns the account details including the access token.
func registerPrincipal(t *testing.T, localpart, password string) principalAccount {
	t.Helper()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: testHomeserverURL,
	})
	if err != nil {
		t.Fatalf("create client for principal registration: %v", err)
	}

	session, err := client.Register(t.Context(), messaging.RegisterRequest{
		Username:          localpart,
		Password:          password,
		RegistrationToken: testRegistrationToken,
	})
	if err != nil {
		t.Fatalf("register principal %q: %v", localpart, err)
	}
	token := session.AccessToken()
	session.Close()

	return principalAccount{
		Localpart: localpart,
		UserID:    "@" + localpart + ":" + testServerName,
		Token:     token,
	}
}

// pushCredentials encrypts a principal's Matrix credentials with the
// machine's public key and pushes them as an m.bureau.credentials state
// event to the machine's config room. The credentials are encrypted so
// only the machine's launcher can decrypt them.
func pushCredentials(t *testing.T, admin *messaging.Session, machine *testMachine, account principalAccount) {
	t.Helper()

	credentialBundle := map[string]string{
		"MATRIX_TOKEN":          account.Token,
		"MATRIX_USER_ID":        account.UserID,
		"MATRIX_HOMESERVER_URL": testHomeserverURL,
	}
	credentialJSON, err := json.Marshal(credentialBundle)
	if err != nil {
		t.Fatalf("marshal credentials for %s: %v", account.Localpart, err)
	}

	ciphertext, err := sealed.Encrypt(credentialJSON, []string{machine.PublicKey})
	if err != nil {
		t.Fatalf("encrypt credentials for %s: %v", account.Localpart, err)
	}

	adminUserID := "@bureau-admin:" + testServerName
	_, err = admin.SendStateEvent(t.Context(), machine.ConfigRoomID,
		"m.bureau.credentials", account.Localpart, map[string]any{
			"version":        1,
			"principal":      account.UserID,
			"encrypted_for":  []string{machine.UserID},
			"keys":           []string{"MATRIX_TOKEN", "MATRIX_USER_ID", "MATRIX_HOMESERVER_URL"},
			"ciphertext":     ciphertext,
			"provisioned_by": adminUserID,
			"provisioned_at": time.Now().UTC().Format(time.RFC3339),
		})
	if err != nil {
		t.Fatalf("push credentials for %s: %v", account.Localpart, err)
	}
}

// pushMachineConfig builds and pushes an m.bureau.machine_config state event
// to the machine's config room. The daemon detects this via /sync and
// reconciles: creating proxies for new principals and destroying proxies
// for removed ones.
func pushMachineConfig(t *testing.T, admin *messaging.Session, machine *testMachine, config deploymentConfig) {
	t.Helper()

	principalConfigs := make([]map[string]any, len(config.Principals))
	for i, spec := range config.Principals {
		entry := map[string]any{
			"localpart":  spec.Account.Localpart,
			"template":   "",
			"auto_start": true,
		}
		if spec.MatrixPolicy != nil {
			entry["matrix_policy"] = spec.MatrixPolicy
		}
		principalConfigs[i] = entry
	}

	machineConfig := map[string]any{
		"principals": principalConfigs,
	}
	if config.DefaultObservePolicy != nil {
		machineConfig["default_observe_policy"] = config.DefaultObservePolicy
	}

	_, err := admin.SendStateEvent(t.Context(), machine.ConfigRoomID,
		"m.bureau.machine_config", machine.Name, machineConfig)
	if err != nil {
		t.Fatalf("push machine config: %v", err)
	}
}

// deployPrincipals encrypts credentials for each principal, pushes
// Credentials and MachineConfig state events to the machine's config room,
// and waits for all proxy sockets to appear. Returns a map from localpart
// to proxy socket path. This is a convenience wrapper around pushCredentials
// and pushMachineConfig for the common case of deploying from scratch.
func deployPrincipals(t *testing.T, admin *messaging.Session, machine *testMachine, config deploymentConfig) map[string]string {
	t.Helper()

	for _, spec := range config.Principals {
		pushCredentials(t, admin, machine, spec.Account)
	}

	pushMachineConfig(t, admin, machine, config)

	proxySockets := make(map[string]string, len(config.Principals))
	for _, spec := range config.Principals {
		socketPath := machine.PrincipalSocketPath(spec.Account.Localpart)
		waitForFile(t, socketPath, 15*time.Second)
		proxySockets[spec.Account.Localpart] = socketPath
	}

	return proxySockets
}
