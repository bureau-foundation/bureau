// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package matrix implements the "bureau matrix" subcommands for
// bootstrapping and administering the Bureau Matrix homeserver.
//
// Subcommands:
//
//   - setup: one-time bootstrap that creates the admin account, Bureau
//     space, global rooms (system, machines, services), and base
//     sandbox templates. Routes directly to the Matrix homeserver
//     since no proxy exists yet during initial deployment.
//   - room: create, list, delete, and inspect room membership.
//   - space: create, list, delete, and inspect space membership.
//   - user: create, list, invite, kick, and whoami. Includes
//     onboardOperator which registers an account and invites it
//     to the Bureau space.
//   - send: send a text message to a room, with resolveRoom helper
//     for alias-to-ID resolution.
//   - state: get and set arbitrary room state events.
//   - doctor: health-check the deployment (rooms, templates, power
//     levels, membership), with --fix for auto-remediation and
//     --dry-run for preview. Returns an exitError with a nonzero
//     exit code when problems are found.
//
// All subcommands except setup use [cli.SessionConfig] for admin-level
// Matrix access via --credential-file. The setup command reads the
// registration token from a file (--registration-token-file, never
// from the command line) and derives the admin password from it.
package matrix
