// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/commands"
	"github.com/bureau-foundation/bureau/cmd/bureau/mcp"
	"github.com/bureau-foundation/bureau/lib/agentdriver"
	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/llm"
	llmcontext "github.com/bureau-foundation/bureau/lib/llm/context"
	"github.com/bureau-foundation/bureau/lib/proxyclient"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/toolserver"
)

// Service socket paths for the native agent's direct service access.
// These are the standard paths the daemon bind-mounts when a template
// declares the corresponding RequiredServices entries.
const (
	artifactServiceSocketPath = "/run/bureau/service/artifact.sock"
	artifactServiceTokenPath  = "/run/bureau/service/token/artifact.token"

	// Model service HTTP compatibility socket. The daemon mounts this
	// when the model service exposes an http.sock alongside its CBOR
	// socket. The role suffix "-http" comes from the daemon's automatic
	// HTTP socket detection in resolveServiceMounts.
	modelServiceHTTPSocketPath = "/run/bureau/service/model-http.sock"
	modelServiceTokenPath      = "/run/bureau/service/token/model.token"
)

// tokenCacheTTL is how long the bearer token transport caches a service
// token before re-reading the file. The daemon refreshes service tokens
// on a 5-minute TTL with 80% renewal (4 min). A 30-second cache ensures
// the agent picks up refreshed tokens promptly without re-reading on
// every HTTP request.
const tokenCacheTTL = 30 * time.Second

// nativeDriver implements agentdriver.Driver for the Bureau-native agent.
// Instead of spawning an external process, it runs the LLM agent loop
// in a goroutine. The agent loop communicates with the lifecycle
// manager (lib/agentdriver.Run) through the same pipes that external agent
// drivers use: stdin for message injection, stdout for structured events.
type nativeDriver struct {
	proxySocketPath string
	serverName      ref.ServerName
	logger          *slog.Logger
}

// Start initializes the LLM provider and MCP tool server, then spawns
// the agent loop goroutine. Returns a process handle and a reader for
// structured event output.
func (driver *nativeDriver) Start(ctx context.Context, config agentdriver.DriverConfig) (agentdriver.Process, io.ReadCloser, error) {
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

	// Create LLM provider. The "anthropic" and "openai" providers
	// route through the Bureau proxy HTTP passthrough. The
	// "model-service" provider routes through the model service's
	// HTTP compatibility socket, which handles credential injection,
	// model alias resolution, and quota tracking internally.
	var provider llm.Provider
	var selectedProviderType providerType
	switch providerName {
	case "anthropic":
		provider = llm.NewAnthropic(proxy.HTTPClient(), service)
		selectedProviderType = providerAnthropic
	case "openai":
		provider = llm.NewOpenAI(proxy.HTTPClient(), service)
		selectedProviderType = providerOpenAI
	case "model-service":
		modelClient := newModelServiceHTTPClient(modelServiceHTTPSocketPath, modelServiceTokenPath, clock.Real())

		// The wire format determines which JSON structure is used on the
		// wire. The model service HTTP proxy routes by path prefix
		// (/openai/... or /anthropic/...) to the correct upstream.
		wireFormat := os.Getenv("BUREAU_AGENT_WIRE_FORMAT")
		if wireFormat == "" {
			wireFormat = "openai"
		}
		switch wireFormat {
		case "anthropic":
			provider = llm.NewAnthropicWithEndpoint(modelClient,
				"http://model/anthropic/v1/messages")
			selectedProviderType = providerAnthropic
		case "openai":
			provider = llm.NewOpenAIWithEndpoint(modelClient,
				"http://model/openai/v1/chat/completions")
			selectedProviderType = providerOpenAI
		default:
			return nil, nil, fmt.Errorf("unknown wire format %q for model-service provider (supported: anthropic, openai)", wireFormat)
		}
	default:
		return nil, nil, fmt.Errorf("unknown LLM provider %q (supported: anthropic, openai, model-service)", providerName)
	}

	// Read system prompt from the temp file written by agentdriver.Run.
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

	// Auto-detect artifact and agent service sockets for context
	// checkpointing. The native agent's bureau-agent-v1 checkpoint
	// format stores full []llm.Message snapshots directly in the
	// artifact service, with commit metadata recorded via the agent
	// service. Both must be present for checkpointing to function.
	// If either is missing, checkpointing is disabled (the loop logs
	// once and operates without persistence).
	//
	// Declare as interface types so a nil variable stays truly nil
	// when assigned to the interface fields in agentLoopConfig.
	// Using concrete types (e.g., *artifactstore.Client) would create
	// non-nil interfaces wrapping nil pointers, bypassing the nil
	// check in newCheckpointTracker.
	var checkpointArtifactClient artifactStorer
	if _, statErr := os.Stat(artifactServiceSocketPath); statErr == nil {
		client, clientErr := artifactstore.NewClient(artifactServiceSocketPath, artifactServiceTokenPath)
		if clientErr != nil {
			return nil, nil, fmt.Errorf("creating artifact client: %w", clientErr)
		}
		checkpointArtifactClient = client
		driver.logger.Info("artifact client initialized for checkpointing")
	}

	var checkpointAgentServiceClient contextCheckpointer
	if _, statErr := os.Stat(agentdriver.DefaultAgentServiceSocketPath); statErr == nil {
		client, clientErr := agentdriver.NewAgentServiceClient(
			agentdriver.DefaultAgentServiceSocketPath,
			agentdriver.DefaultAgentServiceTokenPath,
		)
		if clientErr != nil {
			return nil, nil, fmt.Errorf("creating agent service client: %w", clientErr)
		}
		checkpointAgentServiceClient = client
		driver.logger.Info("agent service client initialized for checkpointing")
	}

	agentTemplate := os.Getenv("BUREAU_AGENT_TEMPLATE")

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
			provider:           provider,
			providerType:       selectedProviderType,
			tools:              mcpServer,
			contextManager:     contextManager,
			model:              model,
			systemPrompt:       systemPrompt,
			maxTokens:          maxTokens,
			artifactClient:     checkpointArtifactClient,
			agentServiceClient: checkpointAgentServiceClient,
			sessionID:          config.SessionID,
			template:           agentTemplate,
			stdin:              stdinReader,
			stdout:             stdoutWriter,
			logger:             driver.logger,
		}, config.Prompt)

		process.mutex.Lock()
		process.exitError = loopErr
		process.mutex.Unlock()
	}()

	return process, stdoutReader, nil
}

