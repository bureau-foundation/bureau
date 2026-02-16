// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package auth implements the "bureau auth" command group for inspecting
// and debugging the authorization system. Commands connect to the daemon's
// observation socket (directly for operators, via proxy forwarding for
// sandboxed agents) to query the live authorization index.
package auth
