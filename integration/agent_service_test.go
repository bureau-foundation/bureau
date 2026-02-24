// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
// The agent is deployed with RestartPolicy "on-failure": a clean exit
// (code 0) is final. The daemon marks the principal completed and does
// not restart it. This is the correct pattern for bounded-task agents
// (one-shot pipeline executors, mock agents in tests).
//
// The template declares RequiredServices: ["agent"], which triggers
// the full production path: daemon resolves the "agent" service
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
	//
	// The token file is bind-mounted read-only into the sandbox.
	// BUREAU_ARTIFACT_SOCKET points to a nonexistent sandbox-internal
	// path — acceptable because no artifact operations run in this test.
	artifactTokenFile := filepath.Join(t.TempDir(), "artifact.token")
	if err := os.WriteFile(artifactTokenFile, []byte("test-artifact-token"), 0600); err != nil {
		t.Fatalf("write artifact token file: %v", err)
	}
	const sandboxArtifactToken = "/run/bureau/artifact-token"

	agentSvc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    testutil.DataBinary(t, "AGENT_SERVICE_BINARY"),
		Name:      "agent-service",
		Localpart: "service/agent/session-test",
		ExtraMounts: []schema.TemplateMount{
			{Source: artifactTokenFile, Dest: sandboxArtifactToken, Mode: "ro"},
		},
		ExtraEnvironmentVariables: map[string]string{
			"BUREAU_ARTIFACT_SOCKET": "/tmp/no-artifact.sock",
			"BUREAU_ARTIFACT_TOKEN":  sandboxArtifactToken,
		},
	})

	// Publish service binding so the daemon maps "agent" role to this
	// service. Must be published before deploying any principal with
	// RequiredServices: ["agent"].
	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeServiceBinding, "agent",
		schema.ServiceBindingContent{Principal: agentSvc.Entity}); err != nil {
		t.Fatalf("publish agent service binding in config room: %v", err)
	}

	// --- Deploy mock agent with agent service ---

	// The mock agent exits after one session. RestartPolicy "on-failure"
	// tells the daemon not to restart on clean exit (code 0). The agent
	// runs its mock driver sequence, EndSession writes state events to
	// the config room, the agent posts "agent-complete", and exits. The
	// daemon sees exit code 0 + "on-failure" policy and marks the
	// principal completed without restarting.
	const agentLocalpart = "agent/session-track-e2e"

	// Create the watch BEFORE deploying the agent so we don't miss
	// the metrics event written by EndSession.
	metricsWatch := watchRoom(t, admin, machine.ConfigRoomID)

	agent := deployAgent(t, admin, machine, agentOptions{
		Binary:           testutil.DataBinary(t, "MOCK_AGENT_BINARY"),
		Localpart:        agentLocalpart,
		RequiredServices: []string{"agent"},
		RestartPolicy:    "on-failure",
	})

	// Wait for the agent service to write metrics. EndSession writes
	// the agent_session state event first, then the agent_metrics state
	// event. Once we see agent_metrics, both events are guaranteed to
	// be readable.
	metricsStateKey := agent.Account.UserID.Localpart()
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
