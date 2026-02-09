// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package observe manages tmux sessions for agent observation.
//
// Each agent runs in its own tmux session for lifecycle independence.
// An observation session ("bureau" by default) links agent windows
// so a human can navigate between agents with standard tmux keybindings.
//
// The key mechanism is tmux link-window: the same window appears in both
// the agent's session and the observation session. No nesting, no prefix
// key conflicts, and the agent survives if the observation session dies.
package observe

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultObserveSession is the name of the shared observation session.
	DefaultObserveSession = "bureau"

	// AgentSessionPrefix is prepended to agent names for their tmux sessions.
	AgentSessionPrefix = "agent-"

	// DefaultBackscrollLines is the number of log lines replayed into a pane's
	// scroll buffer on launch. This provides visual continuity when resuming
	// an agent after a restart, hibernation, or migration to another machine.
	DefaultBackscrollLines = 200
)

// Observer manages agent tmux sessions and the shared observation session.
//
// Each agent gets its own tmux session. The observation session links agent
// windows via tmux link-window, so a human can navigate between agents with
// standard keybindings (Ctrl-b n/p/number) without any nesting.
//
// When LogDir is set, every pane's terminal output is continuously captured
// to a log file via tmux pipe-pane. On subsequent launches (after restarts
// or migration to a different machine), the tail of the log is replayed into
// the pane's scroll buffer so the operator sees recent context when they
// attach — as if the session had never been interrupted.
type Observer struct {
	// ObserveSession is the name of the shared observation tmux session.
	// Defaults to DefaultObserveSession ("bureau").
	ObserveSession string

	// SessionPrefix is prepended to agent names for their tmux sessions.
	// Defaults to AgentSessionPrefix ("agent-").
	SessionPrefix string

	// LogDir is the base directory for pane output logs. Each agent gets a
	// subdirectory: <LogDir>/<agent>/<pane-name>.log
	//
	// When set, two things happen:
	//   - tmux pipe-pane captures all terminal output to the log file
	//   - On launch, if a log file exists, the last BackscrollLines are
	//     replayed into the pane's scroll buffer before the command starts
	//
	// When empty (the default), no logging or replay occurs.
	LogDir string

	// BackscrollLines is the number of log lines replayed into a pane's
	// scroll buffer on launch. Only used when LogDir is set.
	// Defaults to DefaultBackscrollLines (200).
	BackscrollLines int

	// Replace controls behavior when an agent session already exists with
	// live (running) panes. When false (the default), launching into a
	// running agent returns an error. When true, the existing session is
	// killed and replaced.
	//
	// Dead sessions (all panes exited, kept by remain-on-exit) are always
	// cleaned up automatically regardless of this setting.
	Replace bool
}

// New creates an Observer with default settings.
func New() *Observer {
	return &Observer{
		ObserveSession: DefaultObserveSession,
		SessionPrefix:  AgentSessionPrefix,
	}
}

// agentSessionName returns the tmux session name for an agent.
func (o *Observer) agentSessionName(agent string) string {
	return o.SessionPrefix + agent
}

// LaunchConfig describes a multi-pane observation layout for an agent.
//
// Each pane runs its own command in the same tmux window. Exactly one pane
// must have an empty Command — that pane runs the agent's main command as
// passed to LaunchWithLayout. The remaining panes run their own commands
// (log tails, status monitors, dashboards, etc.).
//
// Example YAML:
//
//	layout: main-vertical
//	panes:
//	  - name: agent
//	    # empty command = agent's main process
//	  - name: logs
//	    command: ["tail", "-f", "/var/log/bureau/agent.log"]
//	  - name: status
//	    command: ["watch", "-n5", "bureau-status"]
type LaunchConfig struct {
	// Panes defines the panes in the agent's tmux window.
	// Exactly one pane must have an empty Command (the agent's main process).
	// If empty, a single pane named "terminal" is created.
	Panes []PaneConfig `yaml:"panes"`

	// Layout is the tmux layout algorithm applied after all panes are created.
	// Options: main-vertical, main-horizontal, even-horizontal, even-vertical, tiled.
	// Defaults to "main-vertical".
	Layout string `yaml:"layout"`
}

