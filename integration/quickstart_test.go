// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestQuickstartTestAgent exercises the full sandbox lifecycle for the test
// agent binary:
//
//   - Boot a machine (launcher + daemon)
//   - Publish a sysadmin-test template to the template room
//   - Register a principal, seal credentials, push config
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

	testAgentBinary := resolvedBinary(t, "TEST_AGENT_BINARY")

	// Boot a machine.
	machine := newTestMachine(t, "machine/quickstart-test")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
	})

	// Publish the sysadmin-test template. The template is defined inline
	// with the absolute binary path bind-mounted into the sandbox (same
	// pattern as the workspace integration tests). For the real quickstart
	// CLI, the binary would come from the Nix runner-env PATH.
	templateRoomAlias := "#bureau/template:" + testServerName
	templateRoomID, err := admin.ResolveAlias(ctx, templateRoomAlias)
	if err != nil {
		t.Fatalf("resolve template room: %v", err)
	}

	// Invite the machine to the template room so the daemon can resolve
	// the template during config reconciliation.
	if err := admin.InviteUser(ctx, templateRoomID, machine.UserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			t.Fatalf("invite machine to template room: %v", err)
		}
	}

	_, err = admin.SendStateEvent(ctx, templateRoomID,
		schema.EventTypeTemplate, "sysadmin-test", schema.TemplateContent{
			Description: "Test agent for quickstart integration testing",
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
				// Bind-mount the test agent binary into the sandbox at
				// the same absolute path it occupies on the host.
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
		t.Fatalf("publish sysadmin-test template: %v", err)
	}

	// Register a principal and deploy it with the sysadmin-test template.
	agent := registerPrincipal(t, "sysadmin/quickstart-test", "test-password")
	pushCredentials(t, admin, machine, agent)

	// The test agent sends messages to the config room from inside the
	// sandbox. For that to work, the principal must be a room member.
	// The proxy's default-deny MatrixPolicy blocks JoinRoom, so we handle
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

	templateRef := "bureau/template:sysadmin-test"
	_, err = admin.SendStateEvent(ctx, machine.ConfigRoomID,
		schema.EventTypeMachineConfig, machine.Name, schema.MachineConfig{
			Principals: []schema.PrincipalAssignment{
				{
					Localpart: agent.Localpart,
					Template:  templateRef,
					AutoStart: true,
				},
			},
		})
	if err != nil {
		t.Fatalf("push machine config: %v", err)
	}

	// Wait for the proxy socket to appear (proves sandbox creation worked).
	proxySocketPath := machine.PrincipalSocketPath(agent.Localpart)
	waitForFile(t, proxySocketPath, 30*time.Second)
	t.Logf("proxy socket appeared: %s", proxySocketPath)

	// Wait for "quickstart-test-ready" from the test agent. This proves:
	// identity injection, Matrix authentication, room alias resolution,
	// and message sending all work from inside the sandbox.
	waitForMessageInRoom(t, admin, machine.ConfigRoomID,
		"quickstart-test-ready", agent.UserID, 30*time.Second)
	t.Log("test agent sent ready signal")

	// Send a message to the config room. The test agent will detect it
	// (via polling /messages through the proxy) and respond.
	_, err = admin.SendMessage(ctx, machine.ConfigRoomID,
		messaging.NewTextMessage("hello from test harness"))
	if err != nil {
		t.Fatalf("send message to test agent: %v", err)
	}

	// Wait for the agent's acknowledgment. This proves bidirectional
	// messaging: the agent can read messages from the room and write
	// responses, all through the proxy's Matrix API.
	waitForMessageInRoom(t, admin, machine.ConfigRoomID,
		"quickstart-test-ok: received 'hello from test harness'",
		agent.UserID, 30*time.Second)
	t.Log("test agent acknowledged message — full stack verified")
}
