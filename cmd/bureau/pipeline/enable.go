// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	pipelineschema "github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/messaging"
)

// enableParams holds the parameters for the pipeline enable command.
type enableParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Space      string `json:"space"       flag:"space"       desc:"project space alias (e.g., iree) — enable pipeline execution in all rooms in this space" required:"true"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name (auto-detected from session)"`
}

// enableResult is the JSON output of the pipeline enable command.
type enableResult struct {
	SpaceAlias      string   `json:"space_alias"       desc:"Matrix space alias"`
	SpaceRoomID     string   `json:"space_room_id"     desc:"Matrix space room ID"`
	RoomsConfigured []string `json:"rooms_configured"  desc:"room IDs configured for pipeline execution"`
}

func enableCommand() *cli.Command {
	var params enableParams

	return &cli.Command{
		Name:    "enable",
		Summary: "Enable pipeline execution for a space",
		Description: `Enable pipeline execution in all rooms within a project space.

This operator command configures each room in a Matrix space to allow
pipeline execution. When a room has pipeline_config, the daemon processes
pip- tickets in that room and spawns pipeline executor sandboxes. Rooms
without pipeline_config are skipped during pipeline ticket processing.

The command:
  - Resolves the project space alias
  - For each child room: publishes m.bureau.pipeline_config and
    configures power levels (m.bureau.pipeline_config at PL 100)

Unlike "bureau ticket enable" or "bureau fleet enable", this command does
not deploy a service principal — the daemon handles pipeline execution
directly. Pipeline enable is purely room-level configuration.

Re-running pipeline enable on a space is safe: it re-publishes the same
configuration to each room (useful when new rooms have been added).

Example:

  bureau pipeline enable --space iree

This enables pipeline execution in all rooms under #iree:bureau.local.`,
		Usage: "bureau pipeline enable --space <space> [flags]",
		Examples: []cli.Example{
			{
				Description: "Enable pipeline execution for the iree space",
				Command:     "bureau pipeline enable --space iree",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &enableResult{} },
		Annotations:    cli.Idempotent(),
		RequiredGrants: []string{"command/pipeline/enable"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}
			if params.Space == "" {
				return cli.Validation("--space is required")
			}
			return runEnable(ctx, logger, &params)
		},
	}
}

func runEnable(ctx context.Context, logger *slog.Logger, params *enableParams) error {
	params.ServerName = cli.ResolveServerName(params.ServerName)

	serverName, err := ref.ParseServerName(params.ServerName)
	if err != nil {
		return fmt.Errorf("invalid --server-name: %w", err)
	}

	namespace, err := ref.NewNamespace(serverName, params.Space)
	if err != nil {
		return cli.Validation("invalid space name: %w", err)
	}
	spaceAlias := namespace.SpaceAlias()

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return cli.Internal("connecting session: %w", err)
	}
	defer session.Close()

	// Resolve the space and discover child rooms.
	spaceRoomID, err := session.ResolveAlias(ctx, spaceAlias)
	if err != nil {
		return cli.NotFound("resolving space %s: %w (has 'bureau matrix space create %s' been run?)", spaceAlias, err, params.Space)
	}

	childRoomIDs, err := cli.GetSpaceChildren(ctx, session, spaceRoomID)
	if err != nil {
		return cli.Internal("listing space children: %w", err)
	}

	logger.Info("discovered space", "alias", spaceAlias, "room_id", spaceRoomID, "child_rooms", len(childRoomIDs))

	// Configure each room for pipeline execution.
	configuredRooms := make([]string, 0, len(childRoomIDs))
	for _, roomID := range childRoomIDs {
		if err := ConfigureRoom(ctx, logger, session, roomID, ConfigureRoomParams{}); err != nil {
			logger.Warn("failed to configure room", "room_id", roomID, "error", err)
			continue
		}
		configuredRooms = append(configuredRooms, roomID.String())
		logger.Info("configured room", "room_id", roomID)
	}

	result := enableResult{
		SpaceAlias:      spaceAlias.String(),
		SpaceRoomID:     spaceRoomID.String(),
		RoomsConfigured: configuredRooms,
	}

	if done, emitError := params.EmitJSON(result); done {
		return emitError
	}

	logger.Info("pipeline execution enabled",
		"space_alias", spaceAlias,
		"space_room_id", spaceRoomID,
		"rooms_configured", len(configuredRooms),
	)

	return nil
}

// ConfigureRoomParams holds parameters for ConfigureRoom. Currently
// empty — reserved for future configuration options (allowed pipelines,
// execution constraints).
type ConfigureRoomParams struct{}

// ConfigureRoom enables pipeline execution in an existing room:
//   - Publishes m.bureau.pipeline_config (enables pipeline execution)
//   - Configures power levels (m.bureau.pipeline_config at PL 100)
//
// This is the shared infrastructure called by both the "pipeline enable"
// CLI command and "workspace create". The presence of pipeline_config in
// a room is what the daemon checks before processing pip- tickets.
//
// Power level note: m.bureau.pipeline_result does not need explicit PL
// configuration. In workspace rooms, state_default is 0, so the daemon
// (PL 50) already has permission to publish pipeline results.
func ConfigureRoom(ctx context.Context, logger *slog.Logger, session messaging.Session, roomID ref.RoomID, params ConfigureRoomParams) error {
	// Publish pipeline config (singleton, state_key="").
	pipelineConfig := pipelineschema.PipelineConfigContent{
		Version: pipelineschema.PipelineConfigVersion,
	}
	_, err := session.SendStateEvent(ctx, roomID, schema.EventTypePipelineConfig, "", pipelineConfig)
	if err != nil {
		return cli.Internal("publishing pipeline config: %w", err)
	}

	// Configure power levels. Only m.bureau.pipeline_config needs explicit
	// PL protection — it must be admin-only (PL 100) to prevent agents
	// from enabling pipeline execution in arbitrary rooms. Pipeline result
	// events fall through to the room's state_default.
	if err := configurePipelinePowerLevels(ctx, session, roomID); err != nil {
		return cli.Internal("configuring power levels: %w", err)
	}

	return nil
}

// configurePipelinePowerLevels performs a read-modify-write on a room's
// m.room.power_levels to protect pipeline configuration:
//   - m.bureau.pipeline_config requires PL 100 (admin-only)
func configurePipelinePowerLevels(ctx context.Context, session messaging.Session, roomID ref.RoomID) error {
	return schema.GrantPowerLevels(ctx, session, roomID, schema.PowerLevelGrants{
		Events: map[ref.EventType]int{
			schema.EventTypePipelineConfig: 100,
		},
	})
}
