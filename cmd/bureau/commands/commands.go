// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package commands builds the complete Bureau CLI command tree. Both
// the bureau CLI binary and the bureau-agent binary import this
// package to share a single source of truth for tool discovery.
//
// This creates a cross-binary dependency: bureau-agent's driver
// imports this package (and transitively all subcommand packages)
// to construct the MCP tool server. This is intentional — the agent
// executes the same CLI commands as tools. The agent's core loop
// logic depends on the [toolserver.Server] interface in lib/, not
// on the concrete MCP types; only the driver layer imports from cmd/.
package commands

import (
	"context"
	"fmt"
	"log/slog"

	agentcmd "github.com/bureau-foundation/bureau/cmd/bureau/agent"
	artifactcmd "github.com/bureau-foundation/bureau/cmd/bureau/artifact"
	authcmd "github.com/bureau-foundation/bureau/cmd/bureau/auth"
	cborcmd "github.com/bureau-foundation/bureau/cmd/bureau/cbor"
	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	credentialcmd "github.com/bureau-foundation/bureau/cmd/bureau/credential"
	doctorcmd "github.com/bureau-foundation/bureau/cmd/bureau/doctor"
	environmentcmd "github.com/bureau-foundation/bureau/cmd/bureau/environment"
	fleetcmd "github.com/bureau-foundation/bureau/cmd/bureau/fleet"
	machinecmd "github.com/bureau-foundation/bureau/cmd/bureau/machine"
	"github.com/bureau-foundation/bureau/cmd/bureau/matrix"
	mcpcmd "github.com/bureau-foundation/bureau/cmd/bureau/mcp"
	observecmd "github.com/bureau-foundation/bureau/cmd/bureau/observe"
	pipelinecmd "github.com/bureau-foundation/bureau/cmd/bureau/pipeline"
	servicecmd "github.com/bureau-foundation/bureau/cmd/bureau/service"
	suggestcmd "github.com/bureau-foundation/bureau/cmd/bureau/suggest"
	templatecmd "github.com/bureau-foundation/bureau/cmd/bureau/template"
	ticketcmd "github.com/bureau-foundation/bureau/cmd/bureau/ticket"
	workspacecmd "github.com/bureau-foundation/bureau/cmd/bureau/workspace"
	"github.com/bureau-foundation/bureau/lib/version"
)

// Root builds and returns the complete Bureau CLI command tree.
// Tool discovery walks root.Subcommands, so the MCP command is
// added last (after the tree is constructed) and receives the
// root pointer for introspection.
func Root() *cli.Command {
	root := &cli.Command{
		Name: "bureau",
		Description: `Bureau: AI agent orchestration system.

Manage sandboxed agent processes with credential isolation, live
observation, and structured messaging via Matrix.`,
		Subcommands: []*cli.Command{
			cli.LoginCommand(),
			cli.WhoAmICommand(),
			doctorcmd.Command(),
			observecmd.ObserveCommand(),
			observecmd.DashboardCommand(),
			observecmd.ListCommand(),
			matrix.Command(),
			machinecmd.Command(),
			agentcmd.Command(),
			servicecmd.Command(),
			authcmd.Command(),
			credentialcmd.Command(),
			templatecmd.Command(),
			pipelinecmd.Command(),
			environmentcmd.Command(),
			workspacecmd.Command(),
			ticketcmd.Command(),
			artifactcmd.Command(),
			fleetcmd.Command(),
			cborcmd.Command(),
			{
				Name:    "version",
				Summary: "Print version information",
				Run: func(_ context.Context, args []string, _ *slog.Logger) error {
					fmt.Printf("bureau %s\n", version.Full())
					return nil
				},
			},
		},
		Examples: []cli.Example{
			{
				Description: "Diagnose the operator environment (start here when lost)",
				Command:     "bureau doctor",
			},
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

	// Add commands that need access to the full command tree. These
	// must be added after the tree is constructed because they walk
	// root.Subcommands for tool discovery or search indexing.
	root.Subcommands = append(root.Subcommands,
		mcpcmd.Command(root),
		suggestcmd.Command(root),
	)

	return root
}
