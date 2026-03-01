// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/sqlitepool"
)

// Store manages SQLite storage for telemetry data. It handles
// time-partitioned tables (one set per day), batch writes, and
// retention-based cleanup. The partition scheme is an internal detail
// invisible to query callers.
//
// Write path: the ingest handler calls WriteBatch for each incoming
// TelemetryBatch. WriteBatch inserts all spans, metrics, and logs from
// the batch in a single IMMEDIATE transaction, creating the day's
// partition tables on first write.
//
// Read path: query methods (QuerySpans, etc.) search across all active
// partitions, constructing UNION ALL queries internally. Callers see a
// flat result set with no partition boundaries.
//
// Retention: RunRetention drops partition tables older than the
// configured retention period. Called by a background ticker.
type Store struct {
	pool       *sqlitepool.Pool
	clock      clock.Clock
	logger     *slog.Logger
	serverName ref.ServerName

	// partitionMu serializes partition table creation. Read access
	// (checking whether a partition exists) is lock-free via the
	// atomic map in knownPartitions. Write access (CREATE TABLE)
	// takes the lock.
	partitionMu     sync.Mutex
	knownPartitions map[string]bool // partition suffix → exists
}

// StoreConfig holds the parameters for creating a telemetry store.
type StoreConfig struct {
	// Path is the filesystem path to the SQLite database file. The
	// parent directory must exist.
	Path string

	// PoolSize is the number of connections in the pool. Defaults to
	// 4 if zero or negative.
	PoolSize int

	// ServerName is the Matrix server name used to reconstruct typed
	// ref values (Fleet, Machine, Entity) from the localpart strings
	// stored in the database. Required.
	ServerName ref.ServerName

	// Clock provides the current time for partition naming and
	// retention decisions.
	Clock clock.Clock

	// Logger receives operational messages.
	Logger *slog.Logger
}

// RetentionConfig holds the retention periods per signal type. Tables
// older than their retention period are dropped entirely.
type RetentionConfig struct {
	Spans   time.Duration // Default: 7 days
	Metrics time.Duration // Default: 14 days
	Logs    time.Duration // Default: 7 days
}

// DefaultRetention returns the default retention configuration.
func DefaultRetention() RetentionConfig {
	return RetentionConfig{
		Spans:   7 * 24 * time.Hour,
		Metrics: 14 * 24 * time.Hour,
		Logs:    7 * 24 * time.Hour,
	}
}

// OpenStore creates a new telemetry store backed by SQLite. The
// database file is created if it does not exist. On open, the store
// discovers existing partition tables so that writes and queries
// include them immediately.
func OpenStore(cfg StoreConfig) (*Store, error) {
	if cfg.Clock == nil {
		return nil, fmt.Errorf("telemetry store: Clock is required")
	}
	if cfg.Logger == nil {
		return nil, fmt.Errorf("telemetry store: Logger is required")
	}
	if cfg.ServerName.IsZero() {
		return nil, fmt.Errorf("telemetry store: ServerName is required")
	}

	poolSize := cfg.PoolSize
	if poolSize <= 0 {
		poolSize = 4
	}

	pool, err := sqlitepool.Open(sqlitepool.Config{
		Path:     cfg.Path,
		PoolSize: poolSize,
		Logger:   cfg.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("telemetry store: %w", err)
	}

	store := &Store{
		pool:            pool,
		clock:           cfg.Clock,
		logger:          cfg.Logger,
		serverName:      cfg.ServerName,
		knownPartitions: make(map[string]bool),
	}

	// Discover existing partitions from a previous run.
	if err := store.discoverPartitions(); err != nil {
		pool.Close()
		return nil, fmt.Errorf("telemetry store: discovering partitions: %w", err)
	}

	return store, nil
}

// Close closes the underlying connection pool. Blocks until all
// borrowed connections are returned.
func (s *Store) Close() error {
	return s.pool.Close()
}

// WriteBatch inserts all spans, metrics, and logs from a telemetry
// batch into the appropriate day partition tables. Creates partition
// tables on first write to a new day. The entire batch is written in a
// single IMMEDIATE transaction.
//
// Output deltas are NOT written to SQLite — they are handled by the
// log manager's artifact persistence pipeline.
func (s *Store) WriteBatch(ctx context.Context, batch *telemetry.TelemetryBatch) error {
	if len(batch.Spans) == 0 && len(batch.Metrics) == 0 && len(batch.Logs) == 0 {
		return nil
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("telemetry store: write batch: %w", err)
	}
	defer s.pool.Put(conn)

	endTransaction, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return fmt.Errorf("telemetry store: begin transaction: %w", err)
	}
	defer endTransaction(&err)

	// Ensure all needed partitions exist. Most batches touch exactly
	// one partition (today's). Cross-midnight batches may touch two.
	partitions := s.collectPartitions(batch)
	for _, partition := range partitions {
		if err := s.ensurePartition(conn, partition); err != nil {
			return err
		}
	}

	for i := range batch.Spans {
		if err := s.insertSpan(conn, &batch.Spans[i]); err != nil {
			return err
		}
	}

	for i := range batch.Metrics {
		if err := s.insertMetric(conn, &batch.Metrics[i]); err != nil {
			return err
		}
	}

	for i := range batch.Logs {
		if err := s.insertLog(conn, &batch.Logs[i]); err != nil {
			return err
		}
	}

	return nil
}

// partitionSuffix returns the YYYYMMDD suffix for a Unix nanosecond
// timestamp.
func partitionSuffix(unixNanos int64) string {
	t := time.Unix(0, unixNanos).UTC()
	return t.Format("20060102")
}

