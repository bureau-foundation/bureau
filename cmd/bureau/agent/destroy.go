// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	libagent "github.com/bureau-foundation/bureau/lib/agent"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

type agentDestroyParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Machine    string `json:"machine"     flag:"machine"     desc:"machine localpart (optional — auto-discovers if omitted)"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
	Purge      bool   `json:"purge"       flag:"purge"       desc:"also clear the credential bundle"`
}

type agentDestroyResult struct {
	Localpart     string `json:"localpart"`
	MachineName   string `json:"machine"`
	ConfigRoomID  string `json:"config_room_id"`
	ConfigEventID string `json:"config_event_id"`
	Purged        bool   `json:"purged"`
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
		Run: requireLocalpart("bureau agent destroy <localpart> [--machine <machine>]", func(localpart string) error {
			return runDestroy(localpart, params)
		}),
	}
}

func runDestroy(localpart string, params agentDestroyParams) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	location, machineCount, err := libagent.ResolveAgent(ctx, session, localpart, params.Machine, params.ServerName)
	if err != nil {
		return cli.NotFound("resolve agent: %w", err)
	}

	if params.Machine == "" && machineCount > 0 {
		fmt.Fprintf(os.Stderr, "resolved %s → %s (scanned %d machines)\n", localpart, location.MachineName, machineCount)
	}

	// Read-modify-write MachineConfig: remove this principal's assignment.
	configEventID, err := removePrincipal(ctx, session, location.ConfigRoomID, location.MachineName, localpart)
	if err != nil {
		return cli.Internal("remove agent assignment: %w", err)
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

	if done, err := params.EmitJSON(agentDestroyResult{
		Localpart:     localpart,
		MachineName:   location.MachineName,
		ConfigRoomID:  location.ConfigRoomID,
		ConfigEventID: configEventID,
		Purged:        purged,
	}); done {
		return err
	}

	fmt.Fprintf(os.Stderr, "Removed %s from %s\n", localpart, location.MachineName)
	if purged {
		fmt.Fprintf(os.Stderr, "  Credentials purged\n")
	}
	fmt.Fprintf(os.Stderr, "  Config event: %s\n", configEventID)
	fmt.Fprintf(os.Stderr, "\nThe daemon will tear down the sandbox on its next reconciliation cycle.\n")

	return nil
}

// removePrincipal reads the MachineConfig, removes the named principal,
// and publishes the updated config. Returns an error if the principal
// is not found in the config.
func removePrincipal(ctx context.Context, session *messaging.Session, configRoomID, machineName, localpart string) (string, error) {
	configRaw, err := session.GetStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineName)
	if err != nil {
		return "", fmt.Errorf("read machine config for %s: %w", machineName, err)
	}

	var config schema.MachineConfig
	if err := json.Unmarshal(configRaw, &config); err != nil {
		return "", fmt.Errorf("parse machine config for %s: %w", machineName, err)
	}

	found := false
	filtered := make([]schema.PrincipalAssignment, 0, len(config.Principals))
	for _, assignment := range config.Principals {
		if assignment.Localpart == localpart {
			found = true
			continue
		}
		filtered = append(filtered, assignment)
	}
	if !found {
		return "", fmt.Errorf("principal %q not found in machine config for %s", localpart, machineName)
	}
	config.Principals = filtered

	eventID, err := session.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineName, config)
	if err != nil {
		return "", fmt.Errorf("publish machine config for %s: %w", machineName, err)
	}

	return eventID, nil
}
