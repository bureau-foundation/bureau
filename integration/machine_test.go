// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/bureau-foundation/bureau/lib/credential"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/templatedef"
	"github.com/bureau-foundation/bureau/messaging"
)

// testMachine holds the paths and state for one machine under test. Call
// newTestMachine to create the directory structure, then startMachine to
// boot the launcher and daemon.
type testMachine struct {
	Ref      ref.Machine // typed reference (carries Name, UserID, Fleet, Server)
	Name     string
	UserID   ref.UserID
	StateDir string
	RunDir   string // short /tmp path passed as --run-dir to launcher+daemon

	// Socket paths derived from RunDir via the principal package (same
	// derivation the launcher and daemon use internally).
	LauncherSocket string
	TmuxSocket     string
	ObserveSocket  string
	RelaySocket    string
	WorkspaceRoot  string
	CacheRoot      string

	// Populated by startMachine:
	PublicKey     string     // age public key from the machine_key state event
	ConfigRoomID  ref.RoomID // per-machine config room
	MachineRoomID ref.RoomID // fleet-scoped machine room (daemon status heartbeats)

	// deployedServices tracks service principals deployed via deployService.
	// pushMachineConfig automatically includes these so that subsequent
	// config pushes (e.g., deploying an agent) don't remove running services.
	// Services use AutoStart=true: the daemon manages their lifecycle through
	// the MachineConfig, so they must remain in every config update.
	deployedServices []principalSpec
}

// PrincipalProxySocketPath returns the proxy socket path for a principal on
// this machine, using the same fleet-scoped derivation as the launcher.
func (m *testMachine) PrincipalProxySocketPath(t *testing.T, localpart string) string {
	t.Helper()
	entity, err := ref.NewEntityFromAccountLocalpart(m.Ref.Fleet(), localpart)
	if err != nil {
		t.Fatalf("PrincipalProxySocketPath(%q): %v", localpart, err)
	}
	return entity.ProxySocketPath(m.Ref.Fleet().RunDir(m.RunDir))
}

// PrincipalProxyAdminSocketPath returns the admin socket path for a
// principal on this machine. The admin socket is on the host side (not
// bind-mounted into the sandbox) and is used by the daemon and tests
// to register services on the proxy.
func (m *testMachine) PrincipalProxyAdminSocketPath(t *testing.T, localpart string) string {
	t.Helper()
	entity, err := ref.NewEntityFromAccountLocalpart(m.Ref.Fleet(), localpart)
	if err != nil {
		t.Fatalf("PrincipalProxyAdminSocketPath(%q): %v", localpart, err)
	}
	return entity.ProxyAdminSocketPath(m.Ref.Fleet().RunDir(m.RunDir))
}

// PrincipalServiceSocketPath returns the service CBOR endpoint socket
// path for a service principal on this machine. Only meaningful for
// service entities — agents and machines do not create service sockets.
func (m *testMachine) PrincipalServiceSocketPath(t *testing.T, localpart string) string {
	t.Helper()
	entity, err := ref.NewEntityFromAccountLocalpart(m.Ref.Fleet(), localpart)
	if err != nil {
		t.Fatalf("PrincipalServiceSocketPath(%q): %v", localpart, err)
	}
	return entity.ServiceSocketPath(m.Ref.Fleet().RunDir(m.RunDir))
}

// principalProxySocketPath is a standalone version of
// testMachine.PrincipalProxySocketPath for tests that construct
// launcher/daemon lifecycle manually without a full testMachine.
func principalProxySocketPath(t *testing.T, machineRef ref.Machine, runDir, localpart string) string {
	t.Helper()
	entity, err := ref.NewEntityFromAccountLocalpart(machineRef.Fleet(), localpart)
	if err != nil {
		t.Fatalf("principalProxySocketPath(%q): %v", localpart, err)
	}
	return entity.ProxySocketPath(machineRef.Fleet().RunDir(runDir))
}

// machineOptions configures process binaries and daemon settings for
// startMachine. LauncherBinary, DaemonBinary, and Fleet are required;
// the rest are optional and conditionally passed as CLI flags.
type machineOptions struct {
	LauncherBinary         string     // required
	DaemonBinary           string     // required
	Fleet                  *testFleet // required: fleet-scoped rooms for this machine
	ProxyBinary            string     // defaults to PROXY_BINARY from Bazel runfiles
	LogRelayBinary         string     // defaults to LOG_RELAY_BINARY from Bazel runfiles
	ObserveRelayBinary     string     // optional: daemon --observe-relay-binary
	StatusInterval         string     // optional: daemon --status-interval (default "2s")
	HABaseDelay            string     // optional: daemon --ha-base-delay (default "0s" for tests)
	DrainGracePeriod       string     // optional: daemon --drain-grace-period (default "1s" for tests)
	PipelineExecutorBinary string     // optional: daemon --pipeline-executor-binary
	PipelineEnvironment    string     // optional: daemon --pipeline-environment (Nix store path)
	AdminLocalpart         string     // optional: daemon --admin-user (default: derived from admin session)
}

// principalAccount holds the registration details for a principal Matrix
// account. Created by registerPrincipal.
type principalAccount struct {
	Localpart string
	UserID    ref.UserID
	Token     string
}

// principalSpec describes a principal to deploy on a machine. Localpart is
// the account-level name (e.g., "agent/mock-test"); the fleet-scoped
// localpart is constructed internally from the machine's fleet reference.
type principalSpec struct {
	Localpart                 string                      // required: account localpart
	Template                  string                      // optional: template ref (e.g., "bureau/template:name"); defaults to "bureau/template:test-agent"
	Payload                   map[string]any              // optional: per-instance payload merged over template defaults
	MatrixPolicy              *schema.MatrixPolicy        // optional per-principal policy
	ServiceVisibility         []string                    // optional: glob patterns for service discovery
	Authorization             *schema.AuthorizationPolicy // optional: full authorization policy (overrides shorthand fields)
	Labels                    map[string]string           // optional: key-value labels for the principal assignment
	StartCondition            *schema.StartCondition      // optional: condition that must be true before the principal starts
	RestartPolicy             schema.RestartPolicy        // optional: restart policy (empty, RestartPolicyAlways, RestartPolicyOnFailure, RestartPolicyNever)
	ExtraEnvironmentVariables map[string]string           // optional: extra env vars merged into the PrincipalAssignment
}

// deploymentConfig describes what principals to deploy on a machine and
// any machine-level authorization policy.
type deploymentConfig struct {
	Principals    []principalSpec
	DefaultPolicy *schema.AuthorizationPolicy // optional machine-level authorization policy
}

// deploymentResult holds the results of deploying principals. ProxySockets
// maps localpart to the proxy's Unix socket path. Accounts maps localpart
// to the principalAccount (UserID, Token) created during deployment.
type deploymentResult struct {
	ProxySockets map[string]string
	Accounts     map[string]principalAccount
}

