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
	"strconv"
	"strings"
	"syscall"
	"time"
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

// CapturePane captures the full scrollback and visible content of a pane
// in the named session. Returns the captured text. The pane must still
// exist (e.g., the session has remain-on-exit enabled and the command has
// exited but the session has not been killed).
//
// Uses capture-pane with -p (print to stdout), -S - (start of history),
// and -E - (end of visible area) to get the complete pane content. The
// output includes terminal control sequences if the process used them.
//
// maxLines limits the output to the last N lines. Pass 0 for no limit.
func (s *Server) CapturePane(sessionName string, maxLines int) (string, error) {
	output, err := s.Run("capture-pane", "-t", sessionName, "-p", "-S", "-", "-E", "-")
	if err != nil {
		return "", err
	}

	if maxLines <= 0 {
		return output, nil
	}

	return tailString(output, maxLines), nil
}

// PaneStatus returns whether the pane's command has exited and, if so,
// its exit code. This requires remain-on-exit to be enabled on the
// session (which Bureau always configures): when the command exits, the
// pane stays alive with #{pane_dead} set to 1.
//
// The exit code is derived from two tmux format variables:
//   - #{pane_dead_status}: the WEXITSTATUS value (valid for normal exits)
//   - #{pane_dead_signal}: the signal number (valid for signal deaths)
//
// When the process was killed by a signal, the exit code follows the
// shell convention: 128 + signal number (e.g., SIGTERM=15 → 143).
// This matches what a shell wrapper would report and allows callers
// to distinguish normal exits from signal deaths.
//
// Returns dead=false when the pane process is still running. The
// exitCode value is only meaningful when dead=true.
func (s *Server) PaneStatus(sessionName string) (dead bool, exitCode int, err error) {
	return s.parsePaneStatus(sessionName)
}

// paneStatusRetryDelay is the delay between retries when tmux reports
// dead=1 but hasn't yet populated the exit status fields.
const paneStatusRetryDelay = 50 * time.Millisecond

// paneStatusMaxRetries is the number of times to re-query tmux when
// pane_dead=1 but both pane_dead_status and pane_dead_signal are empty.
// Tmux 3.4+ has a race window between setting pane_dead and populating
// the exit status fields. Under heavy parallel test load, the tmux
// event loop can take longer than a single retry to finish recording
// the exit status. 5 retries × 50ms = 250ms maximum wait, which is
// within the session watcher's 250ms poll interval.
const paneStatusMaxRetries = 5

func (s *Server) parsePaneStatus(sessionName string) (dead bool, exitCode int, err error) {
	for attempt := 0; ; attempt++ {
		output, queryErr := s.Run("display-message", "-t", sessionName, "-p",
			"#{pane_dead} #{pane_dead_status} #{pane_dead_signal}")
		if queryErr != nil {
			return false, 0, queryErr
		}

		// tmux outputs three space-separated values. Empty values collapse:
		//   "0"           (running — no status or signal)
		//   "1 42"        (exit 42 — no signal)
		//   "1  15"       (SIGTERM — empty status, signal 15)
		//   "1"           (status not yet populated — race window)
		//
		// #{pane_dead_signal} is best-effort: tmux sometimes doesn't
		// populate it for signal deaths, and the behavior varies across
		// tmux versions. When available, the exit code follows the shell
		// convention (128 + signal number). When unavailable, signal
		// deaths are reported as exit code 0. The daemon tracks drain
		// state independently and does not rely on signal-specific exit
		// codes for correctness.
		trimmed := strings.TrimRight(output, "\n")
		parts := strings.SplitN(trimmed, " ", 3)
		if len(parts) == 0 || parts[0] == "" {
			return false, 0, fmt.Errorf("empty pane status output")
		}

		deadValue, parseErr := strconv.Atoi(parts[0])
		if parseErr != nil {
			return false, 0, fmt.Errorf("parsing pane_dead %q: %w", parts[0], parseErr)
		}

		if deadValue == 0 {
			return false, 0, nil
		}

		// Process is dead. Check signal first (takes precedence over
		// pane_dead_status, which is the WEXITSTATUS value and undefined
		// for signal deaths).
		hasStatus := len(parts) >= 2 && parts[1] != ""
		hasSignal := len(parts) >= 3 && parts[2] != ""

		if hasSignal {
			signalNumber, parseErr := strconv.Atoi(parts[2])
			if parseErr != nil {
				return true, -1, fmt.Errorf("parsing pane_dead_signal %q: %w", parts[2], parseErr)
			}
			return true, 128 + signalNumber, nil
		}

		if hasStatus {
			status, parseErr := strconv.Atoi(parts[1])
			if parseErr != nil {
				return true, -1, fmt.Errorf("parsing pane_dead_status %q: %w", parts[1], parseErr)
			}
			return true, status, nil
		}

		// Both fields empty. Tmux 3.4+ has a race window between
		// setting pane_dead=1 and populating the status fields. Retry
		// to give the tmux event loop time to finish recording the
		// exit status. After all retries exhausted, treat as exit
		// code 0 (some tmux versions don't set pane_dead_status for
		// code 0).
		if attempt >= paneStatusMaxRetries {
			return true, 0, nil
		}
		time.Sleep(paneStatusRetryDelay)
	}
}

