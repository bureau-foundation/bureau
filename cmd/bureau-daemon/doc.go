// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-daemon is the unprivileged, network-facing Bureau process. It
// connects to the Matrix homeserver via /sync long-polling, reads
// MachineConfig state events to determine which principals to run, and
// orchestrates the launcher via unix socket IPC to create and destroy
// sandboxes. It holds no credentials or private keys.
package main
