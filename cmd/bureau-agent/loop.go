// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/bureau-foundation/bureau/cmd/bureau/mcp"
	"github.com/bureau-foundation/bureau/lib/llm"
)

// agentLoopConfig holds the dependencies for the agent loop.
type agentLoopConfig struct {
	// provider is the LLM backend (Anthropic, OpenAI-compat, etc.).
	provider llm.Provider

	// tools is the MCP server providing tool discovery and execution.
	tools *mcp.Server

	// model is the LLM model identifier (e.g., "claude-sonnet-4-5-20250929").
	model string

	// systemPrompt is the Bureau system prompt content (identity, grants,
	// services, payload).
	systemPrompt string

	// maxTokens is the maximum output tokens per LLM call.
	maxTokens int

	// stdin is the read end of the message injection pipe. The lifecycle
	// manager's message pump writes incoming Matrix messages here.
	stdin io.Reader

	// stdout is the write end of the event pipe. Structured events are
	// written here as JSON lines for ParseOutput to consume.
	stdout io.Writer

	logger *slog.Logger
}

// runAgentLoop runs the core request→think→act→observe cycle. It sends
// the initial prompt to the LLM, processes tool calls, and waits for
// new messages from Matrix between conversational turns.
//
// The loop continues until the context is cancelled, stdin reaches EOF
// (message pump shut down), or an unrecoverable error occurs.
func runAgentLoop(ctx context.Context, config *agentLoopConfig, initialPrompt string) error {
	encoder := json.NewEncoder(config.stdout)
	encoder.SetEscapeHTML(false)
	scanner := bufio.NewScanner(config.stdin)

	// Build tool definitions from the MCP server's authorized tool catalog.
	toolDefinitions := buildToolDefinitions(config.tools)
	config.logger.Info("tool catalog built",
		"tool_count", len(toolDefinitions),
		"model", config.model,
	)

	// Emit init system event.
	encoder.Encode(loopEvent{
		Type:    "system",
		Subtype: "init",
		Message: fmt.Sprintf("bureau-agent starting with model %s, %d tools", config.model, len(toolDefinitions)),
	})

	// Initialize the conversation. If no prompt was configured in the
	// payload, wait for the first message from Matrix instead of making
	// a wasted LLM call. This also ensures the first LLM call only
	// happens after the test (or production caller) has had a chance to
	// register any required services on the proxy.
	var messages []llm.Message
	if initialPrompt != "" {
		messages = []llm.Message{llm.UserMessage(initialPrompt)}
	} else {
		config.logger.Info("no initial prompt, waiting for first message")
		if !waitForMessage(ctx, scanner) {
			config.logger.Info("stdin closed before first message, agent loop ending")
			return nil
		}
		firstMessage := scanner.Text()
		config.logger.Info("first message received", "length", len(firstMessage))
		encoder.Encode(loopEvent{Type: "prompt", Content: firstMessage, Source: "injected"})
		messages = []llm.Message{llm.UserMessage(firstMessage)}
	}

	var totalTurns int64

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		totalTurns++
		config.logger.Info("starting turn", "turn", totalTurns, "messages", len(messages))

		// Call the LLM with the current conversation and tool catalog.
		response, err := callLLM(ctx, config, messages, toolDefinitions)
		if err != nil {
			encoder.Encode(loopEvent{Type: "error", Message: err.Error()})
			return fmt.Errorf("LLM call failed on turn %d: %w", totalTurns, err)
		}

		// Emit per-turn metrics.
		encoder.Encode(loopEvent{
			Type:             "metric",
			InputTokens:      response.Usage.InputTokens,
			OutputTokens:     response.Usage.OutputTokens,
			CacheReadTokens:  response.Usage.CacheReadTokens,
			CacheWriteTokens: response.Usage.CacheWriteTokens,
			TurnCount:        1,
		})

		// Append the assistant's response to the conversation.
		messages = append(messages, llm.Message{
			Role:    llm.RoleAssistant,
			Content: response.Content,
		})

		// Check whether the response contains tool calls.
		toolUses := response.ToolUses()
		if len(toolUses) == 0 {
			// Text-only response: emit it and wait for the next message.
			text := response.TextContent()
			config.logger.Info("text response", "length", len(text), "stop_reason", response.StopReason)
			encoder.Encode(loopEvent{Type: "response", Content: text})

			// Wait for the next injected message from Matrix.
			if !waitForMessage(ctx, scanner) {
				config.logger.Info("stdin closed, agent loop ending")
				return nil
			}

			injectedMessage := scanner.Text()
			config.logger.Info("message injected", "length", len(injectedMessage))
			encoder.Encode(loopEvent{Type: "prompt", Content: injectedMessage, Source: "injected"})

			messages = append(messages, llm.UserMessage(injectedMessage))
			continue
		}

		// Execute each tool call and collect results.
		config.logger.Info("executing tool calls", "count", len(toolUses))
		results := executeToolCalls(ctx, config.tools, toolUses, encoder, config.logger)

		// Append tool results as a user message and loop back for
		// the LLM to process them.
		messages = append(messages, llm.ToolResultMessage(results...))
	}
}

