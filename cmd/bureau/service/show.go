// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/fleet"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	fleetschema "github.com/bureau-foundation/bureau/lib/schema/fleet"
)

type serviceShowParams struct {
	cli.SessionConfig
	fleet.FleetConnection
	cli.JSONOutput
	Machine    string `json:"machine"     flag:"machine"     desc:"machine localpart (optional — auto-discovers if omitted)"`
	Fleet      string `json:"fleet"       flag:"fleet"       desc:"fleet prefix (e.g., bureau/fleet/prod) — required when --machine is omitted"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name (auto-detected from machine.conf)"`
}

// serviceInstanceResult is a single instance in the show output.
type serviceInstanceResult struct {
	Machine    string                      `json:"machine"    desc:"machine hosting this instance"`
	Assignment *schema.PrincipalAssignment `json:"assignment" desc:"principal assignment on the machine"`
}

type serviceShowResult struct {
	PrincipalUserID string                           `json:"principal_user_id,omitempty" desc:"full Matrix user ID"`
	MachineName     string                           `json:"machine,omitempty"           desc:"machine hosting this service"`
	Template        string                           `json:"template"                    desc:"template reference"`
	AutoStart       bool                             `json:"auto_start,omitempty"        desc:"whether the service starts automatically"`
	Labels          map[string]string                `json:"labels,omitempty"            desc:"assignment labels"`
	Definition      *fleetschema.FleetServiceContent `json:"definition,omitempty"        desc:"fleet service definition"`
	Instances       []serviceInstanceResult          `json:"instances,omitempty"         desc:"current placement instances"`
}

