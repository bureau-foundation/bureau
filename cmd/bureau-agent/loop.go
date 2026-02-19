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

	"github.com/bureau-foundation/bureau/lib/llm"
	llmcontext "github.com/bureau-foundation/bureau/lib/llm/context"
	"github.com/bureau-foundation/bureau/lib/toolserver"
)

// providerType identifies the LLM provider for tool search strategy
// selection. Different providers support different mechanisms for
// managing large tool catalogs.
type providerType string

const (
	providerAnthropic providerType = "anthropic"
	providerOpenAI    providerType = "openai"
)

// Tool search thresholds. When the estimated token cost of tool
// definitions exceeds these thresholds, the agent loop enables
// provider-appropriate tool search to reduce per-request overhead.
const (
	// toolSearchTokenThreshold is the minimum estimated token cost
	// before tool search is considered. 10K tokens is roughly 25-30
	// tools with schemas — below this, sending all tools inline is
	// cheaper than the search overhead.
	toolSearchTokenThreshold = 10000

	// toolSearchMinCount is the minimum number of tools before
	// tool search is considered. Even if schemas are verbose,
	// fewer than 15 tools are easily handled by the model without
	// search assistance.
	toolSearchMinCount = 15
)

// anthropicToolSearchBeta is the Anthropic beta header value that
// enables server-side tool search with defer_loading.
const anthropicToolSearchBeta = "advanced-tool-use-2025-04-15"

// agentLoopConfig holds the dependencies for the agent loop.
type agentLoopConfig struct {
	// provider is the LLM backend (Anthropic, OpenAI-compat, etc.).
	provider llm.Provider

	// providerType identifies the provider for tool search strategy
	// selection. Anthropic uses server-side defer_loading; other
	// providers use progressive disclosure meta-tools.
	providerType providerType

	// tools provides tool discovery and execution. In production this
	// is the MCP server backed by the CLI command tree; in tests it
	// can be any implementation of the interface.
	tools toolserver.Server

	// contextManager controls how conversation history is stored,
	// windowed, and prepared for LLM requests. Different implementations
	// provide different strategies: unbounded (no management), truncating
	// (drop oldest turns), or future strategies like summarization.
	contextManager llmcontext.Manager

	// model is the LLM model identifier (e.g., "claude-sonnet-4-5-20250929").
	model string

	// systemPrompt is the Bureau system prompt content (identity, grants,
	// services, payload).
	systemPrompt string

	// maxTokens is the maximum output tokens per LLM call.
	maxTokens int

	// extraHeaders are provider-specific HTTP headers added to every
	// LLM request. For Anthropic, this carries the beta header that
	// enables tool search. Set by the tool search strategy selection.
	extraHeaders map[string]string

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

	// Build tool definitions from the MCP server's authorized tool catalog,
	// then select a tool search strategy if the catalog is large enough
	// to benefit from on-demand discovery.
	toolDefinitions := selectToolStrategy(config)

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
	if initialPrompt != "" {
		config.contextManager.Append(llm.UserMessage(initialPrompt))
	} else {
		config.logger.Info("no initial prompt, waiting for first message")
		if !waitForMessage(ctx, scanner) {
			config.logger.Info("stdin closed before first message, agent loop ending")
			return nil
		}
		firstMessage := scanner.Text()
		config.logger.Info("first message received", "length", len(firstMessage))
		encoder.Encode(loopEvent{Type: "prompt", Content: firstMessage, Source: "injected"})
		config.contextManager.Append(llm.UserMessage(firstMessage))
	}

	// totalAppended tracks total messages added to the context manager.
	// Comparing this with len(messages) from Messages() detects when
	// truncation has occurred, without requiring type assertions on the
	// Manager implementation.
	var totalAppended int64 = 1
	var totalTurns int64

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		totalTurns++

		// Get the windowed conversation from the context manager.
		// The manager may truncate older turn groups to fit within
		// the token budget.
		messages, contextErr := config.contextManager.Messages(ctx)
		if contextErr != nil {
			config.logger.Warn("context budget exceeded after truncation",
				"error", contextErr,
				"appended", totalAppended,
				"windowed", len(messages),
			)
			encoder.Encode(loopEvent{
				Type:    "system",
				Subtype: "context_truncated",
				Message: contextErr.Error(),
			})
		} else if int64(len(messages)) < totalAppended {
			config.logger.Info("context truncated to fit budget",
				"appended", totalAppended,
				"windowed", len(messages),
				"evicted_messages", totalAppended-int64(len(messages)),
			)
			encoder.Encode(loopEvent{
				Type:    "system",
				Subtype: "context_truncated",
				Message: fmt.Sprintf("evicted %d messages to fit context budget (%d appended, %d sent)",
					totalAppended-int64(len(messages)), totalAppended, len(messages)),
			})
		}

		config.logger.Info("starting turn", "turn", totalTurns, "messages", len(messages))

		// Call the LLM with the windowed conversation and tool catalog.
		response, err := callLLM(ctx, config, messages, toolDefinitions)
		if err != nil {
			encoder.Encode(loopEvent{Type: "error", Message: err.Error()})
			return fmt.Errorf("LLM call failed on turn %d: %w", totalTurns, err)
		}

		// Feed actual token usage back to the context manager for
		// estimator calibration.
		config.contextManager.RecordUsage(response.Usage)

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
		config.contextManager.Append(llm.Message{
			Role:    llm.RoleAssistant,
			Content: response.Content,
		})
		totalAppended++

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

			config.contextManager.Append(llm.UserMessage(injectedMessage))
			totalAppended++
			continue
		}

		// Execute each tool call and collect results.
		config.logger.Info("executing tool calls", "count", len(toolUses))
		results := executeToolCalls(ctx, config.tools, toolUses, encoder, config.logger)

		// Append tool results as a user message and loop back for
		// the LLM to process them.
		config.contextManager.Append(llm.ToolResultMessage(results...))
		totalAppended++
	}
}

