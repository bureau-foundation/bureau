// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package ref provides strongly typed, immutable identity references for
// Bureau entities. Every Bureau identity — namespace, fleet, machine,
// service, agent — is represented by a validated value type with
// pre-computed canonical forms.
//
// Ref types enforce the naming conventions defined in
// naming-conventions.md: hierarchical localparts that map to Matrix user
// IDs, room aliases, and filesystem socket paths.
//
// All constructors validate their inputs and return errors for invalid
// names. Once constructed, a ref is immutable — accessor methods return
// pre-computed strings at zero allocation cost for entity-level
// identifiers (localpart, user ID, room alias). Namespace and fleet
// aliases are computed on demand via simple concatenation.
//
// The canonical serialization form is the full Matrix identifier:
//   - Entity refs (Machine, Service, Agent): @localpart:server
//   - Fleet refs: #localpart:server (room alias)
//   - Namespace refs: #namespace:server (space alias)
//
// JSON marshaling uses this canonical form via encoding.TextMarshaler.
package ref
