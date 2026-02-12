// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-ticket-service is a standalone Bureau service that owns the
// ticket lifecycle for rooms it has been invited to. It maintains an
// in-memory index of all tickets in its scope, evaluates gate
// conditions via the Matrix /sync loop, and serves queries and
// mutations over a Unix socket using the NDJSON protocol.
//
// The service is Bureau's first standalone service principal. It
// establishes patterns that future services (artifact, fleet
// controller, RAG) will follow: Matrix session from a state directory,
// service registration in #bureau/service, independent /sync loop
// filtered to relevant event types, and a direct Unix socket API.
//
// # Startup
//
// The service reads its Matrix session from --state-dir/session.json
// (written by the launcher during first-boot registration). It joins
// #bureau/service for discovery, performs an initial /sync to build
// its ticket index from current state, and starts listening on its
// principal socket path.
//
// # Scope
//
// The service's scope is its Matrix room membership. It watches all
// rooms it has been invited to and indexes tickets only in rooms that
// have an m.bureau.ticket_config state event (ticket management
// enabled). Common deployment patterns: one instance per space, one
// per operator, or one for the whole deployment.
//
// # Socket API
//
// Agents and the CLI connect to the service's Unix socket and send
// NDJSON requests (one JSON object per line, one request per
// connection). The "action" field determines the operation: status,
// list, ready, show, create, update, close, etc.
package main
