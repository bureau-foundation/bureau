// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package agent provides the shared library for Bureau agent wrappers
// and agent management operations.
//
// # Agent Wrappers (in-sandbox)
//
// An agent wrapper is a binary that runs inside a Bureau sandbox, manages
// an AI agent process (Claude Code, Codex, Gemini, etc.), and integrates
// it with Bureau's infrastructure: proxy-based credential injection,
// Matrix messaging, session logging, and lifecycle management.
//
//   - Driver: interface that agent-specific wrappers implement to spawn their
//     agent process, parse its output into structured events, and handle
//     interruption.
//
//   - SessionLogWriter: JSONL writer for structured session events (prompts,
//     tool calls, responses, metrics). Produces files suitable for shipping
//     to the artifact service.
//
//   - Run: the main lifecycle function that ties everything together. It
//     creates the proxy client (from lib/proxyclient), builds the agent
//     context, spawns the agent via the Driver, logs events, pumps messages
//     from Matrix, and posts a completion summary.
//
// Each agent runtime (Claude Code, Codex, etc.) lives in its own binary
// under cmd/bureau-agent-<name>/ and implements Driver for its specific
// process management and output parsing.
//
// # Agent Management (operator-facing)
//
//   - Create: registers a Matrix account, provisions encrypted credentials,
//     invites the agent to the config room, and publishes the MachineConfig
//     assignment. Used by "bureau agent create".
//
//   - ResolveAgent: finds which machine a principal is assigned to, either
//     by reading a specific machine's config or by scanning all machines
//     from #bureau/machine. Used by all agent CLI commands that accept an
//     optional --machine flag.
//
//   - ListAgents: returns all principal assignments across machines,
//     optionally filtered to a single machine.
package agent
