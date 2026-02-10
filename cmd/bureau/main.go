// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau is the unified CLI for interacting with a Bureau deployment.
// It provides subcommands for observing agent sessions, viewing composite
// dashboards, and listing observable targets.
//
// Usage:
//
//	bureau <subcommand> [flags]
//
// Subcommands:
//
//	observe     Attach to a principal's terminal session
//	dashboard   Open a composite workstream view
//	list        List observable targets
//	version     Print version information
package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/observe"
	"golang.org/x/term"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		printUsage()
		return fmt.Errorf("subcommand required")
	}

	subcommand := os.Args[1]
	switch subcommand {
	case "observe":
		return runObserve(os.Args[2:])
	case "dashboard":
		return runDashboard(os.Args[2:])
	case "list":
		return runList(os.Args[2:])
	case "version":
		fmt.Printf("bureau %s\n", version.Full())
		return nil
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown subcommand: %q", subcommand)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: bureau <subcommand> [flags]

Subcommands:
  observe <target>        Attach to a principal's terminal session
  dashboard [layout]      Open a composite workstream view
  list [--observable]     List observable targets
  version                 Print version information

Targets:
  @iree/amdgpu/pm            Principal — direct terminal access
  #iree/amdgpu/general       Channel — composite workstream view
  @machine/workstation        Machine — sysadmin dashboard

Run 'bureau <subcommand> --help' for subcommand flags.
`)
}

// runObserve connects to a principal's tmux session via the daemon's
// observation relay. The target argument is a principal localpart
// (e.g., "iree/amdgpu/pm" or "@iree/amdgpu/pm").
func runObserve(args []string) error {
	target := ""
	readonly := false
	socketPath := observe.DefaultDaemonSocket
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--readonly":
			readonly = true
		case "--socket":
			if i+1 >= len(args) {
				return fmt.Errorf("--socket requires an argument")
			}
			i++
			socketPath = args[i]
		case "-h", "--help":
			printObserveUsage()
			return nil
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag: %s", args[i])
			}
			if target != "" {
				return fmt.Errorf("unexpected argument: %s", args[i])
			}
			target = args[i]
		}
	}
	if target == "" {
		printObserveUsage()
		return fmt.Errorf("target argument required")
	}

	// Strip leading "@" if present — the protocol uses bare localparts.
	target = strings.TrimPrefix(target, "@")

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

	// Put the terminal in raw mode so keystrokes pass through immediately
	// rather than being line-buffered. Restore on exit.
	stdinFd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return fmt.Errorf("set terminal raw mode: %w", err)
	}
	defer term.Restore(stdinFd, oldState)

	// Handle SIGINT and SIGTERM: restore terminal state and close the
	// session cleanly. Without this, a Ctrl-C would leave the terminal
	// in raw mode (no echo, no line editing).
	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signalChannel
		term.Restore(stdinFd, oldState)
		session.Close()
		os.Exit(0)
	}()

	return session.Run(os.Stdin, os.Stdout)
}

func printObserveUsage() {
	fmt.Fprintf(os.Stderr, `Usage: bureau observe <target> [flags]

Attach to a principal's terminal session via the observation relay.

Arguments:
  target    Principal localpart or Matrix ID (e.g., iree/amdgpu/pm)

Flags:
  --readonly    Observe without input (read-only mode)
  --socket      Daemon observation socket (default: %s)
`, observe.DefaultDaemonSocket)
}

// runDashboard creates a local tmux session with a composite layout
// from a channel's layout state event. Each pane in the layout runs
// a bureau-observe process connected to the relevant remote principal.
//
// This is a scaffold: the real implementation will be wired in when
// the observe dashboard library (observe.Dashboard) is ready.
func runDashboard(args []string) error {
	for _, arg := range args {
		switch arg {
		case "-h", "--help":
			fmt.Fprintf(os.Stderr, `Usage: bureau dashboard [channel] [flags]

Open a composite workstream view. If no channel is specified, shows
a dashboard of all channels the user is a member of.

Arguments:
  channel   Room alias or ID (e.g., #iree/amdgpu/general)

Flags:
  --socket    Daemon observation socket (default: /run/bureau/observe.sock)
`)
			return nil
		}
	}

	channel := ""
	socketPath := "/run/bureau/observe.sock"
	remaining := args
	for len(remaining) > 0 {
		arg := remaining[0]
		remaining = remaining[1:]
		switch arg {
		case "--socket":
			if len(remaining) == 0 {
				return fmt.Errorf("--socket requires an argument")
			}
			socketPath = remaining[0]
			remaining = remaining[1:]
		default:
			if channel != "" {
				return fmt.Errorf("unexpected argument: %s", arg)
			}
			channel = arg
		}
	}

	// Scaffold: print what would happen and exit.
	if channel == "" {
		fmt.Fprintf(os.Stderr, "bureau dashboard: opening default dashboard (socket=%s)\n", socketPath)
	} else {
		fmt.Fprintf(os.Stderr, "bureau dashboard: opening %q (socket=%s)\n", channel, socketPath)
	}
	return fmt.Errorf("dashboard not yet implemented (waiting for observe dashboard library)")
}

// runList queries the daemon for observable targets: active principals,
// rooms with layout state events, and online machines. Output is grouped
// by machine or namespace.
//
// This is a scaffold: the real implementation will be wired in when
// the observe client library provides target listing.
func runList(args []string) error {
	observable := false
	socketPath := "/run/bureau/observe.sock"

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--observable":
			observable = true
		case "--socket":
			if i+1 >= len(args) {
				return fmt.Errorf("--socket requires an argument")
			}
			i++
			socketPath = args[i]
		case "-h", "--help":
			fmt.Fprintf(os.Stderr, `Usage: bureau list [flags]

List principals, channels, and machines.

Flags:
  --observable    Show only targets that can be observed
  --socket        Daemon observation socket (default: /run/bureau/observe.sock)
`)
			return nil
		default:
			return fmt.Errorf("unknown flag: %s", args[i])
		}
	}

	// Scaffold: print what would happen and exit.
	if observable {
		fmt.Fprintf(os.Stderr, "bureau list: listing observable targets (socket=%s)\n", socketPath)
	} else {
		fmt.Fprintf(os.Stderr, "bureau list: listing all targets (socket=%s)\n", socketPath)
	}
	return fmt.Errorf("list not yet implemented (waiting for observe client library)")
}
