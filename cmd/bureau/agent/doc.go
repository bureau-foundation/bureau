// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package agent implements the "bureau agent" CLI subcommands for agent
// lifecycle management: creation, listing, inspection, and removal.
//
// All commands operate directly against the Matrix homeserver using
// admin-level credentials (--credential-file). The "create" command
// additionally requires the registration token to register new Matrix
// accounts.
//
// Agent resolution is automatic: when --machine is not specified,
// commands scan all machines from #bureau/machine to find where an
// agent is assigned. This O(N) scan is intentionally simple — the
// fleet controller will replace it with an indexed lookup.
package agent
