// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema/observation"
	"github.com/bureau-foundation/bureau/lib/tmux"
	"github.com/bureau-foundation/bureau/observe"
	"golang.org/x/term"
)

// ObserveCommand returns the "observe" subcommand for attaching to a
// principal's terminal session.
func ObserveCommand() *cli.Command {
	var readonly bool
	var socketPath string

	return &cli.Command{
		Name:    "observe",
		Summary: "Attach to a principal's terminal session",
		Description: `Attach to a principal's terminal session via the observation relay.

Requires authentication: run "bureau login" first to save your session.
The saved session is loaded automatically (like SSH keys).

The target is a principal localpart (e.g., "iree/amdgpu/pm") or Matrix
ID (e.g., "@iree/amdgpu/pm:bureau.local"). The leading "@" is stripped
automatically.

On connect, the relay sends the terminal's scrollback history followed
by a status line showing the principal and machine name. In readwrite
mode (the default), your keystrokes are forwarded to the remote terminal
— you are typing into the agent's session. In readonly mode, keystrokes
are not forwarded, making it safe for monitoring.

To disconnect, press Ctrl-C or close your terminal. There is no detach
mechanism — each observation is a live stream, not a tmux client attach.

Access is controlled by the target principal's observation allowances in
its authorization policy. If you get "not authorized", ask a deployment
admin to add an observe allowance for your identity on the target.`,
		Usage: "bureau observe <target> [flags]",
		Examples: []cli.Example{
			{
				Description: "Observe an agent's terminal",
				Command:     "bureau observe iree/amdgpu/pm",
			},
			{
				Description: "Read-only monitoring (no input forwarded)",
				Command:     "bureau observe iree/amdgpu/pm --readonly",
			},
			{
				Description: "Observe a service on a remote machine",
				Command:     "bureau observe service/stt/whisper",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("observe", pflag.ContinueOnError)
			flagSet.BoolVar(&readonly, "readonly", false, "observe without input (read-only mode)")
			flagSet.StringVar(&socketPath, "socket", observe.DefaultDaemonSocket, "daemon observation socket path")
			return flagSet
		},
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("target argument required\n\nUsage: bureau observe <target> [flags]")
			}

			if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}
			target := strings.TrimPrefix(args[0], "@")

			operatorSession, err := cli.LoadSession()
			if err != nil {
				return err
			}

			mode := "readwrite"
			if readonly {
				mode = "readonly"
			}

			session, err := observe.Connect(socketPath, observe.ObserveRequest{
				Principal: target,
				Mode:      mode,
				Observer:  operatorSession.UserID,
				Token:     operatorSession.AccessToken,
			})
			if err != nil {
				if diagnosed := cli.DiagnoseSocketError(err, socketPath); diagnosed != nil {
					return diagnosed
				}
				return cli.Transient("connect to observation relay for %s: %w", target, err).
					WithHint("Check that the Bureau daemon is running on the target machine " +
						"and that the principal is active. Run 'bureau observe list' to see available targets.")
			}
			defer session.Close()

			stdinFd := int(os.Stdin.Fd())
			oldState, err := term.MakeRaw(stdinFd)
			if err != nil {
				return cli.Internal("set terminal raw mode: %w", err)
			}
			defer term.Restore(stdinFd, oldState)

			signalChannel := make(chan os.Signal, 1)
			signal.Notify(signalChannel, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				<-signalChannel
				term.Restore(stdinFd, oldState)
				session.Close()
				os.Exit(0)
			}()

			return session.Run(os.Stdin, os.Stdout)
		},
	}
}

