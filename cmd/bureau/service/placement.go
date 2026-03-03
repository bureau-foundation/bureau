// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"log/slog"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/fleet"
)

// --- place ---

// placeParams holds the parameters for the place command.
type placeParams struct {
	fleet.FleetConnection
	cli.JSONOutput
	Machine string `json:"machine" flag:"machine" desc:"target machine localpart (if omitted, the scoring engine selects)"`
}

// placeResult is the JSON output of the place command.
type placeResult struct {
	Service string `json:"service" desc:"service that was placed"`
	Machine string `json:"machine" desc:"machine where the service was placed"`
	Score   int    `json:"score"   desc:"placement score (-1 for manual placement)"`
}

func placeCommand() *cli.Command {
	var params placeParams

	return &cli.Command{
		Name:    "place",
		Summary: "Place a service on a machine via the fleet controller",
		Usage:   "bureau service place <service> [flags]",
		Description: `Place a fleet-managed service on a machine. If --machine is omitted,
the scoring engine selects the best eligible candidate based on
resource availability, placement constraints, and anti-affinity rules.

If --machine is specified, the service is placed on that machine
directly (manual placement bypasses scoring but still validates
eligibility).

Requires a reachable fleet controller.`,
		Examples: []cli.Example{
			{
				Description: "Place a service (auto-select machine)",
				Command:     "bureau service place service/stt/whisper",
			},
			{
				Description: "Place on a specific machine",
				Command:     "bureau service place service/stt/whisper --machine machine/gpu-server-1",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &placeResult{} },
		Annotations:    cli.Create(),
		RequiredGrants: []string{"command/service/place"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("service localpart required\n\nUsage: bureau service place <service> [flags]")
			}
			serviceLocalpart := args[0]

			client, err := params.Connect()
			if err != nil {
				return err
			}

			ctx, cancel := fleet.CallContext(ctx)
			defer cancel()

			fields := map[string]any{
				"service": serviceLocalpart,
			}
			if params.Machine != "" {
				fields["machine"] = params.Machine
			}

			var response fleet.PlaceResponse
			if err := client.Call(ctx, "place", fields, &response); err != nil {
				return cli.Transient("placing service %q: %w", serviceLocalpart, err).
					WithHint("Run 'bureau service list' to verify the service exists, " +
						"and 'bureau machine list' to verify eligible machines.")
			}

			result := placeResult{
				Service: response.Service,
				Machine: response.Machine,
				Score:   response.Score,
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			logger.Info("placed service", "service", result.Service, "machine", result.Machine, "score", result.Score)
			return nil
		},
	}
}

// --- unplace ---

// unplaceParams holds the parameters for the unplace command.
type unplaceParams struct {
	fleet.FleetConnection
	cli.JSONOutput
	Machine string `json:"machine" flag:"machine" desc:"machine to remove the service from (required)"`
}

// unplaceResult is the JSON output of the unplace command.
type unplaceResult struct {
	Service string `json:"service" desc:"service that was removed"`
	Machine string `json:"machine" desc:"machine the service was removed from"`
}

func unplaceCommand() *cli.Command {
	var params unplaceParams

	return &cli.Command{
		Name:    "unplace",
		Summary: "Remove a service from a machine via the fleet controller",
		Usage:   "bureau service unplace <service> --machine <machine> [flags]",
		Description: `Remove a fleet-managed service from a specific machine. This removes
the PrincipalAssignment from the machine's config room, causing the
daemon to tear down the service's sandbox.

Requires a reachable fleet controller.`,
		Examples: []cli.Example{
			{
				Description: "Remove a service from a machine",
				Command:     "bureau service unplace service/stt/whisper --machine machine/workstation",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &unplaceResult{} },
		Annotations:    cli.Destructive(),
		RequiredGrants: []string{"command/service/unplace"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("service localpart required\n\nUsage: bureau service unplace <service> --machine <machine> [flags]")
			}
			serviceLocalpart := args[0]
			if params.Machine == "" {
				return cli.Validation("--machine is required")
			}

			client, err := params.Connect()
			if err != nil {
				return err
			}

			ctx, cancel := fleet.CallContext(ctx)
			defer cancel()

			var response fleet.UnplaceResponse
			if err := client.Call(ctx, "unplace", map[string]any{
				"service": serviceLocalpart,
				"machine": params.Machine,
			}, &response); err != nil {
				return cli.Transient("removing service %q from machine %q: %w", serviceLocalpart, params.Machine, err).
					WithHint("Run 'bureau service show " + serviceLocalpart + "' to check current placement.")
			}

			result := unplaceResult{
				Service: response.Service,
				Machine: response.Machine,
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			logger.Info("removed service from machine", "service", result.Service, "machine", result.Machine)
			return nil
		},
	}
}

// --- plan ---

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
	var params struct {
		fleet.FleetConnection
		cli.JSONOutput
	}

	return &cli.Command{
		Name:    "plan",
		Summary: "Dry-run placement scoring for a service",
		Usage:   "bureau service plan <service> [flags]",
		Description: `Evaluate placement scoring for a service without making any changes.
Shows all eligible machines with their scores and the service's current
placement. Use this to preview what "place" would choose.

Requires a reachable fleet controller.`,
		Examples: []cli.Example{
			{
				Description: "Preview placement scoring",
				Command:     "bureau service plan service/stt/whisper",
			},
			{
				Description: "Preview scoring as JSON",
				Command:     "bureau service plan service/stt/whisper --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &planResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/service/plan"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("service localpart required\n\nUsage: bureau service plan <service> [flags]")
			}
			serviceLocalpart := args[0]

			client, err := params.Connect()
			if err != nil {
				return err
			}

			ctx, cancel := fleet.CallContext(ctx)
			defer cancel()

			var response fleet.PlanResponse
			if err := client.Call(ctx, "plan", map[string]any{
				"service": serviceLocalpart,
			}, &response); err != nil {
				return cli.Transient("planning placement for service %q: %w", serviceLocalpart, err).
					WithHint("Run 'bureau service list' to verify the service exists.")
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

			logger.Info("placement plan",
				"service", result.Service,
				"current_machines", result.CurrentMachines,
				"candidate_count", len(result.Candidates),
			)
			for _, candidate := range result.Candidates {
				logger.Info("candidate", "machine", candidate.Machine, "score", candidate.Score)
			}

			return nil
		},
	}
}
