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

type agentMetricsParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Machine    string `json:"machine"     flag:"machine"     desc:"machine localpart (optional — auto-discovers if omitted)"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
}

func metricsCommand() *cli.Command {
	var params agentMetricsParams

	return &cli.Command{
		Name:    "metrics",
		Summary: "Show aggregated agent metrics",
		Description: `Display aggregated metrics across all sessions for an agent principal.

Shows lifetime totals: session count, token usage (input/output/cache),
cost, tool calls, turns, errors, and duration. This is a direct view
of the m.bureau.agent_metrics state event.`,
		Usage: "bureau agent metrics <localpart> [--machine <machine>]",
		Examples: []cli.Example{
			{
				Description: "Show metrics (auto-discover machine)",
				Command:     "bureau agent metrics agent/code-review --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &schema.AgentMetricsContent{} },
		RequiredGrants: []string{"command/agent/metrics"},
		Annotations:    cli.ReadOnly(),
		Run: requireLocalpart("bureau agent metrics <localpart> [--machine <machine>]", func(localpart string) error {
			return runMetrics(localpart, params)
		}),
	}
}

func runMetrics(localpart string, params agentMetricsParams) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	location, machineCount, err := principal.Resolve(ctx, session, localpart, params.Machine, params.ServerName)
	if err != nil {
		return cli.NotFound("resolve agent: %w", err)
	}

	if params.Machine == "" && machineCount > 0 {
		fmt.Fprintf(os.Stderr, "resolved %s → %s (scanned %d machines)\n", localpart, location.MachineName, machineCount)
	}

	metricsRaw, err := session.GetStateEvent(ctx, location.ConfigRoomID, schema.EventTypeAgentMetrics, localpart)
	if err != nil {
		return cli.NotFound("no metrics data for %s: %w", localpart, err)
	}

	var content schema.AgentMetricsContent
	if err := json.Unmarshal(metricsRaw, &content); err != nil {
		return cli.Internal("parse metrics data: %w", err)
	}

	if done, err := params.EmitJSON(content); done {
		return err
	}

	fmt.Printf("Agent:    %s\n", principal.MatrixUserID(localpart, params.ServerName))
	fmt.Printf("Machine:  %s\n", location.MachineName)
	fmt.Println()

	fmt.Printf("Sessions:           %d\n", content.TotalSessionCount)
	fmt.Printf("Input tokens:       %s\n", formatTokenCount(content.TotalInputTokens))
	fmt.Printf("Output tokens:      %s\n", formatTokenCount(content.TotalOutputTokens))
	if content.TotalCacheReadTokens > 0 {
		fmt.Printf("Cache read tokens:  %s\n", formatTokenCount(content.TotalCacheReadTokens))
	}
	if content.TotalCacheWriteTokens > 0 {
		fmt.Printf("Cache write tokens: %s\n", formatTokenCount(content.TotalCacheWriteTokens))
	}
	fmt.Printf("Tool calls:         %d\n", content.TotalToolCalls)
	fmt.Printf("Turns:              %d\n", content.TotalTurns)
	if content.TotalErrors > 0 {
		fmt.Printf("Errors:             %d\n", content.TotalErrors)
	}
	if content.TotalCostMilliUSD > 0 {
		fmt.Printf("Cost:               $%.2f\n", float64(content.TotalCostMilliUSD)/1000)
	}
	if content.TotalDurationSeconds > 0 {
		fmt.Printf("Duration:           %s\n", formatDuration(content.TotalDurationSeconds))
	}
	if content.LastUpdatedAt != "" {
		fmt.Printf("Last updated:       %s\n", content.LastUpdatedAt)
	}

	return nil
}
