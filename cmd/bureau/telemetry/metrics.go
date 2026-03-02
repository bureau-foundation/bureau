// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	telemetryschema "github.com/bureau-foundation/bureau/lib/schema/telemetry"
)

// metricEntry is the output type for the metrics command. Flattens
// MetricPoint for tabular display with desc tags for MCP schema.
type metricEntry struct {
	Name      string            `json:"name"      desc:"metric name"`
	Source    string            `json:"source"    desc:"source entity localpart"`
	Machine   string            `json:"machine"   desc:"machine localpart"`
	Kind      string            `json:"kind"      desc:"metric kind: gauge, counter, or histogram"`
	Value     float64           `json:"value"     desc:"metric value (gauge or counter)"`
	Count     uint64            `json:"count"     desc:"observation count (histogram only)"`
	Sum       float64           `json:"sum"       desc:"sum of observations (histogram only)"`
	Labels    map[string]string `json:"labels"    desc:"dimension labels"`
	Timestamp int64             `json:"timestamp" desc:"observation time as Unix nanoseconds"`
}

func metricPointToEntry(point telemetryschema.MetricPoint) metricEntry {
	entry := metricEntry{
		Name:      point.Name,
		Kind:      metricKindName(point.Kind),
		Value:     point.Value,
		Labels:    point.Labels,
		Timestamp: point.Timestamp,
	}
	if !point.Source.IsZero() {
		entry.Source = point.Source.Localpart()
	}
	if !point.Machine.IsZero() {
		entry.Machine = point.Machine.Localpart()
	}
	if point.Histogram != nil {
		entry.Count = point.Histogram.Count
		entry.Sum = point.Histogram.Sum
	}
	return entry
}

type metricsParams struct {
	TelemetryConnection
	cli.JSONOutput
	Name    string   `json:"name"    desc:"metric name to query (exact match)" required:"true"`
	Machine string   `json:"machine" flag:"machine,m" desc:"filter by machine localpart"`
	Source  string   `json:"source"  flag:"source,s"  desc:"filter by entity localpart"`
	Labels  []string `json:"labels"  flag:"label,l"   desc:"filter by label (key=value, repeatable)"`
	Since   string   `json:"since"   flag:"since"     desc:"start of time range (duration or timestamp)" default:"1h"`
	Until   string   `json:"until"   flag:"until"     desc:"end of time range (duration or timestamp)"`
	Limit   int      `json:"limit"   flag:"limit,n"   desc:"maximum number of points to return" default:"1000"`
}

func metricsCommand() *cli.Command {
	var params metricsParams

	return &cli.Command{
		Name:    "metrics",
		Summary: "Query a metric time series",
		Description: `Retrieve metric observations for a named metric. The metric name
is required and must be an exact match (e.g.,
"bureau_proxy_request_duration_seconds").

Labels can be filtered with repeatable --label flags using
key=value syntax. All specified labels must match (AND semantics).

Time ranges use --since and --until, which accept Go durations
(1h, 30m), day suffixes (7d), or timestamps (RFC3339 or
YYYY-MM-DD). Defaults to the last hour.`,
		Usage: "bureau telemetry metrics <name> [flags]",
		Examples: []cli.Example{
			{
				Description: "Query proxy request duration metrics",
				Command:     "bureau telemetry metrics bureau_proxy_request_duration_seconds --service",
			},
			{
				Description: "Filter by label",
				Command:     "bureau telemetry metrics bureau_socket_request_total --service --label action=list",
			},
			{
				Description: "Metrics from a specific machine over 24 hours",
				Command:     "bureau telemetry metrics bureau_sandbox_count --service --machine sharkbox --since 24h",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]metricEntry{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/telemetry/metrics"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 1 {
				params.Name = args[0]
			} else if len(args) > 1 {
				return cli.Validation("expected 1 positional argument, got %d", len(args))
			}
			if params.Name == "" {
				return cli.Validation("metric name is required\n\nUsage: bureau telemetry metrics <name>")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			request := telemetryschema.MetricsRequest{
				Name:    params.Name,
				Machine: params.Machine,
				Source:  params.Source,
				Limit:   params.Limit,
			}

			if len(params.Labels) > 0 {
				request.Labels = make(map[string]string, len(params.Labels))
				for _, label := range params.Labels {
					key, value, ok := strings.Cut(label, "=")
					if !ok {
						return cli.Validation("invalid label %q: expected key=value format", label)
					}
					request.Labels[key] = value
				}
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

			var response telemetryschema.MetricsResponse
			if err := client.Call(ctx, "metrics", request, &response); err != nil {
				return err
			}

			entries := make([]metricEntry, len(response.Metrics))
			for index, point := range response.Metrics {
				entries[index] = metricPointToEntry(point)
			}

			if done, err := params.EmitJSON(entries); done {
				return err
			}

			if len(entries) == 0 {
				logger.Info("no metric points found", "name", params.Name)
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
			fmt.Fprintf(writer, "TIMESTAMP\tSOURCE\tKIND\tVALUE\tLABELS\n")
			for _, entry := range entries {
				valueDisplay := fmt.Sprintf("%.4g", entry.Value)
				if entry.Kind == "histogram" {
					valueDisplay = fmt.Sprintf("sum=%.4g count=%d", entry.Sum, entry.Count)
				}

				labelDisplay := formatLabels(entry.Labels)

				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
					formatTimestamp(entry.Timestamp),
					entry.Source,
					entry.Kind,
					valueDisplay,
					labelDisplay,
				)
			}
			return writer.Flush()
		},
	}
}

// formatLabels formats a label map as a compact "key=value, ..."
// string for tabular display.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(labels))
	for key, value := range labels {
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, ", ")
}
