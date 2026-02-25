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
	"github.com/bureau-foundation/bureau/lib/schema/artifact"
	"github.com/bureau-foundation/bureau/lib/testutil"
)

// TestAgentServiceSessionTracking exercises the full agent lifecycle
// in a real Bureau sandbox, including the production service stack:
//
//   - Proxy client connectivity (Identity, Grants, Services, ResolveAlias)
//   - Agent context building (system prompt generation, payload reading)
//   - Session logging (JSONL event writing)
//   - Matrix messaging (agent-ready and agent-complete)
//   - Agent service session lifecycle (start -> run -> end)
//   - Metrics aggregation (token counts, session count)
//   - Event checkpoint persistence (CBOR deltas, commit chain)
//
// The test uses bureau-agent-mock, which calls lib/agentdriver.Run with
// a mock Driver that emits a fixed sequence of events (including metric
// events with token counts) and exits. The mock agent sets
// CheckpointFormat = "events-v1", which triggers CBOR event
// checkpointing at turn boundaries and session end. Checkpoint data
// flows through the agent service (which stores it in the artifact
// service) rather than the agent talking to the artifact service
// directly, keeping the agent's sandbox surface area minimal.
//
// The agent is deployed with RestartPolicy "on-failure": a clean exit
// (code 0) is final. The daemon marks the principal completed and does
// not restart it. This is the correct pattern for bounded-task agents
// (one-shot pipeline executors, mock agents in tests).
//
// The template declares RequiredServices: ["agent"], which triggers the
// full production path: daemon resolves the "agent" service binding,
// finds the agent service socket, mints an authentication token, and
// passes the socket mount + token directory to the launcher. The agent
// service itself declares RequiredServices: ["artifact"] for checkpoint
// data storage and materialization.
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

	// --- Artifact service setup ---

	artifactBinary := testutil.DataBinary(t, "ARTIFACT_SERVICE_BINARY")
	artifactSvc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    artifactBinary,
		Name:      "artifact-service",
		Localpart: "service/artifact/agent-svc-test",
		Command:   []string{artifactBinary, "--store-dir", "/tmp/artifacts"},
	})

	// Publish the artifact service binding so the daemon can resolve
	// the "artifact" role for services that declare RequiredServices: ["artifact"].
	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeServiceBinding, "artifact",
		schema.ServiceBindingContent{Principal: artifactSvc.Entity}); err != nil {
		t.Fatalf("publish artifact service binding: %v", err)
	}

	// --- Agent service setup ---

	agentSvc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:           testutil.DataBinary(t, "AGENT_SERVICE_BINARY"),
		Name:             "agent-service",
		Localpart:        "service/agent/session-test",
		RequiredServices: []string{"artifact"},
		Authorization: &schema.AuthorizationPolicy{
			Grants: []schema.Grant{
				{Actions: []string{artifact.ActionStore, artifact.ActionFetch}},
			},
		},
	})

	// Publish the agent service binding so the daemon maps "agent"
	// role to this service.
	if _, err := admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeServiceBinding, "agent",
		schema.ServiceBindingContent{Principal: agentSvc.Entity}); err != nil {
		t.Fatalf("publish agent service binding: %v", err)
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

	// Bind-mount a host directory into the sandbox so the mock agent
	// can write debug logs readable from the test.
	debugDir := t.TempDir()
	agent := deployAgent(t, admin, machine, agentOptions{
		Binary:           testutil.DataBinary(t, "MOCK_AGENT_BINARY"),
		Localpart:        agentLocalpart,
		RequiredServices: []string{"agent"},
		RestartPolicy:    schema.RestartPolicyOnFailure,
		ExtraMounts: []schema.TemplateMount{
			{Source: debugDir, Dest: "/run/bureau/debug", Mode: schema.MountModeRW},
		},
	})

	// Wait for the agent service to write metrics. EndSession writes
	// the agent_session state event first, then the agent_metrics state
	// event. Once we see agent_metrics, both events are guaranteed to
	// be readable.
	metricsStateKey := agent.Account.UserID.Localpart()
	metricsWatch.WaitForStateEvent(t, agentschema.EventTypeAgentMetrics, metricsStateKey)
	t.Log("agent service wrote metrics state event")

	// Dump the mock agent's debug log.
	debugLogPath := filepath.Join(debugDir, "agent.log")
	if debugLog, err := os.ReadFile(debugLogPath); err != nil {
		t.Logf("no debug log at %s: %v", debugLogPath, err)
	} else {
		t.Logf("mock agent debug log (%d bytes):\n%s", len(debugLog), debugLog)
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
		t.Error("LatestSessionID is empty, want non-empty (session should be recorded)")
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

	// --- Verify context checkpoint chain ---

	// The mock agent's event sequence produces:
	//   - SystemEvent (init) + ResponseEvent (text1) → turn_boundary checkpoint
	//   - ToolCallEvent + ToolResultEvent + ResponseEvent (text2) → turn_boundary checkpoint
	//   - MetricEvent (result) → session_end checkpoint
	//
	// Plus the final safety checkpoint in Run() that catches any
	// events after the last trigger. The minimum is 2 checkpoints
	// (at least one turn boundary + session end).
	//
	// Read all agent_context_commit state events from the config room.
	// Each commit is a state event keyed by its ctx-* identifier.
	allState, err := admin.GetRoomState(ctx, machine.ConfigRoomID)
	if err != nil {
		t.Fatalf("get config room state: %v", err)
	}

	type commitEntry struct {
		stateKey string
		content  agentschema.ContextCommitContent
	}
	var commits []commitEntry
	for _, event := range allState {
		if event.Type != agentschema.EventTypeAgentContextCommit {
			continue
		}
		if event.StateKey == nil {
			continue
		}
		contentJSON, _ := json.Marshal(event.Content)
		var content agentschema.ContextCommitContent
		if err := json.Unmarshal(contentJSON, &content); err != nil {
			t.Fatalf("unmarshal context commit %s: %v", *event.StateKey, err)
		}
		// Filter to commits from this agent's session.
		if content.SessionID != sessionContent.LatestSessionID {
			continue
		}
		commits = append(commits, commitEntry{stateKey: *event.StateKey, content: content})
	}

	if len(commits) < 2 {
		t.Fatalf("expected at least 2 context commits, got %d", len(commits))
	}

	// Verify each commit has the expected format and a non-empty artifact ref.
	for _, commit := range commits {
		if commit.content.Format != "events-v1" {
			t.Errorf("commit %s: format = %q, want %q", commit.stateKey, commit.content.Format, "events-v1")
		}
		if commit.content.ArtifactRef == "" {
			t.Errorf("commit %s: artifact_ref is empty", commit.stateKey)
		}
		if commit.content.CommitType == "" {
			t.Errorf("commit %s: commit_type is empty", commit.stateKey)
		}
		if commit.content.Checkpoint == "" {
			t.Errorf("commit %s: checkpoint trigger is empty", commit.stateKey)
		}
	}

	// Build parent chain map and verify linkage.
	commitByID := make(map[string]agentschema.ContextCommitContent, len(commits))
	for _, commit := range commits {
		commitByID[commit.stateKey] = commit.content
	}

	// Find the root commit (no parent).
	var rootCount int
	for _, commit := range commits {
		if commit.content.Parent == "" {
			rootCount++
		} else {
			if _, exists := commitByID[commit.content.Parent]; !exists {
				t.Errorf("commit %s references parent %s which is not in the chain",
					commit.stateKey, commit.content.Parent)
			}
		}
	}
	if rootCount != 1 {
		t.Errorf("expected exactly 1 root commit (no parent), got %d", rootCount)
	}

	t.Logf("checkpoint chain verified: %d commits, format=events-v1", len(commits))
}
