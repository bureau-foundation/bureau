// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"os/exec"
	"testing"
)

// testObserver returns an Observer with unique session names that won't
// collide with production sessions or other tests.
func testObserver(t *testing.T) *Observer {
	t.Helper()
	return &Observer{
		ObserveSession: "bureau-test-observe",
		SessionPrefix:  "test-agent-",
	}
}

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found, skipping")
	}
}

func TestShellJoin(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "simple args",
			args: []string{"echo", "hello"},
			want: "echo hello",
		},
		{
			name: "args with spaces",
			args: []string{"echo", "hello world"},
			want: "echo 'hello world'",
		},
		{
			name: "args with single quotes",
			args: []string{"echo", "it's"},
			want: "echo 'it'\\''s'",
		},
		{
			name: "args with special chars",
			args: []string{"bash", "-c", "echo $HOME && ls"},
			want: "bash -c 'echo $HOME && ls'",
		},
		{
			name: "empty arg",
			args: []string{"cmd", ""},
			want: "cmd ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellJoin(tt.args)
			if got != tt.want {
				t.Errorf("shellJoin(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestAgentSessionName(t *testing.T) {
	obs := New()
	if got := obs.agentSessionName("alice"); got != "agent-alice" {
		t.Errorf("agentSessionName(alice) = %q, want %q", got, "agent-alice")
	}

	obs.SessionPrefix = "custom-"
	if got := obs.agentSessionName("bob"); got != "custom-bob" {
		t.Errorf("agentSessionName(bob) = %q, want %q", got, "custom-bob")
	}
}

func TestLaunchAndStopAgent(t *testing.T) {
	requireTmux(t)

	obs := testObserver(t)
	agent := "launch-test"
	sessionName := obs.agentSessionName(agent)

	// Ensure clean state.
	_ = exec.Command("tmux", "kill-session", "-t", sessionName).Run()
	_ = exec.Command("tmux", "kill-session", "-t", obs.ObserveSession).Run()
	defer func() {
		_ = exec.Command("tmux", "kill-session", "-t", sessionName).Run()
		_ = exec.Command("tmux", "kill-session", "-t", obs.ObserveSession).Run()
	}()

	// Launch an agent that sleeps.
	err := obs.Launch(agent, []string{"sleep", "300"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Verify agent session exists.
	if !SessionExists(sessionName) {
		t.Error("agent session does not exist after launch")
	}

	// Verify observation session exists.
	if !SessionExists(obs.ObserveSession) {
		t.Error("observation session does not exist after launch")
	}

	// Verify agent appears in list.
	agents, err := obs.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}

	found := false
	for _, a := range agents {
		if a.Name == agent {
			found = true
			if !a.Running {
				t.Error("agent should be running")
			}
		}
	}
	if !found {
		t.Errorf("agent %q not found in list: %v", agent, agents)
	}

	// Launching the same agent again should fail.
	err = obs.Launch(agent, []string{"sleep", "300"})
	if err == nil {
		t.Error("expected error when launching duplicate agent")
	}

	// Stop the agent.
	err = obs.Stop(agent)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Verify agent session is gone.
	if SessionExists(sessionName) {
		t.Error("agent session still exists after stop")
	}
}

func TestLaunchValidation(t *testing.T) {
	obs := testObserver(t)

	tests := []struct {
		name    string
		agent   string
		command []string
	}{
		{"empty command", "test-agent", nil},
		{"empty agent name", "", []string{"sleep", "1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := obs.Launch(tt.agent, tt.command)
			if err == nil {
				t.Error("expected error for invalid input")
				_ = obs.Stop(tt.agent)
			}
		})
	}
}

func TestStopNonexistent(t *testing.T) {
	requireTmux(t)

	obs := testObserver(t)
	err := obs.Stop("nonexistent-agent-xyz")
	if err == nil {
		t.Error("expected error when stopping nonexistent agent")
	}
}

func TestListAgentsNoSession(t *testing.T) {
	requireTmux(t)

	obs := testObserver(t)

	// Make sure the test observation session doesn't exist.
	_ = exec.Command("tmux", "kill-session", "-t", obs.ObserveSession).Run()

	agents, err := obs.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if agents != nil {
		t.Errorf("expected nil agents, got %v", agents)
	}
}
