// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	telemetryschema "github.com/bureau-foundation/bureau/lib/schema/telemetry"
)

// topResult is the output type for the top command with desc tags
// for MCP schema generation.
type topResult struct {
	SlowestOperations []operationDurationEntry   `json:"slowest_operations"  desc:"operations ranked by P99 duration (highest first)"`
	HighestErrorRate  []operationErrorRateEntry  `json:"highest_error_rate"  desc:"operations ranked by error rate (highest first)"`
	HighestThroughput []operationThroughputEntry `json:"highest_throughput" desc:"operations ranked by span count (highest first)"`
	MachineActivity   []machineActivityEntry     `json:"machine_activity"   desc:"machines ranked by span count (highest first)"`
}

type operationDurationEntry struct {
	Operation   string `json:"operation"    desc:"operation name"`
	P99Duration int64  `json:"p99_duration" desc:"P99 duration in nanoseconds"`
	Count       int64  `json:"count"        desc:"total span count"`
}

type operationErrorRateEntry struct {
	Operation  string  `json:"operation"    desc:"operation name"`
	ErrorRate  float64 `json:"error_rate"   desc:"error rate as fraction (0-1)"`
	ErrorCount int64   `json:"error_count"  desc:"total error spans"`
	TotalCount int64   `json:"total_count"  desc:"total spans"`
}

type operationThroughputEntry struct {
	Operation string `json:"operation" desc:"operation name"`
	Count     int64  `json:"count"     desc:"total span count"`
}

type machineActivityEntry struct {
	Machine    string  `json:"machine"     desc:"machine localpart"`
	SpanCount  int64   `json:"span_count"  desc:"total spans"`
	ErrorCount int64   `json:"error_count" desc:"total error spans"`
	ErrorRate  float64 `json:"error_rate"  desc:"error rate as fraction (0-1)"`
}

type topParams struct {
	TelemetryConnection
	cli.JSONOutput
	Machine string `json:"machine" flag:"machine,m" desc:"restrict overview to a single machine localpart"`
	Since   string `json:"since"   flag:"since"     desc:"start of time range (duration or timestamp)"`
	Until   string `json:"until"   flag:"until"     desc:"end of time range (duration or timestamp)"`
}

// defaultTopWindow is the default lookback duration for the top
// command when --since is not specified: 1 hour in nanoseconds.
const defaultTopWindow = int64(time.Hour)

