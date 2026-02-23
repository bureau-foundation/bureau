// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"log/slog"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
)

// requireLocalpart returns a Run function for commands that take a single
// service localpart as a positional argument. It validates argument count,
// validates the localpart format, and delegates to fn with the context
// and logger from dispatch.
func requireLocalpart(usage string, fn func(ctx context.Context, localpart string, logger *slog.Logger) error) func(context.Context, []string, *slog.Logger) error {
	return func(ctx context.Context, args []string, logger *slog.Logger) error {
		if len(args) < 1 {
			return cli.Validation("service localpart is required\n\nUsage: %s", usage)
		}
		localpart := args[0]
		if len(args) > 1 {
			return cli.Validation("unexpected argument: %s", args[1])
		}
		if err := principal.ValidateLocalpart(localpart); err != nil {
			return cli.Validation("invalid service localpart: %v", err)
		}
		return fn(ctx, localpart, logger)
	}
}
