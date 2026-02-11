// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-launcher is the privileged Bureau process. It manages machine
// identity (age keypair generation, Matrix registration), credential
// decryption, and sandbox lifecycle (bwrap namespace creation, tmux
// session management, proxy spawning). It has no network access after
// first boot; all external communication flows through bureau-daemon
// via a unix socket at /run/bureau/launcher.sock.
package main
