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
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/mcp"
	"github.com/bureau-foundation/bureau/lib/llm"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// mockProvider implements llm.Provider with configurable responses.
type mockProvider struct {
	// responses is the sequence of responses to return, one per
	// Stream/Complete call. After exhausting the list, subsequent
	// calls return the last response.
	responses []*llm.Response
	callCount int
}

func (provider *mockProvider) Complete(ctx context.Context, request llm.Request) (*llm.Response, error) {
	return provider.nextResponse(), nil
}

func (provider *mockProvider) Stream(ctx context.Context, request llm.Request) (*llm.EventStream, error) {
	response := provider.nextResponse()

	// Build the sequence of stream events that reproduce this response.
	var events []llm.StreamEvent
	for _, block := range response.Content {
		events = append(events, llm.StreamEvent{
			Type:         llm.EventContentBlockDone,
			ContentBlock: block,
		})
	}

	index := 0
	stream := llm.NewEventStream(func() (llm.StreamEvent, error) {
		if index >= len(events) {
			return llm.StreamEvent{}, io.EOF
		}
		event := events[index]
		index++
		return event, nil
	}, io.NopCloser(strings.NewReader("")))

	stream.SetModel(response.Model)
	stream.SetUsage(response.Usage)
	stream.SetStopReason(response.StopReason)

	return stream, nil
}

func (provider *mockProvider) nextResponse() *llm.Response {
	if provider.callCount < len(provider.responses) {
		response := provider.responses[provider.callCount]
		provider.callCount++
		return response
	}
	return provider.responses[len(provider.responses)-1]
}

// TestAgentLoop_TextResponse verifies that the agent loop emits a
// response event when the LLM returns a text-only response, then
// waits for the next stdin message.
func TestAgentLoop_TextResponse(t *testing.T) {
	t.Parallel()

	provider := &mockProvider{
		responses: []*llm.Response{
			{
				Content: []llm.ContentBlock{
					llm.TextBlock("Hello! I'm ready to help."),
				},
				StopReason: llm.StopReasonEndTurn,
				Usage:      llm.Usage{InputTokens: 100, OutputTokens: 20},
				Model:      "mock-model",
			},
		},
	}

	root := &cli.Command{Name: "test"}
	toolServer := mcp.NewServer(root, []schema.Grant{
		{Actions: []string{"command/**"}},
	})

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loopDone := make(chan error, 1)
	go func() {
		loopDone <- runAgentLoop(ctx, &agentLoopConfig{
			provider:     provider,
			tools:        toolServer,
			model:        "mock-model",
			systemPrompt: "You are a test agent.",
			maxTokens:    1024,
			stdin:        stdinReader,
			stdout:       stdoutWriter,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		}, "Hello, agent!")
	}()

	scanner := bufio.NewScanner(stdoutReader)

	// First event: "system" (init).
	if !scanner.Scan() {
		t.Fatal("expected system event, got EOF")
	}
	var systemEvent loopEvent
	if err := json.Unmarshal(scanner.Bytes(), &systemEvent); err != nil {
		t.Fatalf("parsing system event: %v", err)
	}
	if systemEvent.Type != "system" {
		t.Errorf("first event type = %q, want %q", systemEvent.Type, "system")
	}

	// Second event: "metric" (turn usage).
	if !scanner.Scan() {
		t.Fatal("expected metric event, got EOF")
	}
	var metricEvent loopEvent
	if err := json.Unmarshal(scanner.Bytes(), &metricEvent); err != nil {
		t.Fatalf("parsing metric event: %v", err)
	}
	if metricEvent.Type != "metric" {
		t.Errorf("second event type = %q, want %q", metricEvent.Type, "metric")
	}
	if metricEvent.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", metricEvent.InputTokens)
	}
	if metricEvent.OutputTokens != 20 {
		t.Errorf("OutputTokens = %d, want 20", metricEvent.OutputTokens)
	}

	// Third event: "response" (text output).
	if !scanner.Scan() {
		t.Fatal("expected response event, got EOF")
	}
	var responseEvent loopEvent
	if err := json.Unmarshal(scanner.Bytes(), &responseEvent); err != nil {
		t.Fatalf("parsing response event: %v", err)
	}
	if responseEvent.Type != "response" {
		t.Errorf("third event type = %q, want %q", responseEvent.Type, "response")
	}
	if responseEvent.Content != "Hello! I'm ready to help." {
		t.Errorf("response content = %q", responseEvent.Content)
	}

	// The loop is now blocked waiting for stdin. Close stdin to end it.
	stdinWriter.Close()

	err := <-loopDone
	if err != nil {
		t.Errorf("loop returned error: %v", err)
	}
}