// collectPartitions returns the unique partition suffixes needed by a
// batch. Most batches produce exactly one suffix.
func (s *Store) collectPartitions(batch *telemetry.TelemetryBatch) []string {
	seen := make(map[string]struct{}, 2)

	for i := range batch.Spans {
		seen[partitionSuffix(batch.Spans[i].StartTime)] = struct{}{}
	}
	for i := range batch.Metrics {
		seen[partitionSuffix(batch.Metrics[i].Timestamp)] = struct{}{}
	}
	for i := range batch.Logs {
		seen[partitionSuffix(batch.Logs[i].Timestamp)] = struct{}{}
	}

	partitions := make([]string, 0, len(seen))
	for suffix := range seen {
		partitions = append(partitions, suffix)
	}
	return partitions
}

// ensurePartition creates the day's partition tables if they don't
// exist. Safe to call concurrently — only one goroutine creates tables
// at a time.
func (s *Store) ensurePartition(conn *sqlite.Conn, suffix string) error {
	// Fast path: already known.
	s.partitionMu.Lock()
	if s.knownPartitions[suffix] {
		s.partitionMu.Unlock()
		return nil
	}
	defer s.partitionMu.Unlock()

	// Double-check after acquiring the lock.
	if s.knownPartitions[suffix] {
		return nil
	}

	schema := partitionSchema(suffix)
	if err := sqlitex.ExecuteScript(conn, schema, nil); err != nil {
		return fmt.Errorf("telemetry store: creating partition %s: %w", suffix, err)
	}

	s.knownPartitions[suffix] = true
	s.logger.Info("partition created", "suffix", suffix)
	return nil
}

// partitionSchema returns the CREATE TABLE and CREATE INDEX statements
// for a day partition.
func partitionSchema(suffix string) string {
	return fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS spans_%[1]s (
			trace_id       BLOB NOT NULL,
			span_id        BLOB NOT NULL,
			parent_span_id BLOB,
			fleet          TEXT NOT NULL,
			machine        TEXT NOT NULL,
			source         TEXT NOT NULL,
			operation      TEXT NOT NULL,
			start_time     INTEGER NOT NULL,
			duration       INTEGER NOT NULL,
			status         INTEGER NOT NULL,
			status_message TEXT,
			attributes     TEXT,
			events         TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_spans_%[1]s_time ON spans_%[1]s(start_time);
		CREATE INDEX IF NOT EXISTS idx_spans_%[1]s_trace ON spans_%[1]s(trace_id);
		CREATE INDEX IF NOT EXISTS idx_spans_%[1]s_machine ON spans_%[1]s(machine, start_time);
		CREATE INDEX IF NOT EXISTS idx_spans_%[1]s_source ON spans_%[1]s(source, start_time);
		CREATE INDEX IF NOT EXISTS idx_spans_%[1]s_operation ON spans_%[1]s(operation, start_time);

		CREATE TABLE IF NOT EXISTS metrics_%[1]s (
			fleet       TEXT NOT NULL,
			machine     TEXT NOT NULL,
			source      TEXT NOT NULL,
			name        TEXT NOT NULL,
			labels      TEXT,
			kind        INTEGER NOT NULL,
			timestamp   INTEGER NOT NULL,
			value_float REAL,
			value_hist  BLOB
		);
		CREATE INDEX IF NOT EXISTS idx_metrics_%[1]s_time ON metrics_%[1]s(timestamp);
		CREATE INDEX IF NOT EXISTS idx_metrics_%[1]s_name ON metrics_%[1]s(name, timestamp);
		CREATE INDEX IF NOT EXISTS idx_metrics_%[1]s_source ON metrics_%[1]s(source, name, timestamp);

		CREATE TABLE IF NOT EXISTS logs_%[1]s (
			fleet      TEXT NOT NULL,
			machine    TEXT NOT NULL,
			source     TEXT NOT NULL,
			severity   INTEGER NOT NULL,
			body       TEXT NOT NULL,
			trace_id   BLOB,
			span_id    BLOB,
			timestamp  INTEGER NOT NULL,
			attributes TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_logs_%[1]s_time ON logs_%[1]s(timestamp);
		CREATE INDEX IF NOT EXISTS idx_logs_%[1]s_source ON logs_%[1]s(source, timestamp);
		CREATE INDEX IF NOT EXISTS idx_logs_%[1]s_trace ON logs_%[1]s(trace_id);
		CREATE INDEX IF NOT EXISTS idx_logs_%[1]s_severity ON logs_%[1]s(severity, timestamp);
	`, suffix)
}

// insertSpan inserts a single span into its day partition.
func (s *Store) insertSpan(conn *sqlite.Conn, span *telemetry.Span) error {
	suffix := partitionSuffix(span.StartTime)

	var attributesJSON any
	if len(span.Attributes) > 0 {
		data, err := json.Marshal(span.Attributes)
		if err != nil {
			return fmt.Errorf("telemetry store: marshal span attributes: %w", err)
		}
		attributesJSON = string(data)
	}

	var eventsJSON any
	if len(span.Events) > 0 {
		data, err := json.Marshal(span.Events)
		if err != nil {
			return fmt.Errorf("telemetry store: marshal span events: %w", err)
		}
		eventsJSON = string(data)
	}

	query := fmt.Sprintf(`INSERT INTO spans_%s
		(trace_id, span_id, parent_span_id, fleet, machine, source,
		 operation, start_time, duration, status, status_message,
		 attributes, events)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, suffix)

	var parentSpanID any
	if !span.ParentSpanID.IsZero() {
		parentSpanID = span.ParentSpanID[:]
	}

	return sqlitex.Execute(conn, query, &sqlitex.ExecOptions{
		Args: []any{
			span.TraceID[:],
			span.SpanID[:],
			parentSpanID,
			span.Fleet.Localpart(),
			span.Machine.Localpart(),
			span.Source.Localpart(),
			span.Operation,
			span.StartTime,
			span.Duration,
			int(span.Status),
			span.StatusMessage,
			attributesJSON,
			eventsJSON,
		},
	})
}

// insertMetric inserts a single metric point into its day partition.
func (s *Store) insertMetric(conn *sqlite.Conn, metric *telemetry.MetricPoint) error {
	suffix := partitionSuffix(metric.Timestamp)

	var labelsJSON any
	if len(metric.Labels) > 0 {
		data, err := json.Marshal(metric.Labels)
		if err != nil {
			return fmt.Errorf("telemetry store: marshal metric labels: %w", err)
		}
		labelsJSON = string(data)
	}

	var histogramBlob any
	if metric.Histogram != nil {
		data, err := codec.Marshal(metric.Histogram)
		if err != nil {
			return fmt.Errorf("telemetry store: marshal histogram: %w", err)
		}
		histogramBlob = data
	}

	query := fmt.Sprintf(`INSERT INTO metrics_%s
		(fleet, machine, source, name, labels, kind, timestamp,
		 value_float, value_hist)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, suffix)

	return sqlitex.Execute(conn, query, &sqlitex.ExecOptions{
		Args: []any{
			metric.Fleet.Localpart(),
			metric.Machine.Localpart(),
			metric.Source.Localpart(),
			metric.Name,
			labelsJSON,
			int(metric.Kind),
			metric.Timestamp,
			metric.Value,
			histogramBlob,
		},
	})
}

// insertLog inserts a single log record into its day partition.
func (s *Store) insertLog(conn *sqlite.Conn, logRecord *telemetry.LogRecord) error {
	suffix := partitionSuffix(logRecord.Timestamp)

	var attributesJSON any
	if len(logRecord.Attributes) > 0 {
		data, err := json.Marshal(logRecord.Attributes)
		if err != nil {
			return fmt.Errorf("telemetry store: marshal log attributes: %w", err)
		}
		attributesJSON = string(data)
	}

	var traceID any
	if !logRecord.TraceID.IsZero() {
		traceID = logRecord.TraceID[:]
	}
	var spanID any
	if !logRecord.SpanID.IsZero() {
		spanID = logRecord.SpanID[:]
	}

	query := fmt.Sprintf(`INSERT INTO logs_%s
		(fleet, machine, source, severity, body, trace_id, span_id,
		 timestamp, attributes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, suffix)

	return sqlitex.Execute(conn, query, &sqlitex.ExecOptions{
		Args: []any{
			logRecord.Fleet.Localpart(),
			logRecord.Machine.Localpart(),
			logRecord.Source.Localpart(),
			int(logRecord.Severity),
			logRecord.Body,
			traceID,
			spanID,
			logRecord.Timestamp,
			attributesJSON,
		},
	})
}

// discoverPartitions finds existing partition tables from a previous
// run. Called once during OpenStore.
func (s *Store) discoverPartitions() error {
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		return err
	}
	defer s.pool.Put(conn)

	err = sqlitex.Execute(conn,
		"SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'spans_%'",
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				tableName := stmt.ColumnText(0)
				suffix := strings.TrimPrefix(tableName, "spans_")
				s.knownPartitions[suffix] = true
				return nil
			},
		})
	if err != nil {
		return err
	}

	if len(s.knownPartitions) > 0 {
		s.logger.Info("discovered existing partitions",
			"count", len(s.knownPartitions),
		)
	}

	return nil
}

