// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	telemetryschema "github.com/bureau-foundation/bureau/lib/schema/telemetry"
)

// --- traces ---

// spanEntry is the output type for the traces command. Flattens the
// Span struct for tabular display with desc tags for MCP schema.
type spanEntry struct {
	TraceID       string `json:"trace_id"       desc:"32-character hex trace identifier"`
	SpanID        string `json:"span_id"        desc:"16-character hex span identifier"`
	ParentSpanID  string `json:"parent_span_id" desc:"parent span identifier (empty for root spans)"`
	Operation     string `json:"operation"      desc:"operation name"`
	Source        string `json:"source"         desc:"source entity localpart"`
	Machine       string `json:"machine"        desc:"machine localpart"`
	Duration      int64  `json:"duration"       desc:"duration in nanoseconds"`
	Status        string `json:"status"         desc:"span status: ok, error, or unset"`
	StatusMessage string `json:"status_message" desc:"error message (empty for non-error spans)"`
	StartTime     int64  `json:"start_time"     desc:"start time as Unix nanoseconds"`
}

func spanToEntry(span telemetryschema.Span) spanEntry {
	entry := spanEntry{
		TraceID:       span.TraceID.String(),
		SpanID:        span.SpanID.String(),
		Operation:     span.Operation,
		Duration:      span.Duration,
		Status:        spanStatusName(span.Status),
		StatusMessage: span.StatusMessage,
		StartTime:     span.StartTime,
	}
	if !span.ParentSpanID.IsZero() {
		entry.ParentSpanID = span.ParentSpanID.String()
	}
	if !span.Source.IsZero() {
		entry.Source = span.Source.Localpart()
	}
	if !span.Machine.IsZero() {
		entry.Machine = span.Machine.Localpart()
	}
	return entry
}

type tracesParams struct {
	TelemetryConnection
	cli.JSONOutput
	Machine     string `json:"machine"      flag:"machine,m"     desc:"filter by machine localpart"`
	Source      string `json:"source"       flag:"source,s"      desc:"filter by entity localpart"`
	Operation   string `json:"operation"    flag:"operation,o"   desc:"filter by operation name (prefix match supported)"`
	MinDuration string `json:"min_duration" flag:"min-duration"  desc:"minimum span duration (e.g., 100ms, 1s)"`
	Status      string `json:"status"       flag:"status"        desc:"filter by span status (ok, error, unset)"`
	Since       string `json:"since"        flag:"since"         desc:"start of time range (duration like 1h, or timestamp)" default:"1h"`
	Until       string `json:"until"        flag:"until"         desc:"end of time range (duration or timestamp)"`
	Limit       int    `json:"limit"        flag:"limit,n"       desc:"maximum number of spans to return" default:"100"`
}

func tracesCommand() *cli.Command {
	var params tracesParams

	return &cli.Command{
		Name:    "traces",
		Summary: "Query trace spans with filters",
		Description: `Search for trace spans matching the specified filters. Returns
spans sorted by start time (newest first).

Time ranges use --since and --until, which accept Go durations
(1h, 30m, 2h30m), day suffixes (7d), or timestamps (RFC3339 or
YYYY-MM-DD). Defaults to the last hour.

The --operation flag supports prefix matching: "proxy." matches
"proxy.forward", "proxy.auth", etc.`,
		Usage: "bureau telemetry traces [flags]",
		Examples: []cli.Example{
			{
				Description: "List recent spans from the last hour",
				Command:     "bureau telemetry traces --service",
			},
			{
				Description: "Filter by operation prefix",
				Command:     "bureau telemetry traces --service --operation proxy.",
			},
			{
				Description: "Find slow operations",
				Command:     "bureau telemetry traces --service --min-duration 1s --since 24h",
			},
			{
				Description: "Error spans from a specific machine",
				Command:     "bureau telemetry traces --service --machine sharkbox --status error",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]spanEntry{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/telemetry/traces"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			request := telemetryschema.TracesRequest{
				Machine:   params.Machine,
				Source:    params.Source,
				Operation: params.Operation,
				Limit:     params.Limit,
			}

			if params.MinDuration != "" {
				nanos, err := parseDurationFlag(params.MinDuration)
				if err != nil {
					return cli.Validation("%v", err)
				}
				request.MinDuration = nanos
			}

			if params.Status != "" {
				status, err := parseSpanStatusFlag(params.Status)
				if err != nil {
					return cli.Validation("%v", err)
				}
				statusValue := uint8(status)
				request.Status = &statusValue
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

			var response telemetryschema.TracesResponse
			if err := client.Call(ctx, "traces", request, &response); err != nil {
				return err
			}

			entries := make([]spanEntry, len(response.Spans))
			for index, span := range response.Spans {
				entries[index] = spanToEntry(span)
			}

			if done, err := params.EmitJSON(entries); done {
				return err
			}

			if len(entries) == 0 {
				logger.Info("no spans found")
				return nil
			}

			writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
			fmt.Fprintf(writer, "TRACE\tOPERATION\tSOURCE\tDURATION\tSTATUS\tTIME\n")
			for _, entry := range entries {
				tracePrefix := entry.TraceID
				if len(tracePrefix) > 12 {
					tracePrefix = tracePrefix[:12]
				}
				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
					tracePrefix,
					truncate(entry.Operation, 40),
					entry.Source,
					formatDuration(entry.Duration),
					entry.Status,
					formatTimestamp(entry.StartTime),
				)
			}
			return writer.Flush()
		},
	}
}

// --- trace (single trace as span tree) ---

