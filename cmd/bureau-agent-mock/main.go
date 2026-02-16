// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// bureau-agent-mock is a test binary that exercises lib/agent.Run with a
// mock Driver. It proves the agent lifecycle works end-to-end in a real
// Bureau sandbox: proxy client connectivity, context building, session
// logging, event processing, Matrix messaging, and graceful shutdown.
//
// The mock driver spawns no external process. Instead it uses an internal
// pipe to emit a fixed sequence of stream-json-like events, then exits.
// This isolates the test to Bureau infrastructure without requiring an
// actual AI agent or API key.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/lib/agent"
)

func main() {
	config := agent.RunConfigFromEnvironment()
	config.SessionLogPath = "/run/bureau/session.jsonl"

	if err := agent.Run(context.Background(), &mockDriver{}, config); err != nil {
		fmt.Fprintf(os.Stderr, "bureau-agent-mock: %v\n", err)
		os.Exit(1)
	}
}

// mockDriver implements agent.Driver with an internal pipe instead of an
// external process.
type mockDriver struct{}

// mockProcess wraps an internal pipe to implement agent.Process.
type mockProcess struct {
	stdinWriter *io.PipeWriter
	done        chan struct{}
}

func (process *mockProcess) Wait() error {
	<-process.done
	return nil
}

func (process *mockProcess) Stdin() io.Writer {
	return process.stdinWriter
}

func (process *mockProcess) Signal(signal os.Signal) error {
	return nil
}

func (driver *mockDriver) Start(ctx context.Context, config agent.DriverConfig) (agent.Process, io.ReadCloser, error) {
	_, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	done := make(chan struct{})

	process := &mockProcess{
		stdinWriter: stdinWriter,
		done:        done,
	}

	// Emit events in a goroutine, then exit.
	go func() {
		defer close(done)
		defer stdoutWriter.Close()

		// Emit a few mock events.
		events := []map[string]any{
			{"type": "system", "subtype": "init", "message": "mock agent starting"},
			{"type": "assistant", "subtype": "text", "text": "I am a mock agent running in Bureau."},
			{"type": "assistant", "subtype": "tool_use", "tool_use_id": "tu-mock-1", "name": "Read", "input": map[string]string{"file_path": "/run/bureau/payload.json"}},
			{"type": "tool", "subtype": "result", "tool_use_id": "tu-mock-1", "content": "payload content", "is_error": false},
			{"type": "assistant", "subtype": "text", "text": "Mock agent task complete."},
			{"type": "result", "subtype": "success", "cost_usd": 0.001, "input_tokens": 100, "output_tokens": 50, "num_turns": 1, "duration_ms": 500},
		}

		for _, event := range events {
			data, _ := json.Marshal(event)
			fmt.Fprintf(stdoutWriter, "%s\n", data)
			time.Sleep(10 * time.Millisecond) // Brief spacing between events.
		}
	}()

	return process, stdoutReader, nil
}

func (driver *mockDriver) ParseOutput(ctx context.Context, stdout io.Reader, events chan<- agent.Event) error {
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var envelope struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal(line, &envelope) != nil {
			events <- agent.Event{
				Timestamp: time.Now(),
				Type:      agent.EventTypeOutput,
				Output:    &agent.OutputEvent{Raw: json.RawMessage(line)},
			}
			continue
		}

		now := time.Now()
		switch envelope.Type {
		case "system":
			var data struct {
				Message string `json:"message"`
			}
			json.Unmarshal(line, &data)
			events <- agent.Event{Timestamp: now, Type: agent.EventTypeSystem, System: &agent.SystemEvent{Subtype: envelope.Subtype, Message: data.Message}}
		case "assistant":
			switch envelope.Subtype {
			case "text":
				var data struct {
					Text string `json:"text"`
				}
				json.Unmarshal(line, &data)
				events <- agent.Event{Timestamp: now, Type: agent.EventTypeResponse, Response: &agent.ResponseEvent{Content: data.Text}}
			case "tool_use":
				var data struct {
					ID    string          `json:"tool_use_id"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				}
				json.Unmarshal(line, &data)
				events <- agent.Event{Timestamp: now, Type: agent.EventTypeToolCall, ToolCall: &agent.ToolCallEvent{ID: data.ID, Name: data.Name, Input: data.Input}}
			}
		case "tool":
			var data struct {
				ID      string `json:"tool_use_id"`
				Content string `json:"content"`
				IsError bool   `json:"is_error"`
			}
			json.Unmarshal(line, &data)
			events <- agent.Event{Timestamp: now, Type: agent.EventTypeToolResult, ToolResult: &agent.ToolResultEvent{ID: data.ID, Output: data.Content, IsError: data.IsError}}
		case "result":
			var data struct {
				CostUSD      float64 `json:"cost_usd"`
				InputTokens  int64   `json:"input_tokens"`
				OutputTokens int64   `json:"output_tokens"`
				TurnCount    int64   `json:"num_turns"`
			}
			json.Unmarshal(line, &data)
			events <- agent.Event{Timestamp: now, Type: agent.EventTypeMetric, Metric: &agent.MetricEvent{InputTokens: data.InputTokens, OutputTokens: data.OutputTokens, CostUSD: data.CostUSD, TurnCount: data.TurnCount}}
		default:
			events <- agent.Event{Timestamp: now, Type: agent.EventTypeOutput, Output: &agent.OutputEvent{Raw: json.RawMessage(append([]byte(nil), line...))}}
		}
	}
	return scanner.Err()
}

func (driver *mockDriver) Interrupt(process agent.Process) error {
	return process.Signal(os.Interrupt)
}
