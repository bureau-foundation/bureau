// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// bureau-agent is the Bureau-native agent: a Go binary that runs the
// LLM agent loop in-process, executing Bureau CLI commands as tools
// without subprocess overhead. It implements lib/agent.Driver and
// delegates lifecycle management to lib/agent.Run.
//
// The agent discovers tools from the same CLI command tree as the MCP
// server, filtered by the principal's authorization grants. Tool
// execution reuses the MCP server's in-process dispatch (parameter
// zeroing, JSON overlay, stdout capture), so all agents — native,
// Claude Code, Codex — share the same tool execution path.
//
// Configuration comes from Bureau environment variables:
//   - BUREAU_PROXY_SOCKET: proxy Unix socket path
//   - BUREAU_MACHINE_NAME: machine localpart
//   - BUREAU_SERVER_NAME: Matrix server name
//   - BUREAU_AGENT_PROVIDER: LLM provider backend (default: anthropic, also: openai)
//   - BUREAU_AGENT_MODEL: LLM model identifier (default: claude-sonnet-4-5-20250929)
//   - BUREAU_AGENT_SERVICE: proxy service name for LLM API (default: anthropic)
//   - BUREAU_AGENT_MAX_TOKENS: max output tokens (default: 8192)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/bureau-foundation/bureau/lib/agent"
)

func main() {
	config := agent.RunConfigFromEnvironment()
	config.SessionLogPath = "/run/bureau/session.jsonl"
	config.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	driver := &nativeDriver{
		proxySocketPath: config.ProxySocketPath,
		serverName:      config.ServerName,
		logger:          config.Logger,
	}

	if err := agent.Run(context.Background(), driver, config); err != nil {
		fmt.Fprintf(os.Stderr, "bureau-agent: %v\n", err)
		os.Exit(1)
	}
}
