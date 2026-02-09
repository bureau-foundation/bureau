// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// bureau-attach attaches to the bureau observation tmux session.
//
// This is a thin wrapper around "tmux attach -t bureau" that also
// handles the case where no agents are running.
//
// Usage:
//
//	bureau-attach
package main

import (
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/observe"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "version":
			fmt.Printf("bureau-attach %s\n", version.Info())
			return
		case "--help", "help", "-h":
			fmt.Print(`bureau-attach - Attach to the bureau observation session

USAGE
    bureau-attach

Navigate between agents with Ctrl-b n/p/number.
Detach with Ctrl-b d.
`)
			return
		}
	}

	obs := observe.New()
	if err := obs.Attach(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
