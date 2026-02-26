// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package doctor implements the top-level "bureau doctor" command for
// diagnosing the operator's environment end-to-end. Unlike the
// domain-specific doctors ("bureau machine doctor" and "bureau matrix
// doctor"), this command requires no flags — it auto-discovers the
// operator session, machine configuration, homeserver, services, and
// fleet state, reporting what's working and what's broken.
//
// The top-level doctor is a read-only triage tool. It never fixes
// anything itself. Each failure includes guidance pointing the operator
// to the specific sub-doctor or command that handles the repair:
//
//   - Operator session issues → "bureau login <username>"
//   - Machine infrastructure → "sudo bureau machine doctor --fix"
//   - Matrix homeserver state → "bureau matrix doctor --fix --credential-file ./creds"
//
// This separation keeps fix logic in one place per domain and keeps the
// top-level doctor fast and simple.
package doctor