// PaneConfig describes a single pane in a multi-pane observation layout.
type PaneConfig struct {
	// Name identifies this pane. Used for the tmux pane title and log file
	// naming (<LogDir>/<agent>/<name>.log). Required.
	Name string `yaml:"name"`

	// Command to run in this pane. If empty, this pane runs the agent's
	// main command as passed to LaunchWithLayout.
	Command []string `yaml:"command"`
}

// LoadLayoutConfig reads a LaunchConfig from a YAML file.
func LoadLayoutConfig(path string) (LaunchConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LaunchConfig{}, fmt.Errorf("read layout config %s: %w", path, err)
	}
	var config LaunchConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return LaunchConfig{}, fmt.Errorf("parse layout config %s: %w", path, err)
	}
	return config, nil
}

// SessionExists checks whether a tmux session with the given name exists.
func SessionExists(name string) bool {
	cmd := exec.Command("tmux", "has-session", "-t", name)
	return cmd.Run() == nil
}

// Launch creates a single-pane tmux session for an agent and links it into
// the observation session. This is shorthand for LaunchWithLayout with an
// empty LaunchConfig.
//
// If LogDir is set on the Observer, the pane's output is logged and replayed
// on subsequent launches.
func (o *Observer) Launch(agent string, command []string) error {
	return o.LaunchWithLayout(agent, command, LaunchConfig{})
}

// LaunchWithLayout creates a tmux session for an agent with a configurable
// multi-pane layout and links it into the observation session.
//
// The command parameter is the agent's main process command. In the layout,
// exactly one pane must have an empty Command field — that pane runs the
// agent's main command. Additional panes run their own commands (log tails,
// status monitors, etc.).
//
// If layout.Panes is empty, a single pane named "terminal" is created.
//
// When LogDir is set on the Observer, each pane's terminal output is captured
// to <LogDir>/<agent>/<pane-name>.log via tmux pipe-pane. If a log file
// already exists from a previous run, the last BackscrollLines are replayed
// into the pane's scroll buffer before the command starts. This means an
// operator who attaches after a restart or migration can scroll up and see
// what was happening before — visual continuity across restarts.
func (o *Observer) LaunchWithLayout(agent string, command []string, layout LaunchConfig) error {
	if len(command) == 0 {
		return fmt.Errorf("command is required")
	}
	if agent == "" {
		return fmt.Errorf("agent name is required")
	}

	// Default to a single pane if none specified.
	panes := make([]PaneConfig, len(layout.Panes))
	copy(panes, layout.Panes)
	if len(panes) == 0 {
		panes = []PaneConfig{{Name: "terminal"}}
	}

	// Find and validate the pane that runs the agent's main command.
	mainPaneIndex := -1
	for i, p := range panes {
		if p.Name == "" {
			return fmt.Errorf("pane at index %d has no name", i)
		}
		if len(p.Command) == 0 {
			if mainPaneIndex != -1 {
				return fmt.Errorf("panes %q and %q both have empty Command; exactly one pane must be designated for the agent's main command", panes[mainPaneIndex].Name, p.Name)
			}
			mainPaneIndex = i
		}
	}
	if mainPaneIndex == -1 {
		return fmt.Errorf("no pane with empty Command; exactly one pane must run the agent's main command (leave its Command empty)")
	}
	panes[mainPaneIndex].Command = command

	sessionName := o.agentSessionName(agent)
	if SessionExists(sessionName) {
		alive := sessionHasLivePanes(sessionName)
		if alive && !o.Replace {
			return fmt.Errorf("agent %q is already running; stop it first or use --replace", agent)
		}
		// Either all panes are dead (remain-on-exit corpse) or Replace is set.
		// Clean up and proceed with a fresh launch.
		if err := tmuxRun("kill-session", "-t", sessionName); err != nil {
			return fmt.Errorf("replace existing session for agent %q: %w", agent, err)
		}
	}

	backscrollLines := o.BackscrollLines
	if backscrollLines <= 0 {
		backscrollLines = DefaultBackscrollLines
	}

	// Create the log directory if logging is enabled.
	if o.LogDir != "" {
		agentLogDir := filepath.Join(o.LogDir, agent)
		if err := os.MkdirAll(agentLogDir, 0o755); err != nil {
			return fmt.Errorf("create log directory %s: %w", agentLogDir, err)
		}
	}

	// Pane creation and pipe-pane setup are interleaved to minimize the window
	// between when a pane starts running and when its output is captured.
	// The backscroll replay (tail of old log) runs as part of the pane's
	// command, so it reads the log BEFORE pipe-pane starts writing to it —
	// no corruption of the replay content.

	// Create the agent's tmux session with the first pane.
	firstPaneCommand := o.buildPaneCommand(panes[0].Command, agent, panes[0].Name, backscrollLines)
	firstPaneID, err := tmuxRunOutput(
		"new-session", "-d",
		"-s", sessionName,
		"-n", agent,
		"-x", "200", "-y", "50",
		"-P", "-F", "#{pane_id}",
		firstPaneCommand,
	)
	if err != nil {
		return fmt.Errorf("create agent session: %w", err)
	}

	// Clean up on any subsequent error — don't leave a half-built session.
	success := false
	defer func() {
		if !success {
			_ = tmuxRun("kill-session", "-t", sessionName)
		}
	}()

	// Set up logging for the first pane immediately.
	if o.LogDir != "" {
		if err := o.setupPaneLogging(firstPaneID, agent, panes[0].Name); err != nil {
			return err
		}
	}

	// Create additional panes, each with logging set up right after creation.
	for i := 1; i < len(panes); i++ {
		paneCommand := o.buildPaneCommand(panes[i].Command, agent, panes[i].Name, backscrollLines)
		paneID, err := tmuxRunOutput(
			"split-window", "-t", sessionName+":"+agent,
			"-P", "-F", "#{pane_id}",
			paneCommand,
		)
		if err != nil {
			return fmt.Errorf("create pane %q: %w", panes[i].Name, err)
		}

		if o.LogDir != "" {
			if err := o.setupPaneLogging(paneID, agent, panes[i].Name); err != nil {
				return err
			}
		}
	}

	// Apply the layout algorithm for multi-pane windows.
	if len(panes) > 1 {
		layoutName := layout.Layout
		if layoutName == "" {
			layoutName = "main-vertical"
		}
		if err := tmuxRun("select-layout", "-t", sessionName+":"+agent, layoutName); err != nil {
			return fmt.Errorf("apply layout %q: %w", layoutName, err)
		}
	}

	// Ensure the observation session exists.
	if !SessionExists(o.ObserveSession) {
		if err := tmuxRun("new-session", "-d", "-s", o.ObserveSession, "-n", "_placeholder"); err != nil {
			return fmt.Errorf("create observation session: %w", err)
		}
	}

	// Link the agent's window into the observation session.
	if err := tmuxRun("link-window", "-s", sessionName+":"+agent, "-t", o.ObserveSession, "-a"); err != nil {
		return fmt.Errorf("link agent window to observation session: %w", err)
	}

	// Remove the placeholder window if it exists (only needed to bootstrap
	// the session, since tmux requires at least one window).
	_ = tmuxRun("kill-window", "-t", o.ObserveSession+":_placeholder")

	success = true
	return nil
}

