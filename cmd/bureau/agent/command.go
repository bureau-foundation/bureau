// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// Command returns the "agent" parent command with all subcommands.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "agent",
		Summary: "Create, inspect, and manage agent principals",
		Description: `Manage agent principals across the Bureau fleet.

Agents are principals running inside Bureau sandboxes, typically AI agent
wrappers (Claude Code, Codex, etc.) that integrate with Matrix messaging,
credential injection, and the agent service for session/metrics tracking.

The "create" command performs the full deployment sequence: register a
Matrix account, provision encrypted credentials, and assign the agent
to a machine. The daemon detects the assignment and creates the sandbox.

Inspection commands (list, show, session, metrics, context) read Matrix
state events from machine config rooms. When --machine is not specified,
they scan all machines to find the agent automatically.`,
		Subcommands: []*cli.Command{
			createCommand(),
			listCommand(),
			showCommand(),
			destroyCommand(),
			sessionCommand(),
			metricsCommand(),
			contextCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Create an agent on a specific machine",
				Command:     "bureau agent create bureau/template:claude-dev --machine machine/workstation --name agent/code-review --credential-file ./creds",
			},
			{
				Description: "List all agents across all machines",
				Command:     "bureau agent list --credential-file ./creds",
			},
			{
				Description: "Show agent details (auto-discovers machine)",
				Command:     "bureau agent show agent/code-review --credential-file ./creds",
			},
			{
				Description: "Remove an agent assignment",
				Command:     "bureau agent destroy agent/code-review --credential-file ./creds",
			},
		},
	}
}
