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
	"github.com/bureau-foundation/bureau/lib/schema"
)

// showParams holds the parameters for the machine show command.
type showParams struct {
	fleet.FleetConnection
	cli.JSONOutput
}

// showResult is the JSON output of machine show.
type showResult struct {
	Localpart    string                       `json:"localpart"      desc:"machine localpart"`
	Info         *schema.MachineInfo          `json:"info"           desc:"hardware info (nil if not yet reported)"`
	Status       *schema.MachineStatus        `json:"status"         desc:"current status (nil if not yet reported)"`
	Assignments  []schema.PrincipalAssignment `json:"assignments"    desc:"principals assigned to this machine"`
	ConfigRoomID string                       `json:"config_room_id" desc:"config room Matrix ID"`
}

func showCommand() *cli.Command {
	var params showParams

	return &cli.Command{
		Name:    "show",
		Summary: "Show detailed info for a machine",
		Usage:   "bureau machine show <localpart> [flags]",
		Description: `Display the full state of a single machine: hardware info, current
resource usage, and all assigned principals.

Requires a reachable fleet controller.`,
		Examples: []cli.Example{
			{
				Description: "Show machine details",
				Command:     "bureau machine show machine/workstation",
			},
			{
				Description: "Show machine details as JSON",
				Command:     "bureau machine show machine/workstation --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &showResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/machine/show"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("machine localpart required\n\nUsage: bureau machine show <localpart> [flags]")
			}
			machineLocalpart := args[0]

			client, err := params.Connect()
			if err != nil {
				return err
			}

			ctx, cancel := fleet.CallContext(ctx)
			defer cancel()

			var response fleet.ShowMachineResponse
			if err := client.Call(ctx, "show-machine", map[string]any{
				"machine": machineLocalpart,
			}, &response); err != nil {
				return cli.Transient("showing machine %q: %w", machineLocalpart, err).
					WithHint("Run 'bureau machine list' to see available machines.")
			}

			result := showResult{
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
					fmt.Fprintf(writer, "  Labels:\t%s\n", fleet.FormatLabels(result.Info.Labels))
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
