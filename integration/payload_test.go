// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestPayloadDeliveryAndHotReload exercises the full payload lifecycle:
//
//   - Deploy a principal with an initial payload via a template
//   - Verify the agent reads /run/bureau/payload.json at startup
//   - Update the MachineConfig with a new payload (same template)
//   - Daemon detects the payload-only change, hot-reloads via IPC
//   - Agent re-reads the file and reports the updated content
//
// This proves the daemon's reconcileRunningPrincipal → payloadChanged →
// update-payload IPC → launcher's handleUpdatePayload in-place write →
// bind-mounted file visible to sandbox flow works end-to-end.
func TestPayloadDeliveryAndHotReload(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	testAgentBinary := resolvedBinary(t, "TEST_AGENT_BINARY")

	// Boot a machine.
	machine := newTestMachine(t, "machine/payload-test")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Publish a test template. Same structure as the quickstart test:
	// PID namespace, security flags, bind-mounted test agent binary,
	// standard environment variable expansion.
	templateRoomAlias := "#bureau/template:" + testServerName
	templateRoomID, err := admin.ResolveAlias(ctx, templateRoomAlias)
	if err != nil {
		t.Fatalf("resolve template room: %v", err)
	}

	if err := admin.InviteUser(ctx, templateRoomID, machine.UserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			t.Fatalf("invite machine to template room: %v", err)
		}
	}

	_, err = admin.SendStateEvent(ctx, templateRoomID,
		schema.EventTypeTemplate, "payload-test-agent", schema.TemplateContent{
			Description: "Test agent for payload delivery and hot-reload",
			Command:     []string{testAgentBinary},
			Namespaces: &schema.TemplateNamespaces{
				PID: true,
			},
			Security: &schema.TemplateSecurity{
				NewSession:    true,
				DieWithParent: true,
				NoNewPrivs:    true,
			},
			Filesystem: []schema.TemplateMount{
				{Source: testAgentBinary, Dest: testAgentBinary, Mode: "ro"},
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
		})
	if err != nil {
		t.Fatalf("publish payload-test-agent template: %v", err)
	}

	// Register the principal and push credentials.
	agent := registerPrincipal(t, "agent/payload-test", "payload-test-password")
	pushCredentials(t, admin, machine, agent)

	// The test agent sends messages to the config room from inside the
	// sandbox. The proxy's default-deny MatrixPolicy blocks JoinRoom, so
	// handle membership before the sandbox starts: admin invites, principal
	// joins via direct session.
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

	// --- Phase 1: Deploy with initial payload ---

	templateRef := "bureau/template:payload-test-agent"
	initialPayload := map[string]any{
		"version": float64(1),
		"task":    "initial",
		"model":   "test-model",
	}

	readyWatch := watchRoom(t, admin, machine.ConfigRoomID)

	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Account:  agent,
			Template: templateRef,
			Payload:  initialPayload,
		}},
	})

	// Wait for proxy socket (proves sandbox creation).
	proxySocketPath := machine.PrincipalSocketPath(agent.Localpart)
	waitForFile(t, proxySocketPath, 30*time.Second)
	t.Logf("proxy socket appeared: %s", proxySocketPath)

	// Wait for the agent's ready message containing the initial payload.
	readyMessage := readyWatch.WaitForMessage(t, "quickstart-test-ready", agent.UserID)
	t.Logf("agent ready message: %s", readyMessage)

	if !strings.Contains(readyMessage, `"version":1`) {
		t.Errorf("ready message missing version 1: %s", readyMessage)
	}
	if !strings.Contains(readyMessage, `"task":"initial"`) {
		t.Errorf("ready message missing task initial: %s", readyMessage)
	}
	if !strings.Contains(readyMessage, `"model":"test-model"`) {
		t.Errorf("ready message missing model: %s", readyMessage)
	}
	t.Log("phase 1 passed: initial payload delivered and reported by agent")

	// --- Phase 2: Hot-reload with updated payload ---

	updatedPayload := map[string]any{
		"version": float64(2),
		"task":    "updated",
		"model":   "test-model",
	}

	reloadWatch := watchRoom(t, admin, machine.ConfigRoomID)

	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Account:  agent,
			Template: templateRef,
			Payload:  updatedPayload,
		}},
	})

	// Wait for the daemon's payload hot-reload notification in the config
	// room. This is the synchronization point: the daemon sends this
	// message after the IPC response confirms the file has been rewritten.
	// The admin session watches the config room (not the agent).
	reloadWatch.WaitForMessage(t, "Payload updated for agent/payload-test",
		machine.UserID)
	t.Log("daemon confirmed payload hot-reload")

	// Send a message to the agent. The agent re-reads payload.json
	// before sending the ack, so the ack should contain the updated payload.
	ackWatch := watchRoom(t, admin, machine.ConfigRoomID)

	_, err = admin.SendMessage(ctx, machine.ConfigRoomID,
		messaging.NewTextMessage("verify-payload-update"))
	if err != nil {
		t.Fatalf("send trigger message to agent: %v", err)
	}

	// Wait for the agent's acknowledgment with the updated payload.
	ackMessage := ackWatch.WaitForMessage(t, "quickstart-test-ok", agent.UserID)
	t.Logf("agent ack message: %s", ackMessage)

	if !strings.Contains(ackMessage, `"version":2`) {
		t.Errorf("ack message missing version 2: %s", ackMessage)
	}
	if !strings.Contains(ackMessage, `"task":"updated"`) {
		t.Errorf("ack message missing task updated: %s", ackMessage)
	}
	// Negative assertion: the ack must NOT contain the old payload values.
	// This catches a bug where the agent cached the initial payload and
	// appended both old and new content.
	if strings.Contains(ackMessage, `"task":"initial"`) {
		t.Errorf("ack message still contains old payload task=initial (stale cache?): %s", ackMessage)
	}

	t.Log("phase 2 passed: payload hot-reloaded and agent sees updated content")
	t.Log("payload delivery and hot-reload verified end-to-end")
}
