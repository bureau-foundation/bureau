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
//   - Proxy bootstrap: create a ProxySession from environment variables
//     injected by the launcher, resolve fleet-scoped rooms, load the
//     daemon's token signing key, and register in the service directory.
//   - Service registration: publish and clear m.bureau.service state
//     events in #bureau/service.
//   - Signing key discovery: fetch the daemon's Ed25519 token signing
//     public key from #bureau/system for request authentication.
//   - Sync loop: incremental Matrix /sync long-poll with backoff,
//     delivering responses to a caller-provided handler.
//   - Socket server: CBOR Unix socket server with action dispatch,
//     service token authentication, and graceful shutdown.
//   - Socket client: CBOR client for making authenticated requests to
//     service sockets. Handles token inclusion, connection lifecycle,
//     and response decoding.
//   - HTTP server: TCP listener for services that need inbound HTTP
//     (webhook ingestion from external forges). Includes HMAC-SHA256
//     signature verification for webhook payloads. Same lifecycle
//     pattern as [SocketServer]: [HTTPServer.Serve] blocks until
//     graceful shutdown completes.
//   - Session persistence: [LoadSession] and [SaveSession] for the
//     launcher and daemon's machine-level Matrix sessions (not used
//     by service bootstrap).
//
// [BootstrapViaProxy] orchestrates the common startup sequence: env var
// validation, identity ref construction, proxy session creation, room
// resolution, signing key discovery, auth configuration, and service
// registration. Services call BootstrapViaProxy from their main() to
// eliminate repeated boilerplate, then add service-specific logic
// (initial sync, socket server, sync loop) using the [BootstrapResult]
// fields.
//
// Services that need pre-bootstrap work (e.g., reading secrets from
// stdin) create their logger via [NewLogger], perform the work, then
// pass the logger to [ProxyBootstrapConfig]. Services without such
// needs let BootstrapViaProxy create the logger automatically.
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
// Ed25519 public key and a local [servicetoken.Blacklist] for emergency
// revocation. No daemon round-trip is required per request.
// [LoadTokenSigningKey] fetches the public key from #bureau/system
// where the daemon publishes it at startup.
//
// # Revocation
//
// When a sandbox is destroyed, the daemon pushes signed revocation
// requests to services via the "revoke-tokens" action. Services that
// use [SocketServer] call [SocketServer.RegisterRevocationHandler] to
// register this standard handler. The handler verifies the daemon's
// Ed25519 signature on the revocation payload and adds the specified
// token IDs to the [AuthConfig.Blacklist]. Subsequent requests
// carrying a blacklisted token ID are rejected.
package service
