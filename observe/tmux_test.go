// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// testObserverWithLogging returns an Observer configured to log pane output
// to a temporary directory that is cleaned up when the test ends.
func testObserverWithLogging(t *testing.T) *Observer {
	t.Helper()
	obs := testObserver(t)
	obs.LogDir = t.TempDir()
	return obs
}

// cleanupSessions kills the agent and observation sessions created during a test.
func cleanupSessions(t *testing.T, obs *Observer, agents ...string) {
	t.Helper()
	for _, agent := range agents {
		_ = exec.Command("tmux", "kill-session", "-t", obs.agentSessionName(agent)).Run()
	}
	_ = exec.Command("tmux", "kill-session", "-t", obs.ObserveSession).Run()
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

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "'simple'"},
		{"path/to/file", "'path/to/file'"},
		{"path with spaces", "'path with spaces'"},
		{"it's", "'it'\\''s'"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := shellQuote(tt.input)
			if got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
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

func TestLogPath(t *testing.T) {
	obs := &Observer{LogDir: "/var/log/bureau"}
	got := obs.logPath("alice", "terminal")
	want := "/var/log/bureau/alice/terminal.log"
	if got != want {
		t.Errorf("logPath = %q, want %q", got, want)
	}

	obs.LogDir = ""
	if got := obs.logPath("alice", "terminal"); got != "" {
		t.Errorf("logPath with empty LogDir = %q, want empty", got)
	}
}

func TestBuildPaneCommand(t *testing.T) {
	remainPrefix := "tmux set-option remain-on-exit on 2>/dev/null;"

	t.Run("without logging", func(t *testing.T) {
		obs := &Observer{}
		got := obs.buildPaneCommand([]string{"sleep", "300"}, "alice", "terminal", 200)
		want := remainPrefix + " exec sleep 300"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("with logging", func(t *testing.T) {
		obs := &Observer{LogDir: "/var/log/bureau"}
		got := obs.buildPaneCommand([]string{"sleep", "300"}, "alice", "terminal", 200)
		want := remainPrefix + " tail -n 200 '/var/log/bureau/alice/terminal.log' 2>/dev/null; exec sleep 300"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("with logging and spaces in path", func(t *testing.T) {
		obs := &Observer{LogDir: "/var/log/my bureau"}
		got := obs.buildPaneCommand([]string{"bash"}, "alice", "agent", 100)
		want := remainPrefix + " tail -n 100 '/var/log/my bureau/alice/agent.log' 2>/dev/null; exec bash"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestLaunchAndStopAgent(t *testing.T) {
	requireTmux(t)

	obs := testObserver(t)
	agent := "launch-test"

	cleanupSessions(t, obs, agent)
	defer cleanupSessions(t, obs, agent)

	// Launch an agent that sleeps.
	err := obs.Launch(agent, []string{"sleep", "300"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Verify agent session exists.
	sessionName := obs.agentSessionName(agent)
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

func TestLaunchReplacesDeadSession(t *testing.T) {
	requireTmux(t)

	obs := testObserver(t)
	agent := "dead-session-test"

	cleanupSessions(t, obs, agent)
	defer cleanupSessions(t, obs, agent)

	// Launch an agent that exits immediately.
	err := obs.Launch(agent, []string{"true"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Wait for the process to exit. The session stays due to remain-on-exit.
	time.Sleep(500 * time.Millisecond)
	sessionName := obs.agentSessionName(agent)
	if !SessionExists(sessionName) {
		t.Fatal("session should still exist (remain-on-exit)")
	}
	if sessionHasLivePanes(sessionName) {
		t.Fatal("pane should be dead after process exits")
	}

	// Relaunching should succeed — the dead session is cleaned up automatically.
	err = obs.Launch(agent, []string{"sleep", "300"})
	if err != nil {
		t.Fatalf("relaunch over dead session should succeed: %v", err)
	}

	if !sessionHasLivePanes(obs.agentSessionName(agent)) {
		t.Error("new session should have live panes")
	}
}

func TestLaunchBlocksOnRunningAgent(t *testing.T) {
	requireTmux(t)

	obs := testObserver(t)
	agent := "block-test"

	cleanupSessions(t, obs, agent)
	defer cleanupSessions(t, obs, agent)

	// Launch a long-running agent.
	err := obs.Launch(agent, []string{"sleep", "300"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Relaunching without Replace should fail.
	err = obs.Launch(agent, []string{"sleep", "300"})
	if err == nil {
		t.Fatal("expected error when launching over a running agent")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error should mention 'already running', got: %v", err)
	}
}

func TestLaunchReplaceRunningAgent(t *testing.T) {
	requireTmux(t)

	obs := testObserver(t)
	obs.Replace = true
	agent := "replace-test"

	cleanupSessions(t, obs, agent)
	defer cleanupSessions(t, obs, agent)

	// Launch a long-running agent.
	err := obs.Launch(agent, []string{"sleep", "300"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Relaunching with Replace should succeed.
	err = obs.Launch(agent, []string{"sleep", "300"})
	if err != nil {
		t.Fatalf("relaunch with Replace should succeed: %v", err)
	}

	if !SessionExists(obs.agentSessionName(agent)) {
		t.Error("session should exist after replace")
	}
}

func TestLaunchWithLayoutMultiPane(t *testing.T) {
	requireTmux(t)

	obs := testObserver(t)
	agent := "multipane-test"

	cleanupSessions(t, obs, agent)
	defer cleanupSessions(t, obs, agent)

	layout := LaunchConfig{
		Layout: "main-vertical",
		Panes: []PaneConfig{
			{Name: "agent"},
			{Name: "monitor", Command: []string{"sleep", "300"}},
			{Name: "status", Command: []string{"sleep", "300"}},
		},
	}

	err := obs.LaunchWithLayout(agent, []string{"sleep", "300"}, layout)
	if err != nil {
		t.Fatalf("LaunchWithLayout: %v", err)
	}

	sessionName := obs.agentSessionName(agent)

	// Verify all three panes exist by counting them.
	cmd := exec.Command("tmux", "list-panes", "-t", sessionName+":"+agent, "-F", "#{pane_index}")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}

	paneLines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(paneLines) != 3 {
		t.Errorf("expected 3 panes, got %d: %v", len(paneLines), paneLines)
	}

	// Verify it appears in the observation session.
	agents, err := obs.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	found := false
	for _, a := range agents {
		if a.Name == agent {
			found = true
		}
	}
	if !found {
		t.Errorf("agent %q not found in observation session", agent)
	}
}

func TestLaunchWithLogging(t *testing.T) {
	requireTmux(t)

	obs := testObserverWithLogging(t)
	agent := "logging-test"

	cleanupSessions(t, obs, agent)
	defer cleanupSessions(t, obs, agent)

	// Launch an agent that prints something after a brief delay. The delay
	// ensures pipe-pane is set up before output starts, since pipe-pane is
	// configured one tmux round-trip after the pane is created.
	err := obs.Launch(agent, []string{"bash", "-c", "sleep 0.2; echo BUREAU_LOG_TEST_MARKER; sleep 300"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Verify the log directory was created.
	agentLogDir := filepath.Join(obs.LogDir, agent)
	if _, err := os.Stat(agentLogDir); os.IsNotExist(err) {
		t.Fatalf("log directory %s was not created", agentLogDir)
	}

	// Verify the log file exists (pipe-pane creates it on first output).
	logPath := filepath.Join(agentLogDir, "terminal.log")

	// Give pipe-pane a moment to capture the echo output.
	var logContent []byte
	for attempt := 0; attempt < 20; attempt++ {
		time.Sleep(100 * time.Millisecond)
		logContent, _ = os.ReadFile(logPath)
		if strings.Contains(string(logContent), "BUREAU_LOG_TEST_MARKER") {
			break
		}
	}

	if !strings.Contains(string(logContent), "BUREAU_LOG_TEST_MARKER") {
		t.Errorf("log file %s does not contain expected marker; contents: %q", logPath, string(logContent))
	}
}

func TestLaunchWithBackscrollReplay(t *testing.T) {
	requireTmux(t)

	obs := testObserverWithLogging(t)
	obs.BackscrollLines = 5
	agent := "backscroll-test"

	cleanupSessions(t, obs, agent)
	defer cleanupSessions(t, obs, agent)

	// Simulate a previous run by writing a log file.
	agentLogDir := filepath.Join(obs.LogDir, agent)
	if err := os.MkdirAll(agentLogDir, 0o755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	logPath := filepath.Join(agentLogDir, "terminal.log")
	logLines := "line1\nline2\nline3\nline4\nline5\nline6\nline7\n"
	if err := os.WriteFile(logPath, []byte(logLines), 0o644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	// Launch — the pane command should replay the last 5 lines before running.
	err := obs.Launch(agent, []string{"sleep", "300"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Give tmux a moment to render the tail output.
	time.Sleep(500 * time.Millisecond)

	// Capture the pane's scroll buffer. Use pane IDs rather than indices
	// since pane-base-index may not be 0.
	sessionName := obs.agentSessionName(agent)
	paneIDs, err := listPaneIDs(sessionName + ":" + agent)
	if err != nil || len(paneIDs) == 0 {
		t.Fatalf("listPaneIDs: %v (got %v)", err, paneIDs)
	}
	cmd := exec.Command("tmux", "capture-pane", "-t", paneIDs[0], "-p", "-S", "-20")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("capture-pane: %v", err)
	}

	captured := string(output)

	// The last 5 lines of the log are line3-line7. Verify at least some
	// of them appear in the pane. (tail -n 5 on a 7-line file gives
	// lines 3-7.)
	for _, expected := range []string{"line3", "line4", "line5", "line6", "line7"} {
		if !strings.Contains(captured, expected) {
			t.Errorf("pane scroll buffer missing %q; captured:\n%s", expected, captured)
		}
	}

	// line1 should NOT be in the buffer (backscroll is 5, file has 7 lines).
	if strings.Contains(captured, "line1") {
		t.Errorf("pane scroll buffer unexpectedly contains line1 (backscroll should be 5); captured:\n%s", captured)
	}
}

func TestLaunchWithLayoutValidation(t *testing.T) {
	obs := testObserver(t)

	tests := []struct {
		name    string
		agent   string
		command []string
		layout  LaunchConfig
		wantErr string
	}{
		{
			name:    "empty command",
			agent:   "test",
			command: nil,
			layout:  LaunchConfig{},
			wantErr: "command is required",
		},
		{
			name:    "empty agent name",
			agent:   "",
			command: []string{"sleep", "1"},
			layout:  LaunchConfig{},
			wantErr: "agent name is required",
		},
		{
			name:    "pane without name",
			agent:   "test",
			command: []string{"sleep", "1"},
			layout: LaunchConfig{
				Panes: []PaneConfig{
					{Name: ""},
				},
			},
			wantErr: "has no name",
		},
		{
			name:    "no main pane",
			agent:   "test",
			command: []string{"sleep", "1"},
			layout: LaunchConfig{
				Panes: []PaneConfig{
					{Name: "logs", Command: []string{"tail", "-f", "/dev/null"}},
					{Name: "status", Command: []string{"watch", "date"}},
				},
			},
			wantErr: "no pane with empty Command",
		},
		{
			name:    "duplicate main pane",
			agent:   "test",
			command: []string{"sleep", "1"},
			layout: LaunchConfig{
				Panes: []PaneConfig{
					{Name: "agent"},
					{Name: "also-agent"},
				},
			},
			wantErr: "both have empty Command",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := obs.LaunchWithLayout(tt.agent, tt.command, tt.layout)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
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

func TestLoadLayoutConfig(t *testing.T) {
	configYAML := `layout: main-horizontal
panes:
  - name: agent
  - name: logs
    command: ["tail", "-f", "/var/log/test.log"]
  - name: status
    command: ["watch", "date"]
`
	path := filepath.Join(t.TempDir(), "layout.yaml")
	if err := os.WriteFile(path, []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	config, err := LoadLayoutConfig(path)
	if err != nil {
		t.Fatalf("LoadLayoutConfig: %v", err)
	}

	if config.Layout != "main-horizontal" {
		t.Errorf("layout = %q, want main-horizontal", config.Layout)
	}
	if len(config.Panes) != 3 {
		t.Fatalf("expected 3 panes, got %d", len(config.Panes))
	}
	if config.Panes[0].Name != "agent" {
		t.Errorf("pane 0 name = %q, want agent", config.Panes[0].Name)
	}
	if len(config.Panes[0].Command) != 0 {
		t.Errorf("pane 0 command should be empty, got %v", config.Panes[0].Command)
	}
	if config.Panes[1].Name != "logs" {
		t.Errorf("pane 1 name = %q, want logs", config.Panes[1].Name)
	}
	if len(config.Panes[1].Command) != 3 || config.Panes[1].Command[0] != "tail" {
		t.Errorf("pane 1 command = %v, want [tail -f /var/log/test.log]", config.Panes[1].Command)
	}
}

func TestLoadLayoutConfigErrors(t *testing.T) {
	t.Run("nonexistent file", func(t *testing.T) {
		_, err := LoadLayoutConfig("/nonexistent/path.yaml")
		if err == nil {
			t.Fatal("expected error for nonexistent file")
		}
	})

	t.Run("invalid YAML", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.yaml")
		if err := os.WriteFile(path, []byte("{{{{not yaml"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := LoadLayoutConfig(path)
		if err == nil {
			t.Fatal("expected error for invalid YAML")
		}
	})
}
