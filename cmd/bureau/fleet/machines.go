// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// --- list-machines ---

// listMachinesParams holds the parameters for the list-machines command.
type listMachinesParams struct {
	FleetConnection
	cli.JSONOutput
}

// listMachinesResult is the JSON output of list-machines.
type listMachinesResult struct {
	Machines []machineEntry `json:"machines" desc:"tracked machines"`
}

func listMachinesCommand() *cli.Command {
	var params listMachinesParams

	return &cli.Command{
		Name:    "list-machines",
		Summary: "List tracked machines with resource usage",
		Description: `Display all machines the fleet controller is tracking. Shows
hostname, CPU and memory utilization, GPU count, label set, and
the number of assigned principals.`,
		Usage: "bureau fleet list-machines [flags]",
		Examples: []cli.Example{
			{
				Description: "List all machines",
				Command:     "bureau fleet list-machines",
			},
			{
				Description: "List machines as JSON",
				Command:     "bureau fleet list-machines --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &listMachinesResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/fleet/list-machines"},
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			var response machinesResponse
			if err := client.Call(ctx, "list-machines", nil, &response); err != nil {
				return cli.Internal("listing machines: %w", err)
			}

			if done, err := params.EmitJSON(listMachinesResult{Machines: response.Machines}); done {
				return err
			}

			if len(response.Machines) == 0 {
				fmt.Fprintln(os.Stderr, "No machines tracked.")
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(writer, "MACHINE\tHOSTNAME\tCPU\tMEMORY\tGPUS\tASSIGNMENTS\tLABELS\n")
			for _, machine := range response.Machines {
				memoryDisplay := "-"
				if machine.MemoryTotalMB > 0 {
					memoryDisplay = fmt.Sprintf("%d/%d MB", machine.MemoryUsedMB, machine.MemoryTotalMB)
				}
				labels := formatLabels(machine.Labels)
				fmt.Fprintf(writer, "%s\t%s\t%d%%\t%s\t%d\t%d\t%s\n",
					machine.Localpart,
					machine.Hostname,
					machine.CPUPercent,
					memoryDisplay,
					machine.GPUCount,
					machine.Assignments,
					labels,
				)
			}
			writer.Flush()
			return nil
		},
	}
}

// --- show-machine ---

// showMachineParams holds the parameters for the show-machine command.
type showMachineParams struct {
	FleetConnection
	cli.JSONOutput
}

// showMachineResult is the JSON output of show-machine.
type showMachineResult struct {
	Localpart    string                       `json:"localpart"      desc:"machine localpart"`
	Info         *schema.MachineInfo          `json:"info"           desc:"hardware info (nil if not yet reported)"`
	Status       *schema.MachineStatus        `json:"status"         desc:"current status (nil if not yet reported)"`
	Assignments  []schema.PrincipalAssignment `json:"assignments"    desc:"principals assigned to this machine"`
	ConfigRoomID string                       `json:"config_room_id" desc:"config room Matrix ID"`
}

// showMachineResponse mirrors the fleet controller's show-machine response.
type showMachineResponse struct {
	Localpart    string                       `cbor:"localpart"`
	Info         *schema.MachineInfo          `cbor:"info"`
	Status       *schema.MachineStatus        `cbor:"status"`
	Assignments  []schema.PrincipalAssignment `cbor:"assignments"`
	ConfigRoomID string                       `cbor:"config_room_id"`
}

func showMachineCommand() *cli.Command {
	var params showMachineParams

	return &cli.Command{
		Name:    "show-machine",
		Summary: "Show detailed info for a machine",
		Usage:   "bureau fleet show-machine <localpart> [flags]",
		Description: `Display the full state of a single tracked machine: hardware info,
current resource usage, and all assigned principals.`,
		Examples: []cli.Example{
			{
				Description: "Show machine details",
				Command:     "bureau fleet show-machine machine/workstation",
			},
			{
				Description: "Show machine details as JSON",
				Command:     "bureau fleet show-machine machine/workstation --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &showMachineResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/fleet/show-machine"},
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("machine localpart required\n\nUsage: bureau fleet show-machine <localpart> [flags]")
			}
			machineLocalpart := args[0]

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			var response showMachineResponse
			if err := client.Call(ctx, "show-machine", map[string]any{
				"machine": machineLocalpart,
			}, &response); err != nil {
				return cli.Internal("showing machine: %w", err)
			}

			result := showMachineResult{
				Localpart:    response.Localpart,
				Info:         response.Info,
				Status:       response.Status,
				Assignments:  response.Assignments,
				ConfigRoomID: response.ConfigRoomID,
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			// Text output.
			writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(writer, "Machine:\t%s\n", result.Localpart)
			fmt.Fprintf(writer, "Config Room:\t%s\n", result.ConfigRoomID)

			if result.Info != nil {
				fmt.Fprintf(writer, "\nHardware Info:\n")
				fmt.Fprintf(writer, "  Hostname:\t%s\n", result.Info.Hostname)
				fmt.Fprintf(writer, "  CPU:\t%s (%d sockets, %d cores/socket)\n",
					result.Info.CPU.Model,
					result.Info.CPU.Sockets,
					result.Info.CPU.CoresPerSocket)
				fmt.Fprintf(writer, "  Memory:\t%d MB\n", result.Info.MemoryTotalMB)
				if len(result.Info.GPUs) > 0 {
					fmt.Fprintf(writer, "  GPUs:\t%d\n", len(result.Info.GPUs))
					for _, gpu := range result.Info.GPUs {
						vramMB := gpu.VRAMTotalBytes / (1024 * 1024)
						fmt.Fprintf(writer, "    %s\t%d MB VRAM\n", gpu.ModelName, vramMB)
					}
				}
				if len(result.Info.Labels) > 0 {
					fmt.Fprintf(writer, "  Labels:\t%s\n", formatLabels(result.Info.Labels))
				}
			}

			if result.Status != nil {
				fmt.Fprintf(writer, "\nCurrent Status:\n")
				fmt.Fprintf(writer, "  CPU:\t%d%%\n", result.Status.CPUPercent)
				fmt.Fprintf(writer, "  Memory:\t%d MB used\n", result.Status.MemoryUsedMB)
			}

			if len(result.Assignments) > 0 {
				fmt.Fprintf(writer, "\nAssignments (%d):\n", len(result.Assignments))
				for _, assignment := range result.Assignments {
					autoStart := "manual"
					if assignment.AutoStart {
						autoStart = "auto"
					}
					fmt.Fprintf(writer, "  %s\t%s\t%s\n", assignment.Principal.Localpart(), assignment.Template, autoStart)
				}
			}
			writer.Flush()
			return nil
		},
	}
}

// formatLabels formats a label map as "key=value, ..." for display.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(labels))
	for key, value := range labels {
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, ", ")
}