// newTestMachine creates the directory structure for a machine. Does not
// start any processes — call startMachine for that. Socket paths are
// derived from RunDir using the principal package, matching the launcher
// and daemon's internal path derivation.
//
// The bareName is the entity-level name (e.g., "restart"), not the
// fleet-relative form ("machine/restart"). The fleet-scoped localpart
// (e.g., "bureau/fleet/testxxx/machine/restart") is constructed from the
// fleet prefix + "machine/" + bareName.
func newTestMachine(t *testing.T, fleet *testFleet, bareName string) *testMachine {
	t.Helper()

	machineRef, err := ref.NewMachine(fleet.Ref, bareName)
	if err != nil {
		t.Fatalf("create machine ref for %q: %v", bareName, err)
	}

	name := machineRef.Localpart()
	stateDir := t.TempDir()
	runDir := tempSocketDir(t)

	return &testMachine{
		Ref:            machineRef,
		Name:           name,
		UserID:         machineRef.UserID(),
		StateDir:       stateDir,
		RunDir:         runDir,
		LauncherSocket: principal.LauncherSocketPath(runDir),
		TmuxSocket:     principal.TmuxSocketPath(runDir),
		ObserveSocket:  principal.ObserveSocketPath(runDir),
		RelaySocket:    principal.RelaySocketPath(runDir),
		WorkspaceRoot:  filepath.Join(stateDir, "workspace"),
		CacheRoot:      filepath.Join(stateDir, "cache"),
	}
}

// startMachine boots the launcher and daemon for a machine, waits for
// readiness signals (launcher socket, machine key publication, status
// heartbeat, config room creation), and populates the machine's PublicKey,
// ConfigRoomID, and MachineRoomID fields.
//
// Both the launcher and daemon are registered for automatic cleanup via
// t.Cleanup. Use startMachineLauncher + startMachineDaemonManual when
// the test needs to kill and restart the daemon mid-test.
func startMachine(t *testing.T, admin *messaging.DirectSession, machine *testMachine, options machineOptions) {
	t.Helper()
	startMachineLauncher(t, admin, machine, options)
	startMachineDaemon(t, admin, machine, options)
}

// startMachineLauncher provisions a machine via the production CLI command
// and boots the launcher with the resulting bootstrap config. This exercises
// the same code path as production deployments:
//
//   - "bureau machine provision" registers the account, invites to all global
//     rooms (machine, service, template, pipeline, system, fleet), and creates
//     the per-machine config room
//   - The launcher reads the bootstrap config, logs in with the one-time
//     password, rotates it to a permanent password derived from the machine's
//     private key, and publishes the machine key
//
// Populates machine.PublicKey and machine.MachineRoomID. Does NOT start
// the daemon — call startMachineDaemon or startMachineDaemonManual after this.
func startMachineLauncher(t *testing.T, admin *messaging.DirectSession, machine *testMachine, options machineOptions) {
	t.Helper()

	if options.LauncherBinary == "" {
		t.Fatal("machineOptions.LauncherBinary is required")
	}
	if options.Fleet == nil {
		t.Fatal("machineOptions.Fleet is required")
	}

	// In production, bureau-proxy and bureau-log-relay are always
	// co-deployed with the launcher. Resolve from Bazel runfiles when
	// the caller doesn't override.
	proxyBinary := options.ProxyBinary
	if proxyBinary == "" {
		proxyBinary = resolvedBinary(t, "PROXY_BINARY")
	}
	logRelayBinary := options.LogRelayBinary
	if logRelayBinary == "" {
		logRelayBinary = resolvedBinary(t, "LOG_RELAY_BINARY")
	}

	// Provision the machine via the production API. This registers the
	// account, invites to all global rooms, creates the config room,
	// and writes a bootstrap config with the one-time password.
	bootstrapFile := filepath.Join(machine.StateDir, "bootstrap.json")
	client := adminClient(t)
	provisionMachine(t, client, admin, machine.Ref, bootstrapFile)

	// The daemon publishes MachineStatus to the fleet's machine room.
	machine.MachineRoomID = options.Fleet.MachineRoomID

	// Start the launcher with the bootstrap config. The launcher logs in
	// with the one-time password from the bootstrap file, rotates it to
	// a permanent password derived from the machine's private key, publishes
	// the machine key to the machine room, and deletes the bootstrap file.
	launcherArgs := []string{
		"--homeserver", testHomeserverURL,
		"--bootstrap-file", bootstrapFile,
		"--machine-name", machine.Name,
		"--server-name", testServerName,
		"--fleet", options.Fleet.Prefix,
		"--run-dir", machine.RunDir,
		"--state-dir", machine.StateDir,
		"--workspace-root", machine.WorkspaceRoot,
		"--cache-root", machine.CacheRoot,
		"--proxy-binary", proxyBinary,
		"--log-relay-binary", logRelayBinary,
		"--operators-group", "",
	}

	keyWatch := watchRoom(t, admin, options.Fleet.MachineRoomID)

	startProcess(t, machine.Name+"-launcher", options.LauncherBinary, launcherArgs...)
	waitForFile(t, machine.LauncherSocket)

	// Retrieve the machine's public key from Matrix.
	machineKeyJSON := keyWatch.WaitForStateEvent(t,
		schema.EventTypeMachineKey, machine.Name)
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
}

// buildDaemonArgs constructs the CLI arguments for starting a daemon
// from the given machine and options. Shared by startMachineDaemon and
// startMachineDaemonManual.
func buildDaemonArgs(machine *testMachine, options machineOptions) []string {
	statusInterval := options.StatusInterval
	if statusInterval == "" {
		statusInterval = "2s"
	}
	haBaseDelay := options.HABaseDelay
	if haBaseDelay == "" {
		haBaseDelay = "0s"
	}
	drainGracePeriod := options.DrainGracePeriod
	if drainGracePeriod == "" {
		drainGracePeriod = "1s"
	}
	adminLocalpart := options.AdminLocalpart
	if adminLocalpart == "" {
		adminLocalpart = "bureau-admin"
	}
	daemonArgs := []string{
		"--homeserver", testHomeserverURL,
		"--machine-name", machine.Name,
		"--server-name", testServerName,
		"--fleet", options.Fleet.Prefix,
		"--run-dir", machine.RunDir,
		"--state-dir", machine.StateDir,
		"--workspace-root", machine.WorkspaceRoot,
		"--admin-user", adminLocalpart,
		"--status-interval", statusInterval,
		"--ha-base-delay", haBaseDelay,
		"--drain-grace-period", drainGracePeriod,
		"--operators-group", "",
	}
	if options.ObserveRelayBinary != "" {
		daemonArgs = append(daemonArgs, "--observe-relay-binary", options.ObserveRelayBinary)
	}
	if options.PipelineExecutorBinary != "" {
		daemonArgs = append(daemonArgs, "--pipeline-executor-binary", options.PipelineExecutorBinary)
	}
	if options.PipelineEnvironment != "" {
		daemonArgs = append(daemonArgs, "--pipeline-environment", options.PipelineEnvironment)
	}
	return daemonArgs
}

