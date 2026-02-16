// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	artifactcmd "github.com/bureau-foundation/bureau/cmd/bureau/artifact"
	cborcmd "github.com/bureau-foundation/bureau/cmd/bureau/cbor"
	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	environmentcmd "github.com/bureau-foundation/bureau/cmd/bureau/environment"
	machinecmd "github.com/bureau-foundation/bureau/cmd/bureau/machine"
	"github.com/bureau-foundation/bureau/cmd/bureau/matrix"
	mcpcmd "github.com/bureau-foundation/bureau/cmd/bureau/mcp"
	observecmd "github.com/bureau-foundation/bureau/cmd/bureau/observe"
	pipelinecmd "github.com/bureau-foundation/bureau/cmd/bureau/pipeline"
	quickstartcmd "github.com/bureau-foundation/bureau/cmd/bureau/quickstart"
	templatecmd "github.com/bureau-foundation/bureau/cmd/bureau/template"
	ticketcmd "github.com/bureau-foundation/bureau/cmd/bureau/ticket"
	workspacecmd "github.com/bureau-foundation/bureau/cmd/bureau/workspace"
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

// rootCommand builds the complete CLI command tree. Extracted from run()
// so that main_test.go can walk the tree for lint checks without
// invoking Execute.
func rootCommand() *cli.Command {
	root := &cli.Command{
		Name: "bureau",
		Description: `Bureau: AI agent orchestration system.

Manage sandboxed agent processes with credential isolation, live
observation, and structured messaging via Matrix.`,
		Subcommands: []*cli.Command{
			quickstartcmd.Command(),
			cli.LoginCommand(),
			cli.WhoAmICommand(),
			observecmd.ObserveCommand(),
			observecmd.DashboardCommand(),
			observecmd.ListCommand(),
			matrix.Command(),
			machinecmd.Command(),
			templatecmd.Command(),
			pipelinecmd.Command(),
			environmentcmd.Command(),
			workspacecmd.Command(),
			ticketcmd.Command(),
			artifactcmd.Command(),
			cborcmd.Command(),
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
				Command:     "bureau template list bureau/template",
			},
			{
				Description: "Build and deploy an environment profile",
				Command:     "bureau environment build workstation --out-link deploy/buildbarn/runner-env",
			},
			{
				Description: "Create a workspace for a project",
				Command:     "bureau workspace create iree/amdgpu/inference --template dev-workspace",
			},
			{
				Description: "Bootstrap the Matrix homeserver",
				Command:     "bureau matrix setup --registration-token-file /path/to/token --credential-file ./creds",
			},
		},
	}

	// Add MCP command with access to the full command tree. Tool
	// discovery walks root.Subcommands, so this must be added after
	// the tree is constructed.
	root.Subcommands = append(root.Subcommands, mcpcmd.Command(root))

	return root
}

func run() error {
	return rootCommand().Execute(os.Args[1:])
}
