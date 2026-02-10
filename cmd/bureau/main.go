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
//	login         Authenticate as an operator
//	whoami        Show the current operator identity
//	list          List observable targets
//	dashboard     Open a composite observation view (machine, channel, or file)
//	observe       Attach to a single principal's terminal session
//	matrix        Matrix homeserver operations
//	template      Manage sandbox templates
//	environment   Manage fleet environment profiles
//	version       Print version information
package main

import (
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	environmentcmd "github.com/bureau-foundation/bureau/cmd/bureau/environment"
	"github.com/bureau-foundation/bureau/cmd/bureau/matrix"
	observecmd "github.com/bureau-foundation/bureau/cmd/bureau/observe"
	templatecmd "github.com/bureau-foundation/bureau/cmd/bureau/template"
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
			cli.WhoAmICommand(),
			observecmd.ObserveCommand(),
			observecmd.DashboardCommand(),
			observecmd.ListCommand(),
			matrix.Command(),
			templatecmd.Command(),
			environmentcmd.Command(),
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
				Description: "See what's running on this machine",
				Command:     "bureau list",
			},
			{
				Description: "Open the machine dashboard (all running principals)",
				Command:     "bureau dashboard",
			},
			{
				Description: "Observe a single agent's terminal",
				Command:     "bureau observe iree/amdgpu/pm",
			},
			{
				Description: "Open a project channel dashboard",
				Command:     "bureau dashboard '#iree/amdgpu/general'",
			},
			{
				Description: "List available sandbox templates",
				Command:     "bureau template list bureau/templates",
			},
			{
				Description: "Build and deploy an environment profile",
				Command:     "bureau environment build workstation --out-link deploy/buildbarn/runner-env",
			},
			{
				Description: "Bootstrap the Matrix homeserver",
				Command:     "bureau matrix setup --registration-token-file /path/to/token --credential-file ./creds",
			},
		},
	}

	return root.Execute(os.Args[1:])
}