// startMachineDaemon starts the daemon for a machine as a managed process
// (registered for automatic cleanup), waits for the first heartbeat, and
// resolves the config room. Populates machine.ConfigRoomID.
func startMachineDaemon(t *testing.T, admin *messaging.DirectSession, machine *testMachine, options machineOptions) {
	t.Helper()

	if options.DaemonBinary == "" {
		t.Fatal("machineOptions.DaemonBinary is required")
	}
	if options.AdminLocalpart == "" {
		options.AdminLocalpart = admin.UserID().Localpart()
	}

	daemonArgs := buildDaemonArgs(machine, options)

	// Set up a watch before starting the daemon to detect the first heartbeat.
	statusWatch := watchRoom(t, admin, machine.MachineRoomID)

	startProcess(t, machine.Name+"-daemon", options.DaemonBinary, daemonArgs...)

	waitForDaemonReady(t, admin, machine, statusWatch)
}

// startMachineDaemonManual starts the daemon for a machine as a manually
// managed process and returns the *exec.Cmd handle. The caller is
// responsible for killing the process (e.g., via cmd.Process.Signal).
// Unlike startMachineDaemon, no automatic cleanup is registered.
//
// Use this for tests that need to kill and restart the daemon mid-test
// (e.g., HA failover, daemon restart recovery).
func startMachineDaemonManual(t *testing.T, admin *messaging.DirectSession, machine *testMachine, options machineOptions) *exec.Cmd {
	t.Helper()

	if options.DaemonBinary == "" {
		t.Fatal("machineOptions.DaemonBinary is required")
	}
	if options.AdminLocalpart == "" {
		options.AdminLocalpart = admin.UserID().Localpart()
	}

	daemonArgs := buildDaemonArgs(machine, options)

	// Set up a watch before starting the daemon to detect the first heartbeat.
	statusWatch := watchRoom(t, admin, machine.MachineRoomID)

	cmd := exec.Command(options.DaemonBinary, daemonArgs...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	t.Logf("daemon started (pid %d)", cmd.Process.Pid)

	waitForDaemonReady(t, admin, machine, statusWatch)

	return cmd
}

// waitForDaemonReady waits for the daemon's first heartbeat and resolves
// the per-machine config room. Shared by startMachineDaemon and
// startMachineDaemonManual.
func waitForDaemonReady(t *testing.T, admin *messaging.DirectSession, machine *testMachine, statusWatch roomWatch) {
	t.Helper()

	ctx := t.Context()

	// Wait for daemon readiness (MachineStatus heartbeat).
	statusWatch.WaitForStateEvent(t,
		schema.EventTypeMachineStatus, machine.Name)

	// Resolve the per-machine config room and join as admin.
	configAlias := machine.Ref.RoomAlias()
	configRoomID, err := admin.ResolveAlias(ctx, configAlias)
	if err != nil {
		t.Fatalf("config room %s not created: %v", configAlias, err)
	}
	if _, err := admin.JoinRoom(ctx, configRoomID); err != nil {
		t.Fatalf("admin join config room: %v", err)
	}
	machine.ConfigRoomID = configRoomID
}

// loginPrincipal logs in to an existing Matrix account and returns a fresh
// principalAccount with a new access token. Use this after registerPrincipal
// to obtain a second token for the same account (e.g., for testing credential
// rotation). Each call creates a new device/session on the homeserver.
func loginPrincipal(t *testing.T, localpart, password string) principalAccount {
	t.Helper()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: testHomeserverURL,
	})
	if err != nil {
		t.Fatalf("create client for principal login: %v", err)
	}

	passwordBuffer, err := secret.NewFromString(password)
	if err != nil {
		t.Fatalf("create password buffer: %v", err)
	}
	defer passwordBuffer.Close()

	session, err := client.Login(t.Context(), localpart, passwordBuffer)
	if err != nil {
		t.Fatalf("login principal %q: %v", localpart, err)
	}
	token := session.AccessToken()
	session.Close()

	return principalAccount{
		Localpart: localpart,
		UserID:    ref.MatrixUserID(localpart, testServer),
		Token:     token,
	}
}

// loginFleetPrincipal logs into a fleet-scoped Matrix account and returns
// a principalAccount with the bare account localpart (matching the convention
// used by registerFleetPrincipal and pushCredentials). Use this for re-login
// scenarios (e.g., token rotation) where the account was originally created
// via registerFleetPrincipal.
func loginFleetPrincipal(t *testing.T, fleet *testFleet, accountLocalpart, password string) principalAccount {
	t.Helper()

	entity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, accountLocalpart)
	if err != nil {
		t.Fatalf("construct fleet-scoped entity for %s: %v", accountLocalpart, err)
	}

	fleetLocalpart := entity.Localpart()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: testHomeserverURL,
	})
	if err != nil {
		t.Fatalf("create client for principal login: %v", err)
	}

	passwordBuffer, err := secret.NewFromString(password)
	if err != nil {
		t.Fatalf("create password buffer: %v", err)
	}
	defer passwordBuffer.Close()

	session, err := client.Login(t.Context(), fleetLocalpart, passwordBuffer)
	if err != nil {
		t.Fatalf("login fleet principal %q (fleet localpart %q): %v", accountLocalpart, fleetLocalpart, err)
	}
	token := session.AccessToken()
	session.Close()

	return principalAccount{
		Localpart: accountLocalpart,
		UserID:    entity.UserID(),
		Token:     token,
	}
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

	passwordBuffer, err := secret.NewFromString(password)
	if err != nil {
		t.Fatalf("create password buffer: %v", err)
	}
	defer passwordBuffer.Close()

	registrationTokenBuffer, err := secret.NewFromString(testRegistrationToken)
	if err != nil {
		t.Fatalf("create registration token buffer: %v", err)
	}
	defer registrationTokenBuffer.Close()

	session, err := client.Register(t.Context(), messaging.RegisterRequest{
		Username:          localpart,
		Password:          passwordBuffer,
		RegistrationToken: registrationTokenBuffer,
	})
	if err != nil {
		t.Fatalf("register principal %q: %v", localpart, err)
	}
	token := session.AccessToken()
	session.Close()

	return principalAccount{
		Localpart: localpart,
		UserID:    ref.MatrixUserID(localpart, testServer),
		Token:     token,
	}
}

// registerFleetPrincipal creates a Matrix account with a fleet-scoped
// localpart, matching the production credential provisioning flow
// (principal.Create registers accounts with entity.Localpart()). The
// returned principalAccount has:
//   - Localpart: the bare account localpart (for pushCredentials, which
//     constructs the fleet-scoped entity via NewEntityFromAccountLocalpart)
//   - UserID: the fleet-scoped Matrix user ID (matching what the daemon
//     invites to rooms and what the proxy authenticates as)
//   - Token: the access token for the fleet-scoped Matrix account
//
// Use this instead of registerPrincipal when the test involves room
// membership operations through the proxy (workspace pipelines, Matrix
// join/invite). Tests that only check proxy socket creation or whoami
// can use registerPrincipal (bare-localpart accounts work fine when no
// room membership is needed).
func registerFleetPrincipal(t *testing.T, fleet *testFleet, accountLocalpart, password string) principalAccount {
	t.Helper()

	entity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, accountLocalpart)
	if err != nil {
		t.Fatalf("construct fleet-scoped entity for %s: %v", accountLocalpart, err)
	}

	fleetLocalpart := entity.Localpart()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: testHomeserverURL,
	})
	if err != nil {
		t.Fatalf("create client for principal registration: %v", err)
	}

	passwordBuffer, err := secret.NewFromString(password)
	if err != nil {
		t.Fatalf("create password buffer: %v", err)
	}
	defer passwordBuffer.Close()

	registrationTokenBuffer, err := secret.NewFromString(testRegistrationToken)
	if err != nil {
		t.Fatalf("create registration token buffer: %v", err)
	}
	defer registrationTokenBuffer.Close()

	session, err := client.Register(t.Context(), messaging.RegisterRequest{
		Username:          fleetLocalpart,
		Password:          passwordBuffer,
		RegistrationToken: registrationTokenBuffer,
	})
	if err != nil {
		t.Fatalf("register fleet principal %q (fleet localpart %q): %v", accountLocalpart, fleetLocalpart, err)
	}
	token := session.AccessToken()
	session.Close()

	return principalAccount{
		Localpart: accountLocalpart,
		UserID:    entity.UserID(),
		Token:     token,
	}
}

