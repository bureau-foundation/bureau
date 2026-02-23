// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"log/slog"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

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

// serveParams holds the parameters for the MCP serve command.
// The json:"-" tags exclude these from MCP tool schemas — this
// command configures the server itself, not an agent-callable tool.
type serveParams struct {
	Progressive bool `json:"-" flag:"progressive" desc:"expose meta-tools for progressive discovery instead of all tools directly"`
}

func serveCommand(root *cli.Command) *cli.Command {
	var params serveParams

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

With --progressive, the server exposes three meta-tools
(bureau_tools_list, bureau_tools_describe, bureau_tools_call)
instead of the full tool catalog. Agents discover and invoke tools
on demand, reducing the initial tool payload from O(n) descriptions
to 3 fixed entries.

This command is intended to be launched by MCP-capable clients
(such as AI agent frameworks) as a subprocess.`,
		Usage: "bureau mcp serve [--progressive]",
		Examples: []cli.Example{
			{
				Description: "Start MCP server with all tools exposed directly",
				Command:     "bureau mcp serve",
			},
			{
				Description: "Start MCP server with progressive discovery meta-tools",
				Command:     "bureau mcp serve --progressive",
			},
		},
		Params: func() any { return &params },
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			grants, err := fetchGrants()
			if err != nil {
				return err
			}
			var options []ServerOption
			if params.Progressive {
				options = append(options, WithProgressiveDisclosure())
			}
			server := NewServer(root, grants, options...)
			return server.Serve()
		},
	}
}
