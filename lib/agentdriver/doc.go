// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package agentdriver provides the Bureau agent wrapper runtime for running
// AI agent processes inside sandboxes.
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
// Principal lifecycle operations (create, resolve, list, destroy) live in
// [lib/principal] and are shared between agents and services.
package agentdriver
