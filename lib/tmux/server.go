// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package tmux provides a typed interface to tmux servers. Bureau runs its
// own dedicated tmux server (distinct from the user's personal tmux) for
// sandbox session management and observation. All operations target a
// specific server socket — there is no default server, and the user's
// ~/.tmux.conf is never loaded unless explicitly requested.
//
// The central type is Server, which represents a connection to a tmux
// server identified by its Unix socket path. All tmux commands go through
// Server, which injects the -S flag automatically. This makes it
// structurally impossible to accidentally target the wrong server or
// forget to specify a socket.
package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Server represents a tmux server identified by its Unix socket path.
// All operations target this specific server — there is no way to run a
// tmux command without specifying which server it applies to.
//
// Bureau never uses the user's personal tmux server. Each Launcher
// instance creates a dedicated server with -f /dev/null to prevent
// loading ~/.tmux.conf.
type Server struct {
	socketPath string
	configFile string // passed as "-f <path>" on new-session; empty = tmux default
}

// NewServer returns a Server that targets the given socket path.
//
// configFile controls which configuration file tmux loads when the server
// starts (which happens on the first new-session call). Pass "/dev/null"
// to prevent loading the user's ~/.tmux.conf — this is required for
// Bureau's production servers and all tests. If configFile is empty, tmux
// uses its default config resolution (~/.tmux.conf, then
// $XDG_CONFIG_HOME/tmux/tmux.conf), which is almost never what Bureau
// wants.
func NewServer(socketPath, configFile string) *Server {
	return &Server{
		socketPath: socketPath,
		configFile: configFile,
	}
}

// SocketPath returns the Unix socket path that identifies this server.
func (s *Server) SocketPath() string {
	return s.socketPath
}

// NewSession creates a detached tmux session on this server. If command
// is non-empty, the session runs that command instead of the default
// shell.
//
// The -f flag (config file) is passed on new-session because this command
// may start the server if it isn't already running. Once the server is
// running, subsequent commands don't re-read the config file, so only
// new-session needs it.
func (s *Server) NewSession(sessionName string, command ...string) error {
	args := s.newSessionArgs(sessionName)
	args = append(args, command...)
	cmd := exec.Command("tmux", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session %q: %w (%s)",
			sessionName, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// newSessionArgs builds the argument list for a new-session command,
// including -f (config), -S (socket), and -d -s (detached, named).
func (s *Server) newSessionArgs(sessionName string) []string {
	var args []string
	if s.configFile != "" {
		args = append(args, "-f", s.configFile)
	}
	args = append(args, "-S", s.socketPath, "new-session", "-d", "-s", sessionName)
	return args
}

// HasSession reports whether a session with the given name exists on
// this server. Returns false if the server is not running.
func (s *Server) HasSession(sessionName string) bool {
	cmd := exec.Command("tmux", "-S", s.socketPath, "has-session", "-t", sessionName)
	return cmd.Run() == nil
}

// KillSession terminates a specific session. Returns nil if the session
// was already gone or the server was not running — these are normal
// conditions during cleanup, not errors.
func (s *Server) KillSession(sessionName string) error {
	cmd := exec.Command("tmux", "-S", s.socketPath, "kill-session", "-t", sessionName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		outputString := strings.TrimSpace(string(output))
		// "can't find session" and "no server running" are benign during
		// cleanup — the session was already gone.
		if strings.Contains(outputString, "can't find session") ||
			strings.Contains(outputString, "no server running") {
			return nil
		}
		return fmt.Errorf("tmux kill-session %q: %w (%s)",
			sessionName, err, outputString)
	}
	return nil
}

// KillServer terminates the entire tmux server, stopping all sessions.
// Returns nil if the server was already stopped — this is a normal
// condition during cleanup, not an error.
func (s *Server) KillServer() error {
	cmd := exec.Command("tmux", "-S", s.socketPath, "kill-server")
	output, err := cmd.CombinedOutput()
	if err != nil {
		outputString := strings.TrimSpace(string(output))
		// "no server running" and "server exited unexpectedly" are benign
		// during cleanup: the server is already gone, which is what we wanted.
		// The "server exited unexpectedly" message appears when the socket
		// file lingers briefly after the server process has exited.
		if strings.Contains(outputString, "no server running") ||
			strings.Contains(outputString, "server exited unexpectedly") {
			return nil
		}
		return fmt.Errorf("tmux kill-server: %w (%s)", err, outputString)
	}
	return nil
}

// SetOption sets a tmux option on this server. If sessionName is empty,
// the option is set globally (-g) and applies to all sessions. If
// sessionName is non-empty, the option is set on that specific session.
func (s *Server) SetOption(sessionName, key, value string) error {
	var args []string
	if sessionName == "" {
		args = []string{"-S", s.socketPath, "set-option", "-g", key, value}
	} else {
		args = []string{"-S", s.socketPath, "set-option", "-t", sessionName, key, value}
	}
	cmd := exec.Command("tmux", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux set-option %q=%q (session %q): %w (%s)",
			key, value, sessionName, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// Run executes an arbitrary tmux subcommand on this server and returns
// the combined output. This is the escape hatch for commands that don't
// have a dedicated method — list-panes, send-keys, capture-pane,
// split-window, list-windows, etc.
//
// The -S flag is automatically prepended. Callers provide only the
// subcommand and its arguments:
//
//	output, err := server.Run("list-panes", "-t", session, "-F", "#{pane_index}")
func (s *Server) Run(args ...string) (string, error) {
	fullArgs := append([]string{"-S", s.socketPath}, args...)
	cmd := exec.Command("tmux", fullArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %w (%s)",
			strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

// Command returns an *exec.Cmd for a tmux subcommand without running it.
// The caller gets full control over Stdin, Stdout, Stderr, and
// SysProcAttr before starting the process.
//
// This is needed for two cases in the observe package:
//   - Relay: attaches tmux to a PTY slave with session leader / ctty
//     configuration via SysProcAttr
//   - ControlClient: pipes stdin/stdout for control mode streaming
//
// The -S flag is automatically prepended, as with Run.
func (s *Server) Command(args ...string) *exec.Cmd {
	fullArgs := append([]string{"-S", s.socketPath}, args...)
	return exec.Command("tmux", fullArgs...)
}

// CommandContext is like Command but accepts a context for cancellation.
// When the context is cancelled, the tmux process receives SIGKILL.
func (s *Server) CommandContext(ctx context.Context, args ...string) *exec.Cmd {
	fullArgs := append([]string{"-S", s.socketPath}, args...)
	return exec.CommandContext(ctx, "tmux", fullArgs...)
}
