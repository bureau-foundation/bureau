// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package authorization implements Bureau's unified access control
// framework. It evaluates whether a principal (actor) can perform an
// action on another principal (target) by checking a two-sided policy:
//
//   - Grants (subject-side): what the actor is allowed to do.
//   - Denials (subject-side): what the actor is explicitly blocked from.
//   - Allowances (target-side): who is allowed to act on the target.
//   - AllowanceDenials (target-side): who is explicitly blocked from the target.
//
// The effective permission is the intersection of both sides: the actor
// must have a matching grant AND the target must have a matching
// allowance, and neither side may have a matching denial.
//
// For self-service actions (joining rooms, discovering services, running
// CLI commands), only the subject side is evaluated — there is no target
// principal, just infrastructure.
//
// # Actions
//
// Actions are hierarchical strings using "/" as separator (same as
// localparts). Glob patterns match using the same semantics as
// principal.MatchPattern: "*" matches one segment, "**" matches any
// number of segments, "?" matches a single non-slash character.
//
//	observe              — read-only terminal observation
//	observe/read-write   — interactive terminal observation
//	interrupt            — send interrupt signal
//	interrupt/terminate  — send termination signal
//	matrix/join          — join rooms
//	service/discover     — discover services
//	ticket/create        — create tickets
//	command/**           — all CLI commands
//
// # Index
//
// The Index holds per-principal resolved policies and supports
// concurrent read access with single-writer updates. The daemon builds
// the index from machine config, room authorization policies, and
// per-principal assignments, then updates it incrementally as state
// events arrive via /sync.
//
// # Temporal Grants
//
// Temporal grants are time-bounded grants added at runtime (typically
// through ticket-backed access requests). The index tracks them
// separately for efficient expiry sweeping.
package authorization