// TestAgentLoop_ToolCallThenText verifies the tool call flow: the LLM
// requests a tool call, the agent executes it, then the LLM responds
// with text incorporating the tool result.
func TestAgentLoop_ToolCallThenText(t *testing.T) {
	t.Parallel()

	provider := &mockProvider{
		responses: []*llm.Response{
			// Turn 1: tool call.
			{
				Content: []llm.ContentBlock{
					llm.ToolUseBlock("tc_01", "test_echo", json.RawMessage(`{"message":"hello"}`)),
				},
				StopReason: llm.StopReasonToolUse,
				Usage:      llm.Usage{InputTokens: 150, OutputTokens: 30},
				Model:      "mock-model",
			},
			// Turn 2: text response after seeing tool result.
			{
				Content: []llm.ContentBlock{
					llm.TextBlock("The echo returned: hello"),
				},
				StopReason: llm.StopReasonEndTurn,
				Usage:      llm.Usage{InputTokens: 200, OutputTokens: 15},
				Model:      "mock-model",
			},
		},
	}

	root := testEchoCommandTree()
	toolServer := mcp.NewServer(root, []schema.Grant{
		{Actions: []string{"command/**"}},
	})

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loopDone := make(chan error, 1)
	go func() {
		loopDone <- runAgentLoop(ctx, &agentLoopConfig{
			provider:     provider,
			tools:        toolServer,
			model:        "mock-model",
			systemPrompt: "You are a test agent.",
			maxTokens:    1024,
			stdin:        stdinReader,
			stdout:       stdoutWriter,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		}, "Echo hello for me")
	}()

	scanner := bufio.NewScanner(stdoutReader)

	// Collect events until we see the final "response".
	var events []loopEvent
	for scanner.Scan() {
		var event loopEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("parsing event: %v", err)
		}
		events = append(events, event)
		if event.Type == "response" {
			break
		}
	}

	// Expected event sequence:
	// system, metric (turn 1), tool_call, tool_result, metric (turn 2), response
	expectedTypes := []string{"system", "metric", "tool_call", "tool_result", "metric", "response"}
	if len(events) != len(expectedTypes) {
		types := make([]string, len(events))
		for i, event := range events {
			types[i] = event.Type
		}
		t.Fatalf("got %d events %v, want %d %v", len(events), types, len(expectedTypes), expectedTypes)
	}
	for i, expected := range expectedTypes {
		if events[i].Type != expected {
			t.Errorf("event[%d].Type = %q, want %q", i, events[i].Type, expected)
		}
	}

	// Verify tool call details.
	toolCallEvent := events[2]
	if toolCallEvent.Name != "test_echo" {
		t.Errorf("tool_call Name = %q, want %q", toolCallEvent.Name, "test_echo")
	}
	if toolCallEvent.ID != "tc_01" {
		t.Errorf("tool_call ID = %q, want %q", toolCallEvent.ID, "tc_01")
	}

	// Verify tool result contains the echoed message.
	toolResultEvent := events[3]
	if toolResultEvent.IsError {
		t.Errorf("tool_result IsError = true, output: %q", toolResultEvent.Output)
	}
	if !strings.Contains(toolResultEvent.Output, "hello") {
		t.Errorf("tool_result Output = %q, should contain 'hello'", toolResultEvent.Output)
	}

	// Verify final text response.
	responseEvent := events[5]
	if responseEvent.Content != "The echo returned: hello" {
		t.Errorf("response Content = %q", responseEvent.Content)
	}

	// Close stdin to end the loop.
	stdinWriter.Close()

	err := <-loopDone
	if err != nil {
		t.Errorf("loop returned error: %v", err)
	}
}

