// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
)

// requireLocalpart returns a Run function for commands that take a single
// agent localpart as a positional argument. It validates argument count,
// validates the localpart format, and delegates to fn.
func requireLocalpart(usage string, fn func(localpart string) error) func([]string) error {
	return func(args []string) error {
		if len(args) < 1 {
			return cli.Validation("agent localpart is required\n\nUsage: %s", usage)
		}
		localpart := args[0]
		if len(args) > 1 {
			return cli.Validation("unexpected argument: %s", args[1])
		}
		if err := principal.ValidateLocalpart(localpart); err != nil {
			return cli.Validation("invalid agent localpart: %v", err)
		}
		return fn(localpart)
	}
}
