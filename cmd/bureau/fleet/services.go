// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
	fleetschema "github.com/bureau-foundation/bureau/lib/schema/fleet"
)

// --- list-services ---

// listServicesParams holds the parameters for the list-services command.
type listServicesParams struct {
	FleetConnection
	cli.JSONOutput
}

// serviceEntry mirrors the fleet controller's serviceSummary.
type serviceEntry struct {
	Localpart string `cbor:"localpart"`
	Template  string `cbor:"template"`
	Replicas  int    `cbor:"replicas_min"`
	Instances int    `cbor:"instances"`
	Failover  string `cbor:"failover"`
	Priority  int    `cbor:"priority"`
}

// listServicesResponse mirrors the fleet controller's list-services response.
type listServicesResponse struct {
	Services []serviceEntry `cbor:"services"`
}

// listServicesResult is the JSON output of list-services.
type listServicesResult struct {
	Services []serviceEntry `json:"services" desc:"fleet-managed services"`
}

func listServicesCommand() *cli.Command {
	var params listServicesParams

	return &cli.Command{
		Name:    "list-services",
		Summary: "List fleet-managed services",
		Description: `Display all services the fleet controller manages. Shows template,
replica count, current instance count, failover policy, and priority.`,
		Usage: "bureau fleet list-services [flags]",
		Examples: []cli.Example{
			{
				Description: "List all fleet services",
				Command:     "bureau fleet list-services",
			},
			{
				Description: "List fleet services as JSON",
				Command:     "bureau fleet list-services --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &listServicesResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/fleet/list-services"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			var response listServicesResponse
			if err := client.Call(ctx, "list-services", nil, &response); err != nil {
				return cli.Transient("listing services: %w", err)
			}

			if done, err := params.EmitJSON(listServicesResult{Services: response.Services}); done {
				return err
			}

			if len(response.Services) == 0 {
				logger.Info("no fleet-managed services")
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(writer, "SERVICE\tTEMPLATE\tREPLICAS\tINSTANCES\tFAILOVER\tPRIORITY\n")
			for _, service := range response.Services {
				template := service.Template
				if template == "" {
					template = "-"
				}
				failover := service.Failover
				if failover == "" {
					failover = "-"
				}
				fmt.Fprintf(writer, "%s\t%s\t%d\t%d\t%s\t%d\n",
					service.Localpart,
					template,
					service.Replicas,
					service.Instances,
					failover,
					service.Priority,
				)
			}
			writer.Flush()
			return nil
		},
	}
}

// --- show-service ---

// showServiceParams holds the parameters for the show-service command.
type showServiceParams struct {
	FleetConnection
	cli.JSONOutput
}

// showServiceInstance mirrors the fleet controller's serviceInstance.
type showServiceInstance struct {
	Machine    string                      `cbor:"machine"`
	Assignment *schema.PrincipalAssignment `cbor:"assignment"`
}

// showServiceResponse mirrors the fleet controller's show-service response.
type showServiceResponse struct {
	Localpart  string                           `cbor:"localpart"`
	Definition *fleetschema.FleetServiceContent `cbor:"definition"`
	Instances  []showServiceInstance            `cbor:"instances"`
}

// showServiceResult is the JSON output of show-service.
type showServiceResult struct {
	Localpart  string                           `json:"localpart"  desc:"service localpart"`
	Definition *fleetschema.FleetServiceContent `json:"definition" desc:"fleet service definition"`
	Instances  []serviceInstanceResult          `json:"instances"  desc:"current placement instances"`
}

// serviceInstanceResult is a single instance in the show-service output.
type serviceInstanceResult struct {
	Machine    string                      `json:"machine"    desc:"machine hosting this instance"`
	Assignment *schema.PrincipalAssignment `json:"assignment" desc:"principal assignment on the machine"`
}

func showServiceCommand() *cli.Command {
	var params showServiceParams

	return &cli.Command{
		Name:    "show-service",
		Summary: "Show a fleet service's definition and placement",
		Usage:   "bureau fleet show-service <localpart> [flags]",
		Description: `Display the full definition and current placement of a fleet-managed
service. Shows template, replicas, resources, placement constraints,
failover policy, and all current instances with their host machines.`,
		Examples: []cli.Example{
			{
				Description: "Show service details",
				Command:     "bureau fleet show-service service/stt/whisper",
			},
			{
				Description: "Show service details as JSON",
				Command:     "bureau fleet show-service service/stt/whisper --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &showServiceResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/fleet/show-service"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("service localpart required\n\nUsage: bureau fleet show-service <localpart> [flags]")
			}
			serviceLocalpart := args[0]

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			var response showServiceResponse
			if err := client.Call(ctx, "show-service", map[string]any{
				"service": serviceLocalpart,
			}, &response); err != nil {
				return cli.Transient("showing service %q: %w", serviceLocalpart, err).
					WithHint("Run 'bureau fleet list-services' to see available services.")
			}

			instances := make([]serviceInstanceResult, len(response.Instances))
			for i, instance := range response.Instances {
				instances[i] = serviceInstanceResult{
					Machine:    instance.Machine,
					Assignment: instance.Assignment,
				}
			}

			result := showServiceResult{
				Localpart:  response.Localpart,
				Definition: response.Definition,
				Instances:  instances,
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			// Text output.
			writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(writer, "Service:\t%s\n", result.Localpart)

			if result.Definition != nil {
				fmt.Fprintf(writer, "\nDefinition:\n")
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
						formatStringList(result.Definition.Placement.Requires))
				}
			}

			if len(result.Instances) > 0 {
				fmt.Fprintf(writer, "\nInstances (%d):\n", len(result.Instances))
				for _, instance := range result.Instances {
					template := ""
					if instance.Assignment != nil {
						template = instance.Assignment.Template
					}
					fmt.Fprintf(writer, "  %s\t%s\n", instance.Machine, template)
				}
			} else {
				fmt.Fprintf(writer, "\nInstances:\tnone\n")
			}
			writer.Flush()
			return nil
		},
	}
}

// formatStringList joins a string slice for display.
func formatStringList(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	result := items[0]
	for _, item := range items[1:] {
		result += ", " + item
	}
	return result
}
