// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
)

type serviceDestroyParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Machine    string `json:"machine"     flag:"machine"     desc:"machine localpart (optional — auto-discovers if omitted)"`
	Fleet      string `json:"fleet"       flag:"fleet"       desc:"fleet prefix (e.g., bureau/fleet/prod) — required when --machine is omitted"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
	Purge      bool   `json:"purge"       flag:"purge"       desc:"also clear the credential bundle"`
}

type serviceDestroyResult struct {
	Localpart     string `json:"localpart"`
	MachineName   string `json:"machine"`
	ConfigRoomID  string `json:"config_room_id"`
	ConfigEventID string `json:"config_event_id"`
	Purged        bool   `json:"purged"`
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
		Run: requireLocalpart("bureau service destroy <localpart> [--machine <machine>]", func(localpart string) error {
			return runDestroy(localpart, params)
		}),
	}
}

func runDestroy(localpart string, params serviceDestroyParams) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	location, machineCount, err := principal.Resolve(ctx, session, localpart, params.Machine, params.Fleet, params.ServerName)
	if err != nil {
		return cli.NotFound("resolve service: %w", err)
	}

	if params.Machine == "" && machineCount > 0 {
		fmt.Fprintf(os.Stderr, "resolved %s → %s (scanned %d machines)\n", localpart, location.MachineName, machineCount)
	}

	destroyResult, err := principal.Destroy(ctx, session, location.ConfigRoomID, location.MachineName, localpart)
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
			fmt.Fprintf(os.Stderr, "warning: failed to purge credentials: %v\n", err)
		} else {
			purged = true
		}
	}

	if done, err := params.EmitJSON(serviceDestroyResult{
		Localpart:     localpart,
		MachineName:   location.MachineName,
		ConfigRoomID:  location.ConfigRoomID,
		ConfigEventID: destroyResult.ConfigEventID,
		Purged:        purged,
	}); done {
		return err
	}

	fmt.Fprintf(os.Stderr, "Removed %s from %s\n", localpart, location.MachineName)
	if purged {
		fmt.Fprintf(os.Stderr, "  Credentials purged\n")
	}
	fmt.Fprintf(os.Stderr, "  Config event: %s\n", destroyResult.ConfigEventID)
	fmt.Fprintf(os.Stderr, "\nThe daemon will tear down the sandbox on its next reconciliation cycle.\n")

	return nil
}
