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
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/commands"
	"github.com/bureau-foundation/bureau/cmd/bureau/mcp"
	"github.com/bureau-foundation/bureau/lib/agent"
	"github.com/bureau-foundation/bureau/lib/llm"
	llmcontext "github.com/bureau-foundation/bureau/lib/llm/context"
	"github.com/bureau-foundation/bureau/lib/proxyclient"
)

// nativeDriver implements agent.Driver for the Bureau-native agent.
// Instead of spawning an external process, it runs the LLM agent loop
// in a goroutine. The agent loop communicates with the lifecycle
// manager (lib/agent.Run) through the same pipes that external agent
// drivers use: stdin for message injection, stdout for structured events.
type nativeDriver struct {
	proxySocketPath string
	serverName      string
	logger          *slog.Logger
}

// Start initializes the LLM provider and MCP tool server, then spawns
// the agent loop goroutine. Returns a process handle and a reader for
// structured event output.
func (driver *nativeDriver) Start(ctx context.Context, config agent.DriverConfig) (agent.Process, io.ReadCloser, error) {
	// Create a proxy client for LLM API calls and grant fetching.
	proxy := proxyclient.New(driver.proxySocketPath, driver.serverName)

	// Fetch grants for tool authorization filtering.
	grants, err := proxy.Grants(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching grants from proxy: %w", err)
	}

	// Build the CLI command tree and create the MCP server for
	// tool discovery and execution.
	root := commands.Root()
	mcpServer := mcp.NewServer(root, grants)

	// Read agent configuration from environment.
	model := os.Getenv("BUREAU_AGENT_MODEL")
	if model == "" {
		model = "claude-sonnet-4-5-20250929"
	}

	service := os.Getenv("BUREAU_AGENT_SERVICE")
	if service == "" {
		service = "anthropic"
	}

	maxTokens := 8192
	if maxTokensEnv := os.Getenv("BUREAU_AGENT_MAX_TOKENS"); maxTokensEnv != "" {
		parsed, parseErr := strconv.Atoi(maxTokensEnv)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("parsing BUREAU_AGENT_MAX_TOKENS=%q: %w", maxTokensEnv, parseErr)
		}
		maxTokens = parsed
	}

	// Compute the context window from the model registry with an
	// optional override. The context window determines how much
	// conversation history the agent can maintain before truncation.
	contextWindow := llmcontext.ContextWindowForModel(model)
	if contextWindowEnv := os.Getenv("BUREAU_AGENT_CONTEXT_WINDOW"); contextWindowEnv != "" {
		parsed, parseErr := strconv.Atoi(contextWindowEnv)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("parsing BUREAU_AGENT_CONTEXT_WINDOW=%q: %w", contextWindowEnv, parseErr)
		}
		contextWindow = parsed
	}

	// Read provider selection from environment.
	providerName := os.Getenv("BUREAU_AGENT_PROVIDER")
	if providerName == "" {
		providerName = "anthropic"
	}

	// Create LLM provider through the proxy HTTP passthrough.
	var provider llm.Provider
	var selectedProviderType providerType
	switch providerName {
	case "anthropic":
		provider = llm.NewAnthropic(proxy.HTTPClient(), service)
		selectedProviderType = providerAnthropic
	case "openai":
		provider = llm.NewOpenAI(proxy.HTTPClient(), service)
		selectedProviderType = providerOpenAI
	default:
		return nil, nil, fmt.Errorf("unknown LLM provider %q (supported: anthropic, openai)", providerName)
	}

	// Read system prompt from the temp file written by agent.Run.
	var systemPrompt string
	if config.SystemPromptFile != "" {
		data, readErr := os.ReadFile(config.SystemPromptFile)
		if readErr != nil {
			return nil, nil, fmt.Errorf("reading system prompt from %s: %w", config.SystemPromptFile, readErr)
		}
		systemPrompt = string(data)
	}

	// Build the token budget and context manager. The overhead
	// estimate covers system prompt, tool definitions, and protocol
	// framing — everything the provider charges per request beyond
	// the conversation messages. This uses the full tool catalog
	// (before tool search may reduce the per-request set), which
	// makes the estimate conservative: a larger overhead means a
	// smaller message budget, so truncation fires earlier rather
	// than risking context overflow on the first turn.
	overheadTokens := estimateOverheadTokens(systemPrompt, mcpServer)
	budget := llmcontext.Budget{
		ContextWindow:   contextWindow,
		MaxOutputTokens: maxTokens,
		OverheadTokens:  overheadTokens,
	}
	messageBudget := budget.MessageTokenBudget()
	if messageBudget <= 0 {
		return nil, nil, fmt.Errorf(
			"context budget exhausted: context window %d tokens, max output %d tokens, "+
				"overhead %d tokens, leaving %d tokens for messages (need > 0)",
			contextWindow, maxTokens, overheadTokens, messageBudget)
	}

	estimator := llmcontext.NewCharEstimator()
	contextManager := llmcontext.NewTruncating(messageBudget, estimator)

	driver.logger.Info("context management configured",
		"model", model,
		"context_window", contextWindow,
		"max_output_tokens", maxTokens,
		"overhead_tokens", overheadTokens,
		"message_budget", messageBudget,
	)

	// Create pipes for communication between the agent loop and
	// the lifecycle manager. The loop writes structured JSON events
	// to stdoutWriter; ParseOutput reads from stdoutReader. The
	// message pump writes to stdinWriter; the loop reads from
	// stdinReader.
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	// Create a cancellable context for the loop goroutine.
	loopCtx, cancelLoop := context.WithCancel(ctx)

	process := &nativeProcess{
		done:     make(chan struct{}),
		stdin:    stdinWriter,
		cancelFn: cancelLoop,
	}

	// Start the agent loop in a background goroutine.
	go func() {
		defer close(process.done)
		defer stdoutWriter.Close()

		loopErr := runAgentLoop(loopCtx, &agentLoopConfig{
			provider:       provider,
			providerType:   selectedProviderType,
			tools:          mcpServer,
			contextManager: contextManager,
			model:          model,
			systemPrompt:   systemPrompt,
			maxTokens:      maxTokens,
			stdin:          stdinReader,
			stdout:         stdoutWriter,
			logger:         driver.logger,
		}, config.Prompt)

		process.mutex.Lock()
		process.exitError = loopErr
		process.mutex.Unlock()
	}()

	return process, stdoutReader, nil
}

