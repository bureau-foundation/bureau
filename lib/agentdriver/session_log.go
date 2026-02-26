// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agentdriver

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// SessionLogWriter writes structured events as JSONL (one JSON object per
// line) to a session log file. It is safe for concurrent use.
type SessionLogWriter struct {
	file    *os.File
	encoder *json.Encoder
	mutex   sync.Mutex
	closed  bool

	// Aggregated summary counters, protected by mutex.
	startTime        time.Time
	eventCount       int64
	inputTokens      int64
	outputTokens     int64
	cacheReadTokens  int64
	cacheWriteTokens int64
	costUSD          float64
	toolCallCount    int64
	errorCount       int64
	turnCount        int64
}

// NewSessionLogWriter creates a new session log file and returns a writer.
// The file is created (or truncated) at the given path.
func NewSessionLogWriter(path string) (*SessionLogWriter, error) {
	file, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("creating session log %q: %w", path, err)
	}
	encoder := json.NewEncoder(file)
	// No indentation — one compact JSON object per line.
	encoder.SetEscapeHTML(false)
	return &SessionLogWriter{
		file:      file,
		encoder:   encoder,
		startTime: time.Now(),
	}, nil
}

// Write appends a single event to the session log and updates summary
// counters. The event is serialized as a single JSON line.
func (writer *SessionLogWriter) Write(event Event) error {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()

	if err := writer.encoder.Encode(event); err != nil {
		return fmt.Errorf("encoding session log event: %w", err)
	}

	// Sync after each write so events are visible even if the process
	// crashes. The performance cost is acceptable for agent session logs
	// which are low-throughput (tens of events per second at most).
	if err := writer.file.Sync(); err != nil {
		return fmt.Errorf("syncing session log: %w", err)
	}

	writer.eventCount++

	switch event.Type {
	case EventTypeToolCall:
		writer.toolCallCount++
	case EventTypeError:
		writer.errorCount++
	case EventTypeMetric:
		if event.Metric != nil {
			writer.inputTokens += event.Metric.InputTokens
			writer.outputTokens += event.Metric.OutputTokens
			writer.cacheReadTokens += event.Metric.CacheReadTokens
			writer.cacheWriteTokens += event.Metric.CacheWriteTokens
			writer.costUSD += event.Metric.CostUSD
			writer.turnCount += event.Metric.TurnCount
		}
	}

	return nil
}

// Close flushes any buffered data and closes the underlying file.
// Close is idempotent — calling it more than once returns nil.
func (writer *SessionLogWriter) Close() error {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	if writer.closed {
		return nil
	}
	writer.closed = true
	return writer.file.Close()
}

// SessionSummary is an aggregated summary of the session log.
type SessionSummary struct {
	EventCount       int64         `json:"event_count"`
	InputTokens      int64         `json:"input_tokens"`
	OutputTokens     int64         `json:"output_tokens"`
	CacheReadTokens  int64         `json:"cache_read_tokens"`
	CacheWriteTokens int64         `json:"cache_write_tokens"`
	CostUSD          float64       `json:"cost_usd"`
	ToolCallCount    int64         `json:"tool_call_count"`
	ErrorCount       int64         `json:"error_count"`
	TurnCount        int64         `json:"turn_count"`
	Duration         time.Duration `json:"duration"`
}

// Summary returns an aggregated summary of all events written so far.
func (writer *SessionLogWriter) Summary() SessionSummary {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return SessionSummary{
		EventCount:       writer.eventCount,
		InputTokens:      writer.inputTokens,
		OutputTokens:     writer.outputTokens,
		CacheReadTokens:  writer.cacheReadTokens,
		CacheWriteTokens: writer.cacheWriteTokens,
		CostUSD:          writer.costUSD,
		ToolCallCount:    writer.toolCallCount,
		ErrorCount:       writer.errorCount,
		TurnCount:        writer.turnCount,
		Duration:         time.Since(writer.startTime),
	}
}
