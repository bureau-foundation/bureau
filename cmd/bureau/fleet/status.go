// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// statusParams holds the parameters for the fleet status command.
type statusParams struct {
	FleetConnection
	cli.JSONOutput
}

// statusResult is the JSON output of the status command.
type statusResult struct {
	UptimeSeconds int             `json:"uptime_seconds" desc:"fleet controller uptime in seconds"`
	Machines      int             `json:"machines"       desc:"total tracked machines"`
	Services      int             `json:"services"       desc:"total fleet-managed services"`
	Definitions   int             `json:"definitions"    desc:"machine definitions loaded"`
	ConfigRooms   int             `json:"config_rooms"   desc:"config rooms tracked"`
	MachineList   []machineHealth `json:"machine_list"   desc:"per-machine health summary"`
}

// machineHealth is a per-machine entry in the status output.
type machineHealth struct {
	Localpart     string `json:"localpart"       desc:"machine localpart"`
	Hostname      string `json:"hostname"        desc:"reported hostname"`
	CPUPercent    int    `json:"cpu_percent"      desc:"CPU utilization percentage"`
	MemoryUsedMB  int    `json:"memory_used_mb"   desc:"memory used in MB"`
	MemoryTotalMB int    `json:"memory_total_mb"  desc:"total memory in MB"`
	Assignments   int    `json:"assignments"     desc:"number of assigned principals"`
}

func statusCommand() *cli.Command {
	var params statusParams

	return &cli.Command{
		Name:    "status",
		Summary: "Show fleet controller status and machine health",
		Description: `Display fleet controller health: uptime, machine count with per-machine
resource usage, and service count.

Connects to the fleet controller's socket and calls the "info" and
"list-machines" actions to gather aggregate and per-machine data.`,
		Usage: "bureau fleet status [flags]",
		Examples: []cli.Example{
			{
				Description: "Show fleet status",
				Command:     "bureau fleet status",
			},
			{
				Description: "Show fleet status as JSON",
				Command:     "bureau fleet status --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &statusResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/fleet/status"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}
			return runStatus(ctx, logger, &params)
		},
	}
}

// infoResponse mirrors the fleet controller's info action response.
type infoResponse struct {
	UptimeSeconds int `cbor:"uptime_seconds"`
	Machines      int `cbor:"machines"`
	Services      int `cbor:"services"`
	Definitions   int `cbor:"definitions"`
	ConfigRooms   int `cbor:"config_rooms"`
}

// machinesResponse mirrors the fleet controller's list-machines response.
type machinesResponse struct {
	Machines []machineEntry `cbor:"machines"`
}

// machineEntry mirrors the fleet controller's machineSummary.
type machineEntry struct {
	Localpart     string            `cbor:"localpart"`
	Hostname      string            `cbor:"hostname"`
	CPUPercent    int               `cbor:"cpu_percent"`
	MemoryUsedMB  int               `cbor:"memory_used_mb"`
	MemoryTotalMB int               `cbor:"memory_total_mb"`
	GPUCount      int               `cbor:"gpu_count"`
	Labels        map[string]string `cbor:"labels"`
	Assignments   int               `cbor:"assignments"`
	ConfigRoomID  string            `cbor:"config_room_id"`
}

func runStatus(ctx context.Context, logger *slog.Logger, params *statusParams) error {
	client, err := params.connect()
	if err != nil {
		return err
	}

	ctx, cancel := callContext(ctx)
	defer cancel()

	// Fetch aggregate info.
	var info infoResponse
	if err := client.Call(ctx, "info", nil, &info); err != nil {
		return cli.Internal("fetching fleet info: %w", err)
	}

	// Fetch per-machine health.
	var machines machinesResponse
	if err := client.Call(ctx, "list-machines", nil, &machines); err != nil {
		return cli.Internal("fetching machine list: %w", err)
	}

	machineList := make([]machineHealth, len(machines.Machines))
	for i, machine := range machines.Machines {
		machineList[i] = machineHealth{
			Localpart:     machine.Localpart,
			Hostname:      machine.Hostname,
			CPUPercent:    machine.CPUPercent,
			MemoryUsedMB:  machine.MemoryUsedMB,
			MemoryTotalMB: machine.MemoryTotalMB,
			Assignments:   machine.Assignments,
		}
	}

	result := statusResult{
		UptimeSeconds: info.UptimeSeconds,
		Machines:      info.Machines,
		Services:      info.Services,
		Definitions:   info.Definitions,
		ConfigRooms:   info.ConfigRooms,
		MachineList:   machineList,
	}

	if done, err := params.EmitJSON(result); done {
		return err
	}

	uptime := time.Duration(result.UptimeSeconds) * time.Second
	logger.Info("fleet controller status",
		"uptime", formatDuration(uptime),
		"machines", result.Machines,
		"services", result.Services,
		"definitions", result.Definitions,
		"config_rooms", result.ConfigRooms,
	)
	for _, machine := range machineList {
		logger.Info("machine",
			"localpart", machine.Localpart,
			"hostname", machine.Hostname,
			"cpu_percent", machine.CPUPercent,
			"memory_used_mb", machine.MemoryUsedMB,
			"memory_total_mb", machine.MemoryTotalMB,
			"assignments", machine.Assignments,
		)
	}

	return nil
}

// callContext returns a context with a reasonable timeout for fleet
// service calls. Fleet operations involve in-memory index queries
// or single Matrix writes.
func callContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 30*time.Second)
}

// formatDuration formats a duration as a human-readable string
// with days, hours, minutes, and seconds.
func formatDuration(duration time.Duration) string {
	days := int(duration.Hours()) / 24
	hours := int(duration.Hours()) % 24
	minutes := int(duration.Minutes()) % 60
	seconds := int(duration.Seconds()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm %ds", days, hours, minutes, seconds)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}
