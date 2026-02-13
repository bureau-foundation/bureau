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
//   - Socket server: CBOR Unix socket server with action dispatch,
//     service token authentication, and graceful shutdown.
//
// Services compose these utilities in their own main() function rather
// than subclassing a framework. The package provides building blocks,
// not a runtime.
//
// # Authentication
//
// The socket server supports Ed25519 service token authentication via
// [AuthConfig]. The daemon mints a signed token per (principal, service)
// pair at sandbox creation time. The token proves the caller's identity
// and carries pre-resolved authorization grants scoped to the service's
// namespace. Agents include the token as an opaque CBOR byte string in
// the "token" field of each request.
//
// Actions registered with [SocketServer.HandleAuth] require a valid
// token. The server verifies the Ed25519 signature, checks expiry and
// audience, consults the revocation blacklist, and passes the decoded
// token to the handler. Actions registered with [SocketServer.Handle]
// do not require authentication (use for health checks and diagnostics).
//
// The token verification is stateless: services need only the daemon's
// Ed25519 public key (published as m.bureau.token_signing_key in
// #bureau/system) and a local [servicetoken.Blacklist] for emergency
// revocation. No daemon round-trip is required per request.
package service
