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
)

type serviceListParams struct {
	cli.SessionConfig
	fleet.FleetConnection
	cli.JSONOutput
	Machine    string `json:"machine"     flag:"machine"     desc:"filter to a specific machine (optional — lists all machines if omitted)"`
	Fleet      string `json:"fleet"       flag:"fleet"       desc:"fleet prefix (e.g., bureau/fleet/prod) — required when --machine is omitted"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name (auto-detected from machine.conf)"`
}

// serviceListEntry is a single row in the unified list output.
type serviceListEntry struct {
	Localpart    string `json:"localpart"              desc:"service localpart"`
	Template     string `json:"template"               desc:"template reference"`
	MachineName  string `json:"machine,omitempty"       desc:"machine hosting this service"`
	AutoStart    bool   `json:"auto_start,omitempty"    desc:"whether the service starts automatically"`
	FleetManaged bool   `json:"fleet_managed"           desc:"whether this service has a fleet definition"`
	Replicas     int    `json:"replicas,omitempty"      desc:"desired minimum replicas (fleet-managed only)"`
	Instances    int    `json:"instances,omitempty"      desc:"current instance count (fleet-managed only)"`
	Failover     string `json:"failover,omitempty"      desc:"failover policy (fleet-managed only)"`
	Priority     int    `json:"priority,omitempty"      desc:"scheduling priority (fleet-managed only)"`
}

type serviceListResult struct {
	Services      []serviceListEntry `json:"services"       desc:"services found"`
	MachineCount  int                `json:"machine_count"  desc:"machines scanned (when --machine omitted)"`
	FleetEnriched bool               `json:"fleet_enriched" desc:"whether fleet controller data is included"`
}

func listCommand() *cli.Command {
	var params serviceListParams

	return &cli.Command{
		Name:    "list",
		Summary: "List services across machines",
		Description: `List all service principals, optionally filtered to a specific machine.

When --machine is omitted, scans all machines from the fleet's machine room and
lists every assigned principal. The scan count is reported for diagnostics.

If a fleet controller is reachable, the output is enriched with fleet metadata:
replica counts, failover policy, priority, and instance counts. Fleet-defined
services that have no running instances are included as unplaced entries.`,
		Usage: "bureau service list [--machine <machine>]",
		Examples: []cli.Example{
			{
				Description: "List all services across all machines",
				Command:     "bureau service list --credential-file ./creds",
			},
			{
				Description: "List services on a specific machine",
				Command:     "bureau service list --credential-file ./creds --machine machine/workstation",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &serviceListResult{} },
		RequiredGrants: []string{"command/service/list"},
		Annotations:    cli.ReadOnly(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}
			return runList(ctx, logger, params)
		},
	}
}

