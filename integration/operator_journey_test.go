// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli/doctor"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/observation"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/lib/schema/workspace"
	"github.com/bureau-foundation/bureau/lib/templatedef"
	"github.com/bureau-foundation/bureau/messaging"
	"github.com/bureau-foundation/bureau/observe"
)

// TestNewOperatorJourney exercises the complete new-operator experience
// using only CLI commands. Every step that a human operator would perform
// runs through the bureau binary as a subprocess. The test IS the UX
// specification: if any step requires a flag that a real operator wouldn't
// know, or produces output that doesn't make sense, the test should fail.
//
// The test infrastructure (fleet, machine, daemon, launcher, ticket service,
// credentials) is set up via Go library code — this mirrors a production
// Bureau deployment that the operator connects to. The journey steps are
// all CLI:
//
//  1. bureau doctor       → environment health check
//  2. bureau machine list → see available machines
//  3. bureau template list → see available templates
//  4. bureau workspace create → create a workspace
//  5. (wait for workspace active via Matrix watch)
//  6. bureau workspace list → see the new workspace
//  7. bureau list           → see running principals
//  8. bureau workspace destroy → tear down the workspace
//  9. (wait for workspace archived via Matrix watch)
func TestNewOperatorJourney(t *testing.T) {
	t.Parallel()

	// ================================================================
	// Infrastructure setup (production deployment simulation)
	// ================================================================

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)
	machine := newTestMachine(t, fleet, "op-journey")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	runnerEnv := findRunnerEnv(t)

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary:         resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:           resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:            resolvedBinary(t, "PROXY_BINARY"),
		PipelineExecutorBinary: resolvedBinary(t, "PIPELINE_EXECUTOR_BINARY"),
		PipelineEnvironment:    runnerEnv,
		Fleet:                  fleet,
	})

	// Set a DefaultPolicy allowing observation. This is part of the
	// production deployment configuration — the machine operator sets
	// authorization policy when provisioning the machine. Without it,
	// the daemon's authorization index has no allowances for the admin
	// to observe workspace agents, so "bureau list" returns nothing.
	// Push this BEFORE deployTicketService: each subsequent config
	// writer (deployTicketService, workspace create) does a
	// read-modify-write that preserves DefaultPolicy.
	if _, err := admin.SendStateEvent(t.Context(), machine.ConfigRoomID,
		schema.EventTypeMachineConfig, machine.Name, schema.MachineConfig{
			DefaultPolicy: &schema.AuthorizationPolicy{
				Allowances: []schema.Allowance{
					{Actions: []string{observation.ActionObserve}, Actors: []string{"**:**"}},
				},
			},
		}); err != nil {
		t.Fatalf("set default authorization policy: %v", err)
	}

	deployTicketService(t, admin, fleet, machine, "op-journey")

	// Seed git repository for the workspace setup pipeline to clone.
	seedRepoPath := machine.WorkspaceRoot + "/seed.git"
	initTestGitRepo(t, t.Context(), seedRepoPath)

	// Publish an agent template with a long-running command. The base
	// template has no command (it's used as a parent), so workspace
	// agents need their own template.
	agentTemplateRef, err := schema.ParseTemplateRef("bureau/template:test-op-journey-agent")
	if err != nil {
		t.Fatalf("parse agent template ref: %v", err)
	}
	grantTemplateAccess(t, admin, machine)
	_, err = templatedef.Push(t.Context(), admin, agentTemplateRef, schema.TemplateContent{
		Description: "Agent template for operator journey test",
		Command:     []string{"sleep", "infinity"},
		Environment: runnerEnv,
		Namespaces:  &schema.TemplateNamespaces{PID: true},
		Security: &schema.TemplateSecurity{
			NewSession:    true,
			DieWithParent: true,
			NoNewPrivs:    true,
		},
		Filesystem: []schema.TemplateMount{
			{Dest: "/tmp", Type: "tmpfs"},
			{Source: "${WORKSPACE_ROOT}", Dest: "/workspace", Mode: schema.MountModeRO},
		},
		CreateDirs: []string{"/tmp", "/var/tmp", "/run/bureau"},
		EnvironmentVariables: map[string]string{
			"HOME": "/workspace",
			"TERM": "xterm-256color",
		},
	}, testServer)
	if err != nil {
		t.Fatalf("push agent template: %v", err)
	}

	// Pre-register workspace principals and provision credentials.
	// In production, the operator runs "bureau credential provision"
	// for each principal after workspace create. Here we pre-provision
	// so the daemon can create sandboxes immediately when MachineConfig
	// arrives. The localparts are deterministic from the workspace alias.
	workspaceAlias := "opjourney/main"
	principalLocalparts := []string{
		"agent/" + workspaceAlias + "/setup",
		"agent/" + workspaceAlias + "/0",
		"agent/" + workspaceAlias + "/teardown",
	}
	var principalAccounts []principalAccount
	for _, localpart := range principalLocalparts {
		account := registerFleetPrincipal(t, fleet, localpart, "test-password")
		pushCredentials(t, admin, machine, account)
		principalAccounts = append(principalAccounts, account)
	}

	// Invite pipeline principals (setup + teardown) to the pipeline room
	// so the pipeline executor can read pipeline definitions.
	pipelineRoomID := resolvePipelineRoom(t, admin)
	for _, index := range []int{0, 2} { // setup=0, teardown=2
		if err := admin.InviteUser(t.Context(), pipelineRoomID, principalAccounts[index].UserID); err != nil {
			if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
				t.Fatalf("invite %s to pipeline room: %v", principalAccounts[index].Localpart, err)
			}
		}
	}

	// ================================================================
	// Operator environment: session file, credential file, machine.conf
	// ================================================================

	credentialFile := writeTestCredentialFile(t,
		testHomeserverURL, admin.UserID().String(), admin.AccessToken())

	sessionFile := writeOperatorSession(t,
		admin.UserID().String(), admin.AccessToken(), testHomeserverURL)

	machineConf := writeMachineConf(t, testHomeserverURL, testServerName, fleet.Prefix, machine.Name)

	env := []string{
		"BUREAU_MACHINE_CONF=" + machineConf,
		"BUREAU_SESSION_FILE=" + sessionFile,
	}

	// ================================================================
	// Step 1: bureau doctor --json
	// ================================================================
	// The doctor checks operator session, machine.conf, homeserver
	// reachability, systemd services, sockets, and Bureau space.
	// Machine.conf is read via BUREAU_MACHINE_CONF (the test environment
	// override), so that check passes. Systemd checks fail because
	// there's no systemd in the test environment.
	t.Log("step 1: bureau doctor")
	doctorOutput, doctorError := runBureauWithEnv(env, "doctor", "--json")

	var doctorJSON doctor.JSONOutput
	if err := json.Unmarshal([]byte(doctorOutput), &doctorJSON); err != nil {
		t.Fatalf("parse doctor JSON: %v\noutput:\n%s\nerror: %v", err, doctorOutput, doctorError)
	}

	// Verify the checks we expect to pass in the test environment.
	doctorChecks := make(map[string]doctor.Result)
	for _, check := range doctorJSON.Checks {
		doctorChecks[check.Name] = check
	}

	if check, ok := doctorChecks["operator session"]; ok {
		if check.Status != doctor.StatusPass {
			t.Errorf("doctor: operator session status = %q, want %q (message: %s)",
				check.Status, doctor.StatusPass, check.Message)
		}
	} else {
		t.Error("doctor: operator session check not found")
	}

	if check, ok := doctorChecks["homeserver reachable"]; ok {
		if check.Status != doctor.StatusPass {
			t.Errorf("doctor: homeserver reachable status = %q, want %q (message: %s)",
				check.Status, doctor.StatusPass, check.Message)
		}
	} else {
		t.Error("doctor: homeserver reachable check not found")
	}

	if check, ok := doctorChecks["machine configuration"]; ok {
		if check.Status != doctor.StatusPass {
			t.Errorf("doctor: machine configuration status = %q, want %q (message: %s)",
				check.Status, doctor.StatusPass, check.Message)
		}
	} else {
		t.Error("doctor: machine configuration check not found")
	}

	if check, ok := doctorChecks["bureau space"]; ok {
		if check.Status != doctor.StatusPass {
			t.Errorf("doctor: bureau space status = %q, want %q (message: %s)",
				check.Status, doctor.StatusPass, check.Message)
		}
	} else {
		t.Error("doctor: bureau space check not found")
	}

	// ================================================================
	// Step 2: bureau machine list --json
	// ================================================================
	t.Log("step 2: bureau machine list")
	machineListOutput := runBureauWithEnvOrFail(t, env,
		"machine", "list", "--credential-file", credentialFile, "--json")

	var machineEntries []struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal([]byte(machineListOutput), &machineEntries); err != nil {
		t.Fatalf("parse machine list JSON: %v\noutput:\n%s", err, machineListOutput)
	}

	var foundMachine bool
	for _, entry := range machineEntries {
		if entry.Name == machine.Name {
			foundMachine = true
			if entry.PublicKey == "" {
				t.Error("machine list: machine has empty public key")
			}
			break
		}
	}
	if !foundMachine {
		t.Errorf("machine list: %q not found in %d entries", machine.Name, len(machineEntries))
	}

	// ================================================================
	// Step 3: bureau template list --json
	// ================================================================
	t.Log("step 3: bureau template list")
	templateListOutput := runBureauWithEnvOrFail(t, env,
		"template", "list", "bureau/template", "--json")

	var templates []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(templateListOutput), &templates); err != nil {
		t.Fatalf("parse template list JSON: %v\noutput:\n%s", err, templateListOutput)
	}

	// Verify at least base + our custom template exist.
	templateNames := make(map[string]bool)
	for _, template := range templates {
		templateNames[template.Name] = true
	}
	if !templateNames["base"] {
		t.Error("template list: 'base' template not found")
	}
	if !templateNames["test-op-journey-agent"] {
		t.Error("template list: 'test-op-journey-agent' template not found")
	}

	// ================================================================
	// Step 4: bureau workspace create --json
	// ================================================================
	t.Log("step 4: bureau workspace create")
	machineLocalpart := machine.Ref.Localpart()
	createOutput := runBureauWithEnvOrFail(t, env,
		"workspace", "create", workspaceAlias,
		"--machine", machineLocalpart,
		"--template", "bureau/template:test-op-journey-agent",
		"--param", "repository=/workspace/seed.git",
		"--credential-file", credentialFile,
		"--json")

	var createResult struct {
		Alias      string   `json:"alias"`
		RoomAlias  string   `json:"room_alias"`
		RoomID     string   `json:"room_id"`
		Project    string   `json:"project"`
		Machine    string   `json:"machine"`
		Principals []string `json:"principals"`
	}
	if err := json.Unmarshal([]byte(createOutput), &createResult); err != nil {
		t.Fatalf("parse workspace create JSON: %v\noutput:\n%s", err, createOutput)
	}

	if createResult.Alias != workspaceAlias {
		t.Errorf("workspace create: alias = %q, want %q", createResult.Alias, workspaceAlias)
	}
	if createResult.Project != "opjourney" {
		t.Errorf("workspace create: project = %q, want %q", createResult.Project, "opjourney")
	}
	if len(createResult.Principals) != 3 {
		t.Errorf("workspace create: got %d principals, want 3", len(createResult.Principals))
	}

	workspaceRoomID, err := ref.ParseRoomID(createResult.RoomID)
	if err != nil {
		t.Fatalf("parse workspace room ID %q: %v", createResult.RoomID, err)
	}

	// ================================================================
	// Step 5: Wait for workspace to become active
	// ================================================================
	// The daemon picks up the MachineConfig, resolves the setup template,
	// applies the pipeline executor overlay, and spawns a sandbox. The
	// dev-workspace-init pipeline clones the seed repo and publishes
	// workspace status "active". This is async infrastructure, not an
	// operator CLI step.
	t.Log("step 5: waiting for workspace active")
	waitForWorkspaceStatus(t, admin, workspaceRoomID, workspace.WorkspaceStatusActive)
	t.Log("workspace status is 'active' — setup pipeline completed")

	verifyPipelineResult(t, admin, workspaceRoomID, "dev-workspace-init", pipeline.ConclusionSuccess)

	// ================================================================
	// Step 6: bureau workspace list --json
	// ================================================================
	t.Log("step 6: bureau workspace list")
	workspaceListOutput := runBureauWithEnvOrFail(t, env, "workspace", "list", "--json")

	var workspaces []struct {
		Alias  string `json:"alias"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(workspaceListOutput), &workspaces); err != nil {
		t.Fatalf("parse workspace list JSON: %v\noutput:\n%s", err, workspaceListOutput)
	}

	var foundWorkspace bool
	for _, ws := range workspaces {
		if ws.Alias == workspaceAlias {
			foundWorkspace = true
			if ws.Status != string(workspace.WorkspaceStatusActive) {
				t.Errorf("workspace list: status = %q, want %q", ws.Status, workspace.WorkspaceStatusActive)
			}
			break
		}
	}
	if !foundWorkspace {
		t.Errorf("workspace list: %q not found in %d workspaces", workspaceAlias, len(workspaces))
	}

	// ================================================================
	// Step 7: bureau list --json (observe list)
	// ================================================================
	// The agent principal should be running (workspace is active, agent
	// is gated on "active"). The observe socket is at a test-specific
	// path — on a real machine it defaults to /run/bureau/observe.sock.
	t.Log("step 7: bureau list (observe)")
	agentSocket := machine.PrincipalProxySocketPath(t, principalAccounts[1].Localpart)
	waitForFile(t, agentSocket)

	observeListOutput := runBureauWithEnvOrFail(t, env,
		"list", "--json", "--socket", machine.ObserveSocket)

	var listResponse observe.ListResponse
	if err := json.Unmarshal([]byte(observeListOutput), &listResponse); err != nil {
		t.Fatalf("parse observe list JSON: %v\noutput:\n%s", err, observeListOutput)
	}

	var foundAgent bool
	agentLocalpart := "agent/" + workspaceAlias + "/0"
	for _, entry := range listResponse.Principals {
		if entry.Localpart == agentLocalpart {
			foundAgent = true
			break
		}
	}
	if !foundAgent {
		localparts := make([]string, len(listResponse.Principals))
		for index, entry := range listResponse.Principals {
			localparts[index] = entry.Localpart
		}
		t.Errorf("observe list: agent %q not found in %v", agentLocalpart, localparts)
	}

	// ================================================================
	// Step 8: bureau workspace destroy
	// ================================================================
	t.Log("step 8: bureau workspace destroy")
	runBureauWithEnvOrFail(t, env,
		"workspace", "destroy", workspaceAlias,
		"--credential-file", credentialFile)

	// Agent stops (condition "active" no longer matches "teardown").
	waitForFileGone(t, agentSocket)
	t.Log("agent proxy socket disappeared after workspace entered teardown")

	// ================================================================
	// Step 9: Wait for workspace archived
	// ================================================================
	t.Log("step 9: waiting for workspace archived")
	waitForWorkspaceStatus(t, admin, workspaceRoomID, workspace.WorkspaceStatusArchived)
	t.Log("workspace status is 'archived' — teardown pipeline completed")

	verifyPipelineResult(t, admin, workspaceRoomID, "dev-workspace-deinit", pipeline.ConclusionSuccess)

	// ================================================================
	// Final: bureau workspace list shows archived status
	// ================================================================
	t.Log("final: verify workspace list shows archived status")
	finalListOutput := runBureauWithEnvOrFail(t, env, "workspace", "list", "--json")

	var finalWorkspaces []struct {
		Alias  string `json:"alias"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(finalListOutput), &finalWorkspaces); err != nil {
		t.Fatalf("parse final workspace list JSON: %v\noutput:\n%s", err, finalListOutput)
	}

	for _, ws := range finalWorkspaces {
		if ws.Alias == workspaceAlias {
			if ws.Status != string(workspace.WorkspaceStatusArchived) {
				t.Errorf("final workspace list: status = %q, want %q", ws.Status, workspace.WorkspaceStatusArchived)
			}
			break
		}
	}

	t.Log("operator journey complete: doctor → machines → templates → create → list → observe → destroy → archived")
}
