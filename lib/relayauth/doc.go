// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package relayauth evaluates relay policies for cross-room ticket
// relay authorization. When a workspace room creates a resource_request
// ticket, the ticket service must check whether the workspace is
// authorized to relay into the target ops room. This package implements
// that check against the m.bureau.relay_policy state event published in
// each ops room.
//
// Relay policy evaluation is a pure function: the caller provides the
// policy, the source room identity, and the ticket type. No network
// calls are made — the caller is responsible for fetching the policy
// from the ops room and providing the source room's canonical alias.
//
// Authorization strategies (RelaySourceMatch):
//
//   - fleet_member: source room's alias localpart starts with the
//     fleet localpart prefix. Matches any room that belongs to a
//     fleet's naming hierarchy.
//   - room: source room ID matches exactly. For pinpoint authorization
//     of a specific room.
//   - namespace: source room's alias localpart starts with the
//     namespace name prefix. Matches any room within a namespace.
//
// Default-deny: if no source matches, authorization is denied. If no
// policy is published (nil), authorization is denied.
package relayauth
