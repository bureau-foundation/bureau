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
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
)

type agentSessionParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Machine    string `json:"machine"     flag:"machine"     desc:"machine localpart (optional — auto-discovers if omitted)"`
	Fleet      string `json:"fleet"       flag:"fleet"       desc:"fleet prefix (e.g., bureau/fleet/prod) — required when --machine is omitted"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
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
		Output:         func() any { return &schema.AgentSessionContent{} },
		RequiredGrants: []string{"command/agent/session"},
		Annotations:    cli.ReadOnly(),
		Run: requireLocalpart("bureau agent session <localpart> [--machine <machine>]", func(localpart string) error {
			return runSession(localpart, params)
		}),
	}
}

func runSession(localpart string, params agentSessionParams) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	location, machineCount, err := principal.Resolve(ctx, session, localpart, params.Machine, params.Fleet, params.ServerName)
	if err != nil {
		return cli.NotFound("resolve agent: %w", err)
	}

	if params.Machine == "" && machineCount > 0 {
		fmt.Fprintf(os.Stderr, "resolved %s → %s (scanned %d machines)\n", localpart, location.MachineName, machineCount)
	}

	sessionRaw, err := session.GetStateEvent(ctx, location.ConfigRoomID, schema.EventTypeAgentSession, localpart)
	if err != nil {
		return cli.NotFound("no session data for %s: %w", localpart, err)
	}

	var content schema.AgentSessionContent
	if err := json.Unmarshal(sessionRaw, &content); err != nil {
		return cli.Internal("parse session data: %w", err)
	}

	if done, err := params.EmitJSON(content); done {
		return err
	}

	fmt.Printf("Agent:    %s\n", principal.MatrixUserID(localpart, params.ServerName))
	fmt.Printf("Machine:  %s\n", location.MachineName)
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
