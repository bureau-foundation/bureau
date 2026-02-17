// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bufio"
	"io"
	"strings"
)

// SSEEvent is a single Server-Sent Event parsed from an SSE stream.
type SSEEvent struct {
	// Type is the event type from the "event:" field. Empty string
	// if no event type was specified (the SSE spec calls this the
	// default event type).
	Type string

	// Data is the event payload, assembled from one or more "data:"
	// lines. Multiple data lines are joined with newlines per the
	// SSE specification.
	Data string
}

// SSEScanner reads Server-Sent Events from an [io.Reader] according
// to the W3C Server-Sent Events specification.
//
// Events are delimited by blank lines. Within an event, lines starting
// with "data:" carry the payload, and lines starting with "event:"
// specify the event type. Comment lines (starting with ":") and
// unknown fields are ignored.
//
// Usage:
//
//	scanner := NewSSEScanner(reader)
//	for scanner.Next() {
//	    event := scanner.Event()
//	    // process event.Type and event.Data
//	}
//	if err := scanner.Err(); err != nil {
//	    // handle error
//	}
type SSEScanner struct {
	reader  *bufio.Reader
	current SSEEvent
	err     error
}

// NewSSEScanner creates a scanner that reads SSE events from reader.
func NewSSEScanner(reader io.Reader) *SSEScanner {
	return &SSEScanner{
		reader: bufio.NewReaderSize(reader, 64*1024),
	}
}

// Next advances to the next event. Returns false when the stream
// ends (EOF) or an error occurs. After Next returns false, call
// [Err] to distinguish EOF from errors.
func (scanner *SSEScanner) Next() bool {
	scanner.current = SSEEvent{}

	var dataLines []string
	var eventType string
	hasData := false

	for {
		line, err := scanner.reader.ReadString('\n')

		// Handle partial last line (no trailing newline before EOF).
		if err != nil && line == "" {
			if err == io.EOF {
				// If we accumulated data, emit the final event.
				if hasData {
					scanner.current = SSEEvent{
						Type: eventType,
						Data: strings.Join(dataLines, "\n"),
					}
					// Set EOF so the next call to Next returns false.
					scanner.err = io.EOF
					return true
				}
				return false
			}
			scanner.err = err
			return false
		}

		// Strip trailing newline (and optional carriage return).
		line = strings.TrimRight(line, "\r\n")

		// Blank line = event boundary.
		if line == "" {
			if hasData {
				scanner.current = SSEEvent{
					Type: eventType,
					Data: strings.Join(dataLines, "\n"),
				}
				return true
			}
			// No data accumulated — skip this empty block and continue.
			eventType = ""
			continue
		}

		// Comment lines start with ":".
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Parse "field: value" or "field:value" (space after colon is optional).
		field, value, hasColon := strings.Cut(line, ":")
		if !hasColon {
			// Lines without a colon are treated as field name with empty value.
			field = line
			value = ""
		} else {
			// Per spec: if value starts with a space, remove exactly one space.
			value = strings.TrimPrefix(value, " ")
		}

		switch field {
		case "data":
			dataLines = append(dataLines, value)
			hasData = true
		case "event":
			eventType = value
		case "id", "retry":
			// Recognized fields we don't need — ignore per spec.
		default:
			// Unknown fields are ignored per the SSE specification.
		}
	}
}

// Event returns the most recently parsed event. Only valid after
// [Next] returns true.
func (scanner *SSEScanner) Event() SSEEvent {
	return scanner.current
}

// Err returns the first error encountered during scanning. Returns
// nil if scanning ended due to a clean EOF.
func (scanner *SSEScanner) Err() error {
	if scanner.err == io.EOF {
		return nil
	}
	return scanner.err
}
