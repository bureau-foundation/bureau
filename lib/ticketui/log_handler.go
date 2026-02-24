// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// logRecordMsg delivers a slog record to the bubbletea model for
// display in the status bar. Only records at or above the handler's
// configured level are delivered.
type logRecordMsg struct {
	// Summary is the human-readable one-line message for the status bar.
	Summary string

	// Structured is the full JSON-encoded log record for clipboard copy.
	Structured string

	// Level is the slog level for styling (warn vs error).
	Level slog.Level
}

// logRecordFadeMsg is sent after a delay to clear the log message
// from the status bar and restore the normal help text.
type logRecordFadeMsg struct{}

// logRecordFadeDelay is how long log messages stay visible in the
// status bar before fading back to the keyboard help line.
const logRecordFadeDelay = 5 * time.Second

// TUILogHandler is a slog.Handler that routes log records into a
// bubbletea program as messages. Records below the configured level
// are silently dropped. Records at or above the level are formatted
// and sent via program.Send().
//
// The handler must be created before the program starts. Call
// SetProgram once the tea.Program is created to enable message
// delivery. Records arriving before SetProgram is called are dropped.
//
// All handlers derived via WithAttrs/WithGroup share the same program
// pointer, so a single SetProgram call propagates to every derived
// handler.
type TUILogHandler struct {
	level   slog.Level
	program *atomic.Pointer[tea.Program]
	attrs   []slog.Attr
	groups  []string
}

// NewTUILogHandler creates a handler that delivers log records at or
// above the given level to the bubbletea program. Call SetProgram
// after creating the tea.Program.
func NewTUILogHandler(level slog.Level) *TUILogHandler {
	return &TUILogHandler{
		level:   level,
		program: &atomic.Pointer[tea.Program]{},
	}
}

// SetProgram sets the bubbletea program that receives log messages.
// Must be called before the program's Run method returns; safe to call
// from any goroutine. Propagates to all handlers derived from this one
// via WithAttrs/WithGroup (they share the same atomic pointer).
func (handler *TUILogHandler) SetProgram(program *tea.Program) {
	handler.program.Store(program)
}

// Enabled reports whether the handler is interested in records at the
// given level.
func (handler *TUILogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= handler.level
}

// Handle formats the record and sends it to the bubbletea program.
// If the program has not been set yet, the record is silently dropped.
func (handler *TUILogHandler) Handle(_ context.Context, record slog.Record) error {
	program := handler.program.Load()
	if program == nil {
		return nil
	}

	// Build the summary line: "message (key=value, ...)"
	summary := record.Message
	var attrParts []string

	// Include handler-level attrs first (from WithAttrs/WithGroup).
	for _, attr := range handler.attrs {
		attrParts = append(attrParts, fmt.Sprintf("%s=%s", attr.Key, attr.Value))
	}

	// Then record-level attrs.
	record.Attrs(func(attr slog.Attr) bool {
		attrParts = append(attrParts, fmt.Sprintf("%s=%s", attr.Key, attr.Value))
		return true
	})

	if len(attrParts) > 0 {
		summary += " ("
		for index, part := range attrParts {
			if index > 0 {
				summary += ", "
			}
			summary += part
		}
		summary += ")"
	}

	// Build the full structured JSON for clipboard copy.
	structured := handler.buildStructuredJSON(record)

	program.Send(logRecordMsg{
		Summary:    summary,
		Structured: structured,
		Level:      record.Level,
	})

	return nil
}

// WithAttrs returns a new handler with the given attributes appended.
// The derived handler shares the same atomic program pointer, so
// SetProgram on the root handler propagates automatically.
func (handler *TUILogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TUILogHandler{
		level:   handler.level,
		program: handler.program,
		attrs:   append(sliceClone(handler.attrs), attrs...),
		groups:  sliceClone(handler.groups),
	}
}

// WithGroup returns a new handler with the given group name appended.
// The derived handler shares the same atomic program pointer, so
// SetProgram on the root handler propagates automatically.
func (handler *TUILogHandler) WithGroup(name string) slog.Handler {
	return &TUILogHandler{
		level:   handler.level,
		program: handler.program,
		attrs:   sliceClone(handler.attrs),
		groups:  append(sliceClone(handler.groups), name),
	}
}

// buildStructuredJSON produces a JSON string containing the full log
// record with all attributes, suitable for clipboard copy.
func (handler *TUILogHandler) buildStructuredJSON(record slog.Record) string {
	fields := map[string]any{
		"time":  record.Time.Format(time.RFC3339),
		"level": record.Level.String(),
		"msg":   record.Message,
	}

	for _, attr := range handler.attrs {
		fields[attr.Key] = attr.Value.String()
	}

	record.Attrs(func(attr slog.Attr) bool {
		fields[attr.Key] = attr.Value.String()
		return true
	})

	data, err := json.Marshal(fields)
	if err != nil {
		return fmt.Sprintf(`{"msg":%q,"error":"marshal failed"}`, record.Message)
	}
	return string(data)
}

// sliceClone returns a shallow copy of a slice. Avoids aliasing when
// building derived handlers with WithAttrs/WithGroup.
func sliceClone[T any](source []T) []T {
	if source == nil {
		return nil
	}
	result := make([]T, len(source))
	copy(result, source)
	return result
}
