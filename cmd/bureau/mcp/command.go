// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import "github.com/bureau-foundation/bureau/cmd/bureau/cli"

// Command returns the "mcp" command group. The root parameter is the
// top-level CLI command tree, used for tool discovery when the "serve"
// subcommand starts.
func Command(root *cli.Command) *cli.Command {
	return &cli.Command{
		Name:    "mcp",
		Summary: "Model Context Protocol server for agent tool access",
		Description: `MCP server that exposes Bureau CLI commands as tools over
newline-delimited JSON-RPC 2.0 on stdin/stdout.

Agents running inside sandboxes use this to interact with Bureau
operations via structured tool calls. The server discovers tools
from the CLI command tree and generates JSON Schema descriptions
from parameter struct tags.`,
		Subcommands: []*cli.Command{
			serveCommand(root),
		},
	}
}

func serveCommand(root *cli.Command) *cli.Command {
	return &cli.Command{
		Name:    "serve",
		Summary: "Start MCP server on stdin/stdout",
		Description: `Start a Model Context Protocol server that reads JSON-RPC 2.0
requests from stdin and writes responses to stdout.

The server discovers all CLI commands with typed parameter structs
and exposes them as MCP tools. Tool names are underscore-joined
command paths (e.g., bureau_pipeline_list).

Tools are filtered by the principal's authorization grants, obtained
from the proxy socket. Only commands whose RequiredGrants are all
satisfied by the principal's grants appear in the tool list.

This command is intended to be launched by MCP-capable clients
(such as AI agent frameworks) as a subprocess.`,
		Usage: "bureau mcp serve",
		Examples: []cli.Example{
			{
				Description: "Start MCP server (typically launched by an agent framework)",
				Command:     "bureau mcp serve",
			},
		},
		Run: func(args []string) error {
			grants, err := fetchGrants()
			if err != nil {
				return err
			}
			server := NewServer(root, grants)
			return server.Serve()
		},
	}
}