// ParseOutput reads structured JSON events from the agent loop's stdout
// and converts them to agent.Event values for the session log.
func (driver *nativeDriver) ParseOutput(ctx context.Context, stdout io.Reader, events chan<- agent.Event) error {
	scanner := bufio.NewScanner(stdout)
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

		event, err := parseLoopEvent(line)
		if err != nil {
			events <- agent.Event{
				Timestamp: time.Now(),
				Type:      agent.EventTypeOutput,
				Output:    &agent.OutputEvent{Raw: json.RawMessage(append([]byte(nil), line...))},
			}
			continue
		}

		events <- event
	}

	return scanner.Err()
}

// Interrupt requests the agent loop to stop by cancelling its context.
func (driver *nativeDriver) Interrupt(process agent.Process) error {
	if nativeProc, ok := process.(*nativeProcess); ok {
		nativeProc.cancelFn()
		return nil
	}
	return fmt.Errorf("unexpected process type: %T", process)
}

// nativeProcess implements agent.Process for the in-process agent loop.
type nativeProcess struct {
	done      chan struct{}
	stdin     *io.PipeWriter
	cancelFn  context.CancelFunc
	mutex     sync.Mutex
	exitError error
}

func (process *nativeProcess) Wait() error {
	<-process.done
	process.mutex.Lock()
	defer process.mutex.Unlock()
	return process.exitError
}

func (process *nativeProcess) Stdin() io.Writer {
	return process.stdin
}

func (process *nativeProcess) Signal(signal os.Signal) error {
	// For the native agent, any signal triggers graceful shutdown
	// via context cancellation.
	process.cancelFn()
	return nil
}

// overheadFloorTokens is the minimum overhead estimate. Even with no
// system prompt and zero tools, the protocol framing (JSON structure,
// role markers, content block wrappers) consumes some tokens.
const overheadFloorTokens = 512