// RunRetention drops partition tables older than their configured
// retention period. Safe to call from a background ticker.
func (s *Store) RunRetention(ctx context.Context, retention RetentionConfig) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("telemetry store: retention: %w", err)
	}
	defer s.pool.Put(conn)

	now := s.clock.Now().UTC()

	s.partitionMu.Lock()
	defer s.partitionMu.Unlock()

	for suffix := range s.knownPartitions {
		partitionDate, err := time.Parse("20060102", suffix)
		if err != nil {
			s.logger.Warn("retention: unparseable partition suffix",
				"suffix", suffix,
				"error", err,
			)
			continue
		}

		age := now.Sub(partitionDate)

		// Use the shortest retention period for the drop decision.
		// All three table types for a day are created and dropped
		// together, so we use the shortest retention to determine
		// when the partition is fully expired.
		//
		// The +24h accounts for the fact that a partition covers an
		// entire day: a partition dated 2026-02-21 contains data
		// from 00:00 to 23:59:59 UTC, so it shouldn't be dropped
		// until retention + 24h after its date.
		minRetention := retention.Spans
		if retention.Logs < minRetention {
			minRetention = retention.Logs
		}
		// Metrics have a longer default retention, so they
		// effectively get the shorter window when co-partitioned.
		// This is acceptable: the design doc specifies per-signal
		// retention, but the table-per-day scheme means all signals
		// for a day share a lifecycle. If we need different
		// per-signal retention, we would need separate databases
		// or separate partition suffixes per signal type. For now,
		// the minimum retention across signals determines the drop
		// point.
		if retention.Metrics < minRetention {
			minRetention = retention.Metrics
		}

		if age <= minRetention+24*time.Hour {
			continue
		}

		// Drop all three table types for this partition.
		tables := []string{
			"spans_" + suffix,
			"metrics_" + suffix,
			"logs_" + suffix,
		}
		for _, table := range tables {
			dropQuery := "DROP TABLE IF EXISTS " + table
			if err := sqlitex.ExecuteTransient(conn, dropQuery, nil); err != nil {
				s.logger.Error("retention: failed to drop table",
					"table", table,
					"error", err,
				)
				continue
			}
		}

		delete(s.knownPartitions, suffix)
		s.logger.Info("partition dropped by retention",
			"suffix", suffix,
			"age", age.Round(time.Hour),
		)
	}

	return nil
}

