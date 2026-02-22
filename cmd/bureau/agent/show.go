// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	agentschema "github.com/bureau-foundation/bureau/lib/schema/agent"
)

type agentShowParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Machine    string `json:"machine"     flag:"machine"     desc:"machine localpart (optional — auto-discovers if omitted)"`
	Fleet      string `json:"fleet"       flag:"fleet"       desc:"fleet prefix (e.g., bureau/fleet/prod) — required when --machine is omitted"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
}

type agentShowResult struct {
	PrincipalUserID string                           `json:"principal_user_id"`
	MachineName     string                           `json:"machine"`
	Template        string                           `json:"template"`
	AutoStart       bool                             `json:"auto_start"`
	Labels          map[string]string                `json:"labels,omitempty"`
	Session         *agentschema.AgentSessionContent `json:"session,omitempty"`
	Metrics         *agentschema.AgentMetricsContent `json:"metrics,omitempty"`
}

func showCommand() *cli.Command {
	var params agentShowParams

	return &cli.Command{
		Name:    "show",
		Summary: "Show detailed information about an agent",
		Description: `Display detailed information about a single agent principal.

If --machine is omitted, scans all machines to find where the agent
is assigned. The scan count is reported for diagnostics.

Shows the agent's assignment details (template, auto-start, labels)
plus agent service state (session lifecycle, aggregated metrics).`,
		Usage: "bureau agent show <localpart> [--machine <machine>]",
		Examples: []cli.Example{
			{
				Description: "Show agent details (auto-discover machine)",
				Command:     "bureau agent show agent/code-review --credential-file ./creds",
			},
			{
				Description: "Show agent on a specific machine",
				Command:     "bureau agent show agent/code-review --credential-file ./creds --machine machine/workstation",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &agentShowResult{} },
		RequiredGrants: []string{"command/agent/show"},
		Annotations:    cli.ReadOnly(),
		Run: requireLocalpart("bureau agent show <localpart> [--machine <machine>]", func(localpart string) error {
			return runShow(localpart, params)
		}),
	}
}

func runShow(localpart string, params agentShowParams) error {
	serverName, err := ref.ParseServerName(params.ServerName)
	if err != nil {
		return fmt.Errorf("invalid --server-name: %w", err)
	}

	agentRef, err := ref.ParseAgent(localpart, serverName)
	if err != nil {
		return cli.Validation("invalid agent localpart: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
		return cli.NotFound("resolve agent: %w", err)
	}

	if machine.IsZero() && machineCount > 0 {
		fmt.Fprintf(os.Stderr, "resolved %s → %s (scanned %d machines)\n", localpart, location.Machine.Localpart(), machineCount)
	}

	sessionContent, metricsContent := readAgentServiceState(ctx, session, *location)

	result := agentShowResult{
		PrincipalUserID: agentRef.UserID().String(),
		MachineName:     location.Machine.Localpart(),
		Template:        location.Assignment.Template,
		AutoStart:       location.Assignment.AutoStart,
		Labels:          location.Assignment.Labels,
		Session:         sessionContent,
		Metrics:         metricsContent,
	}

	if done, err := params.EmitJSON(result); done {
		return err
	}

	// Text output.
	fmt.Printf("Principal:   %s\n", result.PrincipalUserID)
	fmt.Printf("Machine:     %s\n", result.MachineName)
	fmt.Printf("Template:    %s\n", result.Template)
	fmt.Printf("Auto-start:  %v\n", result.AutoStart)

	if len(result.Labels) > 0 {
		fmt.Printf("Labels:\n")
		for key, value := range result.Labels {
			fmt.Printf("  %s: %s\n", key, value)
		}
	}

	if sessionContent != nil {
		fmt.Printf("\nSession:\n")
		if sessionContent.ActiveSessionID != "" {
			fmt.Printf("  Active:    %s (started %s)\n", sessionContent.ActiveSessionID, sessionContent.ActiveSessionStartedAt)
		} else {
			fmt.Printf("  Active:    (none)\n")
		}
		if sessionContent.LatestSessionID != "" {
			fmt.Printf("  Latest:    %s\n", sessionContent.LatestSessionID)
		}
	}

	if metricsContent != nil {
		fmt.Printf("\nMetrics:\n")
		fmt.Printf("  Sessions:       %d\n", metricsContent.TotalSessionCount)
		fmt.Printf("  Input tokens:   %s\n", formatTokenCount(metricsContent.TotalInputTokens))
		fmt.Printf("  Output tokens:  %s\n", formatTokenCount(metricsContent.TotalOutputTokens))
		fmt.Printf("  Tool calls:     %d\n", metricsContent.TotalToolCalls)
		if metricsContent.TotalCostMilliUSD > 0 {
			fmt.Printf("  Cost:           $%.2f\n", float64(metricsContent.TotalCostMilliUSD)/1000)
		}
		if metricsContent.TotalDurationSeconds > 0 {
			fmt.Printf("  Duration:       %s\n", formatDuration(metricsContent.TotalDurationSeconds))
		}
	}

	return nil
}

// formatDuration formats a duration in seconds to a human-readable string
// like "4h 23m" or "12m 30s".
func formatDuration(seconds int64) string {
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	remainingSeconds := seconds % 60

	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, remainingSeconds)
	}
	return fmt.Sprintf("%ds", remainingSeconds)
}