// principalSession creates a messaging.Session for a registered principal
// using its access token. The caller is responsible for closing the session.
func principalSession(t *testing.T, account principalAccount) *messaging.DirectSession {
	t.Helper()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: testHomeserverURL,
	})
	if err != nil {
		t.Fatalf("create client for principal session: %v", err)
	}
	session, err := client.SessionFromToken(account.UserID, account.Token)
	if err != nil {
		t.Fatalf("session from token for %s: %v", account.Localpart, err)
	}
	return session
}

// pushCredentials encrypts a principal's Matrix credentials with the
// machine's public key and pushes them as an m.bureau.credentials state
// event to the machine's config room. Uses the production credential
// provisioning library: fetches the machine key from Matrix, encrypts
// with age, and publishes to the config room.
func pushCredentials(t *testing.T, admin *messaging.DirectSession, machine *testMachine, account principalAccount) {
	t.Helper()

	if machine.Name == "" {
		t.Fatal("machine.Name is required")
	}
	if machine.MachineRoomID.IsZero() {
		t.Fatal("machine.MachineRoomID is required")
	}
	if account.Localpart == "" {
		t.Fatal("account.Localpart is required")
	}
	if account.Token == "" {
		t.Fatal("account.Token is required")
	}

	entity, err := ref.NewEntityFromAccountLocalpart(machine.Ref.Fleet(), account.Localpart)
	if err != nil {
		t.Fatalf("build principal entity for %s: %v", account.Localpart, err)
	}
	_, err = credential.Provision(t.Context(), admin, credential.ProvisionParams{
		Machine:       machine.Ref,
		Principal:     entity,
		MachineRoomID: machine.MachineRoomID,
		Credentials: map[string]string{
			"MATRIX_TOKEN":          account.Token,
			"MATRIX_USER_ID":        account.UserID.String(),
			"MATRIX_HOMESERVER_URL": testHomeserverURL,
		},
	})
	if err != nil {
		t.Fatalf("provision credentials for %s on %s: %v", account.Localpart, machine.Name, err)
	}
}

// pushMachineConfig builds and pushes an m.bureau.machine_config state event
// to the machine's config room. The daemon detects this via /sync and
// reconciles: creating proxies for new principals and destroying proxies
// for removed ones.
func pushMachineConfig(t *testing.T, admin *messaging.DirectSession, machine *testMachine, config deploymentConfig) {
	t.Helper()

	if machine.ConfigRoomID.IsZero() {
		t.Fatal("machine.ConfigRoomID is required")
	}
	if machine.Name == "" {
		t.Fatal("machine.Name is required")
	}

	// Combine explicitly requested principals with any services previously
	// deployed via deployService. Services use AutoStart=true and must
	// remain in every MachineConfig update, otherwise the daemon destroys
	// their sandboxes when it reconciles the new config.
	allSpecs := slices.Concat(config.Principals, machine.deployedServices)

	assignments := make([]schema.PrincipalAssignment, len(allSpecs))
	for i, spec := range allSpecs {
		entity, err := ref.NewEntityFromAccountLocalpart(machine.Ref.Fleet(), spec.Localpart)
		if err != nil {
			t.Fatalf("build principal entity for %s: %v", spec.Localpart, err)
		}
		assignments[i] = schema.PrincipalAssignment{
			Principal:                 entity,
			Template:                  spec.Template,
			AutoStart:                 true,
			Payload:                   spec.Payload,
			MatrixPolicy:              spec.MatrixPolicy,
			ServiceVisibility:         spec.ServiceVisibility,
			Authorization:             spec.Authorization,
			Labels:                    spec.Labels,
			StartCondition:            spec.StartCondition,
			RestartPolicy:             spec.RestartPolicy,
			ExtraEnvironmentVariables: spec.ExtraEnvironmentVariables,
		}
	}

	machineConfig := schema.MachineConfig{
		Principals:    assignments,
		DefaultPolicy: config.DefaultPolicy,
	}

	_, err := admin.SendStateEvent(t.Context(), machine.ConfigRoomID,
		schema.EventTypeMachineConfig, machine.Name, machineConfig)
	if err != nil {
		t.Fatalf("push machine config: %v", err)
	}
}

