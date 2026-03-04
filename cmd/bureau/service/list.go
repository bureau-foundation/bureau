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
)

type serviceListParams struct {
	cli.SessionConfig
	cli.FleetScope
	fleet.FleetConnection
	cli.JSONOutput
}

// serviceListEntry is a single row in the unified list output.
type serviceListEntry struct {
	Localpart   string `json:"localpart"          desc:"service localpart"`
	Template    string `json:"template"           desc:"template reference"`
	MachineName string `json:"machine,omitempty"  desc:"machine hosting this service"`
	AutoStart   bool   `json:"auto_start"         desc:"whether the service starts automatically"`
	Replicas    int    `json:"replicas"           desc:"desired minimum replicas"`
	Instances   int    `json:"instances"          desc:"current instance count"`
	Failover    string `json:"failover"           desc:"failover policy"`
	Priority    int    `json:"priority"           desc:"scheduling priority"`
}

type serviceListResult struct {
	Services     []serviceListEntry `json:"services"      desc:"services found"`
	MachineCount int                `json:"machine_count" desc:"machines scanned (when --machine omitted)"`
}

func listCommand() *cli.Command {
	var params serviceListParams

	return &cli.Command{
		Name:    "list",
		Summary: "List services across machines",
		Description: `List all service principals with fleet metadata.

When --machine is omitted, scans all machines from the fleet's machine room and
lists every assigned principal. The scan count is reported for diagnostics.

Service assignments come from Matrix state events. Fleet metadata (replica
counts, failover policy, priority, instance counts) comes from the fleet
controller. Fleet-defined services with no running instances are included
as unplaced entries. Both sources are required.`,
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
	scope, err := params.FleetScope.Resolve()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	// Matrix scan: ground truth of what's deployed.
	locations, machineCount, err := principal.List(ctx, session, scope.Machine, scope.Fleet)
	if err != nil {
		return cli.Internal("list services: %w", err)
	}

	// Fleet controller: fleet metadata.
	fleetClient, err := params.FleetConnection.Connect()
	if err != nil {
		return err
	}

	fleetCtx, fleetCancel := fleet.CallContext(ctx)
	defer fleetCancel()

	var fleetResponse fleet.ListServicesResponse
	if err := fleetClient.Call(fleetCtx, "list-services", nil, &fleetResponse); err != nil {
		return cli.Transient("fetching service data from fleet controller: %w", err).
			WithHint("Is the fleet controller running? Check with 'bureau fleet status'.")
	}

	fleetIndex := make(map[string]fleet.ServiceEntry, len(fleetResponse.Services))
	for _, entry := range fleetResponse.Services {
		fleetIndex[entry.Localpart] = entry
	}

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
			entry.Replicas = fleetData.Replicas
			entry.Instances = fleetData.Instances
			entry.Failover = fleetData.Failover
			entry.Priority = fleetData.Priority
		}

		entries = append(entries, entry)
	}

	// Add fleet-defined services that have no Matrix assignments (defined
	// but not yet placed, or placed on machines not in the scan scope).
	for localpart, fleetData := range fleetIndex {
		if seen[localpart] {
			continue
		}
		entries = append(entries, serviceListEntry{
			Localpart: localpart,
			Template:  fleetData.Template,
			Replicas:  fleetData.Replicas,
			Instances: fleetData.Instances,
			Failover:  fleetData.Failover,
			Priority:  fleetData.Priority,
		})
	}

	if done, err := params.EmitJSON(serviceListResult{
		Services:     entries,
		MachineCount: machineCount,
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

	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	printTable(writer, entries, params.Machine == "")
	writer.Flush()

	return nil
}

// printTable prints the service list table with fleet metadata columns.
func printTable(writer *tabwriter.Writer, entries []serviceListEntry, showMachine bool) {
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
