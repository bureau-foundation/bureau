// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package credential provides library functions for managing Bureau
// credential bundles. Credential bundles are age-encrypted JSON objects
// containing secrets (Matrix tokens, API keys, etc.) for a specific
// principal on a specific machine. They are published as
// m.bureau.credentials state events in per-machine config rooms.
//
// This package implements the business logic shared by the
// bureau-credentials CLI binary and integration test helpers. CLI
// commands and test code call these functions directly with a
// messaging.Session, getting full type safety and compiler checking.
//
// The encryption flow:
//   - Fetch the machine's age public key from m.bureau.machine_key
//     in #bureau/machine
//   - Optionally add an escrow key for operator recovery
//   - Encrypt the credential JSON with sealed.EncryptJSON
//   - Publish the encrypted bundle as m.bureau.credentials in the
//     machine's config room (fleet-scoped alias matching the machine's localpart)
package credential
