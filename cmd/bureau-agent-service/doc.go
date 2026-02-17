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
//   - Matrix state events in machine config rooms for metadata:
//     session lifecycle (m.bureau.agent_session), context pointers
//     (m.bureau.agent_context), and aggregated metrics
//     (m.bureau.agent_metrics). State events are keyed by principal
//     localpart.
//
//   - Artifact service for bulk content: session logs (JSONL),
//     serialized conversation context, detailed metrics breakdowns,
//     and session index files. Referenced by BLAKE3 hex hashes in
//     the Matrix state events.
//
// The service uses write-through ordering: content is stored in the
// artifact service first, and only after the artifact ref is confirmed
// does the Matrix state event get updated. This guarantees no dangling
// refs — every artifact ref in a state event points to content that
// exists in the artifact service.
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
//   - end-session: mark a session as complete, store log artifact
//   - get-metrics: read aggregated metrics for a principal
package main
