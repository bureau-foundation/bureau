// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package servicetoken implements Ed25519-signed bearer tokens for
// authenticating principals to Bureau services over shared Unix sockets.
//
// Service sockets are bind-mounted into every sandbox whose template
// declares the service as a dependency. From the service's perspective,
// connections arrive from multiple principals on the same listener with
// no inherent way to distinguish callers (SO_PEERCRED is unreliable
// across PID/UID namespace boundaries).
//
// The daemon mints a signed token per (principal, service) pair. The
// token proves the caller's identity and carries pre-resolved
// authorization grants scoped to the service's namespace. Services
// verify tokens cryptographically without a daemon round-trip.
//
// # Wire format
//
// A token is raw bytes: CBOR-encoded payload followed by a 64-byte
// Ed25519 signature over the payload bytes.
//
//	[CBOR payload bytes] [64-byte Ed25519 signature]
//
// The split point is always len(token) - 64. No header, no length
// prefix, no base64 — the algorithm is fixed and the signature size
// is constant.
//
// # Token lifecycle
//
//   - Daemon mints tokens at sandbox creation (one per required service)
//   - Tokens are written to /run/bureau/tokens/<service-role> inside the sandbox
//   - Agents read the token file before each service request
//   - Daemon refreshes tokens at 80% of the TTL (atomic write + rename)
//   - Services reject expired tokens unconditionally
//   - Emergency revocation via Blacklist (token ID set with TTL-based auto-cleanup)
//
// # Dependencies
//
// This package depends on crypto/ed25519 for signing, lib/codec for
// CBOR encoding, and standard library packages. It does not depend on
// lib/authorization/, lib/schema/, or any Bureau subsystem — services
// import it directly without pulling in the daemon's dependency tree.
package servicetoken