// Stop kills an agent's tmux session. The linked window in the
// observation session is automatically removed.
func (o *Observer) Stop(agent string) error {
	sessionName := o.agentSessionName(agent)
	if !SessionExists(sessionName) {
		return fmt.Errorf("agent session %q does not exist", sessionName)
	}
	return tmuxRun("kill-session", "-t", sessionName)
}

// Attach attaches the current terminal to the observation session.
// This blocks until the user detaches.
func (o *Observer) Attach() error {
	if !SessionExists(o.ObserveSession) {
		return fmt.Errorf("observation session %q does not exist (no agents running?)", o.ObserveSession)
	}

	cmd := exec.Command("tmux", "attach", "-t", o.ObserveSession)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ListAgents returns the names of all agent windows in the observation session.
func (o *Observer) ListAgents() ([]AgentInfo, error) {
	if !SessionExists(o.ObserveSession) {
		return nil, nil
	}

	// List windows in the observation session.
	cmd := exec.Command("tmux", "list-windows", "-t", o.ObserveSession,
		"-F", "#{window_name}\t#{pane_dead}\t#{pane_dead_status}")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list windows: %w", err)
	}

	var agents []AgentInfo
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		name := parts[0]

		// Skip internal windows.
		if name == "_placeholder" {
			continue
		}

		info := AgentInfo{
			Name:    name,
			Running: true,
		}

		if len(parts) >= 2 && parts[1] == "1" {
			info.Running = false
			if len(parts) >= 3 {
				info.ExitStatus = parts[2]
			}
		}

		agents = append(agents, info)
	}

	return agents, nil
}

