// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	agentschema "github.com/bureau-foundation/bureau/lib/schema/agent"
	"github.com/bureau-foundation/bureau/messaging"
)

type agentSessionParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Machine    string `json:"machine"     flag:"machine"     desc:"machine localpart (optional — auto-discovers if omitted)"`
	Fleet      string `json:"fleet"       flag:"fleet"       desc:"fleet prefix (e.g., bureau/fleet/prod) — required when --machine is omitted"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name (auto-detected from machine.conf)"`
}

func sessionCommand() *cli.Command {
	var params agentSessionParams

	return &cli.Command{
		Name:    "session",
		Summary: "Show agent session lifecycle state",
		Description: `Display the session lifecycle state for an agent principal.

Shows the current active session (if any), the most recently completed
session, and artifact references for session logs and the session index.
This is a direct view of the m.bureau.agent_session state event.`,
		Usage: "bureau agent session <localpart> [--machine <machine>]",
		Examples: []cli.Example{
			{
				Description: "Show session state (auto-discover machine)",
				Command:     "bureau agent session agent/code-review --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &agentschema.AgentSessionContent{} },
		RequiredGrants: []string{"command/agent/session"},
		Annotations:    cli.ReadOnly(),
		Run: requireLocalpart("bureau agent session <localpart> [--machine <machine>]", func(ctx context.Context, localpart string, logger *slog.Logger) error {
			return runSession(ctx, localpart, logger, params)
		}),
	}
}

func runSession(ctx context.Context, localpart string, logger *slog.Logger, params agentSessionParams) error {
	params.ServerName = cli.ResolveServerName(params.ServerName)
	params.Fleet = cli.ResolveFleet(params.Fleet)

	serverName, err := ref.ParseServerName(params.ServerName)
	if err != nil {
		return cli.Validation("invalid --server-name %q: %w", params.ServerName, err)
	}

	agentRef, err := ref.ParseAgent(localpart, serverName)
	if err != nil {
		return cli.Validation("invalid agent localpart: %v", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	var machine ref.Machine
	if params.Machine != "" {
		machine, err = ref.ParseMachine(params.Machine, serverName)
		if err != nil {
			return cli.Validation("invalid machine: %v", err)
		}
	}

	var fleet ref.Fleet
	if machine.IsZero() {
		fleet, err = ref.ParseFleet(params.Fleet, serverName)
		if err != nil {
			return cli.Validation("invalid fleet: %v", err)
		}
	} else {
		fleet = machine.Fleet()
	}

	location, machineCount, err := principal.Resolve(ctx, session, localpart, machine, fleet)
	if err != nil {
		return cli.NotFound("agent %q not found: %w", localpart, err).
			WithHint("Run 'bureau agent list' to see agents on this machine.")
	}

	if machine.IsZero() && machineCount > 0 {
		logger.Info("resolved agent location", "localpart", localpart, "machine", location.Machine.Localpart(), "machines_scanned", machineCount)
	}

	content, err := messaging.GetState[agentschema.AgentSessionContent](ctx, session, location.ConfigRoomID, agentschema.EventTypeAgentSession, localpart)
	if err != nil {
		return cli.NotFound("no session data for agent %q: %w", localpart, err).
			WithHint(fmt.Sprintf("The agent may not have started a session yet. Run 'bureau agent show %s' to check its status.", localpart))
	}

	if done, err := params.EmitJSON(content); done {
		return err
	}

	fmt.Printf("Agent:    %s\n", agentRef.UserID())
	fmt.Printf("Machine:  %s\n", location.Machine.Localpart())
	fmt.Println()

	if content.ActiveSessionID != "" {
		fmt.Printf("Active session:  %s\n", content.ActiveSessionID)
		fmt.Printf("  Started:       %s\n", content.ActiveSessionStartedAt)
	} else {
		fmt.Printf("Active session:  (none)\n")
	}

	if content.LatestSessionID != "" {
		fmt.Printf("Latest session:  %s\n", content.LatestSessionID)
	} else {
		fmt.Printf("Latest session:  (none)\n")
	}

	if content.LatestSessionArtifactRef != "" {
		fmt.Printf("Session log:     %s\n", content.LatestSessionArtifactRef)
	}
	if content.SessionIndexArtifactRef != "" {
		fmt.Printf("Session index:   %s\n", content.SessionIndexArtifactRef)
	}

	return nil
}
