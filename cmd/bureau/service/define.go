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

type serviceDefineParams struct {
	cli.SessionConfig
	cli.FleetScope
	cli.JSONOutput
	Template          string   `json:"template"           flag:"template"           desc:"template reference (required)"`
	Failover          string   `json:"failover"           flag:"failover"           desc:"failover strategy: migrate, alert, or none (required)"`
	Replicas          int      `json:"replicas"           flag:"replicas"           desc:"minimum replica count" default:"1"`
	MaxReplicas       int      `json:"max_replicas"       flag:"max-replicas"       desc:"maximum replica count (default: same as --replicas)"`
	Priority          int      `json:"priority"           flag:"priority"           desc:"placement priority (lower = higher priority)" default:"50"`
	Requires          []string `json:"requires"           flag:"requires"           desc:"required machine labels (repeatable)"`
	PreferredMachines []string `json:"preferred_machines" flag:"preferred-machine"  desc:"preferred machines in priority order (repeatable)"`
	AllowedMachines   []string `json:"allowed_machines"   flag:"allowed-machines"   desc:"allowed machine glob patterns (repeatable)"`
	AntiAffinity      []string `json:"anti_affinity"      flag:"anti-affinity"      desc:"services to avoid co-locating with (repeatable)"`
	CoLocateWith      []string `json:"co_locate_with"     flag:"co-locate-with"     desc:"services to prefer co-locating with (repeatable)"`
	HAClass           string   `json:"ha_class"           flag:"ha-class"           desc:"HA class: critical (enables daemon watchdog)"`
	Scheduling        string   `json:"scheduling"         flag:"scheduling"         desc:"scheduling class: always or batch" default:"always"`
	Preemptible       bool     `json:"preemptible"        flag:"preemptible"        desc:"service can be displaced by higher priority"`
	Rooms             []string `json:"rooms"              flag:"rooms"              desc:"room alias glob patterns for service binding (repeatable)"`
}

type serviceDefineResult struct {
	Localpart   string `json:"localpart"    desc:"service localpart"`
	FleetRoomID string `json:"fleet_room_id" desc:"fleet room where the definition was published"`
	Template    string `json:"template"     desc:"template reference"`
	Replicas    int    `json:"replicas"     desc:"minimum replica count"`
	MaxReplicas int    `json:"max_replicas" desc:"maximum replica count"`
	Failover    string `json:"failover"     desc:"failover strategy"`
	Priority    int    `json:"priority"     desc:"placement priority"`
}

func defineCommand() *cli.Command {
	var params serviceDefineParams

	return &cli.Command{
		Name:    "define",
		Summary: "Define a fleet-managed service",
		Description: `Publish a fleet service definition to the fleet room. The fleet
controller discovers it via /sync and evaluates placement.

This creates or updates the m.bureau.fleet_service state event for the
given service localpart. The fleet controller then manages the service's
lifecycle: placing instances on machines, handling failover, and
maintaining replica counts.

The --template and --failover flags are required. All other flags have
sensible defaults.

To update an existing definition, run "define" again with the new values.
The state event is overwritten atomically.`,
		Usage: "bureau service define <localpart> --template <ref> --failover <strategy> [flags]",
		Examples: []cli.Example{
			{
				Description: "Define a simple service",
				Command:     "bureau service define service/ticket --template bureau/template:ticket-service --failover migrate --credential-file ./creds",
			},
			{
				Description: "Define a GPU service with placement constraints",
				Command:     "bureau service define service/stt/whisper --template bureau/template:whisper-stt --failover migrate --requires gpu --replicas 2 --priority 10 --credential-file ./creds",
			},
			{
				Description: "Define a batch service",
				Command:     "bureau service define service/batch/worker --template bureau/template:worker --failover alert --scheduling batch --preemptible --priority 100 --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &serviceDefineResult{} },
		RequiredGrants: []string{"command/service/define"},
		Annotations:    cli.Create(),
		Run: cli.RequireLocalpart("service", "bureau service define <localpart> --template <ref> --failover <strategy> [flags]", func(ctx context.Context, localpart string, logger *slog.Logger) error {
			return runDefine(ctx, localpart, logger, params)
		}),
	}
}

func runDefine(ctx context.Context, localpart string, logger *slog.Logger, params serviceDefineParams) error {
	// Validate required flags.
	if params.Template == "" {
		return cli.Validation("--template is required")
	}
	if params.Failover == "" {
		return cli.Validation("--failover is required (migrate, alert, or none)")
	}

	// Validate enum values.
	switch fleet.FailoverStrategy(params.Failover) {
	case fleet.FailoverMigrate, fleet.FailoverAlert, fleet.FailoverNone:
		// Valid.
	default:
		return cli.Validation("--failover must be one of: migrate, alert, none (got %q)", params.Failover)
	}

	if params.HAClass != "" && !fleet.HAClass(params.HAClass).IsKnown() {
		return cli.Validation("--ha-class must be 'critical' (got %q)", params.HAClass)
	}

	switch fleet.SchedulingClass(params.Scheduling) {
	case fleet.SchedulingAlways, fleet.SchedulingBatch:
		// Valid.
	default:
		return cli.Validation("--scheduling must be one of: always, batch (got %q)", params.Scheduling)
	}

	if params.Replicas < 1 {
		return cli.Validation("--replicas must be at least 1 (got %d)", params.Replicas)
	}

	maxReplicas := params.MaxReplicas
	if maxReplicas == 0 {
		maxReplicas = params.Replicas
	}
	if maxReplicas < params.Replicas {
		return cli.Validation("--max-replicas (%d) must be >= --replicas (%d)", maxReplicas, params.Replicas)
	}

	// Resolve fleet room.
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

	// Build the fleet service definition.
	content := fleet.FleetServiceContent{
		Template: params.Template,
		Replicas: fleet.ReplicaSpec{
			Min: params.Replicas,
			Max: maxReplicas,
		},
		Placement: fleet.PlacementConstraints{
			Requires:          params.Requires,
			PreferredMachines: params.PreferredMachines,
			AllowedMachines:   params.AllowedMachines,
			AntiAffinity:      params.AntiAffinity,
			CoLocateWith:      params.CoLocateWith,
		},
		Failover:     fleet.FailoverStrategy(params.Failover),
		HAClass:      fleet.HAClass(params.HAClass),
		ServiceRooms: params.Rooms,
		Priority:     params.Priority,
	}

	// Scheduling: nil means "always running" (the default).
	if fleet.SchedulingClass(params.Scheduling) == fleet.SchedulingBatch {
		content.Scheduling = &fleet.SchedulingSpec{
			Class:       fleet.SchedulingBatch,
			Preemptible: params.Preemptible,
		}
	}

	if _, err := session.SendStateEvent(ctx, fleetRoomID, schema.EventTypeFleetService, localpart, &content); err != nil {
		return cli.Transient("publishing fleet service definition: %w", err)
	}

	result := serviceDefineResult{
		Localpart:   localpart,
		FleetRoomID: fleetRoomID.String(),
		Template:    params.Template,
		Replicas:    params.Replicas,
		MaxReplicas: maxReplicas,
		Failover:    params.Failover,
		Priority:    params.Priority,
	}

	if done, err := params.EmitJSON(result); done {
		return err
	}

	logger.Info("fleet service definition published",
		"localpart", localpart,
		"template", params.Template,
		"replicas", fmt.Sprintf("%d-%d", params.Replicas, maxReplicas),
		"failover", params.Failover,
		"priority", params.Priority,
	)
	logger.Info("the fleet controller will evaluate placement on its next sync cycle")
	return nil
}
