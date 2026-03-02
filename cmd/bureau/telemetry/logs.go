// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	telemetryschema "github.com/bureau-foundation/bureau/lib/schema/telemetry"
)

// logEntry is the output type for the logs command. Flattens
// LogRecord for tabular display with desc tags for MCP schema.
type logEntry struct {
	Timestamp  int64          `json:"timestamp" desc:"log time as Unix nanoseconds"`
	Severity   string         `json:"severity"  desc:"severity level: TRACE, DEBUG, INFO, WARN, ERROR, FATAL"`
	Source     string         `json:"source"    desc:"source entity localpart"`
	Machine    string         `json:"machine"   desc:"machine localpart"`
	Body       string         `json:"body"      desc:"log message text"`
	TraceID    string         `json:"trace_id"  desc:"correlated trace ID (empty if uncorrelated)"`
	Attributes map[string]any `json:"attributes" desc:"structured log attributes"`
}

func logRecordToEntry(record telemetryschema.LogRecord) logEntry {
	entry := logEntry{
		Timestamp:  record.Timestamp,
		Severity:   severityName(record.Severity),
		Body:       record.Body,
		Attributes: record.Attributes,
	}
	if !record.Source.IsZero() {
		entry.Source = record.Source.Localpart()
	}
	if !record.Machine.IsZero() {
		entry.Machine = record.Machine.Localpart()
	}
	if !record.TraceID.IsZero() {
		entry.TraceID = record.TraceID.String()
	}
	return entry
}

type logsParams struct {
	TelemetryConnection
	cli.JSONOutput
	Machine  string `json:"machine"  flag:"machine,m"  desc:"filter by machine localpart"`
	Source   string `json:"source"   flag:"source,s"   desc:"filter by entity localpart"`
	Severity string `json:"severity" flag:"severity"   desc:"minimum severity level (trace, debug, info, warn, error, fatal)"`
	Trace    string `json:"trace"    flag:"trace"      desc:"filter by correlated trace ID (32-char hex)"`
	Search   string `json:"search"   flag:"search,q"   desc:"filter by body substring"`
	Since    string `json:"since"    flag:"since"      desc:"start of time range (duration or timestamp)" default:"1h"`
	Until    string `json:"until"    flag:"until"      desc:"end of time range (duration or timestamp)"`
	Limit    int    `json:"limit"    flag:"limit,n"    desc:"maximum number of records to return" default:"100"`
}

func logsCommand() *cli.Command {
	var params logsParams

	return &cli.Command{
		Name:    "logs",
		Summary: "Query structured log records",
		Description: `Search for log records matching the specified filters. Returns
records sorted by timestamp (newest first).

The --severity flag sets a minimum severity threshold: logs at
or above the specified level are returned. Severity levels follow
OpenTelemetry numbering: trace, debug, info, warn, error, fatal.

The --search flag matches substrings in the log body. The --trace
flag filters to logs correlated with a specific distributed trace.

Time ranges use --since and --until, which accept Go durations
(1h, 30m), day suffixes (7d), or timestamps (RFC3339 or
YYYY-MM-DD). Defaults to the last hour.`,
		Usage: "bureau telemetry logs [flags]",
		Examples: []cli.Example{
			{
				Description: "Recent error logs",
				Command:     "bureau telemetry logs --service --severity error",
			},
			{
				Description: "Search logs for a keyword",
				Command:     "bureau telemetry logs --service --search 'connection refused'",
			},
			{
				Description: "Logs correlated with a trace",
				Command:     "bureau telemetry logs --service --trace abc123def456...",
			},
			{
				Description: "Logs from a specific source over 24 hours",
				Command:     "bureau telemetry logs --service --source stt/whisper --since 24h",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]logEntry{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/telemetry/logs"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			request := telemetryschema.LogsRequest{
				Machine: params.Machine,
				Source:  params.Source,
				Search:  params.Search,
				Limit:   params.Limit,
			}

			if params.Severity != "" {
				severity, err := parseSeverityFlag(params.Severity)
				if err != nil {
					return cli.Validation("%v", err)
				}
				request.MinSeverity = &severity
			}

			if params.Trace != "" {
				var traceID telemetryschema.TraceID
				if err := traceID.UnmarshalText([]byte(params.Trace)); err != nil {
					return cli.Validation("invalid trace ID: %v", err)
				}
				request.TraceID = traceID
			}

			if params.Since != "" {
				start, err := parseTimeFlag(params.Since)
				if err != nil {
					return cli.Validation("--since: %v", err)
				}
				request.Start = start
			}

			if params.Until != "" {
				end, err := parseTimeFlag(params.Until)
				if err != nil {
					return cli.Validation("--until: %v", err)
				}
				request.End = end
			}

			var response telemetryschema.LogsResponse
			if err := client.Call(ctx, "logs", request, &response); err != nil {
				return err
			}

			entries := make([]logEntry, len(response.Logs))
			for index, record := range response.Logs {
				entries[index] = logRecordToEntry(record)
			}

			if done, err := params.EmitJSON(entries); done {
				return err
			}

			if len(entries) == 0 {
				logger.Info("no log records found")
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
			fmt.Fprintf(writer, "TIMESTAMP\tSEVERITY\tSOURCE\tBODY\n")
			for _, entry := range entries {
				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n",
					formatTimestamp(entry.Timestamp),
					entry.Severity,
					entry.Source,
					truncate(entry.Body, 80),
				)
			}
			return writer.Flush()
		},
	}
}
