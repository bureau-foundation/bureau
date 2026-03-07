// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
)

type agentDestroyParams struct {
	cli.SessionConfig
	cli.FleetScope
	cli.JSONOutput
	Purge bool `json:"purge" flag:"purge" desc:"also clear the credential bundle"`
}

type agentDestroyResult struct {
	Localpart     string      `json:"localpart"`
	MachineName   string      `json:"machine"`
	ConfigRoomID  ref.RoomID  `json:"config_room_id"`
	ConfigEventID ref.EventID `json:"config_event_id"`
	Purged        bool        `json:"purged"`
}

func destroyCommand() *cli.Command {
	var params agentDestroyParams

	return &cli.Command{
		Name:    "destroy",
		Summary: "Remove an agent's assignment from a machine",
		Description: `Remove an agent's PrincipalAssignment from the MachineConfig.

The daemon detects the config change via /sync and tears down the agent's
sandbox. The agent's Matrix account is preserved for audit trail purposes.

With --purge, also clears the m.bureau.credentials state event for this
principal (publishes empty content). Without --purge, credentials remain
in the config room and can be reused if the agent is re-created.

Does NOT deactivate the Matrix account — the agent's message history and
state event trail remain intact for auditing.`,
		Usage: "bureau agent destroy <localpart> [--machine <machine>]",
		Examples: []cli.Example{
			{
				Description: "Remove an agent (auto-discover machine)",
				Command:     "bureau agent destroy agent/code-review --credential-file ./creds",
			},
			{
				Description: "Remove and purge credentials",
				Command:     "bureau agent destroy agent/code-review --credential-file ./creds --purge",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &agentDestroyResult{} },
		RequiredGrants: []string{"command/agent/destroy"},
		Annotations:    cli.Destructive(),
		Run: cli.RequireLocalpart("agent", "bureau agent destroy <localpart> [--machine <machine>]", func(ctx context.Context, localpart string, logger *slog.Logger) error {
			return runDestroy(ctx, localpart, logger, params)
		}),
	}
}

func runDestroy(ctx context.Context, localpart string, logger *slog.Logger, params agentDestroyParams) error {
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

	location, machineCount, err := principal.Resolve(ctx, session, localpart, scope.Machine, scope.Fleet)
	if err != nil {
		return cli.NotFound("agent %q not found: %w", localpart, err).
			WithHint("Run 'bureau agent list' to see agents on this machine.")
	}

	if scope.Machine.IsZero() && machineCount > 0 {
		logger.Info("resolved agent location", "localpart", localpart, "machine", location.Machine.Localpart(), "machines_scanned", machineCount)
	}

	destroyResult, err := principal.Destroy(ctx, session, location.ConfigRoomID, location.Machine, localpart)
	if err != nil {
		return cli.Internal("remove agent assignment: %w", err)
	}

	purged := false
	if params.Purge {
		if err := principal.PurgeCredentials(ctx, session, location.ConfigRoomID, location.Assignment.Principal.UserID()); err != nil {
			logger.Warn("failed to purge credentials", "localpart", localpart, "error", err)
		} else {
			purged = true
		}
	}

	if done, err := params.EmitJSON(agentDestroyResult{
		Localpart:     localpart,
		MachineName:   location.Machine.Localpart(),
		ConfigRoomID:  location.ConfigRoomID,
		ConfigEventID: destroyResult.ConfigEventID,
		Purged:        purged,
	}); done {
		return err
	}

	logger.Info("agent assignment removed",
		"localpart", localpart,
		"machine", location.Machine.Localpart(),
		"config_event", destroyResult.ConfigEventID,
		"purged", purged,
	)

	return nil
}
