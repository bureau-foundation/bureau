// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	agentschema "github.com/bureau-foundation/bureau/lib/schema/agent"
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

	// The agent service requires artifact access for context
	// materialization. Provide a valid token file and socket path so
	// the artifact client initializes. The client reads the token at
	// startup but only connects to the socket on demand — this test
	// exercises session tracking, not materialization, so no artifact
	// requests are made.
	artifactTokenFile := filepath.Join(t.TempDir(), "artifact.token")
	if err := os.WriteFile(artifactTokenFile, []byte("test-artifact-token"), 0600); err != nil {
		t.Fatalf("write artifact token file: %v", err)
	}
	artifactSocketPath := filepath.Join(tempSocketDir(t), "artifact.sock")

	agentSvc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    testutil.DataBinary(t, "AGENT_SERVICE_BINARY"),
		Name:      "agent-service",
		Localpart: "service/agent/session-test",
		ExtraEnv: []string{
			"BUREAU_ARTIFACT_SOCKET=" + artifactSocketPath,
			"BUREAU_ARTIFACT_TOKEN=" + artifactTokenFile,
		},
	})

	// Publish room service binding so the daemon maps "agent" role to
	// this service. Must be published before deploying any principal
	// with RequiredServices: ["agent"].
	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeRoomService, "agent",
		schema.RoomServiceContent{Principal: agentSvc.Entity}); err != nil {
		t.Fatalf("publish agent service binding in config room: %v", err)
	}

	// --- Deploy mock agent with agent service ---

	// The mock agent exits after one session. Without a StartCondition,
	// the daemon would restart it on every exit (correct production
	// behavior for long-running agents). Gate on total_session_count == 0
	// so the daemon only starts the agent when no session has been
	// recorded. EndSession writes total_session_count = 1, which makes
	// the condition false — the daemon evaluates the condition on the
	// next reconcile (triggered by watchSandboxExit) and does not restart.
	//
	// Pre-seed the agent_metrics state event with total_session_count = 0
	// so the condition is satisfied on the first reconcile (StartCondition
	// returns false for non-existent events).
	const agentLocalpart = "agent/session-track-e2e"
	agentEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, agentLocalpart)
	if err != nil {
		t.Fatalf("construct agent entity ref: %v", err)
	}
	metricsStateKey := agentEntity.UserID().Localpart()
	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		agentschema.EventTypeAgentMetrics, metricsStateKey,
		agentschema.AgentMetricsContent{Version: agentschema.AgentMetricsVersion}); err != nil {
		t.Fatalf("pre-seed agent metrics: %v", err)
	}

	// Create the watch BEFORE deploying the agent so we don't miss the
	// metrics event. The watch checkpoint is after the pre-seeded metrics
	// event (total_session_count=0), so only the EndSession-written
	// metrics event (total_session_count=1) will match.
	metricsWatch := watchRoom(t, admin, machine.ConfigRoomID)

	agent := deployAgent(t, admin, machine, agentOptions{
		Binary:           testutil.DataBinary(t, "MOCK_AGENT_BINARY"),
		Localpart:        agentLocalpart,
		RequiredServices: []string{"agent"},
		StartCondition: &schema.StartCondition{
			EventType:    agentschema.EventTypeAgentMetrics,
			StateKey:     metricsStateKey,
			ContentMatch: schema.ContentMatch{"total_session_count": schema.Eq(float64(0))},
		},
	})

	// Wait for the agent service to write metrics. EndSession writes
	// the agent_session state event first, then the agent_metrics state
	// event. Once we see agent_metrics, both events are guaranteed to
	// be readable.
	//
	// We wait on the state event rather than on "agent-complete" because
	// the daemon's reconcile loop correctly evaluates the StartCondition
	// after EndSession writes total_session_count=1 — the condition
	// becomes false, and the daemon stops the principal before the agent
	// can post "agent-complete". This is correct production behavior:
	// StartConditions are liveness conditions (used for workspace
	// lifecycle orchestration), not just launch gates.
	metricsWatch.WaitForStateEvent(t, agentschema.EventTypeAgentMetrics, metricsStateKey)
	t.Log("agent service wrote metrics state event")

	// Verify session state: no active session, latest session recorded.
	sessionRaw, err := admin.GetStateEvent(ctx, machine.ConfigRoomID,
		agentschema.EventTypeAgentSession, agent.Account.UserID.Localpart())
	if err != nil {
		t.Fatalf("get agent session state: %v", err)
	}
	var sessionContent agentschema.AgentSessionContent
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
		agentschema.EventTypeAgentMetrics, agent.Account.UserID.Localpart())
	if err != nil {
		t.Fatalf("get agent metrics state: %v", err)
	}
	var metricsContent agentschema.AgentMetricsContent
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
