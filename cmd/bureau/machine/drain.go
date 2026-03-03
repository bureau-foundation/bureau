// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/fleet"
)

// drainParams holds the parameters for the machine drain command.
type drainParams struct {
	fleet.FleetConnection
	cli.JSONOutput
}

// drainResult is the JSON output of machine drain.
type drainResult struct {
	Machine  string             `json:"machine"  desc:"machine that was drained"`
	Moved    []drainMovedResult `json:"moved"    desc:"services that were relocated"`
	Stuck    []drainStuckResult `json:"stuck"    desc:"services that could not be relocated"`
	Cordoned bool               `json:"cordoned" desc:"whether the machine was cordoned by this drain"`
}

// drainMovedResult describes a service that was successfully relocated.
type drainMovedResult struct {
	Service   string `json:"service"    desc:"service localpart"`
	ToMachine string `json:"to_machine" desc:"destination machine"`
	Score     int    `json:"score"      desc:"placement score on destination"`
}

// drainStuckResult describes a service that could not be relocated.
type drainStuckResult struct {
	Service string `json:"service" desc:"service localpart"`
	Reason  string `json:"reason"  desc:"why the service could not be moved"`
}

func drainCommand() *cli.Command {
	var params drainParams

	return &cli.Command{
		Name:    "drain",
		Summary: "Drain all services from a machine",
		Description: `Evacuate all fleet-managed services from a machine, redistributing
them across the fleet via the placement scoring engine. The machine is
automatically cordoned first to prevent new placements during and after
the drain.

Each service is relocated independently: the scoring engine finds the
best eligible alternative, places the service there, then removes it
from the source machine. If some services cannot be relocated (no
eligible candidate), the drain continues with the rest and reports
stuck services in the output.

After maintenance, run "bureau machine uncordon" to re-enable the
machine for fleet placements.`,
		Usage: "bureau machine drain <machine-localpart> [flags]",
		Examples: []cli.Example{
			{
				Description: "Drain a machine before maintenance",
				Command:     "bureau machine drain bureau/fleet/prod/machine/worker-01 --service",
			},
			{
				Description: "Drain with JSON output",
				Command:     "bureau machine drain bureau/fleet/prod/machine/worker-01 --service --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &drainResult{} },
		Annotations:    cli.Destructive(),
		RequiredGrants: []string{"command/machine/drain"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("machine localpart required\n\nUsage: bureau machine drain <machine-localpart> [flags]")
			}
			machineLocalpart := args[0]

			client, err := params.Connect()
			if err != nil {
				return err
			}

			ctx, cancel := fleet.DrainCallContext(ctx)
			defer cancel()

			var response fleet.DrainResponse
			if err := client.Call(ctx, "drain", map[string]any{
				"machine": machineLocalpart,
			}, &response); err != nil {
				return cli.Transient("draining machine %q: %w", machineLocalpart, err).
					WithHint("Run 'bureau machine show " + machineLocalpart + "' to check current assignments.")
			}

			moved := make([]drainMovedResult, len(response.Moved))
			for i, entry := range response.Moved {
				moved[i] = drainMovedResult{
					Service:   entry.Service,
					ToMachine: entry.ToMachine,
					Score:     entry.Score,
				}
			}

			stuck := make([]drainStuckResult, len(response.Stuck))
			for i, entry := range response.Stuck {
				stuck[i] = drainStuckResult{
					Service: entry.Service,
					Reason:  entry.Reason,
				}
			}

			result := drainResult{
				Machine:  response.Machine,
				Moved:    moved,
				Stuck:    stuck,
				Cordoned: response.Cordoned,
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			// Text output.
			cordonStatus := "no (already cordoned)"
			if result.Cordoned {
				cordonStatus = "yes"
			}

			writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(writer, "Machine:\t%s\n", result.Machine)
			fmt.Fprintf(writer, "Cordoned:\t%s\n", cordonStatus)
			fmt.Fprintf(writer, "Moved:\t%d services\n", len(result.Moved))
			fmt.Fprintf(writer, "Stuck:\t%d services\n", len(result.Stuck))
			writer.Flush()

			if len(result.Moved) > 0 {
				fmt.Println()
				writer = tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(writer, "SERVICE\tDESTINATION\tSCORE")
				for _, entry := range result.Moved {
					fmt.Fprintf(writer, "%s\t%s\t%d\n", entry.Service, entry.ToMachine, entry.Score)
				}
				writer.Flush()
			}

			if len(result.Stuck) > 0 {
				fmt.Println()
				writer = tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(writer, "STUCK SERVICE\tREASON")
				for _, entry := range result.Stuck {
					fmt.Fprintf(writer, "%s\t%s\n", entry.Service, entry.Reason)
				}
				writer.Flush()

				logger.Warn("some services could not be moved",
					"stuck", len(result.Stuck),
					"hint", "Run 'bureau machine show "+machineLocalpart+"' to inspect remaining assignments.",
				)
			}

			return nil
		},
	}
}
