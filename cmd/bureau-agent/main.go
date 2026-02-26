// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// bureau-agent is the Bureau-native agent: a Go binary that runs the
// LLM agent loop in-process, executing Bureau CLI commands as tools
// without subprocess overhead. It implements lib/agentdriver.Driver and
// delegates lifecycle management to lib/agentdriver.Run.
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

	"github.com/bureau-foundation/bureau/lib/agentdriver"
	"github.com/bureau-foundation/bureau/lib/process"
	"github.com/bureau-foundation/bureau/lib/ref"
)

func main() {
	if err := run(); err != nil {
		process.Fatal(err)
	}
}

func run() error {
	config := agentdriver.RunConfigFromEnvironment()
	config.SessionLogPath = "/run/bureau/session.jsonl"
	config.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	slog.SetDefault(config.Logger)

	serverName, err := ref.ParseServerName(config.ServerName)
	if err != nil {
		return fmt.Errorf("invalid server name %q: %w", config.ServerName, err)
	}

	driver := &nativeDriver{
		proxySocketPath: config.ProxySocketPath,
		serverName:      serverName,
		logger:          config.Logger,
	}

	return agentdriver.Run(context.Background(), driver, config)
}
