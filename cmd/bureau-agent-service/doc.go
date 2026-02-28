// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-agent-service is a standalone Bureau service that manages
// agent lifecycle data: session tracking, LLM conversation context
// for resume, and aggregated usage metrics. It follows the same
// service principal pattern as the ticket and artifact services:
// Matrix account, service registration in #bureau/service, sync loop,
// and a Unix socket for client operations.
//
// # Storage Model
//
// The service is ephemeral — it holds no persistent local state. All
// data is stored in two external systems:
//
//   - Matrix state events in machine config rooms for per-principal
//     metadata: session lifecycle (m.bureau.agent_session), context
//     pointers (m.bureau.agent_context), and aggregated metrics
//     (m.bureau.agent_metrics). State events are keyed by principal
//     localpart.
//
//   - Artifact service (CAS) for bulk content and immutable data:
//     session logs (JSONL), serialized conversation context deltas,
//     detailed metrics breakdowns, session index files, and context
//     commit metadata (tagged as "ctx/<commitID>"). Context commits
//     are CBOR-encoded artifacts with mutable tags, not Matrix state
//     events — they are immutable data (except for Summary) and the
//     artifact store is the correct home.
//
// # Scope
//
// One agent service instance per top-level space (IREE, bureau, lore,
// etc.), scoped to agents within that space. The service joins the
// machine config room for its machine and reads/writes agent state
// events for principals in its scope.
//
// # Socket API
//
// Agent wrappers (bureau-agent, bureau-agent-claude, etc.) connect to
// the service's Unix socket and send CBOR requests (one CBOR value per
// connection via lib/service SocketServer). The "action" field
// determines the operation:
//
//   - status: health check (unauthenticated)
//   - get-session: read session state for a principal
//   - start-session: mark a session as active
//   - end-session: mark a session as complete, store log artifact ref
//   - store-session-log: store session log data as an artifact
//   - set-context: write a named context entry (artifact ref + metadata)
//   - get-context: read a single context entry by key
//   - delete-context: remove a context entry
//   - list-context: list context entries, optionally filtered by key prefix
//   - checkpoint-context: create a context commit (stores metadata in CAS)
//   - show-context-commit: read a context commit by ID
//   - history-context: walk a commit chain from tip to root
//   - materialize-context: concatenate deltas for a commit chain
//   - update-context-metadata: update a commit's summary
//   - resolve-context: find the nearest commit at or before a timestamp
//   - get-metrics: read aggregated metrics for a principal
package main
