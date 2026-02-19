// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/credential"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/template"
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
	CacheRoot      string

	// Populated by startMachine:
	PublicKey     string // age public key from the machine_key state event
	ConfigRoomID  string // per-machine config room
	MachineRoomID string // fleet-scoped machine room (daemon status heartbeats)
}

// PrincipalSocketPath returns the proxy socket path for a principal on
// this machine, using the same derivation as the launcher.
func (m *testMachine) PrincipalSocketPath(localpart string) string {
	return principal.RunDirSocketPath(m.RunDir, localpart)
}

// PrincipalAdminSocketPath returns the admin socket path for a principal
// on this machine. The admin socket is on the host side (not
// bind-mounted into the sandbox) and is used by the daemon and tests
// to register services on the proxy.
func (m *testMachine) PrincipalAdminSocketPath(localpart string) string {
	return principal.RunDirAdminSocketPath(m.RunDir, localpart)
}

// machineOptions configures process binaries and daemon settings for
// startMachine. LauncherBinary, DaemonBinary, and Fleet are required;
// the rest are optional and conditionally passed as CLI flags.
type machineOptions struct {
	LauncherBinary         string     // required
	DaemonBinary           string     // required
	Fleet                  *testFleet // required: fleet-scoped rooms for this machine
	ProxyBinary            string     // defaults to PROXY_BINARY from Bazel runfiles
	ObserveRelayBinary     string     // optional: daemon --observe-relay-binary
	StatusInterval         string     // optional: daemon --status-interval (default "2s")
	HABaseDelay            string     // optional: daemon --ha-base-delay (default "0s" for tests)
	PipelineExecutorBinary string     // optional: daemon --pipeline-executor-binary
	PipelineEnvironment    string     // optional: daemon --pipeline-environment (Nix store path)
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
	Account           principalAccount
	Template          string                      // optional: template ref (e.g., "bureau/template:name")
	Payload           map[string]any              // optional: per-instance payload merged over template defaults
	MatrixPolicy      *schema.MatrixPolicy        // optional per-principal policy
	ServiceVisibility []string                    // optional: glob patterns for service discovery
	Authorization     *schema.AuthorizationPolicy // optional: full authorization policy (overrides shorthand fields)
}

