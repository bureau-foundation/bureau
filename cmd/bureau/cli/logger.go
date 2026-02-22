// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"log/slog"
	"os"

	"golang.org/x/term"
)

// NewCommandLogger creates a structured logger for CLI command operations.
// When stderr is a terminal, uses slog.TextHandler for human-readable output.
// When stderr is piped or redirected (CI, scripts, MCP, integration tests),
// uses slog.JSONHandler for machine-parseable output compatible with the
// daemon's log format and telemetry ingestion.
//
// Callers scope the logger with command-specific context via With():
//
//	logger := cli.NewCommandLogger().With(
//	    "command", "machine/revoke",
//	    "fleet", fleet.Localpart(),
//	    "machine", machine.Localpart(),
//	)
func NewCommandLogger() *slog.Logger {
	var handler slog.Handler
	options := &slog.HandlerOptions{Level: slog.LevelInfo}
	if term.IsTerminal(int(os.Stderr.Fd())) {
		handler = slog.NewTextHandler(os.Stderr, options)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, options)
	}
	return slog.New(handler)
}
