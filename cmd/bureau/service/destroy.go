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
	"github.com/bureau-foundation/bureau/lib/schema"
)

type serviceDestroyParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Machine    string `json:"machine"     flag:"machine"     desc:"machine localpart (optional — auto-discovers if omitted)"`
	Fleet      string `json:"fleet"       flag:"fleet"       desc:"fleet prefix (e.g., bureau/fleet/prod) — required when --machine is omitted"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name (auto-detected from machine.conf)"`
	Purge      bool   `json:"purge"       flag:"purge"       desc:"also clear the credential bundle"`
}

type serviceDestroyResult struct {
	Localpart     string      `json:"localpart"`
	MachineName   string      `json:"machine"`
	ConfigRoomID  ref.RoomID  `json:"config_room_id"`
	ConfigEventID ref.EventID `json:"config_event_id"`
	Purged        bool        `json:"purged"`
}

func destroyCommand() *cli.Command {
	var params serviceDestroyParams

	return &cli.Command{
		Name:    "destroy",
		Summary: "Remove a service's assignment from a machine",
		Description: `Remove a service's PrincipalAssignment from the MachineConfig.

The daemon detects the config change via /sync and tears down the service's
sandbox. The service's Matrix account is preserved for audit trail purposes.

With --purge, also clears the m.bureau.credentials state event for this
principal (publishes empty content). Without --purge, credentials remain
in the config room and can be reused if the service is re-created.

Does NOT deactivate the Matrix account — the service's message history and
state event trail remain intact for auditing.`,
		Usage: "bureau service destroy <localpart> [--machine <machine>]",
		Examples: []cli.Example{
			{
				Description: "Remove a service (auto-discover machine)",
				Command:     "bureau service destroy service/ticket --credential-file ./creds",
			},
			{
				Description: "Remove and purge credentials",
				Command:     "bureau service destroy service/ticket --credential-file ./creds --purge",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &serviceDestroyResult{} },
		RequiredGrants: []string{"command/service/destroy"},
		Annotations:    cli.Destructive(),
		Run: requireLocalpart("bureau service destroy <localpart> [--machine <machine>]", func(ctx context.Context, localpart string, logger *slog.Logger) error {
			return runDestroy(ctx, localpart, logger, params)
		}),
	}
}

func runDestroy(ctx context.Context, localpart string, logger *slog.Logger, params serviceDestroyParams) error {
	params.ServerName = cli.ResolveServerName(params.ServerName)
	params.Fleet = cli.ResolveFleet(params.Fleet)

	serverName, err := ref.ParseServerName(params.ServerName)
	if err != nil {
		return fmt.Errorf("invalid --server-name: %w", err)
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

	destroyResult, err := principal.Destroy(ctx, session, location.ConfigRoomID, location.Machine, localpart)
	if err != nil {
		return cli.Internal("remove service assignment: %w", err)
	}

	purged := false
	if params.Purge {
		// Clear the credential bundle by publishing empty content.
		_, err := session.SendStateEvent(ctx, location.ConfigRoomID,
			schema.EventTypeCredentials, localpart, struct{}{})
		if err != nil {
			// Non-fatal — the assignment is already removed.
			logger.Warn("failed to purge credentials", "error", err)
		} else {
			purged = true
		}
	}

	if done, err := params.EmitJSON(serviceDestroyResult{
		Localpart:     localpart,
		MachineName:   location.Machine.Localpart(),
		ConfigRoomID:  location.ConfigRoomID,
		ConfigEventID: destroyResult.ConfigEventID,
		Purged:        purged,
	}); done {
		return err
	}

	logger.Info("service assignment removed",
		"localpart", localpart,
		"machine", location.Machine.Localpart(),
		"config_event", destroyResult.ConfigEventID,
		"purged", purged,
	)

	return nil
}
