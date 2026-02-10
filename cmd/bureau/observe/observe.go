// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package observe implements the bureau observe, dashboard, and list
// subcommands. These connect to the daemon's observation relay for
// live terminal access, composite dashboards, and target listing.
package observe

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
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
		Flags: func() *flag.FlagSet {
			flagSet := flag.NewFlagSet("observe", flag.ContinueOnError)
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
	var socketPath string

	return &cli.Command{
		Name:    "dashboard",
		Summary: "Open a composite workstream view",
		Description: `Open a composite workstream view from a channel's layout state event.

Each pane in the layout runs an observation session connected to the
relevant remote principal. If no channel is specified, shows a dashboard
of all channels the user is a member of.`,
		Usage: "bureau dashboard [channel] [flags]",
		Examples: []cli.Example{
			{
				Description: "Open a project dashboard",
				Command:     "bureau dashboard #iree/amdgpu/general",
			},
		},
		Flags: func() *flag.FlagSet {
			flagSet := flag.NewFlagSet("dashboard", flag.ContinueOnError)
			flagSet.StringVar(&socketPath, "socket", "/run/bureau/observe.sock", "daemon observation socket path")
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

			if channel == "" {
				fmt.Fprintf(os.Stderr, "bureau dashboard: opening default dashboard (socket=%s)\n", socketPath)
			} else {
				fmt.Fprintf(os.Stderr, "bureau dashboard: opening %q (socket=%s)\n", channel, socketPath)
			}
			return fmt.Errorf("dashboard not yet wired (waiting for observe dashboard library)")
		},
	}
}

// ListCommand returns the "list" subcommand for querying observable targets.
func ListCommand() *cli.Command {
	var observable bool
	var socketPath string

	return &cli.Command{
		Name:    "list",
		Summary: "List observable targets",
		Description: `List principals, channels, and machines known to the daemon.

With --observable, only targets that can currently be observed (have
an active tmux session) are shown. Output is grouped by machine or
namespace.`,
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
		},
		Flags: func() *flag.FlagSet {
			flagSet := flag.NewFlagSet("list", flag.ContinueOnError)
			flagSet.BoolVar(&observable, "observable", false, "show only targets that can be observed")
			flagSet.StringVar(&socketPath, "socket", "/run/bureau/observe.sock", "daemon observation socket path")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}

			if observable {
				fmt.Fprintf(os.Stderr, "bureau list: listing observable targets (socket=%s)\n", socketPath)
			} else {
				fmt.Fprintf(os.Stderr, "bureau list: listing all targets (socket=%s)\n", socketPath)
			}
			return fmt.Errorf("list not yet wired (waiting for observe client library)")
		},
	}
}