// deployPrincipals registers Matrix accounts, provisions encrypted
// credentials, publishes a MachineConfig, and waits for all proxy sockets
// to appear. Uses the production principal.CreateMultiple path for account
// registration, credential provisioning, and atomic MachineConfig writes.
//
// If any spec has an empty Template, the default test-agent template is
// auto-published. If DefaultPolicy is set, it is written as a pre-step
// so that assignPrincipals preserves it during the read-modify-write.
func deployPrincipals(t *testing.T, admin *messaging.DirectSession, machine *testMachine, config deploymentConfig) deploymentResult {
	t.Helper()

	if len(config.Principals) == 0 {
		t.Fatal("config.Principals must not be empty")
	}
	if machine.ConfigRoomID.IsZero() {
		t.Fatal("machine.ConfigRoomID is required")
	}
	if machine.MachineRoomID.IsZero() {
		t.Fatal("machine.MachineRoomID is required")
	}

	// Auto-publish the default test template if any spec omits Template.
	defaultTemplateRef := ""
	for _, spec := range config.Principals {
		if spec.Template == "" {
			if defaultTemplateRef == "" {
				defaultTemplateRef = publishTestAgentTemplate(t, admin, machine, "test-agent")
			}
			break
		}
	}

	// If DefaultPolicy is set, publish it as a pre-step. The production
	// assignPrincipals function does a read-modify-write, so DefaultPolicy
	// is preserved when principals are added.
	if config.DefaultPolicy != nil {
		_, err := admin.SendStateEvent(t.Context(), machine.ConfigRoomID,
			schema.EventTypeMachineConfig, machine.Name, schema.MachineConfig{
				DefaultPolicy: config.DefaultPolicy,
			})
		if err != nil {
			t.Fatalf("set default policy: %v", err)
		}
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: testHomeserverURL,
	})
	if err != nil {
		t.Fatalf("create client for principal registration: %v", err)
	}

	registrationToken, err := secret.NewFromString(testRegistrationToken)
	if err != nil {
		t.Fatalf("create registration token buffer: %v", err)
	}
	defer registrationToken.Close()

	params := make([]principal.CreateParams, len(config.Principals))
	for i, spec := range config.Principals {
		entity, entityErr := ref.NewEntityFromAccountLocalpart(machine.Ref.Fleet(), spec.Localpart)
		if entityErr != nil {
			t.Fatalf("build principal entity for %s: %v", spec.Localpart, entityErr)
		}

		templateRefString := spec.Template
		if templateRefString == "" {
			templateRefString = defaultTemplateRef
		}
		templateRef, parseErr := schema.ParseTemplateRef(templateRefString)
		if parseErr != nil {
			t.Fatalf("parse template ref %q: %v", templateRefString, parseErr)
		}

		params[i] = principal.CreateParams{
			Machine:     machine.Ref,
			Principal:   entity,
			TemplateRef: templateRef,
			ValidateTemplate: func(ctx context.Context, tr schema.TemplateRef, serverName ref.ServerName) error {
				_, fetchErr := templatedef.Fetch(ctx, admin, tr, serverName)
				return fetchErr
			},
			HomeserverURL:     testHomeserverURL,
			AutoStart:         true,
			MachineRoomID:     machine.MachineRoomID,
			Payload:           spec.Payload,
			MatrixPolicy:      spec.MatrixPolicy,
			ServiceVisibility: spec.ServiceVisibility,
			Authorization:     spec.Authorization,
			Labels:            spec.Labels,
			StartCondition:    spec.StartCondition,
			RestartPolicy:     spec.RestartPolicy,
		}
	}

	results, err := principal.CreateMultiple(t.Context(), client, admin, registrationToken, credential.AsProvisionFunc(), params)
	if err != nil {
		t.Fatalf("create principals: %v", err)
	}

	proxySockets := make(map[string]string, len(config.Principals))
	accounts := make(map[string]principalAccount, len(config.Principals))
	for i, spec := range config.Principals {
		socketPath := machine.PrincipalProxySocketPath(t, spec.Localpart)
		waitForFile(t, socketPath)
		proxySockets[spec.Localpart] = socketPath
		accounts[spec.Localpart] = principalAccount{
			Localpart: spec.Localpart,
			UserID:    results[i].PrincipalUserID,
			Token:     results[i].AccessToken,
		}
	}

	return deploymentResult{
		ProxySockets: proxySockets,
		Accounts:     accounts,
	}
}

// agentOptions configures deployAgent and agentTemplateContent. Binary and
// Localpart are required for deployAgent; agentTemplateContent only reads
// template-relevant fields (Binary, TemplateName, RequiredServices, ExtraEnv).
type agentOptions struct {
	// Binary is the agent binary path. Required.
	Binary string

	// Localpart is the principal localpart (e.g., "agent/mock-test").
	// Required for deployAgent. Must be unique across the test suite
	// (tests run in parallel and share one homeserver).
	Localpart string

	// TemplateName overrides the template state key. Default: Localpart
	// with "/" replaced by "-".
	TemplateName string

	// RequiredServices lists services the template declares (e.g., ["ticket"]).
	RequiredServices []string

	// ExtraEnv adds environment variables beyond the standard base set
	// (HOME, TERM, BUREAU_PROXY_SOCKET, BUREAU_MACHINE_NAME, BUREAU_SERVER_NAME).
	ExtraEnv map[string]string

	// Authorization is the per-principal authorization policy.
	Authorization *schema.AuthorizationPolicy

	// Payload is the per-instance payload merged over template defaults.
	Payload map[string]any

	// StartCondition gates this principal's launch on the existence of a
	// specific state event. When set, the daemon only starts the sandbox
	// if the referenced event exists and matches the content criteria.
	StartCondition *schema.StartCondition

	// RestartPolicy controls what happens when a sandbox exits. See
	// schema.PrincipalAssignment.RestartPolicy for valid values.
	RestartPolicy schema.RestartPolicy

	// ExtraMounts adds filesystem mounts beyond the standard base set.
	// Use for test diagnostics (e.g., bind-mounting a host directory
	// to capture debug logs from inside the sandbox).
	ExtraMounts []schema.TemplateMount

	// SkipWaitForReady skips waiting for "agent-ready" after deployment.
	// Set to true for agents expected to exit/fail before posting ready.
	SkipWaitForReady bool
}

// agentDeployment holds the result of deployAgent: account details, socket
// paths, and template reference for test assertions and post-deployment
// interactions (e.g., registering mock LLM services on the admin socket).
type agentDeployment struct {
	Account         principalAccount
	TemplateRef     schema.TemplateRef
	ProxySocketPath string
	AdminSocketPath string
}

// agentTemplateContent builds a schema.TemplateContent for an agent binary
// with the standard Bureau sandbox base configuration. All agent templates
// share the same Namespaces, Security, Filesystem, CreateDirs, and base
// environment variables — this function defines that base exactly once.
//
// Caller-specified overrides (RequiredServices, ExtraEnv) are merged on top.
// The binary is bind-mounted read-only into the sandbox at its host path.
func agentTemplateContent(binary string, options agentOptions) schema.TemplateContent {
	templateName := options.TemplateName
	if templateName == "" && options.Localpart != "" {
		templateName = strings.ReplaceAll(options.Localpart, "/", "-")
	}

	description := "Agent template"
	if templateName != "" {
		description = "Agent template for " + templateName
	}

	environmentVariables := map[string]string{
		"HOME":                "/workspace",
		"TERM":                "xterm-256color",
		"BUREAU_PROXY_SOCKET": "${PROXY_SOCKET}",
		"BUREAU_MACHINE_NAME": "${MACHINE_NAME}",
		"BUREAU_SERVER_NAME":  "${SERVER_NAME}",
	}
	for key, value := range options.ExtraEnv {
		environmentVariables[key] = value
	}

	filesystem := []schema.TemplateMount{
		{Source: binary, Dest: binary, Mode: schema.MountModeRO},
		{Dest: "/tmp", Type: "tmpfs"},
	}
	filesystem = append(filesystem, options.ExtraMounts...)

	return schema.TemplateContent{
		Description:      description,
		Command:          []string{binary},
		RequiredServices: options.RequiredServices,
		Namespaces:       &schema.TemplateNamespaces{PID: true},
		Security: &schema.TemplateSecurity{
			NewSession:    true,
			DieWithParent: true,
			NoNewPrivs:    true,
		},
		Filesystem:           filesystem,
		CreateDirs:           []string{"/tmp", "/var/tmp", "/run/bureau"},
		EnvironmentVariables: environmentVariables,
	}
}