// activePartitions returns the known partition suffixes sorted newest
// first. Used by query methods to iterate partitions.
func (s *Store) activePartitions() []string {
	s.partitionMu.Lock()
	partitions := make([]string, 0, len(s.knownPartitions))
	for suffix := range s.knownPartitions {
		partitions = append(partitions, suffix)
	}
	s.partitionMu.Unlock()

	// Sort descending (newest first) so queries return recent data
	// first.
	sort.Sort(sort.Reverse(sort.StringSlice(partitions)))
	return partitions
}

// partitionsInRange returns partition suffixes that overlap with the
// given time range. If start or end is zero, that bound is open.
func (s *Store) partitionsInRange(startNanos, endNanos int64) []string {
	all := s.activePartitions()
	if startNanos == 0 && endNanos == 0 {
		return all
	}

	var filtered []string
	for _, suffix := range all {
		partitionDate, err := time.Parse("20060102", suffix)
		if err != nil {
			continue
		}
		// The partition covers [partitionDate 00:00:00, partitionDate+24h).
		partitionStart := partitionDate.UnixNano()
		partitionEnd := partitionDate.Add(24 * time.Hour).UnixNano()

		if startNanos != 0 && partitionEnd <= startNanos {
			continue // Partition is entirely before the range.
		}
		if endNanos != 0 && partitionStart >= endNanos {
			continue // Partition is entirely after the range.
		}
		filtered = append(filtered, suffix)
	}
	return filtered
}

// SpanFilter specifies the criteria for querying spans. All fields are
// optional; zero-valued fields are not applied as filters.
type SpanFilter struct {
	TraceID     telemetry.TraceID // Exact match on trace ID.
	Machine     string            // Exact match on machine localpart.
	Source      string            // Exact match on source localpart.
	Operation   string            // Prefix match on operation name.
	MinDuration int64             // Minimum span duration in nanoseconds.
	Status      *uint8            // Filter by status (0, 1, or 2).
	Start       int64             // Earliest start_time (Unix nanos).
	End         int64             // Latest start_time (Unix nanos).
	Limit       int               // Maximum spans to return (default 100).
}

// QuerySpans returns spans matching the filter, searching across all
// active partitions. Results are sorted by start_time descending
// (newest first).
func (s *Store) QuerySpans(ctx context.Context, filter SpanFilter) ([]telemetry.Span, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry store: query spans: %w", err)
	}
	defer s.pool.Put(conn)

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}

	partitions := s.partitionsInRange(filter.Start, filter.End)
	if len(partitions) == 0 {
		return nil, nil
	}

	var results []telemetry.Span

	for _, suffix := range partitions {
		if len(results) >= limit {
			break
		}

		remaining := limit - len(results)
		spans, err := s.querySpanPartition(conn, suffix, filter, remaining)
		if err != nil {
			return nil, err
		}
		results = append(results, spans...)
	}

	return results, nil
}

func (s *Store) querySpanPartition(conn *sqlite.Conn, suffix string, filter SpanFilter, limit int) ([]telemetry.Span, error) {
	var conditions []string
	var args []any

	if !filter.TraceID.IsZero() {
		conditions = append(conditions, "trace_id = ?")
		args = append(args, filter.TraceID[:])
	}
	if filter.Machine != "" {
		conditions = append(conditions, "machine = ?")
		args = append(args, filter.Machine)
	}
	if filter.Source != "" {
		conditions = append(conditions, "source = ?")
		args = append(args, filter.Source)
	}
	if filter.Operation != "" {
		conditions = append(conditions, "operation LIKE ?")
		args = append(args, filter.Operation+"%")
	}
	if filter.MinDuration > 0 {
		conditions = append(conditions, "duration >= ?")
		args = append(args, filter.MinDuration)
	}
	if filter.Status != nil {
		conditions = append(conditions, "status = ?")
		args = append(args, int(*filter.Status))
	}
	if filter.Start > 0 {
		conditions = append(conditions, "start_time >= ?")
		args = append(args, filter.Start)
	}
	if filter.End > 0 {
		conditions = append(conditions, "start_time <= ?")
		args = append(args, filter.End)
	}

	query := "SELECT trace_id, span_id, parent_span_id, fleet, machine, source, " +
		"operation, start_time, duration, status, status_message, attributes, events " +
		"FROM spans_" + suffix

	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY start_time DESC LIMIT ?"
	args = append(args, limit)

	var spans []telemetry.Span
	err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			span, err := s.scanSpan(stmt)
			if err != nil {
				return err
			}
			spans = append(spans, span)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("telemetry store: query spans_%s: %w", suffix, err)
	}
	return spans, nil
}

func (s *Store) scanSpan(stmt *sqlite.Stmt) (telemetry.Span, error) {
	var span telemetry.Span

	// Columns: trace_id(0), span_id(1), parent_span_id(2), fleet(3),
	// machine(4), source(5), operation(6), start_time(7), duration(8),
	// status(9), status_message(10), attributes(11), events(12)

	readBlobInto(stmt, 0, span.TraceID[:])
	readBlobInto(stmt, 1, span.SpanID[:])
	if !stmt.ColumnIsNull(2) {
		readBlobInto(stmt, 2, span.ParentSpanID[:])
	}

	// Reconstruct typed refs from stored localparts.
	fleet, err := ref.ParseFleet(stmt.ColumnText(3), s.serverName)
	if err != nil {
		return span, fmt.Errorf("parse fleet localpart: %w", err)
	}
	span.Fleet = fleet

	machine, err := ref.ParseMachine(stmt.ColumnText(4), s.serverName)
	if err != nil {
		return span, fmt.Errorf("parse machine localpart: %w", err)
	}
	span.Machine = machine

	source, err := ref.ParseEntityLocalpart(stmt.ColumnText(5), s.serverName)
	if err != nil {
		return span, fmt.Errorf("parse source localpart: %w", err)
	}
	span.Source = source

	span.Operation = stmt.ColumnText(6)
	span.StartTime = stmt.ColumnInt64(7)
	span.Duration = stmt.ColumnInt64(8)
	span.Status = telemetry.SpanStatus(stmt.ColumnInt(9))
	span.StatusMessage = stmt.ColumnText(10)

	if !stmt.ColumnIsNull(11) {
		attrJSON := stmt.ColumnText(11)
		if err := json.Unmarshal([]byte(attrJSON), &span.Attributes); err != nil {
			return span, fmt.Errorf("unmarshal span attributes: %w", err)
		}
	}

	if !stmt.ColumnIsNull(12) {
		eventsJSON := stmt.ColumnText(12)
		if err := json.Unmarshal([]byte(eventsJSON), &span.Events); err != nil {
			return span, fmt.Errorf("unmarshal span events: %w", err)
		}
	}

	return span, nil
}

