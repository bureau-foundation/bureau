// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package forgesub manages real-time event subscriptions for forge
// connector services. Each forge connector (GitHub, Forgejo, GitLab)
// creates its own Manager instance to route webhook events to
// connected agent streams.
//
// The Manager supports two subscription types:
//
//   - Room subscriptions: receive all events for repositories bound to
//     a Bureau room, filtered by the room's ForgeConfig (event
//     categories, triage filters).
//
//   - Entity subscriptions: target a specific forge entity (issue, PR,
//     or CI run). Can be ephemeral (auto-cleaned on entity close) or
//     persistent (survive close events for reopen tracking).
//
// The Manager is a pure routing and filtering engine. It does not
// perform network I/O, CBOR encoding, or heartbeat management. The
// connector service that owns the Manager is responsible for
// connections, encoding, and the subscribe stream event loop.
//
// Thread safety: all exported methods are safe for concurrent use.
// The Manager uses a single sync.RWMutex following the same pattern as
// the ticket service's subscriber management.
package forgesub
