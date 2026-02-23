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
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// listParams holds the parameters for the machine list command.
type listParams struct {
	cli.SessionConfig
	cli.JSONOutput
}

func listCommand() *cli.Command {
	var params listParams

	return &cli.Command{
		Name:    "list",
		Summary: "List fleet machines",
		Description: `List all machines that have published keys to the Bureau fleet.

The argument is a fleet localpart (e.g., "bureau/fleet/prod"). The
server name is derived from the connected session's identity.

Shows each machine's name, public key, and last status heartbeat
(if available). Reads from the fleet's machine room state.`,
		Usage:          "bureau machine list <fleet-localpart> [flags]",
		Params:         func() any { return &params },
		Output:         func() any { return &[]MachineEntry{} },
		RequiredGrants: []string{"command/machine/list"},
		Annotations:    cli.ReadOnly(),
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("fleet localpart is required (e.g., bureau/fleet/prod)")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (fleet localpart), got %d", len(args))
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			matrixSession, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}
			defer matrixSession.Close()

			return runList(ctx, matrixSession, args[0], &params.JSONOutput)
		},
	}
}

// MachineEntry collects the key, status, and hardware info for a single machine.
type MachineEntry struct {
	Name      string `json:"name"                  desc:"machine name"`
	PublicKey string `json:"public_key"            desc:"SSH public key"`
	Algorithm string `json:"algorithm"             desc:"key algorithm"`
	LastSeen  string `json:"last_seen,omitempty"   desc:"last heartbeat timestamp"`
	GPUCount  int    `json:"gpu_count"             desc:"number of GPUs"`
	GPUModel  string `json:"gpu_model,omitempty"   desc:"GPU model name"`
	CPUModel  string `json:"cpu_model,omitempty"   desc:"CPU model name"`
	MemoryMB  int    `json:"memory_mb"             desc:"total memory in megabytes"`
}

// ListMachines returns all machines that have published keys or status to
// the fleet's machine room. The caller provides a connected session, a
// typed Fleet reference, and a context with an appropriate deadline.
func ListMachines(ctx context.Context, session messaging.Session, fleet ref.Fleet) ([]*MachineEntry, error) {
	machineAlias := fleet.MachineRoomAlias()
	machineRoomID, err := session.ResolveAlias(ctx, machineAlias)
	if err != nil {
		return nil, cli.NotFound("resolve fleet machine room %q: %w", machineAlias, err)
	}

	events, err := session.GetRoomState(ctx, machineRoomID)
	if err != nil {
		return nil, cli.Internal("get machine room state: %w", err)
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

func runList(ctx context.Context, session messaging.Session, fleetLocalpart string, jsonOutput *cli.JSONOutput) error {
	server, err := ref.ServerFromUserID(session.UserID().String())
	if err != nil {
		return cli.Internal("cannot determine server name from session: %w", err)
	}
	fleet, err := ref.ParseFleet(fleetLocalpart, server)
	if err != nil {
		return cli.Validation("%v", err)
	}

	entries, err := ListMachines(ctx, session, fleet)
	if err != nil {
		return err
	}

	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "No machines found in the fleet.")
		return nil
	}

	if done, err := jsonOutput.EmitJSON(entries); done {
		return err
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
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
	writer.Flush()

	return nil
}

// getOrCreate returns the machineEntry for the given name, creating it
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
