// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// --- place ---

// placeParams holds the parameters for the place command.
type placeParams struct {
	FleetConnection
	cli.JSONOutput
	Machine string `json:"machine" flag:"machine" desc:"target machine localpart (if omitted, the scoring engine selects)"`
}

// placeResult is the JSON output of the place command.
type placeResult struct {
	Service string `json:"service" desc:"service that was placed"`
	Machine string `json:"machine" desc:"machine where the service was placed"`
	Score   int    `json:"score"   desc:"placement score (-1 for manual placement)"`
}

// placeResponse mirrors the fleet controller's place response.
type placeResponse struct {
	Service string `cbor:"service"`
	Machine string `cbor:"machine"`
	Score   int    `cbor:"score"`
}

func placeCommand() *cli.Command {
	var params placeParams

	return &cli.Command{
		Name:    "place",
		Summary: "Place a service on a machine",
		Usage:   "bureau fleet place <service> [flags]",
		Description: `Place a fleet-managed service on a machine. If --machine is omitted,
the scoring engine selects the best eligible candidate based on
resource availability, placement constraints, and anti-affinity rules.

If --machine is specified, the service is placed on that machine
directly (manual placement bypasses scoring but still validates
eligibility).`,
		Examples: []cli.Example{
			{
				Description: "Place a service (auto-select machine)",
				Command:     "bureau fleet place service/stt/whisper",
			},
			{
				Description: "Place on a specific machine",
				Command:     "bureau fleet place service/stt/whisper --machine machine/gpu-server-1",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &placeResult{} },
		Annotations:    cli.Create(),
		RequiredGrants: []string{"command/fleet/place"},
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("service localpart required\n\nUsage: bureau fleet place <service> [flags]")
			}
			serviceLocalpart := args[0]

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			fields := map[string]any{
				"service": serviceLocalpart,
			}
			if params.Machine != "" {
				fields["machine"] = params.Machine
			}

			var response placeResponse
			if err := client.Call(ctx, "place", fields, &response); err != nil {
				return cli.Internal("placing service: %w", err)
			}

			result := placeResult{
				Service: response.Service,
				Machine: response.Machine,
				Score:   response.Score,
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			fmt.Fprintf(os.Stderr, "Placed %s on %s", result.Service, result.Machine)
			if result.Score >= 0 {
				fmt.Fprintf(os.Stderr, " (score: %d)", result.Score)
			}
			fmt.Fprintln(os.Stderr)
			return nil
		},
	}
}

// --- unplace ---

// unplaceParams holds the parameters for the unplace command.
type unplaceParams struct {
	FleetConnection
	cli.JSONOutput
	Machine string `json:"machine" flag:"machine" desc:"machine to remove the service from (required)"`
}

// unplaceResult is the JSON output of the unplace command.
type unplaceResult struct {
	Service string `json:"service" desc:"service that was removed"`
	Machine string `json:"machine" desc:"machine the service was removed from"`
}

// unplaceResponse mirrors the fleet controller's unplace response.
type unplaceResponse struct {
	Service string `cbor:"service"`
	Machine string `cbor:"machine"`
}

