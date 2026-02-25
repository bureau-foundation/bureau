// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package doctor provides shared infrastructure for Bureau's diagnostic
// commands (matrix doctor, machine doctor, service doctor, etc.).
//
// Each doctor command runs a series of health checks and reports results
// in a consistent format. Fixable failures carry fix closures that can
// be executed in --fix mode. The package provides:
//
//   - [Result] type with status, message, and optional fix action
//   - Constructors: [Pass], [Fail], [FailWithFix], [FailElevated], [Warn], [Skip]
//   - [ExecuteFixes] for running fix closures with elevation awareness
//   - [PrintChecklist] for human-readable output
//   - [BuildJSON] for machine-readable output
//   - [MarkRepaired] for cross-iteration repair tracking
//
// Domain-specific checks (what to check, how to fix) live in each
// doctor command's package. This package provides only the workflow
// infrastructure.
package doctor