// PanePID returns the process ID of the command running in the named
// session's active pane. With remain-on-exit enabled, this value is
// available even after the process has exited — it's the PID that was
// originally assigned when tmux launched the pane's command.
//
// The session watcher uses this to wait for the pane process to actually
// exit (via kill(pid, 0) polling), resolving the tmux 3.4+ race between
// PTY EOF detection and SIGCHLD processing that makes #{pane_dead_status}
// unreliable in the immediate aftermath of pane death.
func (s *Server) PanePID(sessionName string) (int, error) {
	output, err := s.Run("display-message", "-t", sessionName, "-p", "#{pane_pid}")
	if err != nil {
		return 0, fmt.Errorf("getting pane PID: %w", err)
	}

	pid, parseErr := strconv.Atoi(strings.TrimSpace(output))
	if parseErr != nil {
		return 0, fmt.Errorf("parsing pane PID %q: %w", strings.TrimSpace(output), parseErr)
	}

	return pid, nil
}

// SignalPane sends a signal to the process running in the named
// session's active pane. The pane must be alive (the command must still
// be running). Uses #{pane_pid} to discover the process ID, then sends
// the signal directly via syscall.Kill.
//
// This is the mechanism for graceful drain: the daemon sends SIGTERM to
// a sandbox's pane process (bwrap), which forwards the signal to the
// agent inside the namespace.
func (s *Server) SignalPane(sessionName string, signal syscall.Signal) error {
	output, err := s.Run("display-message", "-t", sessionName, "-p", "#{pane_pid}")
	if err != nil {
		return fmt.Errorf("getting pane PID: %w", err)
	}

	pid, parseErr := strconv.Atoi(strings.TrimSpace(output))
	if parseErr != nil {
		return fmt.Errorf("parsing pane PID %q: %w", strings.TrimSpace(output), parseErr)
	}

	if err := syscall.Kill(pid, signal); err != nil {
		return fmt.Errorf("signaling PID %d with %v: %w", pid, signal, err)
	}

	return nil
}

// tailString returns the last n lines of s, matching tail -n semantics:
// a trailing newline terminates the last line (does not start a new one).
// If s has n or fewer lines, it is returned unchanged.
func tailString(s string, n int) string {
	if len(s) == 0 {
		return s
	}

	// A trailing newline terminates the last line — search from before it
	// so it doesn't count as an extra line separator.
	searchFrom := len(s) - 1
	if s[searchFrom] == '\n' {
		searchFrom--
	}

	// Walk backwards counting newline separators. For n lines we need
	// n-1 separators between them, plus one more newline to find the
	// cut point (the newline before the first of our n lines).
	count := 0
	for i := searchFrom; i >= 0; i-- {
		if s[i] == '\n' {
			count++
			if count == n {
				return s[i+1:]
			}
		}
	}
	return s
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