// estimateOverheadTokens estimates the fixed per-request token cost
// of the system prompt and tool definitions. Uses the same chars/4
// heuristic as estimateToolTokens. The estimate includes all
// authorized tools regardless of tool search strategy, so it's an
// upper bound — deferred tools don't contribute to per-request cost
// but are counted here. This is the safe direction: the
// CharEstimator calibration corrects the effective budget after the
// first turn.
func estimateOverheadTokens(systemPrompt string, tools *mcp.Server) int {
	characters := len(systemPrompt)
	for _, export := range tools.AuthorizedTools() {
		characters += len(export.Name) + len(export.Description) + len(export.InputSchema)
	}
	// Protocol framing: JSON structure around each message, role
	// markers, content block type wrappers, tool use/result envelopes.
	characters += 2048
	estimate := characters / 4
	if estimate < overheadFloorTokens {
		return overheadFloorTokens
	}
	return estimate
}

// loopEvent is the JSON format written by the agent loop to stdout.
// ParseOutput reads these lines and converts them to agent.Event.
type loopEvent struct {
	Type string `json:"type"`

	// For "response" and "prompt".
	Content string `json:"content,omitempty"`

	// For "prompt".
	Source string `json:"source,omitempty"`

	// For "tool_call" and "tool_result".
	ID      string          `json:"id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Output  string          `json:"output,omitempty"`
	IsError bool            `json:"is_error,omitempty"`

	// For "metric".
	InputTokens      int64 `json:"input_tokens,omitempty"`
	OutputTokens     int64 `json:"output_tokens,omitempty"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	TurnCount        int64 `json:"turn_count,omitempty"`

	// For "error" and "system".
	Message string `json:"message,omitempty"`

	// For "system".
	Subtype string `json:"subtype,omitempty"`
}

// parseLoopEvent converts a JSON line from the agent loop into an
// agent.Event for session logging.
func parseLoopEvent(line []byte) (agent.Event, error) {
	var event loopEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return agent.Event{}, fmt.Errorf("parsing loop event: %w", err)
	}

	now := time.Now()

	switch event.Type {
	case "response":
		return agent.Event{
			Timestamp: now,
			Type:      agent.EventTypeResponse,
			Response:  &agent.ResponseEvent{Content: event.Content},
		}, nil

	case "tool_call":
		return agent.Event{
			Timestamp: now,
			Type:      agent.EventTypeToolCall,
			ToolCall: &agent.ToolCallEvent{
				ID:    event.ID,
				Name:  event.Name,
				Input: event.Input,
			},
		}, nil

	case "tool_result":
		return agent.Event{
			Timestamp: now,
			Type:      agent.EventTypeToolResult,
			ToolResult: &agent.ToolResultEvent{
				ID:      event.ID,
				IsError: event.IsError,
				Output:  event.Output,
			},
		}, nil

	case "prompt":
		return agent.Event{
			Timestamp: now,
			Type:      agent.EventTypePrompt,
			Prompt: &agent.PromptEvent{
				Content: event.Content,
				Source:  event.Source,
			},
		}, nil

	case "metric":
		return agent.Event{
			Timestamp: now,
			Type:      agent.EventTypeMetric,
			Metric: &agent.MetricEvent{
				InputTokens:     event.InputTokens,
				OutputTokens:    event.OutputTokens,
				CacheReadTokens: event.CacheReadTokens,
				TurnCount:       event.TurnCount,
			},
		}, nil

	case "error":
		return agent.Event{
			Timestamp: now,
			Type:      agent.EventTypeError,
			Error:     &agent.ErrorEvent{Message: event.Message},
		}, nil

	case "system":
		return agent.Event{
			Timestamp: now,
			Type:      agent.EventTypeSystem,
			System: &agent.SystemEvent{
				Subtype: event.Subtype,
				Message: event.Message,
			},
		}, nil

	default:
		return agent.Event{
			Timestamp: now,
			Type:      agent.EventTypeOutput,
			Output:    &agent.OutputEvent{Raw: json.RawMessage(append([]byte(nil), line...))},
		}, nil
	}
}