func runList(ctx context.Context, logger *slog.Logger, params serviceListParams) error {
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

	// Matrix scan: ground truth of what's deployed.
	locations, machineCount, err := principal.List(ctx, session, machine, fleetRef)
	if err != nil {
		return cli.Internal("list services: %w", err)
	}

	// Fleet enrichment: optional, non-fatal.
	fleetClient := tryFleetConnect(&params.FleetConnection, logger)
	var fleetIndex map[string]fleet.ServiceEntry
	if fleetClient != nil {
		fleetCtx, fleetCancel := fleet.CallContext(ctx)
		defer fleetCancel()

		var fleetResponse fleet.ListServicesResponse
		if err := fleetClient.Call(fleetCtx, "list-services", nil, &fleetResponse); err != nil {
			logger.Debug("fleet list-services failed, continuing without fleet data", "error", err)
		} else {
			fleetIndex = make(map[string]fleet.ServiceEntry, len(fleetResponse.Services))
			for _, entry := range fleetResponse.Services {
				fleetIndex[entry.Localpart] = entry
			}
		}
	}

	fleetEnriched := fleetIndex != nil

	// Build unified entries from Matrix locations.
	seen := make(map[string]bool)
	entries := make([]serviceListEntry, 0, len(locations))
	for _, location := range locations {
		localpart := location.Assignment.Principal.Localpart()
		seen[localpart] = true

		entry := serviceListEntry{
			MachineName: location.Machine.Localpart(),
			Localpart:   localpart,
			Template:    location.Assignment.Template,
			AutoStart:   location.Assignment.AutoStart,
		}

		if fleetData, exists := fleetIndex[localpart]; exists {
			entry.FleetManaged = true
			entry.Replicas = fleetData.Replicas
			entry.Instances = fleetData.Instances
			entry.Failover = fleetData.Failover
			entry.Priority = fleetData.Priority
		}

		entries = append(entries, entry)
	}

	// Add fleet-defined services that have no Matrix assignments (defined
	// but not yet placed, or placed on machines not in the scan scope).
	if fleetEnriched {
		for localpart, fleetData := range fleetIndex {
			if seen[localpart] {
				continue
			}
			entries = append(entries, serviceListEntry{
				Localpart:    localpart,
				Template:     fleetData.Template,
				FleetManaged: true,
				Replicas:     fleetData.Replicas,
				Instances:    fleetData.Instances,
				Failover:     fleetData.Failover,
				Priority:     fleetData.Priority,
			})
		}
	}

	if done, err := params.EmitJSON(serviceListResult{
		Services:      entries,
		MachineCount:  machineCount,
		FleetEnriched: fleetEnriched,
	}); done {
		return err
	}

	if params.Machine == "" && machineCount > 0 {
		logger.Info("resolved services", "service_count", len(entries), "machine_count", machineCount)
	}

	if len(entries) == 0 {
		logger.Info("no services found")
		return nil
	}

	// Table format adapts to whether fleet data is available.
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if fleetEnriched {
		printFleetEnrichedTable(writer, entries, params.Machine == "")
	} else {
		printMatrixOnlyTable(writer, entries, params.Machine == "")
	}
	writer.Flush()

	return nil
}

// printMatrixOnlyTable prints the original Matrix-only table format.
func printMatrixOnlyTable(writer *tabwriter.Writer, entries []serviceListEntry, showMachine bool) {
	if showMachine {
		fmt.Fprintln(writer, "MACHINE\tNAME\tTEMPLATE\tAUTO-START")
		for _, entry := range entries {
			fmt.Fprintf(writer, "%s\t%s\t%s\t%v\n",
				entry.MachineName, entry.Localpart, entry.Template, entry.AutoStart)
		}
	} else {
		fmt.Fprintln(writer, "NAME\tTEMPLATE\tAUTO-START")
		for _, entry := range entries {
			fmt.Fprintf(writer, "%s\t%s\t%v\n",
				entry.Localpart, entry.Template, entry.AutoStart)
		}
	}
}

// printFleetEnrichedTable prints a table with fleet metadata columns.
func printFleetEnrichedTable(writer *tabwriter.Writer, entries []serviceListEntry, showMachine bool) {
	if showMachine {
		fmt.Fprintln(writer, "NAME\tTEMPLATE\tMACHINE\tREPLICAS\tINSTANCES\tFAILOVER\tPRIORITY")
		for _, entry := range entries {
			machineName := entry.MachineName
			if machineName == "" {
				machineName = "-"
			}
			failover := entry.Failover
			if failover == "" {
				failover = "-"
			}
			template := entry.Template
			if template == "" {
				template = "-"
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%d\t%s\t%d\n",
				entry.Localpart,
				template,
				machineName,
				entry.Replicas,
				entry.Instances,
				failover,
				entry.Priority,
			)
		}
	} else {
		fmt.Fprintln(writer, "NAME\tTEMPLATE\tREPLICAS\tINSTANCES\tFAILOVER\tPRIORITY")
		for _, entry := range entries {
			failover := entry.Failover
			if failover == "" {
				failover = "-"
			}
			template := entry.Template
			if template == "" {
				template = "-"
			}
			fmt.Fprintf(writer, "%s\t%s\t%d\t%d\t%s\t%d\n",
				entry.Localpart,
				template,
				entry.Replicas,
				entry.Instances,
				failover,
				entry.Priority,
			)
		}
	}
}