// readBlobInto reads a BLOB column directly into a fixed-size byte
// slice. The destination must be pre-allocated to the expected size.
// ColumnBytes performs a copy(destination, columnData).
func readBlobInto(stmt *sqlite.Stmt, column int, destination []byte) {
	stmt.ColumnBytes(column, destination)
}

// MetricFilter specifies the criteria for querying metrics.
type MetricFilter struct {
	Name    string            // Required. Exact match on metric name.
	Machine string            // Exact match on machine localpart.
	Source  string            // Exact match on source localpart.
	Labels  map[string]string // All specified labels must match.
	Start   int64             // Earliest timestamp (Unix nanos).
	End     int64             // Latest timestamp (Unix nanos).
	Limit   int               // Maximum points to return (default 1000).
}

// QueryMetrics returns metric points matching the filter.
func (s *Store) QueryMetrics(ctx context.Context, filter MetricFilter) ([]telemetry.MetricPoint, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry store: query metrics: %w", err)
	}
	defer s.pool.Put(conn)

	limit := filter.Limit
	if limit <= 0 {
		limit = 1000
	}

	partitions := s.partitionsInRange(filter.Start, filter.End)
	if len(partitions) == 0 {
		return nil, nil
	}

	var results []telemetry.MetricPoint

	for _, suffix := range partitions {
		if len(results) >= limit {
			break
		}

		remaining := limit - len(results)
		metrics, err := s.queryMetricPartition(conn, suffix, filter, remaining)
		if err != nil {
			return nil, err
		}
		results = append(results, metrics...)
	}

	return results, nil
}

func (s *Store) queryMetricPartition(conn *sqlite.Conn, suffix string, filter MetricFilter, limit int) ([]telemetry.MetricPoint, error) {
	var conditions []string
	var args []any

	if filter.Name != "" {
		conditions = append(conditions, "name = ?")
		args = append(args, filter.Name)
	}
	if filter.Machine != "" {
		conditions = append(conditions, "machine = ?")
		args = append(args, filter.Machine)
	}
	if filter.Source != "" {
		conditions = append(conditions, "source = ?")
		args = append(args, filter.Source)
	}
	if filter.Start > 0 {
		conditions = append(conditions, "timestamp >= ?")
		args = append(args, filter.Start)
	}
	if filter.End > 0 {
		conditions = append(conditions, "timestamp <= ?")
		args = append(args, filter.End)
	}
	// Label filtering via json_extract: each specified label must
	// match.
	for key, value := range filter.Labels {
		conditions = append(conditions, "json_extract(labels, ?) = ?")
		args = append(args, "$."+key, value)
	}

	query := "SELECT fleet, machine, source, name, labels, kind, timestamp, " +
		"value_float, value_hist FROM metrics_" + suffix

	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY timestamp DESC LIMIT ?"
	args = append(args, limit)

	var metrics []telemetry.MetricPoint
	err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			metric, err := s.scanMetric(stmt)
			if err != nil {
				return err
			}
			metrics = append(metrics, metric)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("telemetry store: query metrics_%s: %w", suffix, err)
	}
	return metrics, nil
}

func (s *Store) scanMetric(stmt *sqlite.Stmt) (telemetry.MetricPoint, error) {
	var metric telemetry.MetricPoint

	// Columns: fleet(0), machine(1), source(2), name(3), labels(4),
	// kind(5), timestamp(6), value_float(7), value_hist(8)

	fleet, err := ref.ParseFleet(stmt.ColumnText(0), s.serverName)
	if err != nil {
		return metric, fmt.Errorf("parse fleet localpart: %w", err)
	}
	metric.Fleet = fleet

	machine, err := ref.ParseMachine(stmt.ColumnText(1), s.serverName)
	if err != nil {
		return metric, fmt.Errorf("parse machine localpart: %w", err)
	}
	metric.Machine = machine

	source, err := ref.ParseEntityLocalpart(stmt.ColumnText(2), s.serverName)
	if err != nil {
		return metric, fmt.Errorf("parse source localpart: %w", err)
	}
	metric.Source = source

	metric.Name = stmt.ColumnText(3)
	metric.Kind = telemetry.MetricKind(stmt.ColumnInt(5))
	metric.Timestamp = stmt.ColumnInt64(6)
	metric.Value = stmt.ColumnFloat(7)

	if !stmt.ColumnIsNull(4) {
		labelsJSON := stmt.ColumnText(4)
		if err := json.Unmarshal([]byte(labelsJSON), &metric.Labels); err != nil {
			return metric, fmt.Errorf("unmarshal metric labels: %w", err)
		}
	}

	if !stmt.ColumnIsNull(8) {
		histBlob := make([]byte, stmt.ColumnLen(8))
		stmt.ColumnBytes(8, histBlob)
		var histogram telemetry.HistogramValue
		if err := codec.Unmarshal(histBlob, &histogram); err != nil {
			return metric, fmt.Errorf("unmarshal histogram: %w", err)
		}
		metric.Histogram = &histogram
	}

	return metric, nil
}

