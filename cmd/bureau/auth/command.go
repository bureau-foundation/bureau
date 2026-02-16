// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"fmt"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// resolvePrincipal extracts a principal localpart from either the --principal
// flag or the first positional argument. Returns the resolved principal and
// the remaining args. Returns a validation error if neither source provides
// a value or if extra positional arguments are present.
func resolvePrincipal(flagValue string, args []string) (string, error) {
	principal := flagValue
	if principal == "" && len(args) > 0 {
		principal = args[0]
		args = args[1:]
	}
	if principal == "" {
		return "", cli.Validation("principal is required (positional or --principal)")
	}
	if len(args) > 0 {
		return "", fmt.Errorf("unexpected argument: %s", args[0])
	}
	return principal, nil
}

// Command returns the "auth" command group for authorization inspection.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "auth",
		Summary: "Inspect and debug authorization policy",
		Description: `Inspect and debug the Bureau authorization system.

These commands query the daemon's live authorization index — the same
index used for runtime access decisions. The results reflect the fully
resolved policy including machine defaults, per-principal assignments,
room-level grants, and active temporal grants.

All commands require authentication: run "bureau login" first. Inside a
sandbox, the proxy injects credentials automatically.

Authorization evaluation follows a two-sided model:

  Subject side (grants + denials): what the actor can do.
  Target side (allowances + allowance denials): who can act on the target.

For self-service actions (no target), only the subject side is checked.
For cross-principal actions, both sides must agree — the actor needs a
matching grant AND the target needs a matching allowance.`,
		Subcommands: []*cli.Command{
			checkCommand(),
			grantsCommand(),
			allowancesCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Check if an agent can observe another agent",
				Command:     "bureau auth check --actor iree/amdgpu/pm --action observe --target iree/amdgpu/compiler",
			},
			{
				Description: "Show all grants for a principal",
				Command:     "bureau auth grants iree/amdgpu/pm",
			},
			{
				Description: "Show who can act on a principal",
				Command:     "bureau auth allowances iree/amdgpu/compiler",
			},
		},
	}
}