// DashboardCommand returns the "dashboard" subcommand for composite
// workstream views.
func DashboardCommand() *cli.Command {
	var (
		socketPath string
		layoutFile string
		tmuxSocket string
		detach     bool
	)

	return &cli.Command{
		Name:    "dashboard",
		Summary: "Open a composite workstream view",
		Description: `Open a composite workstream view where each pane observes a different
principal. The layout defines which principals to show and how to
arrange them.

Requires authentication: run "bureau login" first to save your session.
The saved session is loaded automatically (like SSH keys).

Layout sources:
  (no args)        Machine dashboard: all principals on this machine
  --layout-file    Read layout from a local JSON file
  <channel>        Fetch layout from a channel's Matrix state via the daemon

With no arguments, the dashboard shows all observable principals running
on the local machine, arranged as a vertical stack. This is the default
sysadmin view — type "bureau dashboard" and see everything at a glance.

The dashboard creates a local tmux session and attaches to it. Standard
tmux navigation works inside the dashboard:

  Ctrl-b d         Detach from the dashboard (it keeps running)
  Ctrl-b <arrow>   Move between panes
  Ctrl-b n / p     Next / previous window

To reattach after detaching, use "tmux attach-session -t <name>" where
the session name is printed on creation (e.g., "observe/machine/workstation").

Each "observe" pane connects to the daemon's observation relay for the
target principal. "command" panes run local commands. "role" panes
display their role identity.`,
		Usage: "bureau dashboard [channel] [flags]",
		Examples: []cli.Example{
			{
				Description: "Machine dashboard (all running principals)",
				Command:     "bureau dashboard",
			},
			{
				Description: "Open a project channel dashboard",
				Command:     "bureau dashboard '#iree/amdgpu/general'",
			},
			{
				Description: "Open a dashboard from a layout file",
				Command:     "bureau dashboard --layout-file ./workspace.json",
			},
			{
				Description: "Create dashboard without attaching",
				Command:     "bureau dashboard --detach",
			},
			{
				Description: "Reattach to a detached dashboard",
				Command:     "tmux attach-session -t observe/machine/workstation",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("dashboard", pflag.ContinueOnError)
			flagSet.StringVar(&socketPath, "socket", observe.DefaultDaemonSocket, "daemon observation socket path")
			flagSet.StringVar(&layoutFile, "layout-file", "", "read layout from a JSON file")
			flagSet.StringVar(&tmuxSocket, "tmux-socket", "", "tmux server socket (default: user's default tmux)")
			flagSet.BoolVar(&detach, "detach", false, "create the session without attaching")
			return flagSet
		},
		Run: func(_ context.Context, args []string, logger *slog.Logger) error {
			channel := ""
			if len(args) > 0 {
				channel = args[0]
			}
			if len(args) > 1 {
				return cli.Validation("unexpected argument: %s", args[1])
			}

			// Load operator session for authentication. Required even
			// for file-based layouts: the dashboard spawns bureau observe
			// subprocesses that each authenticate independently via the
			// session file. We verify the session exists up front to
			// fail early with a clear error rather than after tmux
			// session creation.
			operatorSession, err := cli.LoadSession()
			if err != nil {
				return err
			}

			// --- Resolve layout from the specified source ---

			var layout *observe.Layout
			var sessionName string

			switch {
			case layoutFile != "":
				var err error
				layout, err = loadLayoutFile(layoutFile)
				if err != nil {
					return cli.Internal("load layout file: %w", err)
				}
				// Derive session name from the filename.
				name := strings.TrimSuffix(layoutFile, ".json")
				name = strings.TrimSuffix(name, ".yaml")
				// Take just the base name, not the full path.
				if idx := strings.LastIndex(name, "/"); idx >= 0 {
					name = name[idx+1:]
				}
				sessionName = "observe/" + name

			case channel != "":
				// Strip surrounding quotes that shell quoting may leave.
				channel = strings.Trim(channel, "'\"")
				var err error
				layout, err = observe.QueryLayout(socketPath, observe.QueryLayoutRequest{
					Channel:  channel,
					Observer: operatorSession.UserID,
					Token:    operatorSession.AccessToken,
				})
				if err != nil {
					if diagnosed := cli.DiagnoseSocketError(err, socketPath); diagnosed != nil {
						return diagnosed
					}
					return cli.Internal("query channel layout: %w", err)
				}
				// Derive session name from the channel alias. Strip the
				// sigil and server name to get a clean path:
				// "#iree/amdgpu/general:bureau.local" → "observe/iree/amdgpu/general"
				name := channel
				if strings.HasPrefix(name, "#") {
					name = name[1:]
				}
				if colonIndex := strings.Index(name, ":"); colonIndex >= 0 {
					name = name[:colonIndex]
				}
				sessionName = "observe/" + name

			default:
				// No layout source specified — machine dashboard. Query the
				// daemon for a dynamically generated layout showing all
				// principals running on this machine.
				response, err := observe.QueryMachineLayout(socketPath, observe.MachineLayoutRequest{
					Observer: operatorSession.UserID,
					Token:    operatorSession.AccessToken,
				})
				if err != nil {
					if diagnosed := cli.DiagnoseSocketError(err, socketPath); diagnosed != nil {
						return diagnosed
					}
					return cli.Internal("query machine layout: %w", err)
				}
				layout = response.Layout
				sessionName = "observe/machine/" + response.Machine
			}

			// --- Create the dashboard tmux session ---

			// Dashboard sessions live in the user's own tmux server
			// (not Bureau's managed tmux), so they show up in the
			// user's normal `tmux list-sessions`.
			if tmuxSocket == "" {
				tmuxSocket = defaultTmuxSocket()
			}

			tmuxArgs := []string{"-S", tmuxSocket}
			// Dashboard sessions live in the user's tmux server, not
			// Bureau's dedicated server. No config override needed —
			// the user's tmux is already running with their config.
			server := tmux.NewServer(tmuxSocket, "")

			if err := observe.Dashboard(server, sessionName, socketPath, layout); err != nil {
				return cli.Internal("create dashboard: %w", err)
			}

			logger.Info("dashboard session created", "session", sessionName)

			if detach {
				return nil
			}

			// Attach to the session via exec (replaces this process).
			tmuxBinary, err := exec.LookPath("tmux")
			if err != nil {
				return cli.Validation("tmux not found in PATH: %w", err).
					WithHint("Install tmux to use dashboard mode. On Debian/Ubuntu: apt install tmux. On macOS: brew install tmux.")
			}

			// Build the full exec argv without mutating tmuxArgs.
			// syscall.Exec replaces the process — the dashboard CLI
			// becomes the tmux client. This is the standard pattern
			// for "open a tmux session" commands.
			execArgs := make([]string, 0, 1+len(tmuxArgs)+3)
			execArgs = append(execArgs, "tmux")
			execArgs = append(execArgs, tmuxArgs...)
			execArgs = append(execArgs, "attach-session", "-t", sessionName)
			return syscall.Exec(tmuxBinary, execArgs, os.Environ())
		},
	}
}

// loadLayoutFile reads a layout from a JSON file. The file format matches
// the observation.LayoutContent wire format (the same JSON that appears in
// m.bureau.layout state events).
func loadLayoutFile(path string) (*observe.Layout, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var content observation.LayoutContent
	if err := json.Unmarshal(data, &content); err != nil {
		return nil, cli.Internal("parsing layout JSON: %w", err)
	}

	if len(content.Windows) == 0 {
		return nil, cli.Validation("layout has no windows")
	}

	return observe.SchemaToLayout(content), nil
}

// defaultTmuxSocket returns the path to the user's default tmux server
// socket. This matches the path tmux uses when invoked without -S:
// $TMUX_TMPDIR/tmux-$UID/default (or /tmp/tmux-$UID/default if
// TMUX_TMPDIR is unset).
//
// Dashboard sessions go in the user's own tmux (not Bureau's managed
// tmux server) so they appear in regular `tmux list-sessions`.
func defaultTmuxSocket() string {
	tmpDirectory := os.Getenv("TMUX_TMPDIR")
	if tmpDirectory == "" {
		tmpDirectory = os.TempDir()
	}
	return filepath.Join(tmpDirectory, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
}

// listParams holds the parameters for the observe list command.
type listParams struct {
	cli.JSONOutput
	Observable bool   `json:"observable"   flag:"observable"   desc:"show only targets that can be observed"`
	SocketPath string `json:"-"            flag:"socket"       desc:"daemon observation socket path" default:"/run/bureau/observe.sock"`
}

// ListCommand returns the "list" subcommand for querying observable targets.
func ListCommand() *cli.Command {
	var params listParams

	return &cli.Command{
		Name:    "list",
		Summary: "List observable targets",
		Description: `List principals and machines known to the daemon.

Requires authentication: run "bureau login" first to save your session.
The saved session is loaded automatically (like SSH keys). The list is
filtered to only principals your account is authorized to observe.

With --observable, only targets that can currently be observed (have
an active tmux session or are reachable via transport) are shown.

Principals come from two sources: locally running sandboxes and the
service directory (which includes remote services). Machines include
the local machine and all known peers.

Principal statuses:
  running       Active on this machine with a tmux session
  remote        Active on a reachable remote machine
  known         In the service directory but not currently observable
  unreachable   On a remote machine with no transport path

Machine statuses:
  self          This machine
  peer          A remote machine with a known transport address
  unreachable   A remote machine with no transport path`,
		Usage: "bureau list [flags]",
		Examples: []cli.Example{
			{
				Description: "List all known targets",
				Command:     "bureau list",
			},
			{
				Description: "List only targets you can observe right now",
				Command:     "bureau list --observable",
			},
			{
				Description: "Output as JSON for scripting",
				Command:     "bureau list --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &observe.ListResponse{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/observe/list"},
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}

			operatorSession, err := cli.LoadSession()
			if err != nil {
				return err
			}

			response, err := observe.ListTargets(params.SocketPath, observe.ListRequest{
				Observable: params.Observable,
				Observer:   operatorSession.UserID,
				Token:      operatorSession.AccessToken,
			})
			if err != nil {
				if diagnosed := cli.DiagnoseSocketError(err, params.SocketPath); diagnosed != nil {
					return diagnosed
				}
				return err
			}

			if done, err := params.EmitJSON(response); done {
				return err
			}

			return formatListOutput(response)
		},
	}
}

// formatListOutput writes a human-readable table of principals and machines.
func formatListOutput(response *observe.ListResponse) error {
	if len(response.Principals) > 0 {
		fmt.Println("PRINCIPALS")
		fmt.Printf("  %-30s %-25s %s\n", "NAME", "MACHINE", "STATUS")
		for _, principal := range response.Principals {
			status := "known"
			if principal.Local && principal.Observable {
				status = "running"
			} else if !principal.Local && principal.Observable {
				status = "remote"
			} else if !principal.Local && !principal.Observable {
				status = "unreachable"
			}
			fmt.Printf("  %-30s %-25s %s\n", principal.Localpart, principal.Machine, status)
		}
	} else {
		fmt.Println("No principals found.")
	}

	if len(response.Machines) > 0 {
		fmt.Println()
		fmt.Println("MACHINES")
		fmt.Printf("  %-30s %s\n", "NAME", "STATUS")
		for _, machine := range response.Machines {
			status := "peer"
			if machine.Self {
				status = "self"
			} else if !machine.Reachable {
				status = "unreachable"
			}
			fmt.Printf("  %-30s %s\n", machine.Name, status)
		}
	}

	return nil
}
