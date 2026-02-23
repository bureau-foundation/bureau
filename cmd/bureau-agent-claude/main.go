// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// bureau-agent-claude runs Claude Code inside a Bureau sandbox. It
// implements the agentdriver.Driver interface for Claude Code's stream-json
// output format, manages the process lifecycle, and integrates with
// Bureau's proxy, messaging, and session logging infrastructure.
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
)

func main() {
	config := agentdriver.RunConfigFromEnvironment()
	config.SessionLogPath = "/run/bureau/session.jsonl"
	config.CheckpointFormat = "events-v1"

	if err := agentdriver.Run(context.Background(), &claudeDriver{}, config); err != nil {
		fmt.Fprintf(os.Stderr, "bureau-agent-claude: %v\n", err)
		os.Exit(1)
	}
}
