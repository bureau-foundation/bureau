// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package bootstrap defines the machine bootstrap configuration format
// shared between the provisioning CLI (which writes configs) and the
// launcher (which reads them). This avoids import cycles: both the CLI
// and the launcher import lib/bootstrap, neither imports the other.
//
// The [Config] struct contains the four fields needed for a machine's
// first boot: homeserver URL, server name, machine localpart, and a
// one-time password. The password is generated randomly by "bureau
// machine provision" and rotated immediately by the launcher after
// first login, so even if the bootstrap config file is captured after
// delivery, the password is useless.
//
// Machine names are fleet-scoped localparts validated against
// [ref.ParseMachine] to ensure they are structurally valid Bureau
// machine references.
//
// File operations:
//
//   - [WriteConfig] -- writes a bootstrap config as JSON with 0600
//     permissions (the file contains the one-time password)
//   - [ReadConfig] -- reads and validates a bootstrap config from a file
//   - [WriteToStdout] -- writes formatted JSON to stdout for piping,
//     used by "bureau machine provision" when no --output flag is given
//
// Depends on lib/ref for identity validation.
package bootstrap
