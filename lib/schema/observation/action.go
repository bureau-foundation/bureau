// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observation

// Authorization action constants for the observation system.
// Enforced by the daemon (observe_auth.go) when an observer
// requests terminal access to a principal. See lib/schema/action.go
// for the action system overview.
//
// ActionObserve gates read-only streaming observation. ActionReadWrite
// gates interactive terminal access (input injection, resize). The
// daemon checks ActionObserve first, then ActionReadWrite for
// write-capable sessions. ActionReadWrite is a sensitive action that
// triggers audit logging on both allow and deny.
const (
	ActionObserve   = "observe"
	ActionReadWrite = "observe/read-write"
	ActionInput     = "observe/input"
	ActionResize    = "observe/resize"
)

// ActionAll is the wildcard pattern matching all observation
// operations.
const ActionAll = "observe/**"
