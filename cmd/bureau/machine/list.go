// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/fleet"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// listParams holds the parameters for the machine list command.
type listParams struct {
	cli.SessionConfig
	fleet.FleetConnection
	cli.JSONOutput
}

// listResult is the JSON output of machine list.
type listResult struct {
	Machines      []*MachineEntry `json:"machines"       desc:"machines found"`
	FleetEnriched bool            `json:"fleet_enriched" desc:"whether fleet controller data is included"`
}

func listCommand() *cli.Command {
	var params listParams

	return &cli.Command{
		Name:    "list",
		Summary: "List fleet machines",
		Description: `List all machines that have published keys to the Bureau fleet.

The fleet localpart is optional — on a Bureau machine, it is auto-detected
from /etc/bureau/machine.conf. The server name is derived from the
connected session's identity.

When a fleet controller is reachable, the output is enriched with live
operational data: CPU and memory utilization, assignment counts, and
operator-assigned labels. Without a fleet controller, the output shows
static inventory from Matrix state events.`,
		Usage:          "bureau machine list [fleet-localpart] [flags]",
		Params:         func() any { return &params },
		Output:         func() any { return &listResult{} },
		RequiredGrants: []string{"command/machine/list"},
		Annotations:    cli.ReadOnly(),
		Examples: []cli.Example{
			{
				Description: "List machines (auto-detect fleet from machine.conf)",
				Command:     "bureau machine list",
			},
			{
				Description: "List machines in a specific fleet",
				Command:     "bureau machine list bureau/fleet/prod",
			},
		},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) > 1 {
				return cli.Validation("expected at most one argument (fleet localpart), got %d", len(args))
			}

			fleetLocalpart := ""
			if len(args) > 0 {
				fleetLocalpart = args[0]
			}
			fleetLocalpart = cli.ResolveFleet(fleetLocalpart)
			if fleetLocalpart == "" {
				return cli.Validation("fleet localpart is required (e.g., bureau/fleet/prod)").
					WithHint("Pass the fleet localpart as the first argument, or run on a Bureau machine where\n" +
						"/etc/bureau/machine.conf provides the fleet automatically.\n" +
						"Run 'bureau machine doctor' to check machine configuration.")
			}

			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			matrixSession, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}
			defer matrixSession.Close()

			return runList(ctx, matrixSession, fleetLocalpart, &params, logger)
		},
	}
}

// MachineEntry collects the key, status, and hardware info for a single machine.
type MachineEntry struct {
	Name      string `json:"name"                    desc:"machine name"`
	PublicKey string `json:"public_key"              desc:"age public key"`
	Algorithm string `json:"algorithm"               desc:"key algorithm"`
	LastSeen  string `json:"last_seen,omitempty"     desc:"last heartbeat timestamp"`
	GPUCount  int    `json:"gpu_count"               desc:"number of GPUs"`
	GPUModel  string `json:"gpu_model,omitempty"     desc:"GPU model name"`
	CPUModel  string `json:"cpu_model,omitempty"     desc:"CPU model name"`
	MemoryMB  int    `json:"memory_mb"               desc:"total memory in megabytes"`

	// Fleet-enriched fields (present when fleet controller is reachable).
	Hostname     string            `json:"hostname,omitempty"       desc:"reported hostname"`
	CPUPercent   int               `json:"cpu_percent,omitempty"    desc:"CPU utilization percentage"`
	MemoryUsedMB int               `json:"memory_used_mb,omitempty" desc:"memory used in MB"`
	Assignments  int               `json:"assignments,omitempty"    desc:"number of assigned principals"`
	Labels       map[string]string `json:"labels,omitempty"         desc:"operator-assigned labels"`
}