// ParseOutput reads structured JSON events from the agent loop's stdout
// and converts them to agentdriver.Event values for the session log.
func (driver *nativeDriver) ParseOutput(ctx context.Context, stdout io.Reader, events chan<- agentdriver.Event) error {
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
			events <- agentdriver.Event{
				Timestamp: time.Now(),
				Type:      agentdriver.EventTypeOutput,
				Output:    &agentdriver.OutputEvent{Raw: json.RawMessage(append([]byte(nil), line...))},
			}
			continue
		}

		events <- event
	}

	return scanner.Err()
}

// Interrupt requests the agent loop to stop by cancelling its context.
func (driver *nativeDriver) Interrupt(process agentdriver.Process) error {
	if nativeProc, ok := process.(*nativeProcess); ok {
		nativeProc.cancelFn()
		return nil
	}
	return fmt.Errorf("unexpected process type: %T", process)
}

// nativeProcess implements agentdriver.Process for the in-process agent loop.
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
func estimateOverheadTokens(systemPrompt string, tools toolserver.Server) int {
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
// ParseOutput reads these lines and converts them to agentdriver.Event.
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

	// For "tool_call": true when the tool call originated from
	// the provider's server-side tool system (e.g., Anthropic's
	// tool search), not from the agent loop's own tool execution.
	ServerTool bool `json:"server_tool,omitempty"`

	// For "metric".
	InputTokens      int64   `json:"input_tokens,omitempty"`
	OutputTokens     int64   `json:"output_tokens,omitempty"`
	CacheReadTokens  int64   `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64   `json:"cache_write_tokens,omitempty"`
	TurnCount        int64   `json:"turn_count,omitempty"`
	DurationSeconds  float64 `json:"duration_seconds,omitempty"`
	Status           string  `json:"status,omitempty"`

	// For "error" and "system".
	Message string `json:"message,omitempty"`

	// For "system".
	Subtype  string          `json:"subtype,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`

	// For "thinking".
	ThinkingContent   string `json:"thinking_content,omitempty"`
	ThinkingSignature string `json:"thinking_signature,omitempty"`
}