func topCommand() *cli.Command {
	var params topParams

	return &cli.Command{
		Name:    "top",
		Summary: "Operational overview: slowest operations, error rates, throughput",
		Description: `Display an aggregated operational overview for a time range:
slowest operations by P99 duration, highest error rates, highest
throughput operations, and per-machine activity summaries.

Each section shows the top 10 entries. The machine activity
section is omitted when filtering to a single machine.

Defaults to the last hour. Use --since to specify a different
lookback window or an absolute start time.`,
		Usage: "bureau telemetry top [flags]",
		Examples: []cli.Example{
			{
				Description: "Overview for the last hour",
				Command:     "bureau telemetry top --service",
			},
			{
				Description: "Overview for the last 24 hours",
				Command:     "bureau telemetry top --service --since 24h",
			},
			{
				Description: "Overview for a specific machine",
				Command:     "bureau telemetry top --service --machine sharkbox",
			},
			{
				Description: "JSON output for scripting",
				Command:     "bureau telemetry top --service --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &topResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/telemetry/top"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			request := telemetryschema.TopRequest{
				Machine: params.Machine,
			}

			if params.Since != "" {
				start, err := parseTimeFlag(params.Since)
				if err != nil {
					return cli.Validation("--since: %v", err)
				}
				request.Start = start
			} else {
				// Default: use Window so the server resolves from its
				// own clock, avoiding client-server clock skew.
				request.Window = defaultTopWindow
			}

			if params.Until != "" {
				end, err := parseTimeFlag(params.Until)
				if err != nil {
					return cli.Validation("--until: %v", err)
				}
				request.End = end
			}

			var response telemetryschema.TopResponse
			if err := client.Call(ctx, "top", request, &response); err != nil {
				return err
			}

			result := topResult{
				SlowestOperations: make([]operationDurationEntry, len(response.SlowestOperations)),
				HighestErrorRate:  make([]operationErrorRateEntry, len(response.HighestErrorRate)),
				HighestThroughput: make([]operationThroughputEntry, len(response.HighestThroughput)),
				MachineActivity:   make([]machineActivityEntry, len(response.MachineActivity)),
			}
			for index, operation := range response.SlowestOperations {
				result.SlowestOperations[index] = operationDurationEntry{
					Operation:   operation.Operation,
					P99Duration: operation.P99Duration,
					Count:       operation.Count,
				}
			}
			for index, operation := range response.HighestErrorRate {
				result.HighestErrorRate[index] = operationErrorRateEntry{
					Operation:  operation.Operation,
					ErrorRate:  operation.ErrorRate,
					ErrorCount: operation.ErrorCount,
					TotalCount: operation.TotalCount,
				}
			}
			for index, operation := range response.HighestThroughput {
				result.HighestThroughput[index] = operationThroughputEntry{
					Operation: operation.Operation,
					Count:     operation.Count,
				}
			}
			for index, machine := range response.MachineActivity {
				result.MachineActivity[index] = machineActivityEntry{
					Machine:    machine.Machine,
					SpanCount:  machine.SpanCount,
					ErrorCount: machine.ErrorCount,
					ErrorRate:  machine.ErrorRate,
				}
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			if len(response.SlowestOperations) == 0 &&
				len(response.HighestErrorRate) == 0 &&
				len(response.HighestThroughput) == 0 &&
				len(response.MachineActivity) == 0 {
				logger.Info("no telemetry data in the requested time window")
				return nil
			}

			writeTopSections(response)
			return nil
		},
	}
}

// writeTopSections renders the four operational overview sections as
// tabwriter tables.
func writeTopSections(response telemetryschema.TopResponse) {
	if len(response.SlowestOperations) > 0 {
		fmt.Println("Slowest Operations (P99)")
		writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
		fmt.Fprintf(writer, "  OPERATION\tP99\tCOUNT\n")
		for _, operation := range response.SlowestOperations {
			fmt.Fprintf(writer, "  %s\t%s\t%d\n",
				truncate(operation.Operation, 40),
				formatDuration(operation.P99Duration),
				operation.Count,
			)
		}
		writer.Flush()
		fmt.Println()
	}

	if len(response.HighestErrorRate) > 0 {
		fmt.Println("Highest Error Rate")
		writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
		fmt.Fprintf(writer, "  OPERATION\tRATE\tERRORS\tTOTAL\n")
		for _, operation := range response.HighestErrorRate {
			fmt.Fprintf(writer, "  %s\t%.1f%%\t%d\t%d\n",
				truncate(operation.Operation, 40),
				operation.ErrorRate*100,
				operation.ErrorCount,
				operation.TotalCount,
			)
		}
		writer.Flush()
		fmt.Println()
	}

	if len(response.HighestThroughput) > 0 {
		fmt.Println("Highest Throughput")
		writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
		fmt.Fprintf(writer, "  OPERATION\tCOUNT\n")
		for _, operation := range response.HighestThroughput {
			fmt.Fprintf(writer, "  %s\t%d\n",
				truncate(operation.Operation, 40),
				operation.Count,
			)
		}
		writer.Flush()
		fmt.Println()
	}

	if len(response.MachineActivity) > 0 {
		fmt.Println("Machine Activity")
		writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
		fmt.Fprintf(writer, "  MACHINE\tSPANS\tERRORS\tERROR RATE\n")
		for _, machine := range response.MachineActivity {
			fmt.Fprintf(writer, "  %s\t%d\t%d\t%.1f%%\n",
				machine.Machine,
				machine.SpanCount,
				machine.ErrorCount,
				machine.ErrorRate*100,
			)
		}
		writer.Flush()
	}
}
