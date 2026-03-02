// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	telemetryschema "github.com/bureau-foundation/bureau/lib/schema/telemetry"
)

// statusResult is the output type for the status command. All fields
// have desc tags for MCP schema generation. Nested structs are local
// types rather than embedding library types so every field carries a
// desc tag.
type statusResult struct {
	UptimeSeconds        float64            `json:"uptime_seconds"         desc:"service uptime in seconds"`
	ConnectedRelays      int                `json:"connected_relays"       desc:"number of connected telemetry relays"`
	BatchesReceived      uint64             `json:"batches_received"       desc:"total telemetry batches received"`
	SpansReceived        uint64             `json:"spans_received"         desc:"total spans received"`
	MetricsReceived      uint64             `json:"metrics_received"       desc:"total metric points received"`
	LogsReceived         uint64             `json:"logs_received"          desc:"total log records received"`
	OutputDeltasReceived uint64             `json:"output_deltas_received" desc:"total output deltas received"`
	ArtifactPersistence  bool               `json:"artifact_persistence"   desc:"whether artifact store is available for log persistence"`
	Storage              storageStatsResult `json:"storage"                desc:"SQLite storage statistics"`
	LogManager           logManagerResult   `json:"log_manager"            desc:"log manager operational statistics"`
}

type storageStatsResult struct {
	PartitionCount    int    `json:"partition_count"     desc:"number of active day partitions"`
	OldestPartition   string `json:"oldest_partition"    desc:"YYYYMMDD suffix of the oldest partition"`
	NewestPartition   string `json:"newest_partition"    desc:"YYYYMMDD suffix of the newest partition"`
	DatabaseSizeBytes int64  `json:"database_size_bytes" desc:"total database file size in bytes"`
	SpanCount         int64  `json:"span_count"          desc:"total span records across all partitions"`
	MetricCount       int64  `json:"metric_count"        desc:"total metric point records across all partitions"`
	LogCount          int64  `json:"log_count"           desc:"total log records across all partitions"`
}

type logManagerResult struct {
	FlushCount     uint64 `json:"flush_count"      desc:"number of output flush operations"`
	FlushErrors    uint64 `json:"flush_errors"     desc:"number of flush errors"`
	StoreCount     uint64 `json:"store_count"      desc:"number of chunk store operations"`
	StoreErrors    uint64 `json:"store_errors"     desc:"number of store errors"`
	MetadataWrites uint64 `json:"metadata_writes"  desc:"number of metadata write operations"`
	MetadataErrors uint64 `json:"metadata_errors"  desc:"number of metadata write errors"`
	EvictionCount  uint64 `json:"eviction_count"   desc:"number of chunk evictions"`
	ActiveSessions int    `json:"active_sessions"  desc:"number of active log sessions"`
	LastError      string `json:"last_error"       desc:"most recent error message (empty if none)"`
}

type statusParams struct {
	TelemetryConnection
	cli.JSONOutput
}

func statusCommand() *cli.Command {
	var params statusParams

	return &cli.Command{
		Name:    "status",
		Summary: "Show telemetry service health and ingestion statistics",
		Description: `Display operational health of the telemetry service: uptime,
connected relay count, ingestion counters, storage statistics, and
log manager health.

This queries the service's unauthenticated status endpoint. The
service exposes only aggregate counters — no fleet, machine, or
source identifiers that could disclose topology.`,
		Usage: "bureau telemetry status [flags]",
		Examples: []cli.Example{
			{
				Description: "Show telemetry service status",
				Command:     "bureau telemetry status --service",
			},
			{
				Description: "JSON output for scripting",
				Command:     "bureau telemetry status --service --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &statusResult{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/telemetry/status"},
		Run: func(ctx context.Context, args []string, _ *slog.Logger) error {
			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			var serviceStatus telemetryschema.ServiceStatus
			if err := client.Call(ctx, "status", nil, &serviceStatus); err != nil {
				return err
			}

			storage := serviceStatus.Storage
			logManager := serviceStatus.LogManager

			result := statusResult{
				UptimeSeconds:        serviceStatus.UptimeSeconds,
				ConnectedRelays:      serviceStatus.ConnectedRelays,
				BatchesReceived:      serviceStatus.BatchesReceived,
				SpansReceived:        serviceStatus.SpansReceived,
				MetricsReceived:      serviceStatus.MetricsReceived,
				LogsReceived:         serviceStatus.LogsReceived,
				OutputDeltasReceived: serviceStatus.OutputDeltasReceived,
				ArtifactPersistence:  serviceStatus.ArtifactPersistence,
				Storage: storageStatsResult{
					PartitionCount:    storage.PartitionCount,
					OldestPartition:   storage.OldestPartition,
					NewestPartition:   storage.NewestPartition,
					DatabaseSizeBytes: storage.DatabaseSizeBytes,
					SpanCount:         storage.SpanCount,
					MetricCount:       storage.MetricCount,
					LogCount:          storage.LogCount,
				},
				LogManager: logManagerResult{
					FlushCount:     logManager.FlushCount,
					FlushErrors:    logManager.FlushErrors,
					StoreCount:     logManager.StoreCount,
					StoreErrors:    logManager.StoreErrors,
					MetadataWrites: logManager.MetadataWrites,
					MetadataErrors: logManager.MetadataErrors,
					EvictionCount:  logManager.EvictionCount,
					ActiveSessions: logManager.ActiveSessions,
					LastError:      logManager.LastError,
				},
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			fmt.Printf("Uptime:           %s\n", formatUptime(serviceStatus.UptimeSeconds))
			fmt.Printf("Connected relays: %d\n", serviceStatus.ConnectedRelays)
			fmt.Printf("Batches received: %d\n", serviceStatus.BatchesReceived)
			fmt.Printf("Spans:            %d\n", serviceStatus.SpansReceived)
			fmt.Printf("Metrics:          %d\n", serviceStatus.MetricsReceived)
			fmt.Printf("Logs:             %d\n", serviceStatus.LogsReceived)
			fmt.Printf("Output deltas:    %d\n", serviceStatus.OutputDeltasReceived)
			fmt.Printf("Artifact store:   %v\n", serviceStatus.ArtifactPersistence)

			fmt.Printf("\nStorage\n")
			if storage.OldestPartition != "" {
				fmt.Printf("  Partitions:     %d (%s - %s)\n",
					storage.PartitionCount, storage.OldestPartition, storage.NewestPartition)
			} else {
				fmt.Printf("  Partitions:     %d\n", storage.PartitionCount)
			}
			fmt.Printf("  Database:       %s\n", formatBytes(storage.DatabaseSizeBytes))
			fmt.Printf("  Spans:          %d\n", storage.SpanCount)
			fmt.Printf("  Metrics:        %d\n", storage.MetricCount)
			fmt.Printf("  Logs:           %d\n", storage.LogCount)

			fmt.Printf("\nLog Manager\n")
			fmt.Printf("  Active sessions:  %d\n", logManager.ActiveSessions)
			fmt.Printf("  Flushes:          %d (%d errors)\n", logManager.FlushCount, logManager.FlushErrors)
			fmt.Printf("  Stores:           %d (%d errors)\n", logManager.StoreCount, logManager.StoreErrors)
			fmt.Printf("  Metadata writes:  %d (%d errors)\n", logManager.MetadataWrites, logManager.MetadataErrors)
			fmt.Printf("  Evictions:        %d\n", logManager.EvictionCount)
			if logManager.LastError != "" {
				fmt.Printf("  Last error:       %s\n", logManager.LastError)
			}

			return nil
		},
	}
}
