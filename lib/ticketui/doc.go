// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package ticketui implements a terminal user interface for browsing
// Bureau tickets. Built on bubbletea (Elm architecture), it provides
// a split-pane view with a grouped ticket list and a rich detail pane,
// connected to a ticket data source via the [Source] interface.
//
// Generic UI components (theme, overlays, search, dropdowns, modals,
// fuzzy matching, markdown rendering) live in [tui] and are re-exported
// here for internal use. Ticket-specific logic (data sources, key
// bindings, autolinks, filters, detail rendering) stays in this
// package.
//
// The Source abstraction decouples the TUI from the data backend:
// [IndexSource] wraps a local [ticketindex.Index] (used when loading
// from beads JSONL files), while [ServiceSource] communicates with the
// ticket service over CBOR/unix socket. The TUI code is identical in
// both cases.
//
// Data flow:
//
//	[beads JSONL / ticket service]
//	        | (Source interface)
//	    [Model] <- bubbletea event loop
//	        |
//	  [terminal output]
package ticketui