// callLLM sends a streaming request to the LLM and returns the
// accumulated response. Text deltas are not surfaced as individual
// events — only the complete response matters for the session log.
func callLLM(ctx context.Context, config *agentLoopConfig, messages []llm.Message, tools []llm.ToolDefinition) (*llm.Response, error) {
	request := llm.Request{
		Model:        config.model,
		System:       config.systemPrompt,
		Messages:     messages,
		Tools:        tools,
		MaxTokens:    config.maxTokens,
		ExtraHeaders: config.extraHeaders,
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
func executeToolCalls(ctx context.Context, tools toolserver.Server, toolUses []llm.ToolUse, encoder *json.Encoder, logger *slog.Logger) []llm.ToolResult {
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

// toolCatalog holds the full tool catalog with deferral metadata.
// Built once from the MCP server's authorized tools.
type toolCatalog struct {
	definitions []llm.ToolDefinition
	deferrable  []bool
}

// buildToolCatalog converts the MCP server's authorized tool catalog
// into llm.ToolDefinition values with deferral metadata.
func buildToolCatalog(tools toolserver.Server) toolCatalog {
	exports := tools.AuthorizedTools()
	catalog := toolCatalog{
		definitions: make([]llm.ToolDefinition, len(exports)),
		deferrable:  make([]bool, len(exports)),
	}
	for i, export := range exports {
		catalog.definitions[i] = llm.ToolDefinition{
			Name:        export.Name,
			Description: export.Description,
			InputSchema: export.InputSchema,
		}
		catalog.deferrable[i] = export.Deferrable
	}
	return catalog
}

// selectToolStrategy builds the tool catalog and applies the
// appropriate tool search strategy based on catalog size and
// provider type. Returns the tool definitions to send to the LLM.
//
// For small catalogs (below thresholds), all tools are sent inline.
// For large catalogs:
//   - Anthropic: tools are marked with defer_loading for server-side
//     search, and the beta header is set on the config.
//   - Other providers: tools are replaced with 3 progressive
//     disclosure meta-tools that the model uses to discover and
//     invoke tools on demand.
func selectToolStrategy(config *agentLoopConfig) []llm.ToolDefinition {
	catalog := buildToolCatalog(config.tools)
	estimatedTokens := estimateToolTokens(catalog.definitions)

	if estimatedTokens > toolSearchTokenThreshold && len(catalog.definitions) > toolSearchMinCount {
		switch config.providerType {
		case providerAnthropic:
			definitions := applyAnthropicToolSearch(catalog)
			config.extraHeaders = map[string]string{
				"anthropic-beta": anthropicToolSearchBeta,
			}

			deferredCount := 0
			for _, d := range catalog.deferrable {
				if d {
					deferredCount++
				}
			}
			config.logger.Info("tool search enabled (Anthropic defer_loading)",
				"tool_count", len(catalog.definitions),
				"deferred_count", deferredCount,
				"estimated_tool_tokens", estimatedTokens,
				"model", config.model,
			)
			return definitions

		default:
			definitions := buildProgressiveToolDefinitions(config.tools)
			config.logger.Info("tool search enabled (progressive disclosure)",
				"tool_count", len(catalog.definitions),
				"meta_tool_count", len(definitions),
				"estimated_tool_tokens", estimatedTokens,
				"model", config.model,
			)
			return definitions
		}
	}

	config.logger.Info("tool catalog built",
		"tool_count", len(catalog.definitions),
		"estimated_tool_tokens", estimatedTokens,
		"model", config.model,
	)
	return catalog.definitions
}

// estimateToolTokens estimates the total token cost of sending all
// tool definitions in an LLM request. Uses a character-count / 4
// heuristic, which is rough but sufficient for threshold comparison.
func estimateToolTokens(tools []llm.ToolDefinition) int {
	var totalCharacters int
	for _, tool := range tools {
		totalCharacters += len(tool.Name) + len(tool.Description) + len(tool.InputSchema)
	}
	return totalCharacters / 4
}

// applyAnthropicToolSearch marks deferrable tools with DeferLoading
// and appends the Anthropic server-side search tool. Non-deferrable
// tools (read-only query tools like list, show, search, status) stay
// always-visible to ensure the model can discover what's available
// without searching.
func applyAnthropicToolSearch(catalog toolCatalog) []llm.ToolDefinition {
	definitions := make([]llm.ToolDefinition, 0, len(catalog.definitions)+1)
	for i, definition := range catalog.definitions {
		if catalog.deferrable[i] {
			definition.DeferLoading = true
		}
		definitions = append(definitions, definition)
	}
	// Append the Anthropic server-side search tool. The provider
	// manages this tool internally — no description or schema needed.
	definitions = append(definitions, llm.ToolDefinition{
		Type: "tool_search_tool_bm25_20251119",
		Name: "tool_search",
	})
	return definitions
}

// buildProgressiveToolDefinitions returns LLM tool definitions for
// the 3 progressive disclosure meta-tools (bureau_tools_list,
// bureau_tools_describe, bureau_tools_call). Used for non-Anthropic
// providers where server-side tool search is unavailable.
func buildProgressiveToolDefinitions(tools toolserver.Server) []llm.ToolDefinition {
	metaTools := tools.MetaToolDefinitions()
	definitions := make([]llm.ToolDefinition, len(metaTools))
	for i, meta := range metaTools {
		definitions[i] = llm.ToolDefinition{
			Name:        meta.Name,
			Description: meta.Description,
			InputSchema: meta.InputSchema,
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
