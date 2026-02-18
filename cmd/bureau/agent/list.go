// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

type agentListParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Machine    string `json:"machine"     flag:"machine"     desc:"filter to a specific machine (optional — lists all machines if omitted)"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
}

// agentListEntry is a single row in the list output.
type agentListEntry struct {
	MachineName string `json:"machine"`
	Localpart   string `json:"localpart"`
	Template    string `json:"template"`
	AutoStart   bool   `json:"auto_start"`
	Status      string `json:"status"`
	Sessions    int64  `json:"sessions"`
	Tokens      int64  `json:"tokens"`
}

type agentListResult struct {
	Agents       []agentListEntry `json:"agents"`
	MachineCount int              `json:"machine_count"`
}

func listCommand() *cli.Command {
	var params agentListParams

	return &cli.Command{
		Name:    "list",
		Summary: "List agents across machines",
		Description: `List all agent principals, optionally filtered to a specific machine.

When --machine is omitted, scans all machines from #bureau/machine and
lists every assigned principal. The scan count is reported for diagnostics.

Each agent's status is enriched from agent service state events (best-effort):
  - "active": has an active session
  - "idle": no active session, at least one completed session
  - "pending": no session data yet (agent may not have started)
  - "-": agent service data not available`,
		Usage: "bureau agent list [--machine <machine>]",
		Examples: []cli.Example{
			{
				Description: "List all agents across all machines",
				Command:     "bureau agent list --credential-file ./creds",
			},
			{
				Description: "List agents on a specific machine",
				Command:     "bureau agent list --credential-file ./creds --machine machine/workstation",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &agentListResult{} },
		RequiredGrants: []string{"command/agent/list"},
		Annotations:    cli.ReadOnly(),
		Run: func(args []string) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}
			return runList(params)
		},
	}
}

func runList(params agentListParams) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	locations, machineCount, err := principal.List(ctx, session, params.Machine, params.ServerName)
	if err != nil {
		return cli.Internal("list agents: %w", err)
	}

	// Enrich with agent service state (session status, metrics).
	entries := make([]agentListEntry, len(locations))
	for i, location := range locations {
		entry := agentListEntry{
			MachineName: location.MachineName,
			Localpart:   location.Assignment.Localpart,
			Template:    location.Assignment.Template,
			AutoStart:   location.Assignment.AutoStart,
			Status:      "-",
		}

		// Best-effort enrichment from agent service state events.
		sessionContent, metricsContent := readAgentServiceState(ctx, session, location)
		if sessionContent != nil {
			if sessionContent.ActiveSessionID != "" {
				entry.Status = "active"
			} else if sessionContent.LatestSessionID != "" {
				entry.Status = "idle"
			} else {
				entry.Status = "pending"
			}
		}
		if metricsContent != nil {
			entry.Sessions = metricsContent.TotalSessionCount
			entry.Tokens = metricsContent.TotalInputTokens + metricsContent.TotalOutputTokens
		}

		entries[i] = entry
	}

	if done, err := params.EmitJSON(agentListResult{
		Agents:       entries,
		MachineCount: machineCount,
	}); done {
		return err
	}

	if params.Machine == "" && machineCount > 0 {
		fmt.Fprintf(os.Stderr, "resolved %d agent(s) across %d machine(s)\n", len(entries), machineCount)
	}

	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "no agents found")
		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if params.Machine == "" {
		fmt.Fprintln(writer, "MACHINE\tNAME\tTEMPLATE\tSTATUS\tSESSIONS\tTOKENS")
		for _, entry := range entries {
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%d\t%s\n",
				entry.MachineName, entry.Localpart, entry.Template,
				entry.Status, entry.Sessions, formatTokenCount(entry.Tokens))
		}
	} else {
		fmt.Fprintln(writer, "NAME\tTEMPLATE\tSTATUS\tSESSIONS\tTOKENS")
		for _, entry := range entries {
			fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s\n",
				entry.Localpart, entry.Template,
				entry.Status, entry.Sessions, formatTokenCount(entry.Tokens))
		}
	}
	writer.Flush()

	return nil
}

// readAgentServiceState reads the agent session and metrics state events
// for a given location. Returns nil for either if not found or on error
// (best-effort enrichment).
func readAgentServiceState(ctx context.Context, session *messaging.Session, location principal.Location) (*schema.AgentSessionContent, *schema.AgentMetricsContent) {
	localpart := location.Assignment.Localpart
	roomID := location.ConfigRoomID

	var sessionContent *schema.AgentSessionContent
	sessionRaw, err := session.GetStateEvent(ctx, roomID, schema.EventTypeAgentSession, localpart)
	if err == nil {
		var content schema.AgentSessionContent
		if json.Unmarshal(sessionRaw, &content) == nil {
			sessionContent = &content
		}
	}

	var metricsContent *schema.AgentMetricsContent
	metricsRaw, err := session.GetStateEvent(ctx, roomID, schema.EventTypeAgentMetrics, localpart)
	if err == nil {
		var content schema.AgentMetricsContent
		if json.Unmarshal(metricsRaw, &content) == nil {
			metricsContent = &content
		}
	}

	return sessionContent, metricsContent
}

// formatTokenCount formats a token count for display, using K/M suffixes
// for large values.
func formatTokenCount(tokens int64) string {
	if tokens == 0 {
		return "0"
	}
	if tokens >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	}
	if tokens >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(tokens)/1_000)
	}
	return fmt.Sprintf("%d", tokens)
}