// TestAgentLoop_MessageInjection verifies that injected messages from
// stdin become new conversation turns.
func TestAgentLoop_MessageInjection(t *testing.T) {
	t.Parallel()

	provider := &mockProvider{
		responses: []*llm.Response{
			// Response to initial prompt.
			{
				Content:    []llm.ContentBlock{llm.TextBlock("Ready.")},
				StopReason: llm.StopReasonEndTurn,
				Usage:      llm.Usage{InputTokens: 50, OutputTokens: 5},
			},
			// Response to injected message.
			{
				Content:    []llm.ContentBlock{llm.TextBlock("Got your message!")},
				StopReason: llm.StopReasonEndTurn,
				Usage:      llm.Usage{InputTokens: 80, OutputTokens: 10},
			},
		},
	}

	root := &cli.Command{Name: "test"}
	toolServer := mcp.NewServer(root, nil)

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loopDone := make(chan error, 1)
	go func() {
		loopDone <- runAgentLoop(ctx, &agentLoopConfig{
			provider:     provider,
			tools:        toolServer,
			model:        "mock-model",
			systemPrompt: "",
			maxTokens:    1024,
			stdin:        stdinReader,
			stdout:       stdoutWriter,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		}, "Hello")
	}()

	scanner := bufio.NewScanner(stdoutReader)

	// Read events until first "response".
	for scanner.Scan() {
		var event loopEvent
		json.Unmarshal(scanner.Bytes(), &event)
		if event.Type == "response" && event.Content == "Ready." {
			break
		}
	}

	// Inject a message via stdin.
	fmt.Fprintln(stdinWriter, "Follow-up message")

	// Read events until we see the prompt and second response.
	var sawPrompt, sawSecondResponse bool
	for scanner.Scan() {
		var event loopEvent
		json.Unmarshal(scanner.Bytes(), &event)
		if event.Type == "prompt" && event.Content == "Follow-up message" && event.Source == "injected" {
			sawPrompt = true
		}
		if event.Type == "response" && event.Content == "Got your message!" {
			sawSecondResponse = true
			break
		}
	}

	if !sawPrompt {
		t.Error("did not see 'prompt' event for injected message")
	}
	if !sawSecondResponse {
		t.Error("did not see second 'response' event")
	}

	// Verify the provider was called twice.
	if provider.callCount != 2 {
		t.Errorf("provider called %d times, want 2", provider.callCount)
	}

	stdinWriter.Close()
	err := <-loopDone
	if err != nil {
		t.Errorf("loop returned error: %v", err)
	}
}

// TestAgentLoop_ContextCancellation verifies that the loop exits
// cleanly when the context is cancelled.
func TestAgentLoop_ContextCancellation(t *testing.T) {
	t.Parallel()

	provider := &mockProvider{
		responses: []*llm.Response{
			{
				Content:    []llm.ContentBlock{llm.TextBlock("response")},
				StopReason: llm.StopReasonEndTurn,
				Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
			},
		},
	}

	root := &cli.Command{Name: "test"}
	toolServer := mcp.NewServer(root, nil)

	stdinReader, _ := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())

	loopDone := make(chan error, 1)
	go func() {
		loopDone <- runAgentLoop(ctx, &agentLoopConfig{
			provider:     provider,
			tools:        toolServer,
			model:        "mock-model",
			systemPrompt: "",
			maxTokens:    1024,
			stdin:        stdinReader,
			stdout:       io.Discard,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		}, "test")
	}()

	// Cancel context — the loop should exit. The loop will process the
	// mock response (writing events to io.Discard), then block in
	// waitForMessage. The select in waitForMessage picks up ctx.Done()
	// and returns false, ending the loop.
	cancel()

	err := <-loopDone
	if err != nil && err != context.Canceled {
		t.Errorf("loop returned unexpected error: %v", err)
	}
}

