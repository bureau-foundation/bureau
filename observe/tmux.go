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
	"strings"
)

const (
	// DefaultObserveSession is the name of the shared observation session.
	DefaultObserveSession = "bureau"

	// AgentSessionPrefix is prepended to agent names for their tmux sessions.
	AgentSessionPrefix = "agent-"
)

// Observer manages agent tmux sessions and the shared observation session.
type Observer struct {
	// ObserveSession is the name of the shared observation tmux session.
	// Defaults to DefaultObserveSession ("bureau").
	ObserveSession string

	// SessionPrefix is prepended to agent names for their tmux sessions.
	// Defaults to AgentSessionPrefix ("agent-").
	SessionPrefix string
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

// SessionExists checks whether a tmux session with the given name exists.
func SessionExists(name string) bool {
	cmd := exec.Command("tmux", "has-session", "-t", name)
	return cmd.Run() == nil
}

// Launch creates a tmux session for an agent and links it into the
// observation session. If the observation session doesn't exist,
// it is created.
//
// The command is executed inside the agent's tmux session. When the
// command exits, the window remains (with remain-on-exit) so the
// human can see what happened.
func (o *Observer) Launch(agent string, command []string) error {
	if len(command) == 0 {
		return fmt.Errorf("command is required")
	}
	if agent == "" {
		return fmt.Errorf("agent name is required")
	}

	sessionName := o.agentSessionName(agent)

	if SessionExists(sessionName) {
		return fmt.Errorf("agent session %q already exists", sessionName)
	}

	// Build the command string for tmux. tmux new-session takes a shell
	// command string, not an argv, so we need to quote properly.
	shellCommand := shellJoin(command)

	// Create the agent's own tmux session (detached).
	createArgs := []string{
		"new-session", "-d",
		"-s", sessionName,
		"-n", agent,
		"-x", "200", "-y", "50",
		shellCommand,
	}
	if err := tmuxRun(createArgs...); err != nil {
		return fmt.Errorf("create agent session: %w", err)
	}

	// Set remain-on-exit so the window persists after the agent exits.
	if err := tmuxRun("set-option", "-t", sessionName, "remain-on-exit", "on"); err != nil {
		// Non-fatal: the session was created, just won't persist after exit.
		fmt.Fprintf(os.Stderr, "warning: could not set remain-on-exit: %v\n", err)
	}

	// Ensure the observation session exists.
	if !SessionExists(o.ObserveSession) {
		// Create it with a placeholder window we'll remove after linking.
		if err := tmuxRun("new-session", "-d", "-s", o.ObserveSession, "-n", "_placeholder"); err != nil {
			return fmt.Errorf("create observation session: %w", err)
		}
	}

	// Link the agent's window into the observation session.
	if err := tmuxRun("link-window", "-s", sessionName+":"+agent, "-t", o.ObserveSession, "-a"); err != nil {
		return fmt.Errorf("link agent window to observation session: %w", err)
	}

	// Clean up the placeholder window if it exists (it was only needed
	// to create the session, since tmux requires at least one window).
	// Ignore errors — it may not exist if the session already had windows.
	_ = tmuxRun("kill-window", "-t", o.ObserveSession+":_placeholder")

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

// tmuxRun executes a tmux command and returns any error.
func tmuxRun(args ...string) error {
	cmd := exec.Command("tmux", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux %s: %w", strings.Join(args, " "), err)
	}
	return nil
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
