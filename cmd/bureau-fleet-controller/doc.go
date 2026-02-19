// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-fleet-controller is a standalone Bureau service that manages
// placement and lifecycle of fleet services across machines. It
// maintains an in-memory model of all machines, fleet service
// definitions, and placement state, rebuilt from the Matrix /sync loop.
//
// The fleet controller reads fleet service definitions and machine
// definitions from #bureau/fleet, monitors machine health from
// #bureau/machine heartbeats, and writes PrincipalAssignment events
// to per-machine config rooms to place services on machines.
//
// # Startup
//
// The service reads its Matrix session from --state-dir/session.json
// (written by the launcher during first-boot registration). It joins
// #bureau/service for discovery, #bureau/fleet for fleet definitions,
// and #bureau/machine for machine status. It performs an initial /sync
// to build its fleet model from current state, and starts listening on
// its principal socket path.
//
// # Rooms
//
// The fleet controller watches four categories of rooms:
//
//   - #bureau/fleet: fleet service definitions, machine definitions,
//     fleet config, HA leases, fleet alerts
//   - #bureau/machine: machine info (static inventory) and status
//     (periodic heartbeats)
//   - #bureau/service: service registrations and status
//   - per-machine config rooms containing PrincipalAssignment events
//     (joined via invites, not by alias)
//
// # Socket API
//
// Agents and the CLI connect to the service's Unix socket and send
// CBOR requests (one CBOR value per connection). The "action" field
// determines the operation.
package main
