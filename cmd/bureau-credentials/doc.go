// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-credentials provisions encrypted credential bundles and machine
// configurations to Matrix. It reads credentials from stdin or file,
// encrypts them to the target machine's age public key, and publishes
// them as m.bureau.credentials state events in the machine's config room.
// Subcommands: keygen, provision, assign, list.
package main