// AgentInfo describes an agent visible in the observation session.
type AgentInfo struct {
	Name       string
	Running    bool
	ExitStatus string // Set when Running is false.
}

// logPath returns the log file path for a pane. Returns empty string if
// logging is not configured (LogDir is empty).
func (o *Observer) logPath(agent, paneName string) string {
	if o.LogDir == "" {
		return ""
	}
	return filepath.Join(o.LogDir, agent, paneName+".log")
}

// buildPaneCommand constructs the shell command string for a tmux pane.
//
// Every pane command is prefixed with "tmux set-option remain-on-exit on"
// so that panes persist after their process exits. This is done inside the
// pane's own shell rather than as a separate tmux command, because a
// separate command would race with fast-exiting processes — the session
// could vanish before set-option runs.
//
// With logging enabled and a log file from a previous run, the command
// also replays recent scroll history:
//
//	tmux set-option remain-on-exit on; tail -n 200 /path/to/log 2>/dev/null; exec cmd
//
// The tail output lands in the pane's scroll buffer as history, then exec
// replaces the shell with the real command.
func (o *Observer) buildPaneCommand(command []string, agent, paneName string, backscrollLines int) string {
	shellCommand := shellJoin(command)

	// remain-on-exit is set from inside the pane as the very first action.
	// This guarantees it's in effect before the actual command starts,
	// regardless of how fast the command exits.
	prefix := "tmux set-option remain-on-exit on 2>/dev/null;"

	logPath := o.logPath(agent, paneName)
	if logPath == "" {
		return fmt.Sprintf("%s exec %s", prefix, shellCommand)
	}
	return fmt.Sprintf("%s tail -n %d %s 2>/dev/null; exec %s",
		prefix, backscrollLines, shellQuote(logPath), shellCommand)
}

// sessionHasLivePanes returns true if the tmux session has at least one pane
// whose process is still running (as opposed to dead panes kept by
// remain-on-exit).
func sessionHasLivePanes(sessionName string) bool {
	cmd := exec.Command("tmux", "list-panes", "-t", sessionName, "-F", "#{pane_dead}")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "0" {
			return true
		}
	}
	return false
}

// setupPaneLogging configures pipe-pane for a tmux pane to continuously
// capture terminal output to a log file.
func (o *Observer) setupPaneLogging(paneID, agent, paneName string) error {
	logPath := o.logPath(agent, paneName)
	pipeCommand := fmt.Sprintf("cat >> %s", shellQuote(logPath))
	if err := tmuxRun("pipe-pane", "-t", paneID, pipeCommand); err != nil {
		return fmt.Errorf("set up logging for pane %q: %w", paneName, err)
	}
	return nil
}

// listPaneIDs returns the unique pane IDs (e.g. "%0", "%1") for all panes
// in the given tmux window target. Pane IDs are global and stable, unlike
// pane indices which depend on pane-base-index configuration.
func listPaneIDs(windowTarget string) ([]string, error) {
	cmd := exec.Command("tmux", "list-panes", "-t", windowTarget, "-F", "#{pane_id}")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tmux list-panes -t %s: %w", windowTarget, err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var ids []string
	for _, line := range lines {
		if line != "" {
			ids = append(ids, line)
		}
	}
	return ids, nil
}

// tmuxRun executes a tmux command and returns any error.
func tmuxRun(args ...string) error {
	cmd := exec.Command("tmux", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// tmuxRunOutput executes a tmux command and returns its stdout, trimmed.
// Used with -P -F flags to capture pane/window IDs from creation commands.
func tmuxRunOutput(args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
	cmd.Stderr = os.Stderr
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output)), nil
}

// shellJoin quotes and joins arguments into a shell command string.
// This is used because tmux new-session takes a shell string, not argv.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		if strings.ContainsAny(arg, " \t\n\"'\\$`!#&|;(){}[]<>?*~") {
			quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
		} else {
			quoted[i] = arg
		}
	}
	return strings.Join(quoted, " ")
}

// shellQuote wraps a string in single quotes for safe use in shell commands.
// Unlike shellJoin, this always quotes — suitable for paths that may contain
// spaces or other shell metacharacters.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
