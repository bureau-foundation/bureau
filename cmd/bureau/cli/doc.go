// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package cli provides the command-line framework for the bureau unified CLI.
//
// The central type is [Command], which represents a named subcommand with
// optional nested [Command.Subcommands], a [pflag.FlagSet] factory, and a
// Run function. Commands are assembled into a tree in cmd/bureau/main.go
// and dispatched via [Command.Execute], which handles flag parsing,
// subcommand routing, and structured help output with examples.
//
// When a user types an unknown subcommand or flag, the framework computes
// Levenshtein edit distance against all known names and suggests the
// closest match (threshold: distance <= 3). This is implemented in
// suggest.go.
//
// The package also provides two authentication mechanisms used by CLI
// subcommand packages:
//
//   - [OperatorSession] / [LoadSession] / [SaveSession]: operator-level
//     authentication via "bureau login". The session file lives at
//     ~/.config/bureau/session.json and is loaded transparently by
//     commands that require identity (observe, list, dashboard, template).
//
//   - [SessionConfig] / [SessionConfig.Connect]: admin-level Matrix
//     access via --credential-file (or explicit --homeserver/--token/
//     --user-id flags). Used by commands that modify fleet state
//     (workspace create, machine provision, matrix setup).
//
//   - [ResolveLocalMachine]: reads the launcher's session file to
//     discover the local machine's Matrix localpart, enabling
//     --machine=local shorthand.
//
// [ReadCredentialFile] parses the key=value credential file format
// written by "bureau matrix setup".
package cli