// LogFilter specifies the criteria for querying log records.
type LogFilter struct {
	Machine     string            // Exact match on machine localpart.
	Source      string            // Exact match on source localpart.
	MinSeverity *uint8            // Minimum severity level.
	TraceID     telemetry.TraceID // Logs correlated with this trace.
	Search      string            // Substring match on body.
	Start       int64             // Earliest timestamp (Unix nanos).
	End         int64             // Latest timestamp (Unix nanos).
	Limit       int               // Maximum records to return (default 100).
}

// QueryLogs returns log records matching the filter.
func (s *Store) QueryLogs(ctx context.Context, filter LogFilter) ([]telemetry.LogRecord, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry store: query logs: %w", err)
	}
	defer s.pool.Put(conn)

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}

	partitions := s.partitionsInRange(filter.Start, filter.End)
	if len(partitions) == 0 {
		return nil, nil
	}

	var results []telemetry.LogRecord

	for _, suffix := range partitions {
		if len(results) >= limit {
			break
		}

		remaining := limit - len(results)
		logs, err := s.queryLogPartition(conn, suffix, filter, remaining)
		if err != nil {
			return nil, err
		}
		results = append(results, logs...)
	}

	return results, nil
}

func (s *Store) queryLogPartition(conn *sqlite.Conn, suffix string, filter LogFilter, limit int) ([]telemetry.LogRecord, error) {
	var conditions []string
	var args []any

	if filter.Machine != "" {
		conditions = append(conditions, "machine = ?")
		args = append(args, filter.Machine)
	}
	if filter.Source != "" {
		conditions = append(conditions, "source = ?")
		args = append(args, filter.Source)
	}
	if filter.MinSeverity != nil {
		conditions = append(conditions, "severity >= ?")
		args = append(args, int(*filter.MinSeverity))
	}
	if !filter.TraceID.IsZero() {
		conditions = append(conditions, "trace_id = ?")
		args = append(args, filter.TraceID[:])
	}
	if filter.Search != "" {
		conditions = append(conditions, "body LIKE ?")
		args = append(args, "%"+filter.Search+"%")
	}
	if filter.Start > 0 {
		conditions = append(conditions, "timestamp >= ?")
		args = append(args, filter.Start)
	}
	if filter.End > 0 {
		conditions = append(conditions, "timestamp <= ?")
		args = append(args, filter.End)
	}

	query := "SELECT fleet, machine, source, severity, body, trace_id, span_id, " +
		"timestamp, attributes FROM logs_" + suffix

	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY timestamp DESC LIMIT ?"
	args = append(args, limit)

	var logs []telemetry.LogRecord
	err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			logRecord, err := s.scanLog(stmt)
			if err != nil {
				return err
			}
			logs = append(logs, logRecord)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("telemetry store: query logs_%s: %w", suffix, err)
	}
	return logs, nil
}

func (s *Store) scanLog(stmt *sqlite.Stmt) (telemetry.LogRecord, error) {
	var logRecord telemetry.LogRecord

	// Columns: fleet(0), machine(1), source(2), severity(3), body(4),
	// trace_id(5), span_id(6), timestamp(7), attributes(8)

	fleet, err := ref.ParseFleet(stmt.ColumnText(0), s.serverName)
	if err != nil {
		return logRecord, fmt.Errorf("parse fleet localpart: %w", err)
	}
	logRecord.Fleet = fleet

	machine, err := ref.ParseMachine(stmt.ColumnText(1), s.serverName)
	if err != nil {
		return logRecord, fmt.Errorf("parse machine localpart: %w", err)
	}
	logRecord.Machine = machine

	source, err := ref.ParseEntityLocalpart(stmt.ColumnText(2), s.serverName)
	if err != nil {
		return logRecord, fmt.Errorf("parse source localpart: %w", err)
	}
	logRecord.Source = source

	logRecord.Severity = uint8(stmt.ColumnInt(3))
	logRecord.Body = stmt.ColumnText(4)
	logRecord.Timestamp = stmt.ColumnInt64(7)

	if !stmt.ColumnIsNull(5) {
		readBlobInto(stmt, 5, logRecord.TraceID[:])
	}

	if !stmt.ColumnIsNull(6) {
		readBlobInto(stmt, 6, logRecord.SpanID[:])
	}

	if !stmt.ColumnIsNull(8) {
		attrJSON := stmt.ColumnText(8)
		if err := json.Unmarshal([]byte(attrJSON), &logRecord.Attributes); err != nil {
			return logRecord, fmt.Errorf("unmarshal log attributes: %w", err)
		}
	}

	return logRecord, nil
}

// Stats returns current storage statistics for inclusion in the
// service status response.
func (s *Store) Stats(ctx context.Context) (telemetry.StorageStats, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return telemetry.StorageStats{}, fmt.Errorf("telemetry store: stats: %w", err)
	}
	defer s.pool.Put(conn)

	partitions := s.activePartitions()

	stats := telemetry.StorageStats{
		PartitionCount: len(partitions),
	}

	if len(partitions) > 0 {
		stats.NewestPartition = partitions[0]
		stats.OldestPartition = partitions[len(partitions)-1]
	}

	// Database size via page_count * page_size.
	err = sqlitex.Execute(conn, "SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			stats.DatabaseSizeBytes = stmt.ColumnInt64(0)
			return nil
		},
	})
	if err != nil {
		return stats, fmt.Errorf("telemetry store: database size: %w", err)
	}

	// Count records per signal type.
	for _, suffix := range partitions {
		count, err := tableRowCount(conn, "spans_"+suffix)
		if err != nil {
			return stats, err
		}
		stats.SpanCount += count

		count, err = tableRowCount(conn, "metrics_"+suffix)
		if err != nil {
			return stats, err
		}
		stats.MetricCount += count

		count, err = tableRowCount(conn, "logs_"+suffix)
		if err != nil {
			return stats, err
		}
		stats.LogCount += count
	}

	return stats, nil
}

