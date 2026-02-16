// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"fmt"
	"strings"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/observe"
)

// checkParams holds the parameters for the auth check command.
type checkParams struct {
	cli.JSONOutput
	Actor      string `json:"actor"       flag:"actor"       desc:"localpart of the acting principal" required:"true"`
	Action     string `json:"action"      flag:"action"      desc:"action to check (e.g., observe, ticket/create)" required:"true"`
	Target     string `json:"target"      flag:"target"      desc:"localpart of the target principal (empty for self-service)"`
	SocketPath string `json:"-"           flag:"socket"      desc:"daemon observation socket path" default:"/run/bureau/observe.sock"`
}

func checkCommand() *cli.Command {
	var params checkParams

	return &cli.Command{
		Name:    "check",
		Summary: "Evaluate an authorization check with full trace",
		Description: `Evaluate whether a principal can perform an action, showing the full
evaluation trace. The check runs against the daemon's live authorization
index — the same policy used for runtime access decisions.

The output shows:
  - The decision (allow or deny) and the reason for denial
  - Which specific grant, denial, allowance, or allowance denial matched
  - The complete policy context for both actor and target

For self-service actions (no --target), only the subject side (grants and
denials) is checked. For cross-principal actions, both sides must agree.`,
		Usage: "bureau auth check --actor <principal> --action <action> [--target <principal>] [flags]",
		Examples: []cli.Example{
			{
				Description: "Check if an agent can observe another",
				Command:     "bureau auth check --actor iree/amdgpu/pm --action observe --target iree/amdgpu/compiler",
			},
			{
				Description: "Check a self-service action (no target)",
				Command:     "bureau auth check --actor iree/amdgpu/pm --action command/pipeline/list",
			},
			{
				Description: "Check interactive observation access",
				Command:     "bureau auth check --actor iree/amdgpu/pm --action observe/read-write --target service/stt/whisper",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &observe.AuthorizationResponse{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/auth/check"},
		Run: func(args []string) error {
			if params.Actor == "" {
				return cli.Validation("--actor is required")
			}
			if params.Action == "" {
				return cli.Validation("--action is required")
			}

			operatorSession, err := cli.LoadSession()
			if err != nil {
				return err
			}

			response, err := observe.QueryAuthorization(params.SocketPath, observe.AuthorizationRequest{
				Actor:      params.Actor,
				AuthAction: params.Action,
				Target:     params.Target,
				Observer:   operatorSession.UserID,
				Token:      operatorSession.AccessToken,
			})
			if err != nil {
				return err
			}

			if done, jsonErr := params.EmitJSON(response); done {
				return jsonErr
			}

			return formatCheckOutput(response, params.Actor, params.Action, params.Target)
		},
	}
}

// formatCheckOutput writes a human-readable authorization check trace.
func formatCheckOutput(response *observe.AuthorizationResponse, actor, action, target string) error {
	// Decision header.
	if response.Decision == "allow" {
		fmt.Println("DECISION: allow")
	} else {
		fmt.Printf("DECISION: deny\n")
		fmt.Printf("REASON:   %s\n", response.Reason)
	}
	fmt.Println()

	// Evaluation trace.
	fmt.Println("EVALUATION:")
	formatEvaluationTrace(response, actor, action, target)
	fmt.Println()

	// Actor policy.
	fmt.Printf("ACTOR POLICY (%s):\n", actor)
	formatGrantList("  Grants:", response.ActorGrants, response.MatchedGrant)
	formatDenialList("  Denials:", response.ActorDenials, response.MatchedDenial)

	// Target policy (cross-principal only).
	if target != "" {
		fmt.Println()
		fmt.Printf("TARGET POLICY (%s):\n", target)
		formatAllowanceList("  Allowances:", response.TargetAllowances, response.MatchedAllowance)
		formatAllowanceDenialList("  Allowance Denials:", response.TargetAllowanceDenials, response.MatchedAllowanceDenial)
	}

	return nil
}

// formatEvaluationTrace prints the step-by-step evaluation path.
func formatEvaluationTrace(response *observe.AuthorizationResponse, actor, action, target string) {
	step := 1

	if target == "" {
		fmt.Printf("  %d. Searching %d grants for actor %q\n", step, len(response.ActorGrants), actor)
		fmt.Printf("     Action %q (self-service)\n", action)
	} else {
		fmt.Printf("  %d. Searching %d grants for actor %q\n", step, len(response.ActorGrants), actor)
		fmt.Printf("     Action %q on target %q\n", action, target)
	}

	if response.MatchedGrant != nil {
		fmt.Printf("     -> Grant matched: %s\n", formatGrant(*response.MatchedGrant))
	} else {
		fmt.Printf("     -> No matching grant found\n")
		return
	}
	step++

	// Denial check.
	fmt.Printf("  %d. Checking %d denials for actor %q\n", step, len(response.ActorDenials), actor)
	if response.MatchedDenial != nil {
		fmt.Printf("     -> Denial matched: %s\n", formatDenial(*response.MatchedDenial))
		return
	}
	fmt.Printf("     -> No denial matched\n")
	step++

	// Target-side checks (cross-principal only).
	if target == "" {
		return
	}

	fmt.Printf("  %d. Searching %d allowances on target %q\n", step, len(response.TargetAllowances), target)
	if response.MatchedAllowance != nil {
		fmt.Printf("     -> Allowance matched: %s\n", formatAllowance(*response.MatchedAllowance))
	} else {
		fmt.Printf("     -> No matching allowance found\n")
		return
	}
	step++

	fmt.Printf("  %d. Checking %d allowance denials on target %q\n", step, len(response.TargetAllowanceDenials), target)
	if response.MatchedAllowanceDenial != nil {
		fmt.Printf("     -> Allowance denial matched: %s\n", formatAllowanceDenial(*response.MatchedAllowanceDenial))
	} else {
		fmt.Printf("     -> No allowance denial matched\n")
	}
}

// formatGrantList prints a labeled list of grants, marking the matched one.
func formatGrantList(label string, grants []schema.Grant, matched *schema.Grant) {
	fmt.Println(label)
	if len(grants) == 0 {
		fmt.Println("    (none)")
		return
	}
	for _, grant := range grants {
		marker := ""
		if matched != nil && grantsEqual(grant, *matched) {
			marker = "  <- matched"
		}
		fmt.Printf("    %s%s\n", formatGrant(grant), marker)
	}
}

// formatDenialList prints a labeled list of denials, marking the matched one.
func formatDenialList(label string, denials []schema.Denial, matched *schema.Denial) {
	fmt.Println(label)
	if len(denials) == 0 {
		fmt.Println("    (none)")
		return
	}
	for _, denial := range denials {
		marker := ""
		if matched != nil && denialsEqual(denial, *matched) {
			marker = "  <- matched"
		}
		fmt.Printf("    %s%s\n", formatDenial(denial), marker)
	}
}

// formatAllowanceList prints a labeled list of allowances, marking the matched one.
func formatAllowanceList(label string, allowances []schema.Allowance, matched *schema.Allowance) {
	fmt.Println(label)
	if len(allowances) == 0 {
		fmt.Println("    (none)")
		return
	}
	for _, allowance := range allowances {
		marker := ""
		if matched != nil && allowancesEqual(allowance, *matched) {
			marker = "  <- matched"
		}
		fmt.Printf("    %s%s\n", formatAllowance(allowance), marker)
	}
}

// formatAllowanceDenialList prints a labeled list of allowance denials,
// marking the matched one.
func formatAllowanceDenialList(label string, denials []schema.AllowanceDenial, matched *schema.AllowanceDenial) {
	fmt.Println(label)
	if len(denials) == 0 {
		fmt.Println("    (none)")
		return
	}
	for _, denial := range denials {
		marker := ""
		if matched != nil && allowanceDenialsEqual(denial, *matched) {
			marker = "  <- matched"
		}
		fmt.Printf("    %s%s\n", formatAllowanceDenial(denial), marker)
	}
}

// Formatting helpers for individual rules.

func formatGrant(grant schema.Grant) string {
	parts := []string{fmt.Sprintf("actions: [%s]", strings.Join(grant.Actions, ", "))}
	if len(grant.Targets) > 0 {
		parts = append(parts, fmt.Sprintf("targets: [%s]", strings.Join(grant.Targets, ", ")))
	}
	if grant.ExpiresAt != "" {
		parts = append(parts, fmt.Sprintf("expires: %s", grant.ExpiresAt))
	}
	if grant.Ticket != "" {
		parts = append(parts, fmt.Sprintf("ticket: %s", grant.Ticket))
	}
	return strings.Join(parts, ", ")
}

func formatDenial(denial schema.Denial) string {
	parts := []string{fmt.Sprintf("actions: [%s]", strings.Join(denial.Actions, ", "))}
	if len(denial.Targets) > 0 {
		parts = append(parts, fmt.Sprintf("targets: [%s]", strings.Join(denial.Targets, ", ")))
	}
	return strings.Join(parts, ", ")
}

func formatAllowance(allowance schema.Allowance) string {
	return fmt.Sprintf("actions: [%s], actors: [%s]",
		strings.Join(allowance.Actions, ", "),
		strings.Join(allowance.Actors, ", "))
}

func formatAllowanceDenial(denial schema.AllowanceDenial) string {
	return fmt.Sprintf("actions: [%s], actors: [%s]",
		strings.Join(denial.Actions, ", "),
		strings.Join(denial.Actors, ", "))
}

// Equality helpers for marking matched rules. These compare the fields
// that matter for identity (actions, targets, actors) — not metadata
// like GrantedBy which doesn't affect matching.

func grantsEqual(a, b schema.Grant) bool {
	return slicesEqual(a.Actions, b.Actions) &&
		slicesEqual(a.Targets, b.Targets) &&
		a.ExpiresAt == b.ExpiresAt &&
		a.Ticket == b.Ticket
}

func denialsEqual(a, b schema.Denial) bool {
	return slicesEqual(a.Actions, b.Actions) &&
		slicesEqual(a.Targets, b.Targets)
}

func allowancesEqual(a, b schema.Allowance) bool {
	return slicesEqual(a.Actions, b.Actions) &&
		slicesEqual(a.Actors, b.Actors)
}

func allowanceDenialsEqual(a, b schema.AllowanceDenial) bool {
	return slicesEqual(a.Actions, b.Actions) &&
		slicesEqual(a.Actors, b.Actors)
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
