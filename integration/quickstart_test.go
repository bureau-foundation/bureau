// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"testing"

	"github.com/bureau-foundation/bureau/messaging"
)

// TestQuickstartTestAgent exercises the full sandbox lifecycle for the test
// agent binary:
//
//   - Boot a machine (launcher + daemon)
//   - Publish a sysadmin-test template to the template room
//   - Register a principal, provision credentials, push config
//   - Daemon reconciles → launcher creates sandbox → test agent starts
//   - Test agent: verifies identity, whoami, resolves config room, sends
//     "quickstart-test-ready"
//   - Test harness sends a message to the config room
//   - Test agent receives it, sends "quickstart-test-ok: received '...'"
//
// This proves the entire stack works end-to-end: proxy identity injection,
// Matrix authentication through the proxy, room alias resolution, bidirectional
// messaging from inside a bwrap sandbox, and the ${MACHINE_NAME}/${SERVER_NAME}
// template variable expansion.
func TestQuickstartTestAgent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleetRoomID := createFleetRoom(t, admin)

	// Boot a machine.
	machine := newTestMachine(t, "machine/quickstart-test")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		FleetRoomID:    fleetRoomID,
	})

	// Publish the sysadmin-test template.
	templateRef := publishTestAgentTemplate(t, admin, machine, "sysadmin-test")

	// Register a principal and deploy it with the sysadmin-test template.
	agent := registerPrincipal(t, "sysadmin/quickstart-test", "test-password")
	pushCredentials(t, admin, machine, agent)

	// The test agent sends messages to the config room from inside the
	// sandbox. For that to work, the principal must be a room member.
	// The proxy's default-deny grants block JoinRoom, so we handle
	// membership here: admin invites, principal joins via direct session
	// (outside the sandbox) before the sandbox starts.
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

	readyWatch := watchRoom(t, admin, machine.ConfigRoomID)

	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Account:  agent,
			Template: templateRef,
		}},
	})

	// Wait for the proxy socket to appear (proves sandbox creation worked).
	proxySocketPath := machine.PrincipalSocketPath(agent.Localpart)
	waitForFile(t, proxySocketPath)
	t.Logf("proxy socket appeared: %s", proxySocketPath)

	// Wait for "quickstart-test-ready" from the test agent. This proves:
	// identity injection, Matrix authentication, room alias resolution,
	// and message sending all work from inside the sandbox.
	readyWatch.WaitForMessage(t, "quickstart-test-ready", agent.UserID)
	t.Log("test agent sent ready signal")

	// Send a message to the config room. The test agent will detect it
	// (via polling /messages through the proxy) and respond.
	ackWatch := watchRoom(t, admin, machine.ConfigRoomID)

	if _, err := admin.SendMessage(ctx, machine.ConfigRoomID,
		messaging.NewTextMessage("hello from test harness")); err != nil {
		t.Fatalf("send message to test agent: %v", err)
	}

	// Wait for the agent's acknowledgment. This proves bidirectional
	// messaging: the agent can read messages from the room and write
	// responses, all through the proxy's Matrix API.
	ackWatch.WaitForMessage(t,
		"quickstart-test-ok: received 'hello from test harness'",
		agent.UserID)
	t.Log("test agent acknowledged message — full stack verified")
}