// parseLoopEvent converts a JSON line from the agent loop into an
// agentdriver.Event for session logging.
func parseLoopEvent(line []byte) (agentdriver.Event, error) {
	var event loopEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return agentdriver.Event{}, fmt.Errorf("parsing loop event: %w", err)
	}

	now := time.Now()

	switch event.Type {
	case "response":
		return agentdriver.Event{
			Timestamp: now,
			Type:      agentdriver.EventTypeResponse,
			Response:  &agentdriver.ResponseEvent{Content: event.Content},
		}, nil

	case "tool_call":
		return agentdriver.Event{
			Timestamp: now,
			Type:      agentdriver.EventTypeToolCall,
			ToolCall: &agentdriver.ToolCallEvent{
				ID:         event.ID,
				Name:       event.Name,
				Input:      event.Input,
				ServerTool: event.ServerTool,
			},
		}, nil

	case "tool_result":
		return agentdriver.Event{
			Timestamp: now,
			Type:      agentdriver.EventTypeToolResult,
			ToolResult: &agentdriver.ToolResultEvent{
				ID:      event.ID,
				IsError: event.IsError,
				Output:  event.Output,
			},
		}, nil

	case "prompt":
		return agentdriver.Event{
			Timestamp: now,
			Type:      agentdriver.EventTypePrompt,
			Prompt: &agentdriver.PromptEvent{
				Content: event.Content,
				Source:  event.Source,
			},
		}, nil

	case "metric":
		return agentdriver.Event{
			Timestamp: now,
			Type:      agentdriver.EventTypeMetric,
			Metric: &agentdriver.MetricEvent{
				InputTokens:      event.InputTokens,
				OutputTokens:     event.OutputTokens,
				CacheReadTokens:  event.CacheReadTokens,
				CacheWriteTokens: event.CacheWriteTokens,
				TurnCount:        event.TurnCount,
				DurationSeconds:  event.DurationSeconds,
				Status:           event.Status,
			},
		}, nil

	case "error":
		return agentdriver.Event{
			Timestamp: now,
			Type:      agentdriver.EventTypeError,
			Error:     &agentdriver.ErrorEvent{Message: event.Message},
		}, nil

	case "system":
		return agentdriver.Event{
			Timestamp: now,
			Type:      agentdriver.EventTypeSystem,
			System: &agentdriver.SystemEvent{
				Subtype:  event.Subtype,
				Message:  event.Message,
				Metadata: event.Metadata,
			},
		}, nil

	case "thinking":
		return agentdriver.Event{
			Timestamp: now,
			Type:      agentdriver.EventTypeThinking,
			Thinking: &agentdriver.ThinkingEvent{
				Content:   event.ThinkingContent,
				Signature: event.ThinkingSignature,
			},
		}, nil

	default:
		return agentdriver.Event{
			Timestamp: now,
			Type:      agentdriver.EventTypeOutput,
			Output:    &agentdriver.OutputEvent{Raw: json.RawMessage(append([]byte(nil), line...))},
		}, nil
	}
}

// newModelServiceHTTPClient creates an HTTP client that dials the model
// service's HTTP compatibility socket and injects a Bearer token on
// every request. The token is read from the daemon-managed token file
// and cached for tokenCacheTTL to avoid re-reading on every request.
func newModelServiceHTTPClient(socketPath, tokenPath string, clock clock.Clock) *http.Client {
	return &http.Client{
		Timeout: 0, // No overall timeout — streaming responses are long-lived.
		Transport: &bearerTokenTransport{
			base: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
			tokenPath: tokenPath,
			clock:     clock,
		},
	}
}

// bearerTokenTransport is an http.RoundTripper that injects a
// base64-encoded Bearer token from a file into every request's
// Authorization header. The token is cached for tokenCacheTTL
// to balance freshness against file read overhead. The daemon
// atomically replaces the token file on refresh, so reads are safe.
type bearerTokenTransport struct {
	base      http.RoundTripper
	tokenPath string
	clock     clock.Clock
	mutex     sync.Mutex
	cached    string
	expiry    time.Time
}

func (transport *bearerTokenTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	token, err := transport.token()
	if err != nil {
		return nil, err
	}
	request = request.Clone(request.Context())
	request.Header.Set("Authorization", "Bearer "+token)
	return transport.base.RoundTrip(request)
}

func (transport *bearerTokenTransport) token() (string, error) {
	transport.mutex.Lock()
	defer transport.mutex.Unlock()

	now := transport.clock.Now()
	if transport.cached != "" && now.Before(transport.expiry) {
		return transport.cached, nil
	}

	raw, err := os.ReadFile(transport.tokenPath)
	if err != nil {
		return "", fmt.Errorf("reading model service token from %s: %w", transport.tokenPath, err)
	}

	transport.cached = base64.StdEncoding.EncodeToString(raw)
	transport.expiry = now.Add(tokenCacheTTL)
	return transport.cached, nil
}