// ListMachines returns all machines that have published keys or status to
// the fleet's machine room. The caller provides a connected session, a
// typed Fleet reference, and a context with an appropriate deadline.
func ListMachines(ctx context.Context, session messaging.Session, fleetRef ref.Fleet) ([]*MachineEntry, error) {
	machineAlias := fleetRef.MachineRoomAlias()
	machineRoomID, err := session.ResolveAlias(ctx, machineAlias)
	if err != nil {
		return nil, cli.NotFound("resolve fleet machine room %q: %w", machineAlias, err).
			WithHint("Has 'bureau matrix setup' been run for this fleet? Check with 'bureau matrix doctor'.")
	}

	events, err := session.GetRoomState(ctx, machineRoomID)
	if err != nil {
		return nil, cli.Transient("get machine room state: %w", err).
			WithHint("Check that the homeserver is running. Run 'bureau matrix doctor' to diagnose.")
	}

	// Index machine keys, statuses, and hardware info by state_key (machine name).
	machines := make(map[string]*MachineEntry)

	for _, event := range events {
		if event.StateKey == nil {
			continue
		}
		stateKey := *event.StateKey

		switch event.Type {
		case schema.EventTypeMachineKey:
			var key schema.MachineKey
			contentBytes, err := json.Marshal(event.Content)
			if err != nil {
				continue
			}
			if err := json.Unmarshal(contentBytes, &key); err != nil {
				continue
			}
			if key.PublicKey == "" {
				// Empty content means the key was cleared (decommissioned).
				continue
			}
			entry := getOrCreate(machines, stateKey)
			entry.PublicKey = key.PublicKey
			entry.Algorithm = key.Algorithm

		case schema.EventTypeMachineInfo:
			var info schema.MachineInfo
			contentBytes, err := json.Marshal(event.Content)
			if err != nil {
				continue
			}
			if err := json.Unmarshal(contentBytes, &info); err != nil {
				continue
			}
			entry := getOrCreate(machines, stateKey)
			entry.CPUModel = info.CPU.Model
			entry.MemoryMB = info.MemoryTotalMB
			entry.GPUCount = len(info.GPUs)
			if len(info.GPUs) > 0 {
				entry.GPUModel = gpuDisplayName(info.GPUs[0])
			}

		case schema.EventTypeMachineStatus:
			var status schema.MachineStatus
			contentBytes, err := json.Marshal(event.Content)
			if err != nil {
				continue
			}
			if err := json.Unmarshal(contentBytes, &status); err != nil {
				continue
			}
			entry := getOrCreate(machines, stateKey)
			// Use LastActivityAt from the status content if available;
			// it's more meaningful than the event's origin_server_ts
			// because it reflects actual daemon activity, not just
			// the most recent heartbeat.
			if status.LastActivityAt != "" {
				entry.LastSeen = status.LastActivityAt
			} else if status.Principal != "" {
				// Status exists but no activity yet — mark as online.
				entry.LastSeen = "(online, no activity)"
			}
		}
	}

	entries := make([]*MachineEntry, 0, len(machines))
	for _, entry := range machines {
		entries = append(entries, entry)
	}
	return entries, nil
}

func runList(ctx context.Context, session messaging.Session, fleetLocalpart string, params *listParams, logger *slog.Logger) error {
	server, err := ref.ServerFromUserID(session.UserID().String())
	if err != nil {
		return cli.Internal("cannot determine server name from session: %w", err)
	}
	fleetRef, err := ref.ParseFleet(fleetLocalpart, server)
	if err != nil {
		return cli.Validation("%v", err)
	}

	// Matrix scan: ground truth of fleet membership.
	entries, err := ListMachines(ctx, session, fleetRef)
	if err != nil {
		return err
	}

	// Fleet enrichment: optional, non-fatal.
	fleetClient := tryFleetConnect(&params.FleetConnection, logger)
	var fleetIndex map[string]fleet.MachineEntry
	if fleetClient != nil {
		fleetCtx, fleetCancel := fleet.CallContext(ctx)
		defer fleetCancel()

		var fleetResponse fleet.MachinesResponse
		if err := fleetClient.Call(fleetCtx, "list-machines", nil, &fleetResponse); err != nil {
			logger.Debug("fleet list-machines failed, continuing without fleet data", "error", err)
		} else {
			fleetIndex = make(map[string]fleet.MachineEntry, len(fleetResponse.Machines))
			for _, machine := range fleetResponse.Machines {
				fleetIndex[machine.Localpart] = machine
			}
		}
	}

	fleetEnriched := fleetIndex != nil

	// Merge fleet data into Matrix entries.
	if fleetEnriched {
		for _, entry := range entries {
			if fleetData, exists := fleetIndex[entry.Name]; exists {
				entry.Hostname = fleetData.Hostname
				entry.CPUPercent = fleetData.CPUPercent
				entry.MemoryUsedMB = fleetData.MemoryUsedMB
				entry.Assignments = fleetData.Assignments
				entry.Labels = fleetData.Labels
			}
		}
	}

	if done, err := params.EmitJSON(listResult{
		Machines:      entries,
		FleetEnriched: fleetEnriched,
	}); done {
		return err
	}

	if len(entries) == 0 {
		logger.Info("no machines found in the fleet", "fleet", fleetLocalpart)
		return nil
	}

	// Table format adapts to whether fleet data is available.
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if fleetEnriched {
		printFleetEnrichedTable(writer, entries)
	} else {
		printMatrixOnlyTable(writer, entries)
	}
	writer.Flush()

	return nil
}

