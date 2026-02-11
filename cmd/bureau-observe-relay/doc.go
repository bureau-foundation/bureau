// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-observe-relay is the per-session relay process forked by the
// daemon for each observation session. It attaches to a Bureau-managed
// tmux session and relays terminal I/O between the tmux PTY and a unix
// socket inherited on fd 3 from the daemon. Not invoked directly by
// users.
package main
