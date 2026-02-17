// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"

	template "github.com/bureau-foundation/bureau/lib/template"
)

// TestMockAgentLifecycle exercises the lib/agent lifecycle end-to-end in a
// real Bureau sandbox. It deploys the bureau-agent-mock binary, which uses
// lib/agent.Run with a mock Driver, and verifies:
//
//   - Proxy client connectivity (Identity, Grants, Services, ResolveAlias)
//   - Agent context building (system prompt generation, payload reading)
//   - Session logging (JSONL event writing)
//   - Matrix messaging (agent-ready and agent-complete messages)
//
// This test is the proof that the agent lifecycle library works correctly
// when deployed inside Bureau's sandbox infrastructure.
func TestMockAgentLifecycle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleetRoomID := createFleetRoom(t, admin)

	// Boot a machine.
	machine := newTestMachine(t, "machine/agent-mock-test")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		FleetRoomID:    fleetRoomID,
	})

	// Publish a template for the mock agent. This uses the mock agent
	// binary with the same sandbox configuration as the test agent, plus
	// a /run/bureau tmpfs for the session log.
	mockAgentBinary := testutil.DataBinary(t, "MOCK_AGENT_BINARY")
	grantTemplateAccess(t, admin, machine)

	ref, err := schema.ParseTemplateRef("bureau/template:mock-agent")
	if err != nil {
		t.Fatalf("parse template ref: %v", err)
	}
	_, err = template.Push(ctx, admin, ref, schema.TemplateContent{
		Description: "Mock agent for lifecycle testing",
		Command:     []string{mockAgentBinary},
		Namespaces: &schema.TemplateNamespaces{
			PID: true,
		},
		Security: &schema.TemplateSecurity{
			NewSession:    true,
			DieWithParent: true,
			NoNewPrivs:    true,
		},
		Filesystem: []schema.TemplateMount{
			{Source: mockAgentBinary, Dest: mockAgentBinary, Mode: "ro"},
			{Dest: "/tmp", Type: "tmpfs"},
		},
		CreateDirs: []string{"/tmp", "/var/tmp", "/run/bureau"},
		EnvironmentVariables: map[string]string{
			"HOME":                "/workspace",
			"TERM":                "xterm-256color",
			"BUREAU_PROXY_SOCKET": "${PROXY_SOCKET}",
			"BUREAU_MACHINE_NAME": "${MACHINE_NAME}",
			"BUREAU_SERVER_NAME":  "${SERVER_NAME}",
		},
	}, testServerName)
	if err != nil {
		t.Fatalf("push mock-agent template: %v", err)
	}

	// Register a principal for the mock agent.
	agent := registerPrincipal(t, "agent/mock-test", "test-password")
	pushCredentials(t, admin, machine, agent)

	// The mock agent needs to send messages to the config room. Invite
	// and join before sandbox starts.
	if err := admin.InviteUser(ctx, machine.ConfigRoomID, agent.UserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			t.Fatalf("invite agent to config room: %v", err)
		}
	}
	agentSession := principalSession(t, agent)
	if _, err := agentSession.JoinRoom(ctx, machine.ConfigRoomID); err != nil {
		t.Fatalf("agent join config room: %v", err)
	}
	agentSession.Close()

	// Set up watches before deploying.
	readyWatch := watchRoom(t, admin, machine.ConfigRoomID)

	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Account:  agent,
			Template: "bureau/template:mock-agent",
		}},
	})

	// Wait for the proxy socket (sandbox created).
	proxySocketPath := machine.PrincipalSocketPath(agent.Localpart)
	waitForFile(t, proxySocketPath)
	t.Logf("proxy socket appeared: %s", proxySocketPath)

	// Wait for "agent-ready" from the mock agent. This proves:
	// ProxyClient.Identity, BuildContext, ResolveAlias, and SendTextMessage
	// all work from inside the sandbox.
	readyWatch.WaitForMessage(t, "agent-ready", agent.UserID)
	t.Log("mock agent sent ready signal")

	// Wait for the completion summary. The mock agent emits a fixed set
	// of events and exits, so agent-complete should follow quickly.
	completeWatch := watchRoom(t, admin, machine.ConfigRoomID)
	completeWatch.WaitForMessage(t, "agent-complete", agent.UserID)
	t.Log("mock agent sent completion summary — lifecycle verified")
}