// deployAgent performs the full agent deployment flow using lib/agent.Create:
// publish template, register Matrix account, provision encrypted credentials,
// join config room, publish MachineConfig assignment, wait for proxy socket,
// and wait for agent-ready.
//
// For multi-agent deployments (multiple principals sharing one template),
// use agentTemplateContent directly with the lower-level helpers.
func deployAgent(t *testing.T, admin *messaging.DirectSession, machine *testMachine, options agentOptions) agentDeployment {
	t.Helper()

	if options.Binary == "" {
		t.Fatal("agentOptions.Binary is required")
	}
	if options.Localpart == "" {
		t.Fatal("agentOptions.Localpart is required")
	}
	if machine.Name == "" {
		t.Fatal("machine.Name is required")
	}
	if machine.MachineRoomID.IsZero() {
		t.Fatal("machine.MachineRoomID is required")
	}
	if machine.ConfigRoomID.IsZero() {
		t.Fatal("machine.ConfigRoomID is required")
	}

	ctx := t.Context()

	// Derive template name from localpart if not explicitly set.
	templateName := options.TemplateName
	if templateName == "" {
		templateName = strings.ReplaceAll(options.Localpart, "/", "-")
	}

	// Publish the template — Create() validates it exists but doesn't push it.
	grantTemplateAccess(t, admin, machine)

	templateRef, err := schema.ParseTemplateRef("bureau/template:" + templateName)
	if err != nil {
		t.Fatalf("parse template ref for %q: %v", templateName, err)
	}
	_, err = templatedef.Push(ctx, admin, templateRef,
		agentTemplateContent(options.Binary, options), testServer)
	if err != nil {
		t.Fatalf("push agent template %q: %v", templateName, err)
	}

	// Set up watch BEFORE Create() pushes the config so we don't miss
	// the daemon's reconciliation events or agent-ready.
	readyWatch := watchRoom(t, admin, machine.ConfigRoomID)

	// Create the agent via the production library. This registers the
	// Matrix account, provisions encrypted credentials, joins the config
	// room, and publishes the MachineConfig assignment.
	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: testHomeserverURL,
	})
	if err != nil {
		t.Fatalf("create client for agent creation: %v", err)
	}
	registrationTokenBuffer, err := secret.NewFromString(testRegistrationToken)
	if err != nil {
		t.Fatalf("create registration token buffer: %v", err)
	}
	defer registrationTokenBuffer.Close()

	principalEntity, err := ref.NewEntityFromAccountLocalpart(machine.Ref.Fleet(), options.Localpart)
	if err != nil {
		t.Fatalf("build principal entity for %s: %v", options.Localpart, err)
	}
	result, err := principal.Create(ctx, client, admin, registrationTokenBuffer, credential.AsProvisionFunc(), principal.CreateParams{
		Machine:     machine.Ref,
		Principal:   principalEntity,
		TemplateRef: templateRef,
		ValidateTemplate: func(ctx context.Context, ref schema.TemplateRef, serverName ref.ServerName) error {
			_, err := templatedef.Fetch(ctx, admin, ref, serverName)
			return err
		},
		HomeserverURL:  testHomeserverURL,
		AutoStart:      true,
		StartCondition: options.StartCondition,
		RestartPolicy:  options.RestartPolicy,
		MachineRoomID:  machine.MachineRoomID,
		Payload:        options.Payload,
		Authorization:  options.Authorization,
	})
	if err != nil {
		t.Fatalf("create agent %q: %v", options.Localpart, err)
	}

	proxySocketPath := machine.PrincipalProxySocketPath(t, options.Localpart)
	waitForFile(t, proxySocketPath)

	if !options.SkipWaitForReady {
		readyWatch.WaitForMessage(t, "agent-ready", result.PrincipalUserID)
	}

	return agentDeployment{
		Account: principalAccount{
			Localpart: options.Localpart,
			UserID:    result.PrincipalUserID,
			Token:     result.AccessToken,
		},
		TemplateRef:     templateRef,
		ProxySocketPath: proxySocketPath,
		AdminSocketPath: machine.PrincipalProxyAdminSocketPath(t, options.Localpart),
	}
}

// --- Service deployment ---

// serviceDeployOptions holds options for deploying a Bureau service binary
// via deployService. Parallel to agentOptions for deployAgent.
type serviceDeployOptions struct {
	// Binary is the service binary path. Required.
	Binary string

	// Name is the human-readable name for process logs. Required.
	Name string

	// Localpart is the service principal localpart (e.g., "service/ticket/test").
	// Must be unique across the test suite (tests run in parallel and share
	// one homeserver). Required.
	Localpart string

	// Command overrides the default sandbox command (which is just the
	// binary path with no arguments). Use this for services that require
	// flags (e.g., artifact service needs --store-dir).
	Command []string

	// RequiredServices lists service roles this service depends on. The
	// daemon resolves each role's socket via m.bureau.service_binding state
	// events and bind-mounts it into the sandbox at
	// /run/bureau/service/<role>.sock.
	RequiredServices []string

	// ExtraMounts adds filesystem bind mounts to the service template.
	// Use this for test-specific files that must be visible inside the
	// sandbox (e.g., dummy token files for services that require
	// artifact access but don't exercise the artifact path).
	ExtraMounts []schema.TemplateMount

	// ExtraEnvironmentVariables are merged into the MachineConfig
	// PrincipalAssignment, which the daemon passes to the sandbox.
	ExtraEnvironmentVariables map[string]string

	// ExtraRooms lists additional rooms to invite the service to, beyond
	// the system room and fleet service room (which principal.Create handles).
	ExtraRooms []ref.RoomID

	// MatrixPolicy controls which self-service Matrix operations this
	// service's proxy allows (join, invite, create room). When nil,
	// the proxy uses default-deny.
	MatrixPolicy *schema.MatrixPolicy

	// Authorization is the authorization policy for this service
	// principal. Grants are included in service tokens the daemon
	// mints when resolving RequiredServices for other principals.
	Authorization *schema.AuthorizationPolicy
}

// serviceDeployResult holds the result of deployService: entity reference,
// account details, and the canonical service socket path.
type serviceDeployResult struct {
	Entity     ref.Entity
	Account    principalAccount
	SocketPath string
}

// serviceTemplateContent builds a sandbox-ready schema.TemplateContent for
// a service binary. Services run in daemon-managed sandboxes (AutoStart:
// true) and bootstrap via BootstrapViaProxy, which reads BUREAU_PROXY_SOCKET,
// BUREAU_MACHINE_NAME, and BUREAU_SERVER_NAME from the template environment.
// The launcher injects BUREAU_PRINCIPAL_NAME, BUREAU_FLEET, and
// BUREAU_SERVICE_SOCKET for service entities.
//
// When command is non-empty, it overrides the default command (which is
// just [binary]). The binary is still bind-mounted regardless — the command
// override is for services that need flags (e.g., --store-dir).
func serviceTemplateContent(binary string, command, requiredServices []string, extraMounts []schema.TemplateMount) schema.TemplateContent {
	if len(command) == 0 {
		command = []string{binary}
	}

	filesystem := []schema.TemplateMount{
		{Source: binary, Dest: binary, Mode: schema.MountModeRO},
		{Dest: "/tmp", Type: "tmpfs"},
	}
	filesystem = append(filesystem, extraMounts...)

	return schema.TemplateContent{
		Description:      "Service template",
		Command:          command,
		RequiredServices: requiredServices,
		Namespaces:       &schema.TemplateNamespaces{PID: true},
		Security: &schema.TemplateSecurity{
			NewSession:    true,
			DieWithParent: true,
			NoNewPrivs:    true,
		},
		Filesystem: filesystem,
		CreateDirs: []string{"/tmp", "/var/tmp", "/run/bureau"},
		EnvironmentVariables: map[string]string{
			"HOME":                "/workspace",
			"TERM":                "xterm-256color",
			"BUREAU_PROXY_SOCKET": "${PROXY_SOCKET}",
			"BUREAU_MACHINE_NAME": "${MACHINE_NAME}",
			"BUREAU_SERVER_NAME":  "${SERVER_NAME}",
		},
	}
}