// TestAgentLoop_EmptyPromptWaitsForMessage verifies that when the
// initial prompt is empty, the loop does not call the LLM until a
// message arrives on stdin. This is the production behavior for agents
// with no configured task prompt — they wait for a Matrix message.
func TestAgentLoop_EmptyPromptWaitsForMessage(t *testing.T) {
	t.Parallel()

	provider := &mockProvider{
		responses: []*llm.Response{
			{
				Content:    []llm.ContentBlock{llm.TextBlock("Got it!")},
				StopReason: llm.StopReasonEndTurn,
				Usage:      llm.Usage{InputTokens: 50, OutputTokens: 10},
				Model:      "mock-model",
			},
		},
	}

	root := &cli.Command{Name: "test"}
	toolServer := mcp.NewServer(root, nil)

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loopDone := make(chan error, 1)
	go func() {
		loopDone <- runAgentLoop(ctx, &agentLoopConfig{
			provider:     provider,
			tools:        toolServer,
			model:        "mock-model",
			systemPrompt: "",
			maxTokens:    1024,
			stdin:        stdinReader,
			stdout:       stdoutWriter,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		}, "") // Empty initial prompt.
	}()

	scanner := bufio.NewScanner(stdoutReader)

	// First event: "system" (init). The loop emits this immediately.
	if !scanner.Scan() {
		t.Fatal("expected system event, got EOF")
	}
	var systemEvent loopEvent
	if err := json.Unmarshal(scanner.Bytes(), &systemEvent); err != nil {
		t.Fatalf("parsing system event: %v", err)
	}
	if systemEvent.Type != "system" {
		t.Errorf("first event type = %q, want %q", systemEvent.Type, "system")
	}

	// At this point the loop should be blocked in waitForMessage —
	// it should NOT have called the LLM.
	if provider.callCount != 0 {
		t.Fatalf("LLM called %d times before any message was sent, want 0", provider.callCount)
	}

	// Inject a message via stdin.
	fmt.Fprintln(stdinWriter, "Hello from Matrix")

	// Read events: expect prompt, metric, response.
	var sawPrompt, sawResponse bool
	for scanner.Scan() {
		var event loopEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("parsing event: %v", err)
		}
		if event.Type == "prompt" && event.Content == "Hello from Matrix" && event.Source == "injected" {
			sawPrompt = true
		}
		if event.Type == "response" && event.Content == "Got it!" {
			sawResponse = true
			break
		}
	}

	if !sawPrompt {
		t.Error("did not see 'prompt' event for injected message")
	}
	if !sawResponse {
		t.Error("did not see 'response' event")
	}
	if provider.callCount != 1 {
		t.Errorf("LLM called %d times, want 1", provider.callCount)
	}

	stdinWriter.Close()
	err := <-loopDone
	if err != nil {
		t.Errorf("loop returned error: %v", err)
	}
}

// TestAgentLoop_EmptyPromptStdinClosed verifies that when the initial
// prompt is empty and stdin is closed before any message arrives, the
// loop exits cleanly without calling the LLM.
func TestAgentLoop_EmptyPromptStdinClosed(t *testing.T) {
	t.Parallel()

	provider := &mockProvider{
		responses: []*llm.Response{
			{
				Content:    []llm.ContentBlock{llm.TextBlock("should not be called")},
				StopReason: llm.StopReasonEndTurn,
				Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
			},
		},
	}

	root := &cli.Command{Name: "test"}
	toolServer := mcp.NewServer(root, nil)

	stdinReader, stdinWriter := io.Pipe()

	loopDone := make(chan error, 1)
	go func() {
		loopDone <- runAgentLoop(context.Background(), &agentLoopConfig{
			provider:     provider,
			tools:        toolServer,
			model:        "mock-model",
			systemPrompt: "",
			maxTokens:    1024,
			stdin:        stdinReader,
			stdout:       io.Discard,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		}, "") // Empty initial prompt.
	}()

	// Close stdin immediately — loop should exit without calling LLM.
	stdinWriter.Close()

	err := <-loopDone
	if err != nil {
		t.Errorf("loop returned error: %v", err)
	}
	if provider.callCount != 0 {
		t.Errorf("LLM called %d times, want 0", provider.callCount)
	}
}

// testEchoCommandTree creates a command tree with a single echo tool.
func testEchoCommandTree() *cli.Command {
	type echoParams struct {
		Message string `json:"message" flag:"message" desc:"message to echo" required:"true"`
	}

	var params echoParams
	return &cli.Command{
		Name: "test",
		Subcommands: []*cli.Command{
			{
				Name:           "echo",
				Summary:        "Echo a message",
				Description:    "Returns the input message unchanged.",
				Annotations:    cli.ReadOnly(),
				Params:         func() any { return &params },
				RequiredGrants: []string{"command/test/echo"},
				Run: func(args []string) error {
					fmt.Println(params.Message)
					return nil
				},
			},
		},
	}
}