// traceResult is the output type for the trace command, wrapping span
// entries with trace-level summary information.
type traceResult struct {
	TraceID   string      `json:"trace_id"    desc:"32-character hex trace identifier"`
	SpanCount int         `json:"span_count"  desc:"total number of spans in the trace"`
	Duration  int64       `json:"duration"    desc:"total trace duration in nanoseconds (root span duration)"`
	Spans     []spanEntry `json:"spans"       desc:"all spans in the trace, ordered by start time"`
}

type traceParams struct {
	TelemetryConnection
	cli.JSONOutput
	TraceID string `json:"trace_id" desc:"32-character hex trace identifier" required:"true"`
}

func traceCommand() *cli.Command {
	var params traceParams

	return &cli.Command{
		Name:    "trace",
		Summary: "Display a single trace as a span tree",
		Description: `Fetch all spans belonging to a trace and render them as an
indented tree showing parent-child relationships, timing, and
status. The tree is rooted at spans with no parent.`,
		Usage: "bureau telemetry trace <trace-id> [flags]",
		Examples: []cli.Example{
			{
				Description: "Show a trace tree",
				Command:     "bureau telemetry trace abc123def456... --service",
			},
			{
				Description: "JSON output",
				Command:     "bureau telemetry trace abc123def456... --service --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &traceResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/telemetry/trace"},
		Run: func(ctx context.Context, args []string, _ *slog.Logger) error {
			if len(args) == 1 {
				params.TraceID = args[0]
			} else if len(args) > 1 {
				return cli.Validation("expected 1 positional argument, got %d", len(args))
			}
			if params.TraceID == "" {
				return cli.Validation("trace ID is required\n\nUsage: bureau telemetry trace <trace-id>")
			}

			var traceID telemetryschema.TraceID
			if err := traceID.UnmarshalText([]byte(params.TraceID)); err != nil {
				return cli.Validation("invalid trace ID: %v", err)
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			request := telemetryschema.TracesRequest{
				TraceID: traceID,
				Limit:   1000,
			}

			var response telemetryschema.TracesResponse
			if err := client.Call(ctx, "traces", request, &response); err != nil {
				return err
			}

			entries := make([]spanEntry, len(response.Spans))
			for index, span := range response.Spans {
				entries[index] = spanToEntry(span)
			}

			// Compute total trace duration from the root span (or
			// the longest span if multiple roots exist).
			var totalDuration int64
			for _, span := range response.Spans {
				if span.Duration > totalDuration {
					totalDuration = span.Duration
				}
			}

			result := traceResult{
				TraceID:   params.TraceID,
				SpanCount: len(entries),
				Duration:  totalDuration,
				Spans:     entries,
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			if len(response.Spans) == 0 {
				fmt.Printf("Trace %s: no spans found\n", params.TraceID)
				return nil
			}

			fmt.Printf("Trace %s  (%d spans, %s)\n\n",
				params.TraceID, len(response.Spans), formatDuration(totalDuration))

			renderSpanTree(response.Spans)
			return nil
		},
	}
}

// renderSpanTree prints spans as an indented tree using box-drawing
// characters. Root spans (no parent) are top-level entries. Children
// are indented under their parents.
func renderSpanTree(spans []telemetryschema.Span) {
	// Sort by start time for consistent ordering within each level.
	sort.Slice(spans, func(i, j int) bool {
		return spans[i].StartTime < spans[j].StartTime
	})

	// Index children by parent span ID.
	type spanNode struct {
		span     telemetryschema.Span
		children []int
	}

	nodes := make([]spanNode, len(spans))
	spanIndex := make(map[telemetryschema.SpanID]int, len(spans))
	for index, span := range spans {
		nodes[index] = spanNode{span: span}
		spanIndex[span.SpanID] = index
	}

	var roots []int
	for index, span := range spans {
		if span.ParentSpanID.IsZero() {
			roots = append(roots, index)
		} else if parentIndex, ok := spanIndex[span.ParentSpanID]; ok {
			nodes[parentIndex].children = append(nodes[parentIndex].children, index)
		} else {
			// Orphan span (parent not in result set) — treat as root.
			roots = append(roots, index)
		}
	}

	var printNode func(index int, prefix string, isLast bool)
	printNode = func(index int, prefix string, isLast bool) {
		node := nodes[index]
		span := node.span

		// Build the connector string.
		var connector string
		if prefix == "" {
			connector = ""
		} else if isLast {
			connector = prefix + "└── "
		} else {
			connector = prefix + "├── "
		}

		// Format the status suffix.
		statusSuffix := ""
		switch span.Status {
		case telemetryschema.SpanStatusError:
			if span.StatusMessage != "" {
				statusSuffix = fmt.Sprintf(" (error: %s)", truncate(span.StatusMessage, 60))
			} else {
				statusSuffix = " (error)"
			}
		case telemetryschema.SpanStatusOK:
			statusSuffix = " (ok)"
		}

		// Source prefix if available.
		sourcePrefix := ""
		if !span.Source.IsZero() {
			sourcePrefix = span.Source.Localpart() + " "
		}

		fmt.Printf("%s[%s] %s%s%s\n",
			connector,
			formatDuration(span.Duration),
			sourcePrefix,
			span.Operation,
			statusSuffix,
		)

		// Recurse into children.
		childPrefix := prefix
		if prefix != "" {
			if isLast {
				childPrefix = prefix + "    "
			} else {
				childPrefix = prefix + "│   "
			}
		}

		for childIndex, childNodeIndex := range node.children {
			childIsLast := childIndex == len(node.children)-1
			printNode(childNodeIndex, childPrefix, childIsLast)
		}
	}

	for _, rootIndex := range roots {
		printNode(rootIndex, "", true)
	}
}