// callLLM sends a streaming request to the LLM and returns the
// accumulated response. Text deltas are not surfaced as individual
// events — only the complete response matters for the session log.
func callLLM(ctx context.Context, config *agentLoopConfig, messages []llm.Message, tools []llm.ToolDefinition) (*llm.Response, error) {
	request := llm.Request{
		Model:     config.model,
		System:    config.systemPrompt,
		Messages:  messages,
		Tools:     tools,
		MaxTokens: config.maxTokens,
	}

	stream, err := config.provider.Stream(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("starting stream: %w", err)
	}
	defer stream.Close()

	// Drain the stream. We don't emit per-delta events — the session
	// log records the complete response and individual tool calls.
	for {
		_, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading stream: %w", err)
		}
	}

	response := stream.Response()
	return &response, nil
}

// executeToolCalls runs each tool call through the MCP server's
// in-process execution pipeline and returns the results for the LLM.
func executeToolCalls(ctx context.Context, tools *mcp.Server, toolUses []llm.ToolUse, encoder *json.Encoder, logger *slog.Logger) []llm.ToolResult {
	results := make([]llm.ToolResult, 0, len(toolUses))

	for _, toolUse := range toolUses {
		if ctx.Err() != nil {
			results = append(results, llm.ToolResult{
				ToolUseID: toolUse.ID,
				Content:   "execution cancelled",
				IsError:   true,
			})
			continue
		}

		// Emit tool call event.
		encoder.Encode(loopEvent{
			Type:  "tool_call",
			ID:    toolUse.ID,
			Name:  toolUse.Name,
			Input: toolUse.Input,
		})

		logger.Info("executing tool", "name", toolUse.Name, "id", toolUse.ID)

		// Execute through the MCP server's dispatch pipeline:
		// param zeroing → default application → JSON overlay → stdout capture.
		output, isError, err := tools.CallTool(toolUse.Name, toolUse.Input)
		if err != nil {
			// Infrastructure error: unknown tool or authorization denied.
			output = err.Error()
			isError = true
			logger.Warn("tool infrastructure error", "name", toolUse.Name, "error", err)
		} else if isError {
			logger.Info("tool returned error", "name", toolUse.Name)
		} else {
			logger.Info("tool completed", "name", toolUse.Name, "output_length", len(output))
		}

		// Emit tool result event.
		encoder.Encode(loopEvent{
			Type:    "tool_result",
			ID:      toolUse.ID,
			Name:    toolUse.Name,
			Output:  output,
			IsError: isError,
		})

		results = append(results, llm.ToolResult{
			ToolUseID: toolUse.ID,
			Content:   output,
			IsError:   isError,
		})
	}

	return results
}

// buildToolDefinitions converts the MCP server's authorized tool catalog
// into llm.ToolDefinition values for the LLM request.
func buildToolDefinitions(tools *mcp.Server) []llm.ToolDefinition {
	exports := tools.AuthorizedTools()
	definitions := make([]llm.ToolDefinition, len(exports))
	for i, export := range exports {
		definitions[i] = llm.ToolDefinition{
			Name:        export.Name,
			Description: export.Description,
			InputSchema: export.InputSchema,
		}
	}
	return definitions
}

// waitForMessage blocks until a line is available on the scanner or the
// context is cancelled. Returns true if a line was read (available via
// scanner.Text()), false if the context was cancelled or stdin closed.
func waitForMessage(ctx context.Context, scanner *bufio.Scanner) bool {
	// The scanner blocks on I/O, so we run it in a goroutine to
	// make it cancellable via context.
	type scanResult struct {
		ok bool
	}
	channel := make(chan scanResult, 1)
	go func() {
		channel <- scanResult{ok: scanner.Scan()}
	}()

	select {
	case <-ctx.Done():
		return false
	case result := <-channel:
		return result.ok
	}
}
