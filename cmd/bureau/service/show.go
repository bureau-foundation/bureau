// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
)

type serviceShowParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Machine    string `json:"machine"     flag:"machine"     desc:"machine localpart (optional — auto-discovers if omitted)"`
	Fleet      string `json:"fleet"       flag:"fleet"       desc:"fleet prefix (e.g., bureau/fleet/prod) — required when --machine is omitted"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name (auto-detected from machine.conf)"`
}

type serviceShowResult struct {
	PrincipalUserID string            `json:"principal_user_id"`
	MachineName     string            `json:"machine"`
	Template        string            `json:"template"`
	AutoStart       bool              `json:"auto_start"`
	Labels          map[string]string `json:"labels,omitempty"`
}

func showCommand() *cli.Command {
	var params serviceShowParams

	return &cli.Command{
		Name:    "show",
		Summary: "Show detailed information about a service",
		Description: `Display detailed information about a single service principal.

If --machine is omitted, scans all machines to find where the service
is assigned. The scan count is reported for diagnostics.`,
		Usage: "bureau service show <localpart> [--machine <machine>]",
		Examples: []cli.Example{
			{
				Description: "Show service details (auto-discover machine)",
				Command:     "bureau service show service/ticket --credential-file ./creds",
			},
			{
				Description: "Show service on a specific machine",
				Command:     "bureau service show service/ticket --credential-file ./creds --machine machine/workstation",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &serviceShowResult{} },
		RequiredGrants: []string{"command/service/show"},
		Annotations:    cli.ReadOnly(),
		Run: requireLocalpart("bureau service show <localpart> [--machine <machine>]", func(ctx context.Context, localpart string, logger *slog.Logger) error {
			return runShow(ctx, localpart, logger, params)
		}),
	}
}

func runShow(ctx context.Context, localpart string, logger *slog.Logger, params serviceShowParams) error {
	params.ServerName = cli.ResolveServerName(params.ServerName)
	params.Fleet = cli.ResolveFleet(params.Fleet)

	serverName, err := ref.ParseServerName(params.ServerName)
	if err != nil {
		return fmt.Errorf("invalid --server-name: %w", err)
	}

	serviceRef, err := ref.ParseService(localpart, serverName)
	if err != nil {
		return cli.Validation("invalid service localpart: %v", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	var machine ref.Machine
	var fleet ref.Fleet
	if params.Machine != "" {
		machine, err = ref.ParseMachine(params.Machine, serverName)
		if err != nil {
			return cli.Validation("invalid machine: %v", err)
		}
	} else {
		fleet, err = ref.ParseFleet(params.Fleet, serverName)
		if err != nil {
			return cli.Validation("invalid fleet: %v", err)
		}
	}

	location, machineCount, err := principal.Resolve(ctx, session, localpart, machine, fleet)
	if err != nil {
		return cli.NotFound("resolve service: %w", err)
	}

	if machine.IsZero() && machineCount > 0 {
		logger.Info("resolved service location", "machine", location.Machine.Localpart(), "machines_scanned", machineCount)
	}

	result := serviceShowResult{
		PrincipalUserID: serviceRef.UserID().String(),
		MachineName:     location.Machine.Localpart(),
		Template:        location.Assignment.Template,
		AutoStart:       location.Assignment.AutoStart,
		Labels:          location.Assignment.Labels,
	}

	if done, err := params.EmitJSON(result); done {
		return err
	}

	fmt.Printf("Principal:   %s\n", result.PrincipalUserID)
	fmt.Printf("Machine:     %s\n", result.MachineName)
	fmt.Printf("Template:    %s\n", result.Template)
	fmt.Printf("Auto-start:  %v\n", result.AutoStart)

	if len(result.Labels) > 0 {
		fmt.Printf("Labels:\n")
		for key, value := range result.Labels {
			fmt.Printf("  %s: %s\n", key, value)
		}
	}

	return nil
}
