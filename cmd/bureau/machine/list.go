// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

func listCommand() *cli.Command {
	var (
		session    cli.SessionConfig
		serverName string
	)

	return &cli.Command{
		Name:    "list",
		Summary: "List fleet machines",
		Description: `List all machines that have published keys to the Bureau fleet.

Shows each machine's name, public key, and last status heartbeat
(if available). Reads from the #bureau/machine room state.`,
		Usage: "bureau machine list [flags]",
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("list", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			matrixSession, err := session.Connect(ctx)
			if err != nil {
				return err
			}
			defer matrixSession.Close()

			return runList(ctx, matrixSession, serverName)
		},
	}
}

// machineEntry collects the key, status, and hardware info for a single machine.
type machineEntry struct {
	name      string
	publicKey string
	algorithm string
	lastSeen  string
	gpuCount  int
	gpuModel  string // first GPU's model name or vendor+device for display
	cpuModel  string
	memoryMB  int
}

func runList(ctx context.Context, session *messaging.Session, serverName string) error {
	machineAlias := principal.RoomAlias("bureau/machine", serverName)
	machineRoomID, err := session.ResolveAlias(ctx, machineAlias)
	if err != nil {
		return fmt.Errorf("resolve machine room %q: %w", machineAlias, err)
	}

	events, err := session.GetRoomState(ctx, machineRoomID)
	if err != nil {
		return fmt.Errorf("get machine room state: %w", err)
	}

	// Index machine keys, statuses, and hardware info by state_key (machine name).
	machines := make(map[string]*machineEntry)

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
			entry.publicKey = key.PublicKey
			entry.algorithm = key.Algorithm

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
			entry.cpuModel = info.CPU.Model
			entry.memoryMB = info.MemoryTotalMB
			entry.gpuCount = len(info.GPUs)
			if len(info.GPUs) > 0 {
				entry.gpuModel = gpuDisplayName(info.GPUs[0])
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
				entry.lastSeen = status.LastActivityAt
			} else if status.Principal != "" {
				// Status exists but no activity yet — mark as online.
				entry.lastSeen = "(online, no activity)"
			}
		}
	}

	if len(machines) == 0 {
		fmt.Fprintln(os.Stderr, "No machines found in the fleet.")
		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "MACHINE\tGPUS\tMEMORY\tLAST SEEN")

	for _, entry := range machines {
		gpuDisplay := "-"
		if entry.gpuCount > 0 {
			if entry.gpuCount == 1 {
				gpuDisplay = fmt.Sprintf("1x %s", entry.gpuModel)
			} else {
				gpuDisplay = fmt.Sprintf("%dx %s", entry.gpuCount, entry.gpuModel)
			}
		}
		memoryDisplay := "-"
		if entry.memoryMB > 0 {
			memoryDisplay = fmt.Sprintf("%d GB", entry.memoryMB/1024)
		}
		lastSeen := entry.lastSeen
		if lastSeen == "" {
			lastSeen = "-"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", entry.name, gpuDisplay, memoryDisplay, lastSeen)
	}
	writer.Flush()

	return nil
}

// getOrCreate returns the machineEntry for the given name, creating it
// if it doesn't exist yet.
func getOrCreate(machines map[string]*machineEntry, name string) *machineEntry {
	entry, exists := machines[name]
	if !exists {
		entry = &machineEntry{name: name}
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
