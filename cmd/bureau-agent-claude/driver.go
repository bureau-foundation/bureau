// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/agent"
)

// claudeDriver implements agent.Driver for Claude Code.
type claudeDriver struct{}

// claudeProcess wraps an exec.Cmd to implement agent.Process.
type claudeProcess struct {
	command *exec.Cmd
	stdin   io.WriteCloser
}

func (process *claudeProcess) Wait() error {
	return process.command.Wait()
}

func (process *claudeProcess) Stdin() io.Writer {
	return process.stdin
}

func (process *claudeProcess) Signal(signal os.Signal) error {
	if process.command.Process == nil {
		return fmt.Errorf("process not started")
	}
	return process.command.Process.Signal(signal)
}

// Start spawns a Claude Code process with stream-json output.
func (driver *claudeDriver) Start(ctx context.Context, config agent.DriverConfig) (agent.Process, io.ReadCloser, error) {
	binaryPath := os.Getenv("CLAUDE_BINARY")
	if binaryPath == "" {
		binaryPath = "claude"
	}

	arguments := []string{
		"--output-format", "stream-json",
		"--print",
		"--verbose",
	}
	if config.SystemPromptFile != "" {
		arguments = append(arguments, "--append-system-prompt-file", config.SystemPromptFile)
	}
	// Initial prompt as positional argument.
	arguments = append(arguments, config.Prompt)

	command := exec.CommandContext(ctx, binaryPath, arguments...)
	command.Dir = config.WorkingDirectory
	command.Stderr = os.Stderr
	command.Env = append(os.Environ(), config.ExtraEnv...)

	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("creating stdin pipe: %w", err)
	}

	stdout, err := command.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	if err := command.Start(); err != nil {
		stdin.Close()
		return nil, nil, fmt.Errorf("starting claude: %w", err)
	}

	process := &claudeProcess{
		command: command,
		stdin:   stdin,
	}

	return process, stdout, nil
}

// ParseOutput reads Claude Code's stream-json stdout line by line and
// emits structured events. Each line is a JSON object with a "type" field.
//
// Claude Code stream-json event types:
//   - {"type":"system","subtype":"init",...} → EventTypeSystem
//   - {"type":"assistant","subtype":"text",...} → EventTypeResponse
//   - {"type":"assistant","subtype":"tool_use",...} → EventTypeToolCall
//   - {"type":"tool","subtype":"result",...} → EventTypeToolResult
//   - {"type":"result","subtype":"success",...} → EventTypeMetric
//   - Unknown types → EventTypeOutput (raw JSON preserved)
func (driver *claudeDriver) ParseOutput(ctx context.Context, stdout io.Reader, events chan<- agent.Event) error {
	scanner := bufio.NewScanner(stdout)
	// Claude Code can produce long lines (tool results with large file contents).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		event, err := parseStreamJSONLine(line)
		if err != nil {
			// Malformed line — log as output event with raw content.
			events <- agent.Event{
				Timestamp: time.Now(),
				Type:      agent.EventTypeOutput,
				Output:    &agent.OutputEvent{Raw: json.RawMessage(line)},
			}
			continue
		}

		events <- event
	}

	return scanner.Err()
}

// Interrupt sends SIGINT to Claude Code, which finishes the current tool
// call and exits gracefully.
func (driver *claudeDriver) Interrupt(process agent.Process) error {
	return process.Signal(syscall.SIGINT)
}

// streamJSONEvent is the common envelope for Claude Code stream-json output.
type streamJSONEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
}

// parseStreamJSONLine parses a single line of Claude Code stream-json output
// into a structured agent.Event.
func parseStreamJSONLine(line []byte) (agent.Event, error) {
	var envelope streamJSONEvent
	if err := json.Unmarshal(line, &envelope); err != nil {
		return agent.Event{}, fmt.Errorf("parsing stream-json envelope: %w", err)
	}

	now := time.Now()

	switch envelope.Type {
	case "system":
		return agent.Event{
			Timestamp: now,
			Type:      agent.EventTypeSystem,
			System: &agent.SystemEvent{
				Subtype: envelope.Subtype,
				Message: extractStringField(line, "message"),
			},
		}, nil

	case "assistant":
		return parseAssistantEvent(now, envelope.Subtype, line)

	case "tool":
		return parseToolEvent(now, envelope.Subtype, line)

	case "result":
		return parseResultEvent(now, line)

	default:
		// Unknown event type — preserve as raw output.
		return agent.Event{
			Timestamp: now,
			Type:      agent.EventTypeOutput,
			Output:    &agent.OutputEvent{Raw: json.RawMessage(append([]byte(nil), line...))},
		}, nil
	}
}

