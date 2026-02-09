// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// bureau-launch starts an agent in a sandboxed tmux session and links it
// into the bureau observation session.
//
// Usage:
//
//	bureau-launch <agent-name> -- <command> [args...]
//	bureau-launch --stop <agent-name>
//	bureau-launch --list
package main

import (
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/observe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]

	if len(args) == 0 {
		printUsage()
		return fmt.Errorf("no arguments provided")
	}

	obs := observe.New()
	var configPath string

	// Extract flags that precede the agent name.
	for len(args) > 0 {
		switch args[0] {
		case "--version", "version":
			fmt.Printf("bureau-launch %s\n", version.Info())
			return nil
		case "--help", "help", "-h":
			printUsage()
			return nil
		case "--list", "list":
			return listCmd(obs)
		case "--stop", "stop":
			if len(args) < 2 {
				return fmt.Errorf("--stop requires an agent name")
			}
			return obs.Stop(args[1])
		case "--config":
			if len(args) < 2 {
				return fmt.Errorf("--config requires a path to a layout YAML file")
			}
			configPath = args[1]
			args = args[2:]
			continue
		case "--log-dir":
			if len(args) < 2 {
				return fmt.Errorf("--log-dir requires a directory path")
			}
			obs.LogDir = args[1]
			args = args[2:]
			continue
		case "--backscroll":
			if len(args) < 2 {
				return fmt.Errorf("--backscroll requires a line count")
			}
			lines, err := fmt.Sscanf(args[1], "%d", &obs.BackscrollLines)
			if err != nil || lines != 1 {
				return fmt.Errorf("--backscroll requires a numeric line count, got %q", args[1])
			}
			args = args[2:]
			continue
		}
		break
	}

	if len(args) == 0 {
		return fmt.Errorf("agent name is required\n\nusage: bureau-launch [options] <agent-name> -- <command> [args...]")
	}

	// Parse: [options] <agent-name> -- <command> [args...]
	agentName := args[0]
	command := findCommandAfterSeparator(args[1:])
	if len(command) == 0 {
		return fmt.Errorf("command required after '--'\n\nusage: bureau-launch [options] <agent-name> -- <command> [args...]")
	}

	// Launch with layout config if provided, otherwise single-pane.
	if configPath != "" {
		layout, err := observe.LoadLayoutConfig(configPath)
		if err != nil {
			return err
		}
		if err := obs.LaunchWithLayout(agentName, command, layout); err != nil {
			return err
		}
	} else {
		if err := obs.Launch(agentName, command); err != nil {
			return err
		}
	}

	fmt.Printf("Agent %q launched in tmux session %q\n", agentName, observe.AgentSessionPrefix+agentName)
	if obs.LogDir != "" {
		fmt.Printf("Logs: %s/%s/\n", obs.LogDir, agentName)
	}
	fmt.Printf("Attach with: bureau-attach\n")
	return nil
}

func listCmd(obs *observe.Observer) error {
	agents, err := obs.ListAgents()
	if err != nil {
		return err
	}

	if len(agents) == 0 {
		fmt.Println("No agents running.")
		return nil
	}

	fmt.Println("Agents:")
	for _, agent := range agents {
		status := "running"
		if !agent.Running {
			status = "exited"
			if agent.ExitStatus != "" {
				status = fmt.Sprintf("exited (%s)", agent.ExitStatus)
			}
		}
		fmt.Printf("  %-20s %s\n", agent.Name, status)
	}
	return nil
}

// findCommandAfterSeparator finds the command after "--" in args.
func findCommandAfterSeparator(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return args[i+1:]
		}
	}
	return nil
}

func printUsage() {
	fmt.Print(`bureau-launch - Start an agent in a sandboxed tmux session

USAGE
    bureau-launch [options] <agent-name> -- <command> [args...]
    bureau-launch --stop <agent-name>
    bureau-launch --list

OPTIONS
    --config <path>      Layout config YAML (multi-pane, see below)
    --log-dir <path>     Enable pane output logging to this directory
    --backscroll <lines> Lines of log to replay on resume (default 200)

EXAMPLES
    # Launch a single-pane agent
    bureau-launch alice -- bash

    # Launch with logging (pane output saved, replayed on restart)
    bureau-launch --log-dir /var/log/bureau alice -- bash

    # Launch with a multi-pane layout
    bureau-launch --config layout.yaml --log-dir /var/log/bureau alice -- bash

    # List running agents
    bureau-launch --list

    # Stop an agent
    bureau-launch --stop alice

LAYOUT CONFIG
    A YAML file describing the pane arrangement. Example:

        layout: main-vertical
        panes:
          - name: agent
            # empty command = agent's main process
          - name: logs
            command: ["tail", "-f", "/var/log/bureau/agent.log"]
          - name: status
            command: ["watch", "-n5", "bureau-status"]

    Layouts: main-vertical, main-horizontal, even-horizontal,
    even-vertical, tiled.

OBSERVATION
    Agents are linked into the "bureau" tmux observation session.
    Attach with: bureau-attach (or: tmux attach -t bureau)
    Navigate agents with Ctrl-b n/p/number.

LOGGING
    When --log-dir is set, all pane output is continuously captured
    to <log-dir>/<agent>/<pane-name>.log via tmux pipe-pane. On
    subsequent launches, the last --backscroll lines from the log
    are replayed into the pane's scroll buffer, so you see recent
    context when you attach — as if the session never restarted.
`)
}
