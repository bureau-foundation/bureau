// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package fleet implements the "bureau fleet" CLI subcommand group
// for managing fleet controller placement, health monitoring, and
// configuration.
//
// Commands connect to the fleet controller's Unix socket for
// operational queries (status, list-machines, list-services, place,
// unplace, plan). The enable command uses direct Matrix access to
// bootstrap the fleet controller's service account and principal
// assignment. The config command uses Matrix access to read and
// write FleetConfigContent in #bureau/fleet.
//
// Connection parameters default to the in-sandbox paths where the
// daemon provisions sockets and tokens. Operators running outside a
// sandbox can override with --socket and --token-file flags (or the
// BUREAU_FLEET_SOCKET and BUREAU_FLEET_TOKEN environment variables).
package fleet
