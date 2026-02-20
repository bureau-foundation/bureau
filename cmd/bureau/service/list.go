// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
)

type serviceListParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Machine    string `json:"machine"     flag:"machine"     desc:"filter to a specific machine (optional — lists all machines if omitted)"`
	Fleet      string `json:"fleet"       flag:"fleet"       desc:"fleet prefix (e.g., bureau/fleet/prod) — required when --machine is omitted"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
}

// serviceListEntry is a single row in the list output.
type serviceListEntry struct {
	MachineName string `json:"machine"`
	Localpart   string `json:"localpart"`
	Template    string `json:"template"`
	AutoStart   bool   `json:"auto_start"`
}

type serviceListResult struct {
	Services     []serviceListEntry `json:"services"`
	MachineCount int                `json:"machine_count"`
}

func listCommand() *cli.Command {
	var params serviceListParams

	return &cli.Command{
		Name:    "list",
		Summary: "List services across machines",
		Description: `List all service principals, optionally filtered to a specific machine.

When --machine is omitted, scans all machines from the fleet's machine room and
lists every assigned principal. The scan count is reported for diagnostics.`,
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
		Run: func(args []string) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}
			return runList(params)
		},
	}
}

func runList(params serviceListParams) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	var machine ref.Machine
	var fleet ref.Fleet
	if params.Machine != "" {
		machine, err = ref.ParseMachine(params.Machine, params.ServerName)
		if err != nil {
			return cli.Validation("invalid machine: %v", err)
		}
	} else {
		fleet, err = ref.ParseFleet(params.Fleet, params.ServerName)
		if err != nil {
			return cli.Validation("invalid fleet: %v", err)
		}
	}

	locations, machineCount, err := principal.List(ctx, session, machine, fleet)
	if err != nil {
		return cli.Internal("list services: %w", err)
	}

	entries := make([]serviceListEntry, len(locations))
	for i, location := range locations {
		entries[i] = serviceListEntry{
			MachineName: location.Machine.Localpart(),
			Localpart:   location.Assignment.Principal.Localpart(),
			Template:    location.Assignment.Template,
			AutoStart:   location.Assignment.AutoStart,
		}
	}

	if done, err := params.EmitJSON(serviceListResult{
		Services:     entries,
		MachineCount: machineCount,
	}); done {
		return err
	}

	if params.Machine == "" && machineCount > 0 {
		fmt.Fprintf(os.Stderr, "resolved %d principal(s) across %d machine(s)\n", len(entries), machineCount)
	}

	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "no services found")
		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if params.Machine == "" {
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
	writer.Flush()

	return nil
}
