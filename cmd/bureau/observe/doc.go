// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package observe implements the "bureau observe", "bureau dashboard",
// and "bureau list" subcommands for live terminal observation.
//
// All three commands route through the daemon's observation unix socket
// (default /run/bureau/observe.sock) and require an operator session
// created by "bureau login". The session is loaded automatically from
// ~/.config/bureau/session.json; the daemon verifies the access token
// via the Matrix homeserver's /account/whoami endpoint.
//
// ObserveCommand attaches to a single principal's terminal session,
// relaying I/O between the operator's terminal and the remote PTY.
// It supports readwrite (default) and readonly modes, controlled by
// the target principal's observation allowances.
//
// DashboardCommand creates a local tmux session with one pane per
// principal. Layout sources include: no arguments (machine dashboard
// showing all local principals), a channel alias (layout from Matrix
// state), or --layout-file (local JSON). The dashboard exec's into
// tmux attach-session unless --detach is specified.
//
// ListCommand queries the daemon for observable targets (principals
// and machines) and displays them in a tabular format, with optional
// --observable filtering and --json output.
package observe
