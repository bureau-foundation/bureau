// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package auth

// Principal policy commands: bureau auth grants and bureau auth allowances.
// These query one side of the authorization model for a principal. Grants
// show the subject side (what the principal can do), allowances show the
// target side (who can act on the principal).

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/observe"
)

// policyParams holds the shared parameters for principal policy query
// commands (bureau auth grants and bureau auth allowances).
type policyParams struct {
	cli.JSONOutput
	Principal  string `json:"principal"   flag:"principal"   desc:"localpart of the principal to query"`
	SocketPath string `json:"-"           flag:"socket"      desc:"daemon observation socket path" default:"/run/bureau/observe.sock"`
}

// policyCommandSpec captures the per-command variable parts of a principal
// policy query command. The invariant parts (params, annotations, run flow)
// are handled by newPolicyCommand.
type policyCommandSpec struct {
	name        string
	summary     string
	description string
	usage       string
	examples    []cli.Example
	grant       string
	output      func() any
	query       func(socketPath, principal, observer, token string) (any, error)
	format      func(response any) error
}

// newPolicyCommand constructs a *cli.Command from a policyCommandSpec. The
// command resolves a principal from --principal or a positional argument,
// loads the operator session, executes the query, and either emits JSON or
// calls the human-readable formatter.
func newPolicyCommand(spec policyCommandSpec) *cli.Command {
	var params policyParams

	return &cli.Command{
		Name:           spec.name,
		Summary:        spec.summary,
		Description:    spec.description,
		Usage:          spec.usage,
		Examples:       spec.examples,
		Params:         func() any { return &params },
		Output:         spec.output,
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{spec.grant},
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			principal, err := resolvePrincipal(params.Principal, args)
			if err != nil {
				return err
			}

			operatorSession, err := cli.LoadSession()
			if err != nil {
				return err
			}

			response, err := spec.query(params.SocketPath, principal, operatorSession.UserID, operatorSession.AccessToken)
			if err != nil {
				return err
			}

			if done, jsonErr := params.EmitJSON(response); done {
				return jsonErr
			}

			return spec.format(response)
		},
	}
}

// Query adapters bridge the typed observe query functions to the
// untyped policyCommandSpec.query signature.

func queryGrantsAdapter(socketPath, principal, observer, token string) (any, error) {
	return observe.QueryGrants(socketPath, observe.GrantsRequest{
		Principal: principal, Observer: observer, Token: token,
	})
}

func queryAllowancesAdapter(socketPath, principal, observer, token string) (any, error) {
	return observe.QueryAllowances(socketPath, observe.AllowancesRequest{
		Principal: principal, Observer: observer, Token: token,
	})
}

// Format adapters bridge the typed format functions to the untyped
// policyCommandSpec.format signature.

func formatGrantsAdapter(response any) error {
	return formatGrantsOutput(response.(*observe.GrantsResponse))
}

func formatAllowancesAdapter(response any) error {
	return formatAllowancesOutput(response.(*observe.AllowancesResponse))
}

func grantsCommand() *cli.Command {
	return newPolicyCommand(policyCommandSpec{
		name:    "grants",
		summary: "Show grants and denials for a principal",
		description: `Show the complete resolved grant and denial policy for a principal.

Grants define what the principal can do. Denials explicitly block actions
that would otherwise be granted (evaluated after grants).

The output reflects the fully resolved policy from the daemon's live
authorization index, including machine defaults, per-principal assignments,
and active temporal grants.`,
		usage: "bureau auth grants <principal> [flags]",
		examples: []cli.Example{
			{Description: "Show grants for an agent", Command: "bureau auth grants iree/amdgpu/pm"},
			{Description: "Show grants as JSON", Command: "bureau auth grants iree/amdgpu/pm --json"},
		},
		grant:  "command/auth/grants",
		output: func() any { return &observe.GrantsResponse{} },
		query:  queryGrantsAdapter,
		format: formatGrantsAdapter,
	})
}

func allowancesCommand() *cli.Command {
	return newPolicyCommand(policyCommandSpec{
		name:    "allowances",
		summary: "Show allowances and allowance denials for a principal",
		description: `Show the complete resolved allowance and allowance denial policy for a
principal. This is the target side of the authorization model.

Allowances define who can act on this principal (e.g., who can observe it,
who can interrupt it). Allowance denials explicitly block actors that
would otherwise be allowed.

For a cross-principal action to succeed, the actor needs a matching grant
AND the target needs a matching allowance. Use "bureau auth check" to
evaluate both sides together.`,
		usage: "bureau auth allowances <principal> [flags]",
		examples: []cli.Example{
			{Description: "Show who can act on an agent", Command: "bureau auth allowances iree/amdgpu/compiler"},
			{Description: "Show allowances as JSON", Command: "bureau auth allowances iree/amdgpu/compiler --json"},
		},
		grant:  "command/auth/allowances",
		output: func() any { return &observe.AllowancesResponse{} },
		query:  queryAllowancesAdapter,
		format: formatAllowancesAdapter,
	})
}

// policySection describes a labeled section in human-readable policy output
// (e.g., "GRANTS" followed by a list of formatted grant strings).
type policySection struct {
	name  string
	items []string
}

// formatPolicySections writes a human-readable view of a principal's policy.
// Each section is printed with a header, a "(none)" placeholder when empty,
// and indented items.
func formatPolicySections(principal string, sections []policySection) error {
	fmt.Printf("PRINCIPAL: %s\n\n", principal)

	for i, section := range sections {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("%s:\n", section.name)
		if len(section.items) == 0 {
			fmt.Println("  (none)")
		}
		for _, item := range section.items {
			fmt.Printf("  %s\n", item)
		}
	}

	return nil
}

// formatItems maps a typed slice to pre-formatted display strings.
func formatItems[T any](items []T, format func(T) string) []string {
	result := make([]string, len(items))
	for i, item := range items {
		result[i] = format(item)
	}
	return result
}

// formatGrantsOutput writes a human-readable view of a principal's grants
// and denials.
func formatGrantsOutput(response *observe.GrantsResponse) error {
	return formatPolicySections(response.Principal, []policySection{
		{"GRANTS", formatItems(response.Grants, formatGrant)},
		{"DENIALS", formatItems(response.Denials, formatDenial)},
	})
}

// formatAllowancesOutput writes a human-readable view of a principal's
// allowances and allowance denials.
func formatAllowancesOutput(response *observe.AllowancesResponse) error {
	return formatPolicySections(response.Principal, []policySection{
		{"ALLOWANCES", formatItems(response.Allowances, formatAllowance)},
		{"ALLOWANCE DENIALS", formatItems(response.AllowanceDenials, formatAllowanceDenial)},
	})
}
