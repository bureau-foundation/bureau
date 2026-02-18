// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package principal handles Bureau principal identity, validation, and
// lifecycle management.
//
// # Identity
//
// Bureau uses Matrix localparts with "/" separators to create a
// hierarchical namespace that maps 1:1 to filesystem paths:
//
//	@iree/amdgpu/pm:bureau.local  ->  /run/bureau/iree/amdgpu/pm.sock
//
// [ValidateLocalpart] enforces the invariants that make this mapping
// safe: no ".." traversal, no hidden segments, no empty segments, only
// the Matrix localpart charset (a-z, 0-9, ., _, =, -, /), and at most
// 84 characters (derived from the 108-byte Unix socket path limit).
//
// Agent-facing and daemon-only sockets share the same directory,
// distinguished by suffix:
//
//   - [SocketPath] -- <run-dir>/<localpart>.sock (agent-facing)
//   - [AdminSocketPath] -- <run-dir>/<localpart>.admin.sock (daemon-only)
//
// The .admin.sock suffix replaces the old /admin/ subdirectory layout.
// The launcher mounts individual socket files into sandboxes, so
// co-locating both suffixes in the same directory has no security
// implications — the .admin.sock file is simply never mounted.
//
// # Fleet Localparts
//
// Fleet-scoped entities use a structured localpart:
//
//	<namespace>/fleet/<fleet-name>/<entity-type>/<entity-name>
//
// [FleetLocalpart] constructs these, [ParseFleetLocalpart] decomposes them,
// [FleetRelativeName] strips the fleet prefix to get entityType/entityName,
// [IsFleetScoped] does a quick structural check, and [FleetPrefix] returns
// the common prefix for all entities in a fleet.
//
// [MatchPattern] and [MatchAnyPattern] provide glob-based access
// control matching: "*" (single segment), "**" (recursive), and
// interior patterns like "iree/**/pm". Malformed patterns deny by
// default rather than propagating errors.
//
// # Lifecycle Management
//
// Principal lifecycle operations are shared by agents and services:
//
//   - [Create]: registers a Matrix account, provisions encrypted credentials,
//     joins the config room, and publishes the MachineConfig assignment.
//     Used by "bureau agent create" and "bureau service create".
//
//   - [Resolve]: finds which machine a principal is assigned to, either
//     by reading a specific machine's config or by scanning all machines
//     from #bureau/machine.
//
//   - [List]: returns all principal assignments across machines,
//     optionally filtered to a single machine.
//
//   - [Destroy]: removes a principal's assignment from the MachineConfig.
//     The daemon detects the change and tears down the sandbox.
package principal
