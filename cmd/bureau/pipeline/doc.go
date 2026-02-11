// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package pipeline implements the "bureau pipeline" subcommands for
// managing automation pipelines stored as Matrix state events.
//
// A pipeline defines a structured sequence of steps — shell commands,
// Matrix state event publications, and interactive sessions — that
// run inside Bureau sandboxes. Pipelines are the automation primitive
// for workspace setup, service lifecycle, maintenance, and deployment.
//
// All commands that access Matrix authenticate via the operator session
// from "bureau login" (stored at ~/.config/bureau/session.json) and
// connect directly to the Matrix homeserver. The --server-name flag
// controls how room alias localparts are resolved to full Matrix aliases.
//
// Subcommands:
//
//   - list: enumerate pipelines in a room by reading m.bureau.pipeline
//     state events.
//   - show: display a pipeline's content (steps, variables, description).
//   - validate: check a local JSONC pipeline file for structural
//     correctness without publishing.
//   - push: publish a local JSONC pipeline file to Matrix as a state
//     event.
//   - execute: send a pipeline.execute command to a machine's daemon.
package pipeline
