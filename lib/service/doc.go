// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package service provides shared infrastructure for Bureau services.
//
// A Bureau service is a standalone Go binary with its own Matrix account,
// its own /sync loop, and a Unix socket API. Services register in
// #bureau/service for discovery and are accessed by agents via direct
// socket bind-mounts (not through the proxy). This package extracts
// the common scaffolding that every service needs:
//
//   - Session loading: read session.json from the state directory,
//     create an authenticated Matrix client and session.
//   - Service registration: publish and clear m.bureau.service state
//     events in #bureau/service.
//   - Sync loop: incremental Matrix /sync long-poll with backoff,
//     delivering responses to a caller-provided handler.
//   - Socket server: NDJSON Unix socket server with action dispatch,
//     connection timeouts, and graceful shutdown.
//
// Services compose these utilities in their own main() function rather
// than subclassing a framework. The package provides building blocks,
// not a runtime.
//
// # Authentication
//
// Socket-level caller authentication is not yet implemented. Physical
// access control (the sandbox boundary) determines who can reach a
// service socket: only principals whose template declares the service
// in RequiredServices get the socket bind-mounted. Verified caller
// identity will be provided by the authorization framework (daemon-side
// intermediary sockets that inject authenticated principal metadata
// into requests before forwarding). The NDJSON protocol is designed
// to accommodate this: JSON objects are extensible, and the reserved
// _principal field will carry daemon-injected identity when available.
// Services must never trust a _principal field until the authorization
// framework is in place.
package service