// parseAssistantEvent handles {"type":"assistant",...} events.
func parseAssistantEvent(timestamp time.Time, subtype string, line []byte) (agent.Event, error) {
	switch subtype {
	case "text":
		return agent.Event{
			Timestamp: timestamp,
			Type:      agent.EventTypeResponse,
			Response: &agent.ResponseEvent{
				Content: extractStringField(line, "text"),
			},
		}, nil

	case "tool_use":
		var toolUse struct {
			ID    string          `json:"tool_use_id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		json.Unmarshal(line, &toolUse)
		return agent.Event{
			Timestamp: timestamp,
			Type:      agent.EventTypeToolCall,
			ToolCall: &agent.ToolCallEvent{
				ID:    toolUse.ID,
				Name:  toolUse.Name,
				Input: toolUse.Input,
			},
		}, nil

	default:
		return agent.Event{
			Timestamp: timestamp,
			Type:      agent.EventTypeOutput,
			Output:    &agent.OutputEvent{Raw: json.RawMessage(append([]byte(nil), line...))},
		}, nil
	}
}

// parseToolEvent handles {"type":"tool",...} events.
func parseToolEvent(timestamp time.Time, subtype string, line []byte) (agent.Event, error) {
	switch subtype {
	case "result":
		var toolResult struct {
			ToolUseID string `json:"tool_use_id"`
			IsError   bool   `json:"is_error"`
			Content   string `json:"content"`
		}
		json.Unmarshal(line, &toolResult)
		return agent.Event{
			Timestamp: timestamp,
			Type:      agent.EventTypeToolResult,
			ToolResult: &agent.ToolResultEvent{
				ID:      toolResult.ToolUseID,
				IsError: toolResult.IsError,
				Output:  toolResult.Content,
			},
		}, nil

	default:
		return agent.Event{
			Timestamp: timestamp,
			Type:      agent.EventTypeOutput,
			Output:    &agent.OutputEvent{Raw: json.RawMessage(append([]byte(nil), line...))},
		}, nil
	}
}

// parseResultEvent handles {"type":"result",...} events, extracting metrics.
func parseResultEvent(timestamp time.Time, line []byte) (agent.Event, error) {
	var result struct {
		CostUSD          float64 `json:"cost_usd"`
		InputTokens      int64   `json:"input_tokens"`
		OutputTokens     int64   `json:"output_tokens"`
		CacheReadTokens  int64   `json:"cache_read_input_tokens"`
		CacheWriteTokens int64   `json:"cache_creation_input_tokens"`
		DurationSeconds  float64 `json:"duration_seconds"`
		DurationMS       float64 `json:"duration_ms"`
		TurnCount        int64   `json:"num_turns"`
	}
	json.Unmarshal(line, &result)

	durationSeconds := result.DurationSeconds
	if durationSeconds == 0 && result.DurationMS > 0 {
		durationSeconds = result.DurationMS / 1000.0
	}

	return agent.Event{
		Timestamp: timestamp,
		Type:      agent.EventTypeMetric,
		Metric: &agent.MetricEvent{
			InputTokens:     result.InputTokens,
			OutputTokens:    result.OutputTokens,
			CacheReadTokens: result.CacheReadTokens,
			CostUSD:         result.CostUSD,
			DurationSeconds: durationSeconds,
			TurnCount:       result.TurnCount,
		},
	}, nil
}

// extractStringField extracts a string field from a JSON object without
// full deserialization. Falls back to empty string on any error.
func extractStringField(data []byte, field string) string {
	var parsed map[string]json.RawMessage
	if json.Unmarshal(data, &parsed) != nil {
		return ""
	}
	raw, ok := parsed[field]
	if !ok {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}
