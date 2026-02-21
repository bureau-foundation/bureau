// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"os"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/template"
)

// TestSandboxExitOutputCapture verifies that when a sandbox process exits
// with a non-zero exit code, the daemon captures the terminal output from
// the tmux pane and includes it in the config room notification. This is
// the end-to-end test for the remain-on-exit + capture-pane error capture
// mechanism.
//
// The test deploys a principal whose command prints an error to stderr and
// exits non-zero. The launcher's session watcher captures the pane output
// before destroying the tmux session, sends it through IPC, and the daemon
// posts it to the config room.
func TestSandboxExitOutputCapture(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "output-capture")
	if err := os.MkdirAll(machine.WorkspaceRoot, 0755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}

	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Publish a template with a command that prints an error and exits
	// non-zero. Uses sh -c so the command passes binary validation (sh
	// is a shell interpreter, always available). The error message is
	// distinctive so we can verify it appears in the captured output.
	grantTemplateAccess(t, admin, machine)

	failingRef, err := schema.ParseTemplateRef("bureau/template:failing-agent")
	if err != nil {
		t.Fatalf("parse template ref: %v", err)
	}
	_, err = template.Push(t.Context(), admin, failingRef, schema.TemplateContent{
		Description: "Agent that fails immediately for output capture test",
		Command:     []string{"/bin/sh", "-c", "echo 'FATAL: bureau-output-capture-test-error' >&2; exit 42"},
		Namespaces:  &schema.TemplateNamespaces{PID: true},
		Security: &schema.TemplateSecurity{
			NewSession:    true,
			DieWithParent: true,
			NoNewPrivs:    true,
		},
		Filesystem: []schema.TemplateMount{
			{Source: "/bin", Dest: "/bin", Mode: "ro"},
			{Source: "/usr", Dest: "/usr", Mode: "ro"},
			{Source: "/lib", Dest: "/lib", Mode: "ro"},
			{Source: "/lib64", Dest: "/lib64", Mode: "ro"},
			{Dest: "/tmp", Type: "tmpfs"},
		},
		CreateDirs: []string{"/tmp", "/run/bureau"},
		EnvironmentVariables: map[string]string{
			"HOME": "/tmp",
			"TERM": "xterm-256color",
		},
	}, testServer)
	if err != nil {
		t.Fatalf("push failing template: %v", err)
	}

	// Register a principal and push credentials.
	agent := registerFleetPrincipal(t, fleet, "agent/will-fail", "password")
	pushCredentials(t, admin, machine, agent)

	// Watch the config room BEFORE pushing the machine config so we
	// catch the sandbox exit notification via /sync long-polling.
	exitWatch := watchRoom(t, admin, machine.ConfigRoomID)

	// Push the machine config. The daemon will reconcile, create the
	// sandbox, and the command will print an error and exit 42.
	pushMachineConfig(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Account:  agent,
			Template: "bureau/template:failing-agent",
		}},
	})

	// Wait for the exit notification. The daemon posts this to the
	// config room when the sandbox exits. The typed message includes
	// exit code and captured terminal output as structured fields.
	exitMsg := waitForNotification[schema.SandboxExitedMessage](
		t, &exitWatch, schema.MsgTypeSandboxExited, machine.UserID.String(),
		nil, "sandbox exit notification")

	// The exit code should be 42 (our explicit exit code).
	if exitMsg.ExitCode != 42 {
		t.Errorf("expected exit code 42, got %d", exitMsg.ExitCode)
	}

	// The captured output should contain our distinctive error message.
	if !strings.Contains(exitMsg.CapturedOutput, "bureau-output-capture-test-error") {
		t.Errorf("captured output missing error message, got: %s", exitMsg.CapturedOutput)
	}
}
