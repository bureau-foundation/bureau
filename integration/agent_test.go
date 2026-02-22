// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/testutil"
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

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	// Boot a machine.
	machine := newTestMachine(t, fleet, "agent-mock-test")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Deploy the mock agent. deployAgent handles: template publish,
	// principal registration, credential provisioning, config room
	// membership, machine config push, proxy socket wait, and
	// agent-ready synchronization.
	agent := deployAgent(t, admin, machine, agentOptions{
		Binary:    testutil.DataBinary(t, "MOCK_AGENT_BINARY"),
		Localpart: "agent/mock-test",
	})

	// Wait for the completion summary. The mock agent emits a fixed set
	// of events and exits, so agent-complete should follow quickly.
	completeWatch := watchRoom(t, admin, machine.ConfigRoomID)
	completeWatch.WaitForMessage(t, "agent-complete", agent.Account.UserID)
	t.Log("mock agent sent completion summary — lifecycle verified")
}
