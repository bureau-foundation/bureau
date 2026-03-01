// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

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

func run() error {
	return commands.Root().Execute(os.Args[1:])
}
