// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/messaging"
)

type serviceDeleteParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Fleet      string `json:"fleet"       flag:"fleet"       desc:"fleet prefix (auto-detected from machine.conf)"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name (auto-detected)"`
	Yes        bool   `json:"yes"         flag:"yes"         desc:"confirm deletion without prompting"`
}

type serviceDeleteResult struct {
	Localpart string `json:"localpart" desc:"deleted service localpart"`
	Template  string `json:"template"  desc:"template of the deleted definition"`
}

func deleteCommand() *cli.Command {
	var params serviceDeleteParams

	return &cli.Command{
		Name:    "delete",
		Summary: "Delete a fleet-managed service definition",
		Description: `Remove a fleet service definition from the fleet room by publishing
an empty state event (tombstone).

The fleet controller detects the removal via /sync and removes all
fleet-managed PrincipalAssignment events for this service from every
machine it was placed on. Running instances are torn down as daemons
detect the config changes.

Requires --yes to confirm, since deletion affects all instances across
the fleet.`,
		Usage: "bureau service delete <localpart> --yes [flags]",
		Examples: []cli.Example{
			{
				Description: "Delete a fleet service definition",
				Command:     "bureau service delete service/batch/worker --yes --credential-file ./creds",
			},
			{
				Description: "Preview what would be deleted",
				Command:     "bureau service delete service/batch/worker --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &serviceDeleteResult{} },
		RequiredGrants: []string{"command/service/delete"},
		Annotations:    cli.Destructive(),
		Run: requireLocalpart("bureau service delete <localpart> --yes [flags]", func(ctx context.Context, localpart string, logger *slog.Logger) error {
			return runDelete(ctx, localpart, logger, params)
		}),
	}
}

func runDelete(ctx context.Context, localpart string, logger *slog.Logger, params serviceDeleteParams) error {
	params.ServerName = cli.ResolveServerName(params.ServerName)
	params.Fleet = cli.ResolveFleet(params.Fleet)

	serverName, err := ref.ParseServerName(params.ServerName)
	if err != nil {
		return cli.Validation("invalid --server-name %q: %w", params.ServerName, err)
	}

	fleetRef, err := ref.ParseFleet(params.Fleet, serverName)
	if err != nil {
		return cli.Validation("invalid --fleet %q: %w", params.Fleet, err)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	fleetRoomAlias := fleetRef.RoomAlias()
	fleetRoomID, err := session.ResolveAlias(ctx, fleetRoomAlias)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return cli.NotFound("fleet room %s not found", fleetRoomAlias).
				WithHint("Has 'bureau matrix setup' been run for this fleet?")
		}
		return cli.Transient("resolving fleet room %s: %w", fleetRoomAlias, err)
	}

	// Read the existing definition to confirm it exists and display details.
	content, err := messaging.GetState[fleet.FleetServiceContent](ctx, session, fleetRoomID, schema.EventTypeFleetService, localpart)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return cli.NotFound("no fleet service definition for %q", localpart).
				WithHint("Run 'bureau service list' to see defined services.")
		}
		return cli.Transient("reading fleet service definition for %q: %w", localpart, err)
	}

	if !params.Yes {
		fmt.Printf("Fleet service definition to delete:\n")
		fmt.Printf("  Localpart:  %s\n", localpart)
		fmt.Printf("  Template:   %s\n", content.Template)
		fmt.Printf("  Replicas:   %d (min) / %d (max)\n", content.Replicas.Min, content.Replicas.Max)
		fmt.Printf("  Failover:   %s\n", content.Failover)
		fmt.Printf("  Priority:   %d\n", content.Priority)
		fmt.Println()
		return cli.Validation("pass --yes to confirm deletion").
			WithHint("Deleting a fleet service definition causes the fleet controller to\n" +
				"remove all managed instances across the fleet.")
	}

	// Tombstone: publish empty content to remove the definition.
	if _, err := session.SendStateEvent(ctx, fleetRoomID, schema.EventTypeFleetService, localpart, struct{}{}); err != nil {
		return cli.Transient("deleting fleet service definition: %w", err)
	}

	result := serviceDeleteResult{
		Localpart: localpart,
		Template:  content.Template,
	}

	if done, err := params.EmitJSON(result); done {
		return err
	}

	logger.Info("fleet service definition deleted",
		"localpart", localpart,
		"template", content.Template,
	)
	logger.Info("the fleet controller will remove all managed instances on its next sync cycle")
	return nil
}
