// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	agentschema "github.com/bureau-foundation/bureau/lib/schema/agent"
	"github.com/bureau-foundation/bureau/lib/schema/artifact"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestClaudeAgentLifecycle verifies that bureau-agent-claude correctly
// wraps a Claude Code subprocess inside a Bureau sandbox and produces
// the expected lifecycle state events.
//
// The test deploys the real bureau-agent-claude binary as an agent,
// with CLAUDE_BINARY pointing to bureau-test-agent-claude (a mock
// that speaks stream-json). This exercises the unique Claude agent
// driver behavior:
//
//   - claudeDriver.Start() writes .claude/settings.local.json (hooks,
//     permissions, sandbox config, plan storage) before spawning the
//     subprocess
//   - claudeDriver.Start() reads CLAUDE_BINARY and passes the correct
//     CLI flags (--output-format stream-json, --print, --verbose,
//     --dangerously-skip-permissions)
//   - claudeDriver.ParseOutput() parses stream-json lines into
//     agentdriver.Event values (system, assistant, tool, result)
//   - agentdriver.Run processes events through the full Bureau
//     pipeline: session lifecycle, metrics aggregation, event
//     checkpointing, and Matrix state publishing
//
// The mock emits a scripted event sequence: system init, assistant
// text, Read tool call/result, assistant text, and final metrics.
// The test waits for the agent_metrics state event (which is written
// last in the EndSession flow) and verifies session state and token
// counts.
//
// This test does NOT verify hook behavior — hooks are Claude Code
// functionality, and the mock doesn't fire them. Hook logic is
// covered by unit tests in cmd/bureau-agent-claude/hooks_test.go.
func TestClaudeAgentLifecycle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "claude-agent")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// --- Artifact service ---

	artifactBinary := testutil.DataBinary(t, "ARTIFACT_SERVICE_BINARY")
	artifactSvc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    artifactBinary,
		Name:      "artifact-service",
		Localpart: "service/artifact/claude-agent",
		Command:   []string{artifactBinary, "--store-dir", "/tmp/artifacts"},
	})

	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeServiceBinding, "artifact",
		schema.ServiceBindingContent{Principal: artifactSvc.Entity}); err != nil {
		t.Fatalf("publish artifact service binding: %v", err)
	}

	// --- Agent service ---

	agentSvc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:           testutil.DataBinary(t, "AGENT_SERVICE_BINARY"),
		Name:             "agent-service",
		Localpart:        "service/agent/claude-agent",
		RequiredServices: []string{"artifact"},
		Authorization: &schema.AuthorizationPolicy{
			Grants: []schema.Grant{
				{Actions: []string{artifact.ActionStore, artifact.ActionFetch}},
			},
		},
	})

	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeServiceBinding, "agent",
		schema.ServiceBindingContent{Principal: agentSvc.Entity}); err != nil {
		t.Fatalf("publish agent service binding: %v", err)
	}

	// --- Deploy Claude agent ---
	//
	// The real bureau-agent-claude binary wraps the mock Claude Code
	// binary. CLAUDE_BINARY tells the driver where to find the mock.
	// The mock binary is bind-mounted into the sandbox at its host
	// path via ExtraMounts.
	//
	// RestartPolicy "on-failure": the mock exits cleanly (code 0),
	// so the daemon does not restart it.

	const agentLocalpart = "agent/claude-lifecycle-e2e"

	metricsWatch := watchRoom(t, admin, machine.ConfigRoomID)

	claudeAgentBinary := testutil.DataBinary(t, "CLAUDE_AGENT_BINARY")
	claudeMockBinary := testutil.DataBinary(t, "CLAUDE_MOCK_BINARY")

	// The bureau CLI binary is mounted into the sandbox for the MCP
	// server subprocess. Claude Code launches "bureau mcp serve" via
	// the mcpServers settings to access Bureau tools.
	bureauBinary, err := findBureauBinary()
	if err != nil {
		t.Fatalf("find bureau binary: %v", err)
	}

	debugDir := t.TempDir()
	agent := deployAgent(t, admin, machine, agentOptions{
		Binary:           claudeAgentBinary,
		Localpart:        agentLocalpart,
		RequiredServices: []string{"agent"},
		RestartPolicy:    schema.RestartPolicyOnFailure,
		ExtraEnv: map[string]string{
			"CLAUDE_BINARY":     claudeMockBinary,
			"BUREAU_MCP_BINARY": bureauBinary,
		},
		ExtraMounts: []schema.TemplateMount{
			// Bind-mount the mock binary into the sandbox so the
			// Claude driver can exec it.
			{Source: claudeMockBinary, Dest: claudeMockBinary, Mode: schema.MountModeRO},
			// Bind-mount the bureau CLI for the MCP server subprocess.
			{Source: bureauBinary, Dest: bureauBinary, Mode: schema.MountModeRO},
			// Bind-mount a debug directory for the mock's diagnostic log.
			{Source: debugDir, Dest: "/run/bureau/debug", Mode: schema.MountModeRW},
		},
	})

	// --- Wait for lifecycle completion ---
	//
	// The agent_metrics state event is written last in the EndSession
	// flow (after agent_session). Once we see it, both events are
	// readable and the full pipeline has completed.

	metricsStateKey := agent.Account.UserID.Localpart()
	metricsWatch.WaitForStateEvent(t, agentschema.EventTypeAgentMetrics, metricsStateKey)
	t.Log("Claude agent service wrote metrics state event")

	// --- Verify debug log and MCP settings ---

	debugLogPath := filepath.Join(debugDir, "claude-mock.log")
	debugLog, readError := os.ReadFile(debugLogPath)
	if readError != nil {
		t.Fatalf("no mock debug log at %s: %v", debugLogPath, readError)
	}
	t.Logf("mock Claude debug log (%d bytes):\n%s", len(debugLog), debugLog)

	debugLogContent := string(debugLog)
	if !strings.Contains(debugLogContent, "MCP_SERVERS_VALID") {
		t.Error("debug log should contain MCP_SERVERS_VALID confirming bureau MCP server configuration")
	}

	// --- Verify session state ---

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
		t.Error("LatestSessionID is empty, want non-empty")
	}
	if sessionContent.LatestSessionArtifactRef == "" {
		t.Error("LatestSessionArtifactRef is empty, want non-empty (session log should be uploaded)")
	}
	if sessionContent.LatestContextCommitID == "" {
		t.Error("LatestContextCommitID is empty, want non-empty (checkpoint chain should exist)")
	}

	// --- Verify metrics ---

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
	// The mock emits input_tokens: 750 and output_tokens: 300.
	if metricsContent.TotalInputTokens == 0 {
		t.Error("TotalInputTokens = 0, want > 0 (mock emits input_tokens: 750)")
	}
	if metricsContent.TotalOutputTokens == 0 {
		t.Error("TotalOutputTokens = 0, want > 0 (mock emits output_tokens: 300)")
	}

	t.Logf("Claude agent lifecycle verified: session=%s, sessions=%d, input_tokens=%d, output_tokens=%d",
		sessionContent.LatestSessionID,
		metricsContent.TotalSessionCount,
		metricsContent.TotalInputTokens,
		metricsContent.TotalOutputTokens,
	)

	// --- Verify settings.local.json was written ---
	//
	// The mock writes diagnostics about settings discovery to its
	// debug log. If SETTINGS_FOUND appears, the driver wrote
	// .claude/settings.local.json before starting the mock. If
	// SETTINGS_VALID appears, the settings contained valid JSON
	// with the expected structure.

	if debugLog, err := os.ReadFile(debugLogPath); err == nil {
		logContent := string(debugLog)
		if !strings.Contains(logContent, "SETTINGS_FOUND") {
			t.Error("mock debug log does not contain SETTINGS_FOUND — driver did not write settings.local.json before starting mock")
		}
		if !strings.Contains(logContent, "SETTINGS_VALID") {
			t.Error("mock debug log does not contain SETTINGS_VALID — settings.local.json is not valid JSON")
		}
	} else {
		t.Log("skipping settings verification: no debug log available")
	}
}

// TestClaudeAgentSettingsInjection verifies that the Claude agent
// driver's settings.local.json is written with the correct structure
// by checking the mock's debug log diagnostics. This is a focused
// test for the settings injection path that runs as a subtest of the
// lifecycle test above, sharing the same infrastructure.
//
// The settings verification in TestClaudeAgentLifecycle is sufficient
// for the integration test — this comment documents why we don't need
// a separate test. The unit tests in hooks_test.go provide exhaustive
// coverage of settings content and structure.

// waitForClaudeMetrics waits for agent_metrics from a specific
// principal. Factored out for use by multiple Claude-related tests
// if needed in the future.
func waitForClaudeMetrics(t *testing.T, watch *roomWatch, stateKey string) {
	t.Helper()
	watch.WaitForEvent(t, func(event messaging.Event) bool {
		if event.Type != agentschema.EventTypeAgentMetrics {
			return false
		}
		if event.StateKey == nil || *event.StateKey != stateKey {
			return false
		}
		return true
	}, "Claude agent metrics for "+stateKey)
}