// deploymentConfig describes what principals to deploy on a machine and
// any machine-level authorization policy.
type deploymentConfig struct {
	Principals    []principalSpec
	DefaultPolicy *schema.AuthorizationPolicy // optional machine-level authorization policy
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

	name := fleet.Prefix + "/machine/" + bareName
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

	// In production, bureau-proxy is always co-deployed with the launcher.
	// Resolve it from Bazel runfiles when the caller doesn't override.
	proxyBinary := options.ProxyBinary
	if proxyBinary == "" {
		proxyBinary = resolvedBinary(t, "PROXY_BINARY")
	}

	// Provision the machine via the production CLI command. This registers
	// the account, invites to all global rooms, creates the config room,
	// and writes a bootstrap config with the one-time password.
	bootstrapFile := filepath.Join(machine.StateDir, "bootstrap.json")
	_, bareMachineName, err := ref.ExtractEntityName(machine.Name)
	if err != nil {
		t.Fatalf("extract entity name from %q: %v", machine.Name, err)
	}
	runBureauOrFail(t, "machine", "provision", options.Fleet.Prefix, bareMachineName,
		"--credential-file", credentialFile,
		"--output", bootstrapFile,
	)

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
	daemonArgs := []string{
		"--homeserver", testHomeserverURL,
		"--machine-name", machine.Name,
		"--server-name", testServerName,
		"--fleet", options.Fleet.Prefix,
		"--run-dir", machine.RunDir,
		"--state-dir", machine.StateDir,
		"--workspace-root", machine.WorkspaceRoot,
		"--admin-user", "bureau-admin",
		"--status-interval", statusInterval,
		"--ha-base-delay", haBaseDelay,
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

	daemonArgs := buildDaemonArgs(machine, options)

	// Set up a watch before starting the daemon to detect the first heartbeat.
	statusWatch := watchRoom(t, admin, machine.MachineRoomID)

	cmd := exec.Command(options.DaemonBinary, daemonArgs...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
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
	configAlias := schema.FullRoomAlias(schema.EntityConfigRoomAlias(machine.Name), testServerName)
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
		UserID:    "@" + localpart + ":" + testServerName,
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
		UserID:    "@" + localpart + ":" + testServerName,
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
	if machine.MachineRoomID == "" {
		t.Fatal("machine.MachineRoomID is required")
	}
	if account.Localpart == "" {
		t.Fatal("account.Localpart is required")
	}
	if account.Token == "" {
		t.Fatal("account.Token is required")
	}

	_, err := credential.Provision(t.Context(), admin, credential.ProvisionParams{
		MachineName:   machine.Name,
		Principal:     account.Localpart,
		ServerName:    testServerName,
		MachineRoomID: machine.MachineRoomID,
		Credentials: map[string]string{
			"MATRIX_TOKEN":          account.Token,
			"MATRIX_USER_ID":        account.UserID,
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

	if machine.ConfigRoomID == "" {
		t.Fatal("machine.ConfigRoomID is required")
	}
	if machine.Name == "" {
		t.Fatal("machine.Name is required")
	}

	assignments := make([]schema.PrincipalAssignment, len(config.Principals))
	for i, spec := range config.Principals {
		assignments[i] = schema.PrincipalAssignment{
			Localpart:         spec.Account.Localpart,
			Template:          spec.Template,
			AutoStart:         true,
			Payload:           spec.Payload,
			MatrixPolicy:      spec.MatrixPolicy,
			ServiceVisibility: spec.ServiceVisibility,
			Authorization:     spec.Authorization,
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

// deployPrincipals encrypts credentials for each principal, pushes
// Credentials and MachineConfig state events to the machine's config room,
// and waits for all proxy sockets to appear. Returns a map from localpart
// to proxy socket path. This is a convenience wrapper around pushCredentials
// and pushMachineConfig for the common case of deploying from scratch.
func deployPrincipals(t *testing.T, admin *messaging.DirectSession, machine *testMachine, config deploymentConfig) map[string]string {
	t.Helper()

	if len(config.Principals) == 0 {
		t.Fatal("config.Principals must not be empty")
	}
	if machine.ConfigRoomID == "" {
		t.Fatal("machine.ConfigRoomID is required")
	}
	if machine.MachineRoomID == "" {
		t.Fatal("machine.MachineRoomID is required")
	}

	for _, spec := range config.Principals {
		pushCredentials(t, admin, machine, spec.Account)
	}

	pushMachineConfig(t, admin, machine, config)

	proxySockets := make(map[string]string, len(config.Principals))
	for _, spec := range config.Principals {
		socketPath := machine.PrincipalSocketPath(spec.Account.Localpart)
		waitForFile(t, socketPath)
		proxySockets[spec.Account.Localpart] = socketPath
	}

	return proxySockets
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

	// SkipWaitForReady skips waiting for "agent-ready" after deployment.
	// Set to true for agents expected to exit/fail before posting ready.
	SkipWaitForReady bool
}

// agentDeployment holds the result of deployAgent: account details, socket
// paths, and template reference for test assertions and post-deployment
// interactions (e.g., registering mock LLM services on the admin socket).
type agentDeployment struct {
	Account         principalAccount
	TemplateRef     string
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
		Filesystem: []schema.TemplateMount{
			{Source: binary, Dest: binary, Mode: "ro"},
			{Dest: "/tmp", Type: "tmpfs"},
		},
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
	if machine.MachineRoomID == "" {
		t.Fatal("machine.MachineRoomID is required")
	}
	if machine.ConfigRoomID == "" {
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

	ref, err := schema.ParseTemplateRef("bureau/template:" + templateName)
	if err != nil {
		t.Fatalf("parse template ref for %q: %v", templateName, err)
	}
	_, err = template.Push(ctx, admin, ref,
		agentTemplateContent(options.Binary, options), testServerName)
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

	result, err := principal.Create(ctx, client, admin, registrationTokenBuffer, credential.AsProvisionFunc(), principal.CreateParams{
		MachineName:   machine.Name,
		Localpart:     options.Localpart,
		TemplateRef:   ref.String(),
		ServerName:    testServerName,
		HomeserverURL: testHomeserverURL,
		AutoStart:     true,
		MachineRoomID: machine.MachineRoomID,
		Payload:       options.Payload,
		Authorization: options.Authorization,
	})
	if err != nil {
		t.Fatalf("create agent %q: %v", options.Localpart, err)
	}

	proxySocketPath := machine.PrincipalSocketPath(options.Localpart)
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
		TemplateRef:     ref.String(),
		ProxySocketPath: proxySocketPath,
		AdminSocketPath: machine.PrincipalAdminSocketPath(options.Localpart),
	}
}

// grantTemplateAccess resolves the #bureau/template room and invites the
// machine so the daemon can read templates during config reconciliation.
// Returns the template room ID for tests that need to publish custom
// templates.
func grantTemplateAccess(t *testing.T, admin *messaging.DirectSession, machine *testMachine) string {
	t.Helper()

	if machine.UserID == "" {
		t.Fatal("machine.UserID is required")
	}

	templateRoomID, err := admin.ResolveAlias(t.Context(),
		schema.FullRoomAlias(schema.RoomAliasTemplate, testServerName))
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

	_, err = template.Push(t.Context(), admin, ref,
		agentTemplateContent(testAgentBinary, agentOptions{TemplateName: templateName}),
		testServerName)
	if err != nil {
		t.Fatalf("push test agent template %q: %v", templateName, err)
	}

	return ref.String()
}

// joinConfigRoom invites an agent to the config room and joins via a
// direct session. The agent needs config room membership so the lifecycle
// manager can post messages (agent-ready, text responses) to the room.
func joinConfigRoom(t *testing.T, admin *messaging.DirectSession, configRoomID string, agent principalAccount) {
	t.Helper()

	if configRoomID == "" {
		t.Fatal("configRoomID is required")
	}
	if agent.UserID == "" {
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

// readDaemonMintedToken reads a daemon-minted service token from the
// machine's state directory. The daemon writes tokens to
// <stateDir>/tokens/<localpart>/<role>.token during sandbox provisioning.
// Call this after the proxy socket appears (which proves the daemon
// completed sandbox creation including token minting).
func readDaemonMintedToken(t *testing.T, machine *testMachine, localpart, serviceRole string) []byte {
	t.Helper()

	if localpart == "" {
		t.Fatal("localpart is required")
	}
	if serviceRole == "" {
		t.Fatal("serviceRole is required")
	}

	tokenPath := filepath.Join(machine.StateDir, "tokens", localpart, serviceRole+".token")
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read daemon-minted %s token for %s at %s: %v", serviceRole, localpart, tokenPath, err)
	}
	if len(token) == 0 {
		t.Fatalf("daemon-minted %s token for %s is empty", serviceRole, localpart)
	}
	return token
}
