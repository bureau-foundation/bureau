// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// bureau-agent-claude runs Claude Code inside a Bureau sandbox. It
// implements the agentdriver.Driver interface for Claude Code's stream-json
// output format, manages the process lifecycle, and integrates with
// Bureau's proxy, messaging, and session logging infrastructure.
//
// The binary also serves as its own hook handler: when invoked as
// "bureau-agent-claude hook <event-type>", it handles Claude Code hook
// events (PreToolUse, PostToolUse, Stop) instead of running the agent
// driver. This dual-mode design avoids shipping a separate hook binary
// into sandboxes.
//
// The binary reads its configuration from Bureau environment variables
// (BUREAU_PROXY_SOCKET, BUREAU_MACHINE_NAME, BUREAU_SERVER_NAME) and
// delegates lifecycle management to lib/agentdriver.Run.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/lib/agentdriver"
	"github.com/bureau-foundation/bureau/lib/process"
)

func main() {
	// Hook handler mode: when invoked as "bureau-agent-claude hook
	// <event-type>", handle the Claude Code hook event and exit.
	// Claude Code spawns this binary as a subprocess for each hook
	// invocation, passing the event as JSON on stdin.
	if len(os.Args) >= 2 && os.Args[1] == "hook" {
		if err := runHook(os.Args[2:]); err != nil {
			process.Fatal(fmt.Errorf("hook: %w", err))
		}
		return
	}

	config := agentdriver.RunConfigFromEnvironment()
	config.SessionLogPath = "/run/bureau/session.jsonl"
	config.CheckpointFormat = "events-v1"

	if err := agentdriver.Run(context.Background(), &claudeDriver{}, config); err != nil {
		process.Fatal(err)
	}
}
