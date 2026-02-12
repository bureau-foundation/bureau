// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package ticket provides an in-memory ticket index for the Bureau ticket
// service. It maintains secondary indexes over [schema.TicketContent]
// values for fast filtered queries, a dependency graph for readiness
// computation, and a parent-child hierarchy for epic breakdown.
//
// The index is a pure data structure with no Matrix client, no network,
// and no concurrency control. The ticket service feeds it parsed state
// events from its /sync loop and queries it to serve socket API
// requests.
//
// # Lifecycle
//
// Create an index with [NewIndex]. Populate it by calling [Index.Put]
// for each m.bureau.ticket state event received during initial sync.
// After initial sync, call [Index.Put] incrementally for each state
// event update delivered by /sync, and [Index.Remove] when a ticket
// state event is redacted or tombstoned (empty content).
//
// # Readiness
//
// A ticket is "ready" when:
//   - Its status is "open"
//   - All tickets in its blocked_by list have status "closed"
//   - All gates have status "satisfied"
//
// Missing blockers (IDs not present in the index) are treated as
// unresolved — the ticket is not ready. This follows Bureau's
// no-silent-failure principle: a dangling reference means something
// is wrong, not that the dependency is satisfied.
//
// # Concurrency
//
// Index is not safe for concurrent use. The ticket service serializes
// access through its event loop or wraps the index with a mutex.
//
// # Dependencies
//
// This package depends only on [schema] for the TicketContent type.
// It has no dependency on messaging, Matrix, or any I/O package.
package ticket
