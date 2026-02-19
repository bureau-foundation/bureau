// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/testutil"
)

// TestAgentServiceSessionTracking exercises the full agent service
// integration path: daemon service discovery, sandbox creation with
// agent service socket and token mount, agent auto-detecting the agent
// service socket via RunConfigFromEnvironment, session lifecycle
// (start → run → end), and metrics aggregation verified via Matrix
// state events.
//
// The test uses bureau-agent-mock, which calls lib/agent.Run with a
// mock Driver that emits a fixed sequence of events (including metric
// events with token counts) and exits. The mock driver exits naturally,
// so Run()'s cleanup fires without needing sandbox destruction: it
// calls EndSession (writing state events to the config room via the
// agent service) then posts "agent-complete". No external LLM mock is
// needed because the mock driver handles event generation internally.
//
// The template declares RequiredServices: ["agent"], which triggers
// the full production path: daemon resolves the "agent" room service
// binding, finds the agent service socket, mints an authentication
// token, and passes the socket mount + token directory to the launcher.
// The launcher bind-mounts both into the sandbox. Run() auto-detects
// the socket at /run/bureau/service/agent.sock, reads the token from
// /run/bureau/service/token/agent.token, and uses them to communicate
// with the agent service.
func TestAgentServiceSessionTracking(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	// Boot a machine.
	machine := newTestMachine(t, fleet, "agent-svc-test")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// --- Agent service setup ---

	agentServiceLocalpart := "service/agent/session-test"
	agentServiceAccount := registerPrincipal(t, agentServiceLocalpart, "agent-svc-password")

	agentServiceStateDir := t.TempDir()
	writeServiceSession(t, agentServiceStateDir, agentServiceAccount)

	systemRoomID := resolveSystemRoom(t, admin)
	inviteToRooms(t, admin, agentServiceAccount.UserID, systemRoomID, fleet.ServiceRoomID)

	// Start agent service and wait for daemon discovery.
	serviceWatch := watchRoom(t, admin, machine.ConfigRoomID)

	agentServiceSocketPath := principal.RunDirSocketPath(machine.RunDir, agentServiceLocalpart)
	if err := os.MkdirAll(filepath.Dir(agentServiceSocketPath), 0755); err != nil {
		t.Fatalf("create socket parent directory: %v", err)
	}

	agentServiceBinary := testutil.DataBinary(t, "AGENT_SERVICE_BINARY")
	startProcess(t, "agent-service", agentServiceBinary,
		"--homeserver", testHomeserverURL,
		"--machine-name", machine.Name,
		"--principal-name", agentServiceLocalpart,
		"--server-name", testServerName,
		"--run-dir", machine.RunDir,
		"--state-dir", agentServiceStateDir,
		"--fleet", fleet.Prefix,
	)
	waitForFile(t, agentServiceSocketPath)

	// The service binary registers with a fleet-scoped localpart
	// (fleet.Prefix + "/" + principalName), so the daemon's directory
	// update message uses the fleet-scoped name.
	fleetScopedServiceLocalpart := fleet.Prefix + "/" + agentServiceLocalpart
	waitForNotification[schema.ServiceDirectoryUpdatedMessage](
		t, &serviceWatch, schema.MsgTypeServiceDirectoryUpdated, machine.UserID,
		func(message schema.ServiceDirectoryUpdatedMessage) bool {
			return slices.Contains(message.Added, fleetScopedServiceLocalpart)
		}, "service directory update adding "+fleetScopedServiceLocalpart)

	// Publish room service binding so the daemon maps "agent" role to
	// this service. Must be published before deploying any principal
	// with RequiredServices: ["agent"].
	agentServiceUserID := principal.MatrixUserID(agentServiceLocalpart, testServerName)
	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeRoomService, "agent",
		schema.RoomServiceContent{Principal: agentServiceUserID}); err != nil {
		t.Fatalf("publish agent service binding in config room: %v", err)
	}

	// --- Deploy mock agent with agent service ---

	agent := deployAgent(t, admin, machine, agentOptions{
		Binary:           testutil.DataBinary(t, "MOCK_AGENT_BINARY"),
		Localpart:        "agent/session-track-e2e",
		RequiredServices: []string{"agent"},
	})

	// Wait for the mock agent to complete. The mock driver emits a
	// fixed sequence of events and exits naturally, triggering Run()'s
	// cleanup: EndSession (writes state events to config room via
	// agent service) then "agent-complete" summary posting.
	completeWatch := watchRoom(t, admin, machine.ConfigRoomID)
	completeWatch.WaitForMessage(t, "agent-complete", agent.Account.UserID)
	t.Log("agent posted completion summary")

	// --- Verify agent service state events ---
	//
	// Run() calls EndSession before posting "agent-complete", so by
	// the time we see the summary message, the agent service has
	// already written session and metrics state events to the config
	// room. Reading them is synchronous (no waiting needed).

	// Verify session state: no active session, latest session recorded.
	sessionRaw, err := admin.GetStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeAgentSession, agent.Account.Localpart)
	if err != nil {
		t.Fatalf("get agent session state: %v", err)
	}
	var sessionContent schema.AgentSessionContent
	if err := json.Unmarshal(sessionRaw, &sessionContent); err != nil {
		t.Fatalf("unmarshal agent session: %v", err)
	}

	if sessionContent.ActiveSessionID != "" {
		t.Errorf("ActiveSessionID = %q, want empty (session should be ended)", sessionContent.ActiveSessionID)
	}
	if sessionContent.LatestSessionID == "" {
		t.Error("LatestSessionID is empty, want non-empty (session should be recorded)")
	}

	// Verify metrics: one session counted, non-zero token counts from
	// the mock driver's metric event (input_tokens: 100, output_tokens: 50).
	metricsRaw, err := admin.GetStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeAgentMetrics, agent.Account.Localpart)
	if err != nil {
		t.Fatalf("get agent metrics state: %v", err)
	}
	var metricsContent schema.AgentMetricsContent
	if err := json.Unmarshal(metricsRaw, &metricsContent); err != nil {
		t.Fatalf("unmarshal agent metrics: %v", err)
	}

	if metricsContent.TotalSessionCount != 1 {
		t.Errorf("TotalSessionCount = %d, want 1", metricsContent.TotalSessionCount)
	}
	if metricsContent.LastSessionID != sessionContent.LatestSessionID {
		t.Errorf("LastSessionID = %q, want %q (should match latest session)",
			metricsContent.LastSessionID, sessionContent.LatestSessionID)
	}
	if metricsContent.TotalInputTokens == 0 {
		t.Error("TotalInputTokens = 0, want > 0 (mock driver emits input_tokens: 100)")
	}
	if metricsContent.TotalOutputTokens == 0 {
		t.Error("TotalOutputTokens = 0, want > 0 (mock driver emits output_tokens: 50)")
	}

	t.Logf("session verified: latest=%s, sessions=%d, input_tokens=%d, output_tokens=%d",
		sessionContent.LatestSessionID,
		metricsContent.TotalSessionCount,
		metricsContent.TotalInputTokens,
		metricsContent.TotalOutputTokens,
	)
}