// printMatrixOnlyTable prints the original Matrix-only table format.
func printMatrixOnlyTable(writer *tabwriter.Writer, entries []*MachineEntry) {
	fmt.Fprintln(writer, "MACHINE\tGPUS\tMEMORY\tLAST SEEN")
	for _, entry := range entries {
		gpuDisplay := "-"
		if entry.GPUCount > 0 {
			if entry.GPUCount == 1 {
				gpuDisplay = fmt.Sprintf("1x %s", entry.GPUModel)
			} else {
				gpuDisplay = fmt.Sprintf("%dx %s", entry.GPUCount, entry.GPUModel)
			}
		}
		memoryDisplay := "-"
		if entry.MemoryMB > 0 {
			memoryDisplay = fmt.Sprintf("%d GB", entry.MemoryMB/1024)
		}
		lastSeen := entry.LastSeen
		if lastSeen == "" {
			lastSeen = "-"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", entry.Name, gpuDisplay, memoryDisplay, lastSeen)
	}
}

// printFleetEnrichedTable prints a table with fleet operational data.
func printFleetEnrichedTable(writer *tabwriter.Writer, entries []*MachineEntry) {
	fmt.Fprintln(writer, "MACHINE\tHOSTNAME\tCPU\tMEMORY\tGPUS\tASSIGNMENTS\tLABELS")
	for _, entry := range entries {
		hostname := entry.Hostname
		if hostname == "" {
			hostname = "-"
		}
		memoryDisplay := "-"
		if entry.MemoryMB > 0 {
			if entry.MemoryUsedMB > 0 {
				memoryDisplay = fmt.Sprintf("%d/%d MB", entry.MemoryUsedMB, entry.MemoryMB)
			} else {
				memoryDisplay = fmt.Sprintf("%d MB", entry.MemoryMB)
			}
		}
		labels := fleet.FormatLabels(entry.Labels)
		fmt.Fprintf(writer, "%s\t%s\t%d%%\t%s\t%d\t%d\t%s\n",
			entry.Name,
			hostname,
			entry.CPUPercent,
			memoryDisplay,
			entry.GPUCount,
			entry.Assignments,
			labels,
		)
	}
}

// getOrCreate returns the MachineEntry for the given name, creating it
// if it doesn't exist yet.
func getOrCreate(machines map[string]*MachineEntry, name string) *MachineEntry {
	entry, exists := machines[name]
	if !exists {
		entry = &MachineEntry{Name: name}
		machines[name] = entry
	}
	return entry
}

// gpuDisplayName returns a short human-readable name for a GPU. Prefers
// the model name (e.g., "NVIDIA GeForce RTX 4090") if available, then
// falls back to vendor + PCI device ID (e.g., "AMD 0x744a").
func gpuDisplayName(gpu schema.GPUInfo) string {
	if gpu.ModelName != "" {
		return gpu.ModelName
	}
	if gpu.Vendor != "" && gpu.PCIDeviceID != "" {
		return gpu.Vendor + " " + gpu.PCIDeviceID
	}
	if gpu.Vendor != "" {
		return gpu.Vendor
	}
	return gpu.PCIDeviceID
}