// deployService deploys a Bureau service binary using the production
// principal.Create() path with AutoStart: true. The daemon creates a
// sandbox, the proxy injects credentials, and the service bootstraps
// via BootstrapViaProxy.
//
// The deployment sequence:
//   - Pushes a sandbox-ready service template to Matrix
//   - Calls principal.Create(AutoStart: true) to register the account,
//     provision credentials, join the config room, and publish a
//     MachineConfig assignment
//   - Invites the service to any ExtraRooms (system room and fleet
//     service room are handled by daemon reconciliation)
//   - Waits for the proxy socket (daemon created the sandbox)
//   - Waits for the daemon's service directory update notification
//     (service registered via BootstrapViaProxy and daemon propagated
//     the directory to all proxies)
func deployService(
	t *testing.T,
	admin *messaging.DirectSession,
	fleet *testFleet,
	machine *testMachine,
	options serviceDeployOptions,
) serviceDeployResult {
	t.Helper()

	if options.Binary == "" {
		t.Fatal("serviceDeployOptions.Binary is required")
	}
	if options.Name == "" {
		t.Fatal("serviceDeployOptions.Name is required")
	}
	if options.Localpart == "" {
		t.Fatal("serviceDeployOptions.Localpart is required")
	}

	ctx := t.Context()

	// Derive template name from localpart.
	templateName := strings.ReplaceAll(options.Localpart, "/", "-")

	// Push a sandbox-ready service template. The daemon uses this to
	// build the sandbox spec (command, mounts, env vars, security).
	grantTemplateAccess(t, admin, machine)

	templateRef, err := schema.ParseTemplateRef("bureau/template:" + templateName)
	if err != nil {
		t.Fatalf("parse service template ref for %q: %v", templateName, err)
	}
	_, err = templatedef.Push(ctx, admin, templateRef,
		serviceTemplateContent(options.Binary, options.Command, options.RequiredServices, options.ExtraMounts), testServer)
	if err != nil {
		t.Fatalf("push service template %q: %v", templateName, err)
	}

	// Set up the room watch BEFORE principal.Create() publishes the
	// MachineConfig, so we don't miss the daemon's service directory
	// update notification.
	serviceWatch := watchRoom(t, admin, machine.ConfigRoomID)

	// Create the principal via the production path. AutoStart is true:
	// the daemon creates a sandbox, the proxy injects credentials, and
	// the service bootstraps via BootstrapViaProxy.
	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: testHomeserverURL,
	})
	if err != nil {
		t.Fatalf("create client for service deployment: %v", err)
	}
	registrationTokenBuffer, err := secret.NewFromString(testRegistrationToken)
	if err != nil {
		t.Fatalf("create registration token buffer: %v", err)
	}
	defer registrationTokenBuffer.Close()

	principalEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, options.Localpart)
	if err != nil {
		t.Fatalf("build service entity for %s: %v", options.Localpart, err)
	}
	result, err := principal.Create(ctx, client, admin, registrationTokenBuffer, credential.AsProvisionFunc(), principal.CreateParams{
		Machine:     machine.Ref,
		Principal:   principalEntity,
		TemplateRef: templateRef,
		ValidateTemplate: func(ctx context.Context, ref schema.TemplateRef, serverName ref.ServerName) error {
			_, err := templatedef.Fetch(ctx, admin, ref, serverName)
			return err
		},
		HomeserverURL:             testHomeserverURL,
		AutoStart:                 true,
		MachineRoomID:             machine.MachineRoomID,
		ExtraEnvironmentVariables: options.ExtraEnvironmentVariables,
		MatrixPolicy:              options.MatrixPolicy,
		Authorization:             options.Authorization,
	})
	if err != nil {
		t.Fatalf("create service %q: %v", options.Localpart, err)
	}

	// Invite the service to any extra rooms the caller specified.
	// System room and fleet service room invitations are handled by
	// daemon reconciliation (ensurePrincipalRoomAccess for service
	// entity types). These invitations happen between principal.Create
	// returning and the daemon processing the MachineConfig: the
	// daemon picks up the config on its next /sync cycle, so there
	// is always a window for the invitations.
	if len(options.ExtraRooms) > 0 {
		inviteToRooms(t, admin, result.PrincipalUserID, options.ExtraRooms...)
	}

	// Wait for the proxy socket — indicates the daemon created the
	// sandbox and the proxy is ready.
	proxySocketPath := machine.PrincipalProxySocketPath(t, options.Localpart)
	waitForFile(t, proxySocketPath)

	// Wait for daemon to discover the service registration (posted by
	// BootstrapViaProxy to the fleet service room) via /sync and
	// propagate the service directory to all proxies.
	fleetScopedLocalpart := principalEntity.Localpart()
	waitForNotification[schema.ServiceDirectoryUpdatedMessage](
		t, &serviceWatch, schema.MsgTypeServiceDirectoryUpdated, machine.UserID,
		func(message schema.ServiceDirectoryUpdatedMessage) bool {
			return slices.Contains(message.Added, fleetScopedLocalpart)
		}, "service directory update adding "+fleetScopedLocalpart)

	// The service socket is behind a symlink created by the launcher
	// (ServiceSocketPath → configDir/listen/service.sock). The symlink
	// itself is created during sandbox setup, but the target doesn't
	// exist until the service binary creates its CBOR socket. Because
	// BootstrapViaProxy registers the service BEFORE the binary creates
	// the socket, the ServiceDirectoryUpdatedMessage can arrive before
	// the socket target exists. Resolve the symlink and wait for the
	// target file to appear.
	socketPath := machine.PrincipalServiceSocketPath(t, options.Localpart)
	socketTarget, err := os.Readlink(socketPath)
	if err != nil {
		t.Fatalf("readlink service socket %s: %v", socketPath, err)
	}
	waitForFile(t, socketTarget)

	// Record the service in the machine's deployed services list so that
	// subsequent pushMachineConfig calls (e.g., deploying an agent) include
	// this service in the MachineConfig. Without this, the daemon would see
	// the service missing from the new config and destroy its sandbox.
	machine.deployedServices = append(machine.deployedServices, principalSpec{
		Localpart:                 options.Localpart,
		Template:                  "bureau/template:" + templateName,
		MatrixPolicy:              options.MatrixPolicy,
		ExtraEnvironmentVariables: options.ExtraEnvironmentVariables,
	})

	return serviceDeployResult{
		Entity: principalEntity,
		Account: principalAccount{
			Localpart: options.Localpart,
			UserID:    result.PrincipalUserID,
			Token:     result.AccessToken,
		},
		SocketPath: socketPath,
	}
}