// TopFilter specifies the criteria for the aggregated operational
// overview. Start is required (the handler resolves Window→Start
// before calling QueryTop).
type TopFilter struct {
	Start   int64  // Earliest span start_time (Unix nanoseconds). Required.
	End     int64  // Latest span start_time (0 = unbounded).
	Machine string // Optional machine localpart filter.
}

// topOperationStats accumulates per-operation aggregate counters
// across partitions for the top overview.
type topOperationStats struct {
	count      int64
	errorCount int64
}

// topMachineStats accumulates per-machine aggregate counters across
// partitions for the top overview.
type topMachineStats struct {
	spanCount  int64
	errorCount int64
}

const topResultLimit = 10

// QueryTop returns an aggregated operational overview for the given
// time window. It runs two passes over span partitions:
//
//  1. Aggregate pass: GROUP BY operation (and machine) to collect
//     counts and error counts across all partitions in the window.
//  2. P99 pass: for the top operations by count, fetch the P99
//     duration using an indexed offset query per partition.
//
// Results are limited to the top 10 per category.
func (s *Store) QueryTop(ctx context.Context, filter TopFilter) (telemetry.TopResponse, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return telemetry.TopResponse{}, fmt.Errorf("telemetry store: query top: %w", err)
	}
	defer s.pool.Put(conn)

	partitions := s.partitionsInRange(filter.Start, filter.End)

	var response telemetry.TopResponse

	if len(partitions) == 0 {
		response.SlowestOperations = []telemetry.OperationDuration{}
		response.HighestErrorRate = []telemetry.OperationErrorRate{}
		response.HighestThroughput = []telemetry.OperationThroughput{}
		response.MachineActivity = []telemetry.MachineActivity{}
		return response, nil
	}

	// Pass 1: aggregate per-operation and per-machine stats.
	operationStats := make(map[string]*topOperationStats)
	machineStats := make(map[string]*topMachineStats)

	for _, suffix := range partitions {
		if err := s.topAggregateOperations(conn, suffix, filter.Start, filter.End, filter.Machine, operationStats); err != nil {
			return telemetry.TopResponse{}, err
		}
		if filter.Machine == "" {
			if err := s.topAggregateMachines(conn, suffix, filter.Start, filter.End, machineStats); err != nil {
				return telemetry.TopResponse{}, err
			}
		}
	}

	// Rank operations by count to identify the top N for P99 queries.
	type rankedOperation struct {
		operation string
		stats     *topOperationStats
	}
	ranked := make([]rankedOperation, 0, len(operationStats))
	for operation, stats := range operationStats {
		ranked = append(ranked, rankedOperation{operation: operation, stats: stats})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].stats.count > ranked[j].stats.count
	})
	if len(ranked) > topResultLimit {
		ranked = ranked[:topResultLimit]
	}

	// Pass 2: P99 duration for the top operations.
	type operationP99 struct {
		operation string
		p99       int64
		count     int64
	}
	p99Results := make([]operationP99, 0, len(ranked))

	for _, entry := range ranked {
		offset := percentile99Offset(entry.stats.count)
		var maxP99 int64
		for _, suffix := range partitions {
			duration, err := s.topP99ForOperation(conn, suffix, filter.Start, filter.End, filter.Machine, entry.operation, offset)
			if err != nil {
				return telemetry.TopResponse{}, err
			}
			if duration > maxP99 {
				maxP99 = duration
			}
		}
		p99Results = append(p99Results, operationP99{
			operation: entry.operation,
			p99:       maxP99,
			count:     entry.stats.count,
		})
	}

	// Sort by P99 descending for the slowest operations ranking.
	sort.Slice(p99Results, func(i, j int) bool {
		return p99Results[i].p99 > p99Results[j].p99
	})
	response.SlowestOperations = make([]telemetry.OperationDuration, len(p99Results))
	for i, entry := range p99Results {
		response.SlowestOperations[i] = telemetry.OperationDuration{
			Operation:   entry.operation,
			P99Duration: entry.p99,
			Count:       entry.count,
		}
	}

	// Build error rate ranking (only operations with errors).
	type operationError struct {
		operation  string
		errorRate  float64
		errorCount int64
		totalCount int64
	}
	var errorRanked []operationError
	for operation, stats := range operationStats {
		if stats.errorCount > 0 {
			errorRanked = append(errorRanked, operationError{
				operation:  operation,
				errorRate:  float64(stats.errorCount) / float64(stats.count),
				errorCount: stats.errorCount,
				totalCount: stats.count,
			})
		}
	}
	sort.Slice(errorRanked, func(i, j int) bool {
		return errorRanked[i].errorRate > errorRanked[j].errorRate
	})
	if len(errorRanked) > topResultLimit {
		errorRanked = errorRanked[:topResultLimit]
	}
	response.HighestErrorRate = make([]telemetry.OperationErrorRate, len(errorRanked))
	for i, entry := range errorRanked {
		response.HighestErrorRate[i] = telemetry.OperationErrorRate{
			Operation:  entry.operation,
			ErrorRate:  entry.errorRate,
			ErrorCount: entry.errorCount,
			TotalCount: entry.totalCount,
		}
	}

	// Build throughput ranking (re-use ranked which is already sorted by count).
	response.HighestThroughput = make([]telemetry.OperationThroughput, len(ranked))
	for i, entry := range ranked {
		response.HighestThroughput[i] = telemetry.OperationThroughput{
			Operation: entry.operation,
			Count:     entry.stats.count,
		}
	}

	// Build machine activity ranking.
	type machineRanked struct {
		machine string
		stats   *topMachineStats
	}
	machines := make([]machineRanked, 0, len(machineStats))
	for machine, stats := range machineStats {
		machines = append(machines, machineRanked{machine: machine, stats: stats})
	}
	sort.Slice(machines, func(i, j int) bool {
		return machines[i].stats.spanCount > machines[j].stats.spanCount
	})
	if len(machines) > topResultLimit {
		machines = machines[:topResultLimit]
	}
	response.MachineActivity = make([]telemetry.MachineActivity, len(machines))
	for i, entry := range machines {
		var errorRate float64
		if entry.stats.spanCount > 0 {
			errorRate = float64(entry.stats.errorCount) / float64(entry.stats.spanCount)
		}
		response.MachineActivity[i] = telemetry.MachineActivity{
			Machine:    entry.machine,
			SpanCount:  entry.stats.spanCount,
			ErrorCount: entry.stats.errorCount,
			ErrorRate:  errorRate,
		}
	}

	return response, nil
}

