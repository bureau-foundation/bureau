// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import "github.com/bureau-foundation/bureau/cmd/bureau/cli"

// Command returns the "telemetry" subcommand group.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "telemetry",
		Summary: "Query Bureau telemetry data",
		Description: `View traces, metrics, logs, and operational overviews from
the telemetry service.

The telemetry service collects data from telemetry relays on
each machine: distributed trace spans, metric time series,
structured log records, and raw terminal output. These commands
query the stored data for debugging, monitoring, and operational
visibility.

Commands connect to the telemetry service's CBOR socket. Inside
a sandbox, the socket and token paths default to the standard
provisioned locations. Outside a sandbox, use --service mode
(requires 'bureau login') or --socket and --token-file flags.`,
		Subcommands: []*cli.Command{
			statusCommand(),
			tracesCommand(),
			traceCommand(),
			metricsCommand(),
			logsCommand(),
			topCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "Check telemetry service health",
				Command:     "bureau telemetry status --service",
			},
			{
				Description: "List recent trace spans",
				Command:     "bureau telemetry traces --service",
			},
			{
				Description: "View a specific trace as a span tree",
				Command:     "bureau telemetry trace <trace-id> --service",
			},
			{
				Description: "Query a metric time series",
				Command:     "bureau telemetry metrics bureau_proxy_request_duration_seconds --service",
			},
			{
				Description: "Search recent error logs",
				Command:     "bureau telemetry logs --service --severity error",
			},
			{
				Description: "Operational overview for the last hour",
				Command:     "bureau telemetry top --service",
			},
		},
	}
}