// resolveSystemRoom resolves the #bureau/system room ID.
func resolveSystemRoom(t *testing.T, admin *messaging.DirectSession) ref.RoomID {
	t.Helper()

	systemRoomID, err := admin.ResolveAlias(t.Context(), testNamespace.SystemRoomAlias())
	if err != nil {
		t.Fatalf("resolve system room: %v", err)
	}
	return systemRoomID
}

// inviteToRooms invites a user to one or more rooms, ignoring M_FORBIDDEN
// (already joined).
func inviteToRooms(t *testing.T, admin *messaging.DirectSession, userID ref.UserID, roomIDs ...ref.RoomID) {
	t.Helper()

	ctx := t.Context()
	for _, roomID := range roomIDs {
		if err := admin.InviteUser(ctx, roomID, userID); err != nil {
			if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
				t.Fatalf("invite %s to %s: %v", userID, roomID, err)
			}
		}
	}
}

// grantTemplateAccess resolves the #bureau/template room and invites the
// machine so the daemon can read templates during config reconciliation.
// Returns the template room ID for tests that need to publish custom
// templates.
func grantTemplateAccess(t *testing.T, admin *messaging.DirectSession, machine *testMachine) ref.RoomID {
	t.Helper()

	if machine.UserID.IsZero() {
		t.Fatal("machine.UserID is required")
	}

	templateRoomID, err := admin.ResolveAlias(t.Context(),
		testNamespace.TemplateRoomAlias())
	if err != nil {
		t.Fatalf("resolve template room: %v", err)
	}
	if err := admin.InviteUser(t.Context(), templateRoomID, machine.UserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			t.Fatalf("invite machine to template room: %v", err)
		}
	}
	return templateRoomID
}

// publishTestAgentTemplate grants template room access and publishes a
// minimal test-agent template suitable for sandbox creation. Uses the
// production template push library to resolve the room and publish the
// state event. Returns the template reference string (e.g.,
// "bureau/template:fleet-test-agent") for use in fleet service
// definitions or PrincipalAssignment templates.
func publishTestAgentTemplate(t *testing.T, admin *messaging.DirectSession, machine *testMachine, templateName string) string {
	t.Helper()

	if templateName == "" {
		t.Fatal("templateName is required")
	}

	testAgentBinary := resolvedBinary(t, "TEST_AGENT_BINARY")
	grantTemplateAccess(t, admin, machine)

	ref, err := schema.ParseTemplateRef("bureau/template:" + templateName)
	if err != nil {
		t.Fatalf("parse template ref for %q: %v", templateName, err)
	}

	_, err = templatedef.Push(t.Context(), admin, ref,
		agentTemplateContent(testAgentBinary, agentOptions{TemplateName: templateName}),
		testServer)
	if err != nil {
		t.Fatalf("push test agent template %q: %v", templateName, err)
	}

	return ref.String()
}

// joinConfigRoom invites an agent to the config room and joins via a
// direct session. The agent needs config room membership so the lifecycle
// manager can post messages (agent-ready, text responses) to the room.
func joinConfigRoom(t *testing.T, admin *messaging.DirectSession, configRoomID ref.RoomID, agent principalAccount) {
	t.Helper()

	if configRoomID.IsZero() {
		t.Fatal("configRoomID is required")
	}
	if agent.UserID.IsZero() {
		t.Fatal("agent.UserID is required")
	}
	if agent.Localpart == "" {
		t.Fatal("agent.Localpart is required")
	}

	ctx := t.Context()
	if err := admin.InviteUser(ctx, configRoomID, agent.UserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			t.Fatalf("invite agent %s to config room: %v", agent.Localpart, err)
		}
	}
	agentSession := principalSession(t, agent)
	if _, err := agentSession.JoinRoom(ctx, configRoomID); err != nil {
		t.Fatalf("agent %s join config room: %v", agent.Localpart, err)
	}
	agentSession.Close()
}

// mintTestServiceToken creates a service token signed by the machine's
// Ed25519 key pair. This produces a token that the target service will
// accept (signature validates, audience matches, not blacklisted),
// independent of the daemon's sandbox lifecycle.
//
// Use this instead of reading tokens from the daemon's state directory:
// daemon-minted tokens are revoked when the sandbox exits, so any test
// that outlives the sandbox (which is most of them — the test agent
// exits almost immediately) would use a revoked token.
func mintTestServiceToken(t *testing.T, machine *testMachine, principal ref.Entity, serviceRole string, grants []servicetoken.Grant) []byte {
	t.Helper()

	_, privateKey, err := servicetoken.LoadKeypair(machine.StateDir)
	if err != nil {
		t.Fatalf("load machine signing keypair from %s: %v", machine.StateDir, err)
	}

	tokenID := make([]byte, 16)
	if _, err := rand.Read(tokenID); err != nil {
		t.Fatalf("generate token ID: %v", err)
	}

	// Use fixed timestamps far in the future so the token is always
	// valid during test execution. Wall clock is not needed here —
	// the token just needs IssuedAt < now < ExpiresAt at verification
	// time. 2025-01-01 to 2099-01-01 satisfies that for any test run.
	token := &servicetoken.Token{
		Subject:   principal.UserID(),
		Machine:   machine.Ref,
		Audience:  serviceRole,
		Grants:    grants,
		ID:        hex.EncodeToString(tokenID),
		IssuedAt:  1735689600, // 2025-01-01T00:00:00Z
		ExpiresAt: 4070908800, // 2099-01-01T00:00:00Z
	}

	tokenBytes, err := servicetoken.Mint(privateKey, token)
	if err != nil {
		t.Fatalf("mint %s token for %s: %v", serviceRole, principal, err)
	}
	return tokenBytes
}

// mintTestServiceTokenForUser mints a service token with an arbitrary
// ref.UserID as the subject. Unlike mintTestServiceToken which takes a
// fleet Entity, this variant accepts a bare UserID — useful for minting
// tokens that represent non-fleet principals (e.g., an admin user acting
// as a reviewer in the stewardship flow).
func mintTestServiceTokenForUser(t *testing.T, machine *testMachine, subject ref.UserID, serviceRole string, grants []servicetoken.Grant) []byte {
	t.Helper()

	_, privateKey, err := servicetoken.LoadKeypair(machine.StateDir)
	if err != nil {
		t.Fatalf("load machine signing keypair from %s: %v", machine.StateDir, err)
	}

	tokenID := make([]byte, 16)
	if _, err := rand.Read(tokenID); err != nil {
		t.Fatalf("generate token ID: %v", err)
	}

	token := &servicetoken.Token{
		Subject:   subject,
		Machine:   machine.Ref,
		Audience:  serviceRole,
		Grants:    grants,
		ID:        hex.EncodeToString(tokenID),
		IssuedAt:  1735689600, // 2025-01-01T00:00:00Z
		ExpiresAt: 4070908800, // 2099-01-01T00:00:00Z
	}

	tokenBytes, err := servicetoken.Mint(privateKey, token)
	if err != nil {
		t.Fatalf("mint %s token for %s: %v", serviceRole, subject, err)
	}
	return tokenBytes
}
