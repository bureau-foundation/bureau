// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package ticket implements the "bureau ticket" CLI subcommand group
// for viewing and managing Bureau tickets via the ticket service's
// Unix socket.
//
// Query commands (list, show, ready, blocked, ranked, grep, stats,
// info, deps, children, epic-health) read from the service's in-memory
// index. Mutation commands (create, update, close, reopen, batch) write
// to Matrix via the service and update the index immediately.
//
// The "gate" subcommand group manages async coordination conditions.
// The "dep" subcommand group provides convenience wrappers for
// modifying the blocked_by dependency list.
//
// The "viewer" subcommand launches an interactive terminal UI for
// browsing tickets loaded from a beads JSONL file.
package ticket
