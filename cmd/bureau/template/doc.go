// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package template implements the "bureau template" subcommands for
// managing sandbox templates stored as Matrix state events.
//
// A sandbox template defines the full sandbox configuration: command,
// filesystem mounts, Linux namespace settings, resource limits,
// security policy, and environment variables. Templates support
// multi-level inheritance via an "inherits" field, allowing a base
// template to be extended by project-specific overrides.
//
// All commands authenticate via the operator session from "bureau login"
// (stored at ~/.config/bureau/session.json) and connect directly to
// the Matrix homeserver. The --server-name flag controls how room alias
// localparts are resolved to full Matrix aliases.
//
// Subcommands:
//
//   - list: enumerate templates in a room by reading m.bureau.template
//     state events.
//   - show: display a template's content, optionally resolving the
//     full inheritance chain with --resolve.
//   - push: publish a local YAML template file to Matrix as a state
//     event.
//   - validate: check a local YAML template file for structural
//     correctness without publishing.
//   - diff: compare a Matrix template against a local file, showing
//     line-by-line differences.
//   - impact: analyze which principals would be affected by changing
//     a template, classifying changes by severity (field additions,
//     modifications, deletions).
package template
