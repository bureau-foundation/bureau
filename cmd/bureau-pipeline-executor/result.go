// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
)

// resultLog writes structured JSONL to a file during pipeline execution.
// Each line is an independent JSON object, making the log:
//
//   - Crash-safe: a SIGKILL mid-pipeline preserves all completed step
//     results. A single JSON file would be truncated and unparseable.
//   - Streamable: the daemon can tail the file for real-time step-by-step
//     progress instead of waiting for completion.
//
// The file path is controlled by the BUREAU_RESULT_PATH environment
// variable. When not set, the result log is disabled (all methods are
// nil-safe no-ops). The launcher sets this variable when spawning
// executor sandboxes to enable daemon integration.
type resultLog struct {
	logger  *slog.Logger
	file    *os.File
	encoder *json.Encoder
}

// newResultLog creates a JSONL result log at the given path. The file
// is created (truncating any existing content) immediately. Returns an
// error if the file cannot be created.
func newResultLog(path string, logger *slog.Logger) (*resultLog, error) {
	file, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("creating result log %s: %w", path, err)
	}
	return &resultLog{
		logger:  logger,
		file:    file,
		encoder: json.NewEncoder(file),
	}, nil
}

// Close flushes and closes the result log file.
func (r *resultLog) Close() error {
	if r == nil {
		return nil
	}
	return r.file.Close()
}

// writeStart records pipeline execution start.
func (r *resultLog) writeStart(pipeline string, stepCount int) {
	if r == nil {
		return
	}
	r.write(resultStartEntry{
		Type:      "start",
		Pipeline:  pipeline,
		StepCount: stepCount,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// writeStep records the outcome of a single step.
func (r *resultLog) writeStep(index int, name string, status pipeline.StepResultStatus, durationMS int64, stepError string, outputs map[string]string) {
	if r == nil {
		return
	}
	r.write(resultStepEntry{
		Type:       "step",
		Index:      index,
		Name:       name,
		Status:     status,
		DurationMS: durationMS,
		Error:      stepError,
		Outputs:    outputs,
	})
}

// writeComplete records successful pipeline completion.
func (r *resultLog) writeComplete(durationMS int64, logEventID ref.EventID, outputs map[string]string) {
	if r == nil {
		return
	}
	r.write(resultCompleteEntry{
		Type:       "complete",
		Status:     "ok",
		DurationMS: durationMS,
		LogEventID: logEventID,
		Outputs:    outputs,
	})
}

// writeFailed records pipeline failure.
func (r *resultLog) writeFailed(failedStep, errorMessage string, durationMS int64, logEventID ref.EventID) {
	if r == nil {
		return
	}
	r.write(resultFailedEntry{
		Type:       "failed",
		Status:     "failed",
		Error:      errorMessage,
		FailedStep: failedStep,
		DurationMS: durationMS,
		LogEventID: logEventID,
	})
}

// writeAborted records a clean pipeline abort (precondition not met).
// Unlike writeFailed, this indicates the pipeline did not encounter an error
// — the work was simply not needed because the precondition changed.
func (r *resultLog) writeAborted(abortedStep, reason string, durationMS int64, logEventID ref.EventID) {
	if r == nil {
		return
	}
	r.write(resultAbortedEntry{
		Type:        "aborted",
		Status:      "aborted",
		Reason:      reason,
		AbortedStep: abortedStep,
		DurationMS:  durationMS,
		LogEventID:  logEventID,
	})
}

func (r *resultLog) write(entry any) {
	if err := r.encoder.Encode(entry); err != nil {
		r.logger.Warn("failed to write result log entry", "error", err)
		return
	}
	// Sync after each line so that partial results survive a crash and
	// are visible to readers (daemon tailing for progress) immediately.
	if err := r.file.Sync(); err != nil {
		r.logger.Warn("failed to sync result log", "error", err)
	}
}

// JSONL entry types. Each struct documents exactly which fields appear
// in that line type. Separate structs (rather than one with omitempty
// everywhere) make the wire format explicit and self-documenting.

// resultStartEntry is the first line, written at pipeline start.
type resultStartEntry struct {
	Type      string `json:"type"`
	Pipeline  string `json:"pipeline"`
	StepCount int    `json:"step_count"`
	Timestamp string `json:"timestamp"`
}

// resultStepEntry is written after each step completes (or is skipped).
type resultStepEntry struct {
	Type       string                    `json:"type"`
	Index      int                       `json:"index"`
	Name       string                    `json:"name"`
	Status     pipeline.StepResultStatus `json:"status"`
	DurationMS int64                     `json:"duration_ms"`
	Error      string                    `json:"error,omitempty"`
	Outputs    map[string]string         `json:"outputs,omitempty"`
}

// resultCompleteEntry is the last line on successful pipeline completion.
type resultCompleteEntry struct {
	Type       string            `json:"type"`
	Status     string            `json:"status"`
	DurationMS int64             `json:"duration_ms"`
	LogEventID ref.EventID       `json:"log_event_id"`
	Outputs    map[string]string `json:"outputs,omitempty"`
}

// resultFailedEntry is the last line when the pipeline fails.
type resultFailedEntry struct {
	Type       string      `json:"type"`
	Status     string      `json:"status"`
	Error      string      `json:"error"`
	FailedStep string      `json:"failed_step"`
	DurationMS int64       `json:"duration_ms"`
	LogEventID ref.EventID `json:"log_event_id"`
}

// resultAbortedEntry is the last line when the pipeline aborts cleanly
// due to an assert_state precondition mismatch with on_mismatch="abort".
type resultAbortedEntry struct {
	Type        string      `json:"type"`
	Status      string      `json:"status"`
	Reason      string      `json:"reason"`
	AbortedStep string      `json:"aborted_step"`
	DurationMS  int64       `json:"duration_ms"`
	LogEventID  ref.EventID `json:"log_event_id"`
}