func unplaceCommand() *cli.Command {
	var params unplaceParams

	return &cli.Command{
		Name:    "unplace",
		Summary: "Remove a service from a machine",
		Usage:   "bureau fleet unplace <service> --machine <machine> [flags]",
		Description: `Remove a fleet-managed service from a specific machine. This removes
the PrincipalAssignment from the machine's config room, causing the
daemon to tear down the service's sandbox.`,
		Examples: []cli.Example{
			{
				Description: "Remove a service from a machine",
				Command:     "bureau fleet unplace service/stt/whisper --machine machine/workstation",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &unplaceResult{} },
		Annotations:    cli.Destructive(),
		RequiredGrants: []string{"command/fleet/unplace"},
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("service localpart required\n\nUsage: bureau fleet unplace <service> --machine <machine> [flags]")
			}
			serviceLocalpart := args[0]
			if params.Machine == "" {
				return cli.Validation("--machine is required")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			var response unplaceResponse
			if err := client.Call(ctx, "unplace", map[string]any{
				"service": serviceLocalpart,
				"machine": params.Machine,
			}, &response); err != nil {
				return cli.Internal("unplacing service: %w", err)
			}

			result := unplaceResult{
				Service: response.Service,
				Machine: response.Machine,
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			fmt.Fprintf(os.Stderr, "Removed %s from %s\n", result.Service, result.Machine)
			return nil
		},
	}
}

// --- plan ---

// planParams holds the parameters for the plan command.
type planParams struct {
	FleetConnection
	cli.JSONOutput
}

// planCandidate mirrors the fleet controller's planCandidate.
type planCandidate struct {
	Machine string `cbor:"machine"`
	Score   int    `cbor:"score"`
}

// planResponse mirrors the fleet controller's plan response.
type planResponseData struct {
	Service         string          `cbor:"service"`
	Candidates      []planCandidate `cbor:"candidates"`
	CurrentMachines []string        `cbor:"current_machines"`
}

// planResult is the JSON output of the plan command.
type planResult struct {
	Service         string              `json:"service"          desc:"service evaluated"`
	Candidates      []planCandidateJSON `json:"candidates"       desc:"scored candidate machines"`
	CurrentMachines []string            `json:"current_machines" desc:"machines currently hosting this service"`
}

// planCandidateJSON is a scored machine in the plan output.
type planCandidateJSON struct {
	Machine string `json:"machine" desc:"candidate machine localpart"`
	Score   int    `json:"score"   desc:"placement score (higher is better)"`
}

func planCommand() *cli.Command {
	var params planParams

	return &cli.Command{
		Name:    "plan",
		Summary: "Dry-run placement scoring for a service",
		Usage:   "bureau fleet plan <service> [flags]",
		Description: `Evaluate placement scoring for a service without making any changes.
Shows all eligible machines with their scores and the service's current
placement. Use this to preview what "place" would choose.`,
		Examples: []cli.Example{
			{
				Description: "Preview placement scoring",
				Command:     "bureau fleet plan service/stt/whisper",
			},
			{
				Description: "Preview scoring as JSON",
				Command:     "bureau fleet plan service/stt/whisper --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &planResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/fleet/plan"},
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("service localpart required\n\nUsage: bureau fleet plan <service> [flags]")
			}
			serviceLocalpart := args[0]

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			var response planResponseData
			if err := client.Call(ctx, "plan", map[string]any{
				"service": serviceLocalpart,
			}, &response); err != nil {
				return cli.Internal("planning placement: %w", err)
			}

			candidates := make([]planCandidateJSON, len(response.Candidates))
			for i, candidate := range response.Candidates {
				candidates[i] = planCandidateJSON{
					Machine: candidate.Machine,
					Score:   candidate.Score,
				}
			}

			result := planResult{
				Service:         response.Service,
				Candidates:      candidates,
				CurrentMachines: response.CurrentMachines,
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			// Text output.
			fmt.Fprintf(os.Stderr, "Placement plan for %s\n\n", result.Service)

			if len(result.CurrentMachines) > 0 {
				fmt.Fprintf(os.Stderr, "Current placement:\n")
				for _, machine := range result.CurrentMachines {
					fmt.Fprintf(os.Stderr, "  %s\n", machine)
				}
				fmt.Fprintln(os.Stderr)
			} else {
				fmt.Fprintf(os.Stderr, "Current placement: none\n\n")
			}

			if len(result.Candidates) > 0 {
				fmt.Fprintf(os.Stderr, "Candidates:\n")
				writer := tabwriter.NewWriter(os.Stderr, 0, 4, 2, ' ', 0)
				fmt.Fprintf(writer, "  MACHINE\tSCORE\n")
				for _, candidate := range result.Candidates {
					fmt.Fprintf(writer, "  %s\t%d\n", candidate.Machine, candidate.Score)
				}
				writer.Flush()
			} else {
				fmt.Fprintf(os.Stderr, "Candidates: none (no eligible machines)\n")
			}

			return nil
		},
	}
}
