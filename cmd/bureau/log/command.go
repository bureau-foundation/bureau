// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package log implements the "bureau log" CLI subcommands for viewing
// raw terminal output captured from sandboxed Bureau processes.
//
// Bureau's telemetry pipeline captures stdout/stderr from every sandbox
// as OutputDelta messages. The telemetry service persists these as CAS
// artifacts (via its log manager) and tracks session metadata as CBOR
// artifacts with mutable tags named "log/<source-localpart>/<session-id>".
//
// This is distinct from "bureau telemetry logs", which queries structured
// slog-style log records. "bureau log" operates on the raw byte stream
// from process terminals — ANSI escape sequences and all.
//
// The four subcommands cover the full lifecycle:
//
//   - list: enumerate captured output sessions (artifact tag listing)
//   - show: display the content of a session to stdout
//   - export: write session content to a file
//   - tail: stream live output from running sandboxes
//
// list, show, and export connect to the artifact service (where chunks
// and metadata are stored). tail connects to the telemetry service's
// streaming tail action.
package log

import "github.com/bureau-foundation/bureau/cmd/bureau/cli"

// Command returns the "log" subcommand group.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "log",
		Summary: "View raw terminal output from sandboxed processes",
		Description: `View and stream captured terminal output (stdout/stderr) from
Bureau sandboxed processes.

The telemetry pipeline captures output from every sandbox. Completed
sessions are stored as content-addressed artifacts; live sessions can
be streamed via the telemetry service.

The list, show, and export commands connect to the artifact service
to read stored output. The tail command connects to the telemetry
service for live streaming. Use --service mode from the host (requires
'bureau login') or the default in-sandbox socket paths.`,
		Subcommands: []*cli.Command{
			listCommand(),
			showCommand(),
			exportCommand(),
			tailCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "List all captured output sessions",
				Command:     "bureau log list --service",
			},
			{
				Description: "Show output from a specific source",
				Command:     "bureau log show my_bureau/fleet/prod/agent/reviewer --service",
			},
			{
				Description: "Export output to a file",
				Command:     "bureau log export my_bureau/fleet/prod/agent/reviewer -o output.bin --service",
			},
			{
				Description: "Stream live output matching a pattern",
				Command:     "bureau log tail 'my_bureau/fleet/prod/**' --service",
			},
			{
				Description: "Stream live output for a pipeline ticket",
				Command:     "bureau log tail tkt-a3f9 --service",
			},
		},
	}
}
