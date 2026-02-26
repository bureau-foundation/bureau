// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package process provides binary entrypoint helpers for Bureau service
// and agent binaries. These functions centralize the two legitimate raw
// I/O patterns that exist before or after the structured logger:
//
//   - Fatal error reporting to stderr when the logger may not be
//     initialized (pre-logger).
//   - Process exit after an unrecoverable error in main().
//
// The pre-commit hook (script/check-raw-output) bans direct fmt.Fprintf
// and fmt.Printf calls in non-CLI code. This package is one of two
// excluded paths (the other is lib/version). All other raw I/O in
// service/agent binaries should be replaced with calls to this package.
package process
