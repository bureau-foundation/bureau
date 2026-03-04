// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"log/slog"

	"github.com/bureau-foundation/bureau/lib/principal"
)

// RequireLocalpart returns a Run function for commands that take a single
// principal localpart as a positional argument. It validates argument count,
// validates the localpart format via [principal.ValidateLocalpart], and
// delegates to fn with the validated localpart.
//
// The noun parameter (e.g., "agent", "service") appears in error messages
// so the user sees "agent localpart is required" or "invalid service
// localpart" as appropriate.
func RequireLocalpart(noun, usage string, fn func(ctx context.Context, localpart string, logger *slog.Logger) error) func(context.Context, []string, *slog.Logger) error {
	return func(ctx context.Context, args []string, logger *slog.Logger) error {
		if len(args) < 1 {
			return Validation("%s localpart is required\n\nUsage: %s", noun, usage)
		}
		localpart := args[0]
		if len(args) > 1 {
			return Validation("unexpected argument: %s", args[1])
		}
		if err := principal.ValidateLocalpart(localpart); err != nil {
			return Validation("invalid %s localpart: %v", noun, err)
		}
		return fn(ctx, localpart, logger)
	}
}
