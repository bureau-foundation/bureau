// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/messaging"
)

type serviceScaleParams struct {
	cli.SessionConfig
	cli.FleetScope
	cli.JSONOutput
	Replicas    int `json:"replicas"     flag:"replicas"     desc:"new minimum replica count"`
	MaxReplicas int `json:"max_replicas" flag:"max-replicas" desc:"new maximum replica count"`
}

type serviceScaleResult struct {
	Localpart      string `json:"localpart"       desc:"service localpart"`
	OldReplicasMin int    `json:"old_replicas_min" desc:"previous minimum replica count"`
	OldReplicasMax int    `json:"old_replicas_max" desc:"previous maximum replica count"`
	NewReplicasMin int    `json:"new_replicas_min" desc:"updated minimum replica count"`
	NewReplicasMax int    `json:"new_replicas_max" desc:"updated maximum replica count"`
}

func scaleCommand() *cli.Command {
	var params serviceScaleParams

	return &cli.Command{
		Name:    "scale",
		Summary: "Update the replica count of a fleet-managed service",
		Description: `Update the minimum and/or maximum replica count of an existing fleet
service definition. Reads the current definition from the fleet room,
applies the requested changes, and writes the updated definition back.

The fleet controller detects the change via /sync and reconciles: if
the new minimum exceeds the current instance count, it places new
instances; if the new maximum is below the current count, it removes
excess instances.

At least one of --replicas or --max-replicas must be specified.`,
		Usage: "bureau service scale <localpart> --replicas <N> [flags]",
		Examples: []cli.Example{
			{
				Description: "Scale up to 3 minimum replicas",
				Command:     "bureau service scale service/stt/whisper --replicas 3 --credential-file ./creds",
			},
			{
				Description: "Set both min and max",
				Command:     "bureau service scale service/stt/whisper --replicas 2 --max-replicas 5 --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &serviceScaleResult{} },
		RequiredGrants: []string{"command/service/scale"},
		Annotations:    cli.Idempotent(),
		Run: cli.RequireLocalpart("service", "bureau service scale <localpart> --replicas <N> [flags]", func(ctx context.Context, localpart string, logger *slog.Logger) error {
			return runScale(ctx, localpart, logger, params)
		}),
	}
}

func runScale(ctx context.Context, localpart string, logger *slog.Logger, params serviceScaleParams) error {
	if params.Replicas == 0 && params.MaxReplicas == 0 {
		return cli.Validation("at least one of --replicas or --max-replicas must be specified")
	}

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

	fleetRoomAlias := scope.Fleet.RoomAlias()
	fleetRoomID, err := session.ResolveAlias(ctx, fleetRoomAlias)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return cli.NotFound("fleet room %s not found", fleetRoomAlias).
				WithHint("Has 'bureau matrix setup' been run for this fleet?")
		}
		return cli.Transient("resolving fleet room %s: %w", fleetRoomAlias, err)
	}

	// Read the existing definition.
	content, err := messaging.GetState[fleet.FleetServiceContent](ctx, session, fleetRoomID, schema.EventTypeFleetService, localpart)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return cli.NotFound("no fleet service definition for %q", localpart).
				WithHint("Use 'bureau service define' to create a fleet service definition first.")
		}
		return cli.Transient("reading fleet service definition for %q: %w", localpart, err)
	}

	oldMin := content.Replicas.Min
	oldMax := content.Replicas.Max

	// Apply changes.
	if params.Replicas > 0 {
		content.Replicas.Min = params.Replicas
	}
	if params.MaxReplicas > 0 {
		content.Replicas.Max = params.MaxReplicas
	}

	// Validate the result.
	if content.Replicas.Min < 1 {
		return cli.Validation("--replicas must be at least 1 (got %d)", content.Replicas.Min)
	}
	if content.Replicas.Max > 0 && content.Replicas.Max < content.Replicas.Min {
		return cli.Validation("--max-replicas (%d) must be >= --replicas (%d)", content.Replicas.Max, content.Replicas.Min)
	}

	// Write the updated definition.
	if _, err := session.SendStateEvent(ctx, fleetRoomID, schema.EventTypeFleetService, localpart, &content); err != nil {
		return cli.Transient("updating fleet service definition: %w", err)
	}

	result := serviceScaleResult{
		Localpart:      localpart,
		OldReplicasMin: oldMin,
		OldReplicasMax: oldMax,
		NewReplicasMin: content.Replicas.Min,
		NewReplicasMax: content.Replicas.Max,
	}

	if done, err := params.EmitJSON(result); done {
		return err
	}

	logger.Info("fleet service replicas updated",
		"localpart", localpart,
		"old_replicas", fmt.Sprintf("%d-%d", oldMin, oldMax),
		"new_replicas", fmt.Sprintf("%d-%d", content.Replicas.Min, content.Replicas.Max),
	)
	logger.Info("the fleet controller will reconcile on its next sync cycle")
	return nil
}
