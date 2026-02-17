// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package machine implements the "bureau machine" subcommands for
// managing the lifecycle of machines in the Bureau fleet.
//
// All subcommands route directly to the Matrix homeserver using
// admin credentials from --credential-file (via [cli.SessionConfig]
// or [cli.ReadCredentialFile]).
//
// Subcommands:
//
//   - provision: registers a new machine's Matrix account with a
//     random one-time password, creates the per-machine config room
//     (#bureau/config/<machine>), invites the machine to all global
//     rooms (system, machine, service, template, pipeline, fleet),
//     and writes a bootstrap config file. The bootstrap file is
//     transferred to the new machine and consumed by bureau-launcher
//     --bootstrap-file, which logs in, rotates the password, generates
//     a keypair, and publishes the machine's key. Uses the bootstrap
//     package for config serialization. If the account already exists,
//     provision verifies full decommission (zero Bureau room
//     memberships, cleared state events) before allowing re-provision.
//   - list: reads m.bureau.machine_key and m.bureau.machine_status
//     state events from #bureau/machine to show all fleet machines
//     with their public keys and last heartbeat times.
//   - decommission: clears a machine's key and status state events,
//     removes credentials from its config room, and kicks the machine
//     account from all Bureau rooms (system, machine, service,
//     template, pipeline, fleet, config). Verifies zero memberships
//     at the end. The machine name can be re-provisioned afterward.
//   - revoke: emergency credential revocation for a compromised machine.
//     Deactivates the machine's Matrix account (causing the daemon to
//     self-destruct), clears all state events, kicks from rooms, and
//     publishes an m.bureau.credential_revocation event for fleet-wide
//     notification. This is the panic button for security incidents.
package machine
