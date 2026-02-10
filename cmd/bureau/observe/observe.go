// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package observe implements the bureau observe, dashboard, and list
// subcommands. These connect to the daemon's observation relay for
// live terminal access, composite dashboards, and target listing.
package observe

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
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

The target is a principal localpart (e.g., "iree/amdgpu/pm") or Matrix
ID (e.g., "@iree/amdgpu/pm:bureau.local"). The leading "@" is stripped
automatically.

In read-only mode, keystrokes are not forwarded to the remote session.
Use this for monitoring without risk of accidental input.`,
		Usage: "bureau observe <target> [flags]",
		Examples: []cli.Example{
			{
				Description: "Observe an agent's terminal",
				Command:     "bureau observe iree/amdgpu/pm",
			},
			{
				Description: "Read-only observation",
				Command:     "bureau observe iree/amdgpu/pm --readonly",
			},
			{
				Description: "Use a custom daemon socket",
				Command:     "bureau observe iree/amdgpu/pm --socket /tmp/test.sock",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("observe", pflag.ContinueOnError)
			flagSet.BoolVar(&readonly, "readonly", false, "observe without input (read-only mode)")
			flagSet.StringVar(&socketPath, "socket", observe.DefaultDaemonSocket, "daemon observation socket path")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("target argument required\n\nUsage: bureau observe <target> [flags]")
			}
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}
			target := strings.TrimPrefix(args[0], "@")

			mode := "readwrite"
			if readonly {
				mode = "readonly"
			}

			session, err := observe.Connect(socketPath, observe.ObserveRequest{
				Principal: target,
				Mode:      mode,
			})
			if err != nil {
				return fmt.Errorf("connect to %s: %w", target, err)
			}
			defer session.Close()

			stdinFd := int(os.Stdin.Fd())
			oldState, err := term.MakeRaw(stdinFd)
			if err != nil {
				return fmt.Errorf("set terminal raw mode: %w", err)
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

Layout sources (exactly one required):
  --layout-file    Read layout from a local JSON file
  <channel>        Fetch layout from a channel's Matrix state via the daemon

Each "observe" pane in the layout connects to the daemon's observation
relay for the target principal. "command" panes run local commands.
"role" panes display their role identity.`,
		Usage: "bureau dashboard [channel] [flags]",
		Examples: []cli.Example{
			{
				Description: "Open a dashboard from a layout file",
				Command:     "bureau dashboard --layout-file ./workspace.json",
			},
			{
				Description: "Open a project channel dashboard",
				Command:     "bureau dashboard '#iree/amdgpu/general'",
			},
			{
				Description: "Create dashboard without attaching",
				Command:     "bureau dashboard --layout-file ./workspace.json --detach",
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
		Run: func(args []string) error {
			channel := ""
			if len(args) > 0 {
				channel = args[0]
			}
			if len(args) > 1 {
				return fmt.Errorf("unexpected argument: %s", args[1])
			}

			// --- Resolve layout from the specified source ---

			var layout *observe.Layout
			var sessionName string

			switch {
			case layoutFile != "":
				var err error
				layout, err = loadLayoutFile(layoutFile)
				if err != nil {
					return fmt.Errorf("load layout file: %w", err)
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
				layout, err = observe.QueryLayout(socketPath, channel)
				if err != nil {
					return fmt.Errorf("query channel layout: %w", err)
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
				return fmt.Errorf("layout source required: use --layout-file or specify a channel\n\nUsage: bureau dashboard [channel] [flags]")
			}

			// --- Create the dashboard tmux session ---

			// Dashboard sessions live in the user's own tmux server
			// (not Bureau's managed tmux), so they show up in the
			// user's normal `tmux list-sessions`.
			if tmuxSocket == "" {
				tmuxSocket = defaultTmuxSocket()
			}

			tmuxArgs := []string{"-S", tmuxSocket}
			serverSocket := tmuxSocket

			if err := observe.Dashboard(serverSocket, sessionName, socketPath, layout); err != nil {
				return fmt.Errorf("create dashboard: %w", err)
			}

			fmt.Fprintf(os.Stderr, "dashboard session created: %s\n", sessionName)

			if detach {
				return nil
			}

			// Attach to the session via exec (replaces this process).
			tmuxBinary, err := exec.LookPath("tmux")
			if err != nil {
				return fmt.Errorf("tmux not found in PATH: %w", err)
			}

			attachArgs := append(tmuxArgs, "attach-session", "-t", sessionName)
			// syscall.Exec replaces the process — the dashboard CLI
			// becomes the tmux client. This is the standard pattern
			// for "open a tmux session" commands.
			return syscall.Exec(tmuxBinary, append([]string{"tmux"}, attachArgs...), os.Environ())
		},
	}
}

// loadLayoutFile reads a layout from a JSON file. The file format matches
// the schema.LayoutContent wire format (the same JSON that appears in
// m.bureau.layout state events).
func loadLayoutFile(path string) (*observe.Layout, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var content schema.LayoutContent
	if err := json.Unmarshal(data, &content); err != nil {
		return nil, fmt.Errorf("parsing layout JSON: %w", err)
	}

	if len(content.Windows) == 0 {
		return nil, fmt.Errorf("layout has no windows")
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

// ListCommand returns the "list" subcommand for querying observable targets.
func ListCommand() *cli.Command {
	var observable bool
	var socketPath string
	var outputJSON bool

	return &cli.Command{
		Name:    "list",
		Summary: "List observable targets",
		Description: `List principals and machines known to the daemon.

With --observable, only targets that can currently be observed (have
an active tmux session or are reachable via transport) are shown.

Principals come from two sources: locally running sandboxes and the
service directory (which includes remote services). Machines include
the local machine and all known peers.`,
		Usage: "bureau list [flags]",
		Examples: []cli.Example{
			{
				Description: "List all known targets",
				Command:     "bureau list",
			},
			{
				Description: "List only observable targets",
				Command:     "bureau list --observable",
			},
			{
				Description: "Output as JSON for scripting",
				Command:     "bureau list --json",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("list", pflag.ContinueOnError)
			flagSet.BoolVar(&observable, "observable", false, "show only targets that can be observed")
			flagSet.BoolVar(&outputJSON, "json", false, "output as JSON")
			flagSet.StringVar(&socketPath, "socket", observe.DefaultDaemonSocket, "daemon observation socket path")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}

			response, err := observe.ListTargets(socketPath, observable)
			if err != nil {
				return err
			}

			if outputJSON {
				encoder := json.NewEncoder(os.Stdout)
				encoder.SetIndent("", "  ")
				return encoder.Encode(response)
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
