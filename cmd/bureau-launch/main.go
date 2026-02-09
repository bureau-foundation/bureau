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
	}

	// Parse: bureau-launch <agent-name> -- <command> [args...]
	agentName := args[0]
	command := findCommandAfterSeparator(args[1:])
	if len(command) == 0 {
		return fmt.Errorf("command required after '--'\n\nusage: bureau-launch <agent-name> -- <command> [args...]")
	}

	if err := obs.Launch(agentName, command); err != nil {
		return err
	}

	fmt.Printf("Agent %q launched in tmux session %q\n", agentName, observe.AgentSessionPrefix+agentName)
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
    bureau-launch <agent-name> -- <command> [args...]
    bureau-launch --stop <agent-name>
    bureau-launch --list

EXAMPLES
    # Launch an agent
    bureau-launch alice -- bureau-sandbox run --profile=developer --worktree=/work/alice -- claude

    # List running agents
    bureau-launch --list

    # Stop an agent
    bureau-launch --stop alice

OBSERVATION
    Agents are linked into the "bureau" tmux observation session.
    Attach with: bureau-attach (or: tmux attach -t bureau)
    Navigate agents with Ctrl-b n/p/number.
`)
}
