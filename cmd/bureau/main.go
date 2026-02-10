// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau is the unified CLI for interacting with a Bureau deployment.
// It provides subcommands for observing agent sessions, managing the
// Matrix homeserver, and administering the deployment.
//
// Usage:
//
//	bureau <command> [flags]
//
// Commands:
//
//	observe     Attach to a principal's terminal session
//	dashboard   Open a composite workstream view
//	list        List observable targets
//	matrix      Matrix homeserver operations
//	version     Print version information
package main

import (
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/matrix"
	observecmd "github.com/bureau-foundation/bureau/cmd/bureau/observe"
	"github.com/bureau-foundation/bureau/lib/version"
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
	root := &cli.Command{
		Name: "bureau",
		Description: `Bureau: AI agent orchestration system.

Manage sandboxed agent processes with credential isolation, live
observation, and structured messaging via Matrix.`,
		Subcommands: []*cli.Command{
			cli.LoginCommand(),
			observecmd.ObserveCommand(),
			observecmd.DashboardCommand(),
			observecmd.ListCommand(),
			matrix.Command(),
			{
				Name:    "version",
				Summary: "Print version information",
				Run: func(args []string) error {
					fmt.Printf("bureau %s\n", version.Full())
					return nil
				},
			},
		},
		Examples: []cli.Example{
			{
				Description: "Authenticate as an operator (saves session locally)",
				Command:     "bureau login ben",
			},
			{
				Description: "Observe an agent's terminal",
				Command:     "bureau observe iree/amdgpu/pm",
			},
			{
				Description: "Bootstrap the Matrix homeserver",
				Command:     "bureau matrix setup --registration-token-file /path/to/token --credential-file ./creds",
			},
			{
				Description: "Open a project dashboard",
				Command:     "bureau dashboard #iree/amdgpu/general",
			},
		},
	}

	return root.Execute(os.Args[1:])
}
