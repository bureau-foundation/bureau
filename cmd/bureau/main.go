// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/commands"
	viewercmd "github.com/bureau-foundation/bureau/cmd/bureau/ticket/viewer"
)

func main() {
	if err := run(); err != nil {
		// Commands that print their own output (like doctor) return an
		// exitError with the desired exit code. Don't print a redundant
		// "error:" line for those.
		if coder, ok := err.(interface{ ExitCode() int }); ok {
			os.Exit(coder.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// rootCommand returns the complete CLI command tree. Delegates to
// [commands.Root] for the shared tree (used by both bureau CLI and
// bureau-agent), then adds interactive commands that require a
// terminal. These are kept out of commands.Root() so bureau-agent
// avoids linking TUI dependencies it can never use.
func rootCommand() *cli.Command {
	root := commands.Root()
	addInteractiveCommands(root)
	return root
}

// addInteractiveCommands appends CLI-only commands that depend on TUI
// libraries (charmbracelet/bubbletea, lib/ticketui). These commands
// require a terminal and cannot run over MCP, so they are excluded
// from the shared command tree in [commands.Root].
func addInteractiveCommands(root *cli.Command) {
	for _, sub := range root.Subcommands {
		if sub.Name == "ticket" {
			sub.Subcommands = append(sub.Subcommands, viewercmd.Command())
			return
		}
	}
}

func run() error {
	return rootCommand().Execute(os.Args[1:])
}