func showCommand() *cli.Command {
	var params serviceShowParams

	return &cli.Command{
		Name:    "show",
		Summary: "Show detailed information about a service",
		Description: `Display detailed information about a single service principal.

If --machine is omitted, scans all machines to find where the service
is assigned. The scan count is reported for diagnostics.

The output includes the fleet service definition (resources, placement
constraints, failover) and all instances from the fleet controller.
Both Matrix and fleet controller are required.`,
		Usage: "bureau service show <localpart> [--machine <machine>]",
		Examples: []cli.Example{
			{
				Description: "Show service details (auto-discover machine)",
				Command:     "bureau service show service/ticket --credential-file ./creds",
			},
			{
				Description: "Show service on a specific machine",
				Command:     "bureau service show service/ticket --credential-file ./creds --machine machine/workstation",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &serviceShowResult{} },
		RequiredGrants: []string{"command/service/show"},
		Annotations:    cli.ReadOnly(),
		Run: requireLocalpart("bureau service show <localpart> [--machine <machine>]", func(ctx context.Context, localpart string, logger *slog.Logger) error {
			return runShow(ctx, localpart, logger, params)
		}),
	}
}

func runShow(ctx context.Context, localpart string, logger *slog.Logger, params serviceShowParams) error {
	params.ServerName = cli.ResolveServerName(params.ServerName)
	params.Fleet = cli.ResolveFleet(params.Fleet)

	serverName, err := ref.ParseServerName(params.ServerName)
	if err != nil {
		return cli.Validation("invalid --server-name %q: %w", params.ServerName, err)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	var machine ref.Machine
	var fleetRef ref.Fleet
	if params.Machine != "" {
		machine, err = ref.ParseMachine(params.Machine, serverName)
		if err != nil {
			return cli.Validation("invalid machine: %v", err)
		}
	} else {
		fleetRef, err = ref.ParseFleet(params.Fleet, serverName)
		if err != nil {
			return cli.Validation("invalid fleet: %v", err)
		}
	}

	// Matrix resolve: find the actual assignment.
	var result serviceShowResult
	matrixFound := false

	location, machineCount, matrixErr := principal.Resolve(ctx, session, localpart, machine, fleetRef)
	if matrixErr == nil {
		matrixFound = true
		if machine.IsZero() && machineCount > 0 {
			logger.Info("resolved service location", "machine", location.Machine.Localpart(), "machines_scanned", machineCount)
		}

		serviceRef, parseErr := ref.ParseService(localpart, serverName)
		if parseErr != nil {
			return cli.Validation("invalid service localpart: %v", parseErr)
		}

		result.PrincipalUserID = serviceRef.UserID().String()
		result.MachineName = location.Machine.Localpart()
		result.Template = location.Assignment.Template
		result.AutoStart = location.Assignment.AutoStart
		result.Labels = location.Assignment.Labels
	}

	// Fleet controller: fleet definition and instance data.
	fleetClient, err := params.FleetConnection.Connect()
	if err != nil {
		return err
	}

	fleetCtx, fleetCancel := fleet.CallContext(ctx)
	defer fleetCancel()

	fleetFound := false
	var fleetResponse fleet.ShowServiceResponse
	if err := fleetClient.Call(fleetCtx, "show-service", map[string]any{
		"service": localpart,
	}, &fleetResponse); err != nil {
		// The fleet connection is required, but a specific service not
		// being in the fleet controller's index is valid (manually placed
		// services without fleet definitions). Log and continue.
		logger.Debug("fleet show-service returned no data for this service", "error", err)
	} else {
		fleetFound = true
		result.Definition = fleetResponse.Definition
		instances := make([]serviceInstanceResult, len(fleetResponse.Instances))
		for i, instance := range fleetResponse.Instances {
			instances[i] = serviceInstanceResult{
				Machine:    instance.Machine,
				Assignment: instance.Assignment,
			}
		}
		result.Instances = instances

		// If Matrix didn't find the service but fleet did, fill in
		// basic fields from the fleet definition.
		if !matrixFound && fleetResponse.Definition != nil {
			result.Template = fleetResponse.Definition.Template
		}
	}

	// If neither source found the service, report not found.
	if !matrixFound && !fleetFound {
		return cli.NotFound("service %q not found: %w", localpart, matrixErr).
			WithHint("Run 'bureau service list' to see running services.")
	}

	if done, err := params.EmitJSON(result); done {
		return err
	}

	// Text output.
	if matrixFound {
		fmt.Printf("Principal:   %s\n", result.PrincipalUserID)
		fmt.Printf("Machine:     %s\n", result.MachineName)
		fmt.Printf("Template:    %s\n", result.Template)
		fmt.Printf("Auto-start:  %v\n", result.AutoStart)

		if len(result.Labels) > 0 {
			fmt.Printf("Labels:\n")
			for key, value := range result.Labels {
				fmt.Printf("  %s: %s\n", key, value)
			}
		}
	} else {
		fmt.Printf("Service:     %s (fleet-defined, not yet placed)\n", localpart)
		if result.Template != "" {
			fmt.Printf("Template:    %s\n", result.Template)
		}
	}

	if result.Definition != nil {
		writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(writer, "\nFleet Definition:\n")
		fmt.Fprintf(writer, "  Template:\t%s\n", result.Definition.Template)
		fmt.Fprintf(writer, "  Replicas:\t%d (min) / %d (max)\n",
			result.Definition.Replicas.Min, result.Definition.Replicas.Max)
		fmt.Fprintf(writer, "  Priority:\t%d\n", result.Definition.Priority)
		fmt.Fprintf(writer, "  Failover:\t%s\n", result.Definition.Failover)
		if result.Definition.HAClass != "" {
			fmt.Fprintf(writer, "  HA Class:\t%s\n", result.Definition.HAClass)
		}
		if result.Definition.Resources.MemoryMB > 0 {
			fmt.Fprintf(writer, "  Memory:\t%d MB\n", result.Definition.Resources.MemoryMB)
		}
		if result.Definition.Resources.CPUMillicores > 0 {
			fmt.Fprintf(writer, "  CPU:\t%d millicores\n", result.Definition.Resources.CPUMillicores)
		}
		if result.Definition.Resources.GPU {
			fmt.Fprintf(writer, "  GPU:\trequired")
			if result.Definition.Resources.GPUMemoryMB > 0 {
				fmt.Fprintf(writer, " (%d MB VRAM)", result.Definition.Resources.GPUMemoryMB)
			}
			fmt.Fprintln(writer)
		}
		if len(result.Definition.Placement.Requires) > 0 {
			fmt.Fprintf(writer, "  Requires:\t%s\n",
				fleet.FormatStringList(result.Definition.Placement.Requires))
		}
		writer.Flush()
	}

	if len(result.Instances) > 0 {
		fmt.Printf("\nInstances (%d):\n", len(result.Instances))
		for _, instance := range result.Instances {
			template := ""
			if instance.Assignment != nil {
				template = instance.Assignment.Template
			}
			fmt.Printf("  %s  %s\n", instance.Machine, template)
		}
	} else if result.Definition != nil {
		fmt.Printf("\nInstances:  none\n")
	}

	return nil
}
