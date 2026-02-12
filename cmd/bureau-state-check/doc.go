// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-state-check reads a Matrix state event through the Bureau proxy
// and evaluates a condition against a single field. It is a pipeline
// building block: shell steps can use it as a "when" guard or "check"
// assertion without parsing JSON themselves.
//
// Exit codes:
//
//	0  condition matched
//	1  condition did not match (actual value printed to stderr)
//	2  error (state event not found, proxy unreachable, bad arguments)
//
// Requires BUREAU_SANDBOX=1 and a reachable proxy socket.
package main