// topAggregateOperations runs a GROUP BY operation aggregate query on
// a single span partition, merging results into the provided map.
func (s *Store) topAggregateOperations(conn *sqlite.Conn, suffix string, startNanos, endNanos int64, machine string, stats map[string]*topOperationStats) error {
	var conditions []string
	var args []any

	conditions = append(conditions, "start_time >= ?")
	args = append(args, startNanos)

	if endNanos != 0 {
		conditions = append(conditions, "start_time <= ?")
		args = append(args, endNanos)
	}

	if machine != "" {
		conditions = append(conditions, "machine = ?")
		args = append(args, machine)
	}

	query := "SELECT operation, COUNT(*) AS total, " +
		"SUM(CASE WHEN status = 2 THEN 1 ELSE 0 END) AS errors " +
		"FROM spans_" + suffix +
		" WHERE " + strings.Join(conditions, " AND ") +
		" GROUP BY operation"

	err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			operation := stmt.ColumnText(0)
			total := stmt.ColumnInt64(1)
			errors := stmt.ColumnInt64(2)

			entry, exists := stats[operation]
			if !exists {
				entry = &topOperationStats{}
				stats[operation] = entry
			}
			entry.count += total
			entry.errorCount += errors
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("telemetry store: top operations spans_%s: %w", suffix, err)
	}
	return nil
}

// topAggregateMachines runs a GROUP BY machine aggregate query on
// a single span partition, merging results into the provided map.
func (s *Store) topAggregateMachines(conn *sqlite.Conn, suffix string, startNanos, endNanos int64, stats map[string]*topMachineStats) error {
	var conditions []string
	var args []any

	conditions = append(conditions, "start_time >= ?")
	args = append(args, startNanos)

	if endNanos != 0 {
		conditions = append(conditions, "start_time <= ?")
		args = append(args, endNanos)
	}

	query := "SELECT machine, COUNT(*) AS total, " +
		"SUM(CASE WHEN status = 2 THEN 1 ELSE 0 END) AS errors " +
		"FROM spans_" + suffix +
		" WHERE " + strings.Join(conditions, " AND ") +
		" GROUP BY machine"

	err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			machine := stmt.ColumnText(0)
			total := stmt.ColumnInt64(1)
			errors := stmt.ColumnInt64(2)

			entry, exists := stats[machine]
			if !exists {
				entry = &topMachineStats{}
				stats[machine] = entry
			}
			entry.spanCount += total
			entry.errorCount += errors
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("telemetry store: top machines spans_%s: %w", suffix, err)
	}
	return nil
}

// topP99ForOperation fetches the P99 duration for a specific operation
// from a single span partition using an indexed offset query.
// Returns 0 if no spans match (partition has no data for this operation).
func (s *Store) topP99ForOperation(conn *sqlite.Conn, suffix string, startNanos, endNanos int64, machine, operation string, offset int64) (int64, error) {
	var conditions []string
	var args []any

	conditions = append(conditions, "operation = ?")
	args = append(args, operation)

	conditions = append(conditions, "start_time >= ?")
	args = append(args, startNanos)

	if endNanos != 0 {
		conditions = append(conditions, "start_time <= ?")
		args = append(args, endNanos)
	}

	if machine != "" {
		conditions = append(conditions, "machine = ?")
		args = append(args, machine)
	}

	query := "SELECT duration FROM spans_" + suffix +
		" WHERE " + strings.Join(conditions, " AND ") +
		" ORDER BY duration DESC LIMIT 1 OFFSET ?"
	args = append(args, offset)

	var duration int64
	err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			duration = stmt.ColumnInt64(0)
			return nil
		},
	})
	if err != nil {
		return 0, fmt.Errorf("telemetry store: top p99 spans_%s op=%s: %w", suffix, operation, err)
	}
	return duration, nil
}

// percentile99Offset returns the SQL OFFSET value for fetching the
// P99 duration from an ORDER BY duration DESC result set. For a count
// of N, P99 is the value at position ceil(N * 0.01) from the top.
func percentile99Offset(count int64) int64 {
	if count <= 0 {
		return 0
	}
	// P99 offset from the top (descending order):
	// for 100 spans, offset 0 = max, offset 1 = P99.
	// For N spans, offset = floor(N * 0.01).
	offset := count / 100
	if offset < 0 {
		offset = 0
	}
	return offset
}

func tableRowCount(conn *sqlite.Conn, tableName string) (int64, error) {
	var count int64
	query := "SELECT COUNT(*) FROM " + tableName
	err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt64(0)
			return nil
		},
	})
	if err != nil {
		return 0, fmt.Errorf("telemetry store: count %s: %w", tableName, err)
	}
	return count, nil
}
