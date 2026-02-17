// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/commands"
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
// [commands.Root] so the tree definition lives in one place, shared
// by both the bureau CLI and the bureau-agent binary.
func rootCommand() *cli.Command {
	return commands.Root()
}

func run() error {
	return rootCommand().Execute(os.Args[1:])
}
