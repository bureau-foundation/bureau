// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/mcp"
	"github.com/bureau-foundation/bureau/lib/agentdriver"
	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestParseLoopEvent_Response(t *testing.T) {
	t.Parallel()

	line := []byte(`{"type":"response","content":"Hello, I can help with that."}`)
	event, err := parseLoopEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agentdriver.EventTypeResponse {
		t.Errorf("Type = %q, want %q", event.Type, agentdriver.EventTypeResponse)
	}
	if event.Response == nil {
		t.Fatal("Response is nil")
	}
	if event.Response.Content != "Hello, I can help with that." {
		t.Errorf("Content = %q, want %q", event.Response.Content, "Hello, I can help with that.")
	}
}

func TestParseLoopEvent_ToolCall(t *testing.T) {
	t.Parallel()

	line := []byte(`{"type":"tool_call","id":"tc_01","name":"bureau_ticket_list","input":{"room":"!abc:test"}}`)
	event, err := parseLoopEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agentdriver.EventTypeToolCall {
		t.Errorf("Type = %q, want %q", event.Type, agentdriver.EventTypeToolCall)
	}
	if event.ToolCall == nil {
		t.Fatal("ToolCall is nil")
	}
	if event.ToolCall.ID != "tc_01" {
		t.Errorf("ID = %q, want %q", event.ToolCall.ID, "tc_01")
	}
	if event.ToolCall.Name != "bureau_ticket_list" {
		t.Errorf("Name = %q, want %q", event.ToolCall.Name, "bureau_ticket_list")
	}
	if string(event.ToolCall.Input) != `{"room":"!abc:test"}` {
		t.Errorf("Input = %s, want %s", event.ToolCall.Input, `{"room":"!abc:test"}`)
	}
}

func TestParseLoopEvent_ToolResult(t *testing.T) {
	t.Parallel()

	line := []byte(`{"type":"tool_result","id":"tc_01","output":"[{\"title\":\"Fix bug\"}]","is_error":false}`)
	event, err := parseLoopEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agentdriver.EventTypeToolResult {
		t.Errorf("Type = %q, want %q", event.Type, agentdriver.EventTypeToolResult)
	}
	if event.ToolResult == nil {
		t.Fatal("ToolResult is nil")
	}
	if event.ToolResult.ID != "tc_01" {
		t.Errorf("ID = %q, want %q", event.ToolResult.ID, "tc_01")
	}
	if event.ToolResult.IsError {
		t.Error("IsError = true, want false")
	}
}

func TestParseLoopEvent_ToolResultError(t *testing.T) {
	t.Parallel()

	line := []byte(`{"type":"tool_result","id":"tc_02","output":"not found","is_error":true}`)
	event, err := parseLoopEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agentdriver.EventTypeToolResult {
		t.Errorf("Type = %q, want %q", event.Type, agentdriver.EventTypeToolResult)
	}
	if !event.ToolResult.IsError {
		t.Error("IsError = false, want true")
	}
	if event.ToolResult.Output != "not found" {
		t.Errorf("Output = %q, want %q", event.ToolResult.Output, "not found")
	}
}

func TestParseLoopEvent_Prompt(t *testing.T) {
	t.Parallel()

	line := []byte(`{"type":"prompt","content":"please check ticket #5","source":"injected"}`)
	event, err := parseLoopEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agentdriver.EventTypePrompt {
		t.Errorf("Type = %q, want %q", event.Type, agentdriver.EventTypePrompt)
	}
	if event.Prompt.Content != "please check ticket #5" {
		t.Errorf("Content = %q", event.Prompt.Content)
	}
	if event.Prompt.Source != "injected" {
		t.Errorf("Source = %q, want %q", event.Prompt.Source, "injected")
	}
}

func TestParseLoopEvent_Metric(t *testing.T) {
	t.Parallel()

	line := []byte(`{"type":"metric","input_tokens":1500,"output_tokens":200,"cache_read_tokens":500,"turn_count":1}`)
	event, err := parseLoopEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agentdriver.EventTypeMetric {
		t.Errorf("Type = %q, want %q", event.Type, agentdriver.EventTypeMetric)
	}
	if event.Metric == nil {
		t.Fatal("Metric is nil")
	}
	if event.Metric.InputTokens != 1500 {
		t.Errorf("InputTokens = %d, want 1500", event.Metric.InputTokens)
	}
	if event.Metric.OutputTokens != 200 {
		t.Errorf("OutputTokens = %d, want 200", event.Metric.OutputTokens)
	}
	if event.Metric.CacheReadTokens != 500 {
		t.Errorf("CacheReadTokens = %d, want 500", event.Metric.CacheReadTokens)
	}
	if event.Metric.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", event.Metric.TurnCount)
	}
}

func TestParseLoopEvent_Error(t *testing.T) {
	t.Parallel()

	line := []byte(`{"type":"error","message":"rate limited by provider"}`)
	event, err := parseLoopEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agentdriver.EventTypeError {
		t.Errorf("Type = %q, want %q", event.Type, agentdriver.EventTypeError)
	}
	if event.Error == nil {
		t.Fatal("Error is nil")
	}
	if event.Error.Message != "rate limited by provider" {
		t.Errorf("Message = %q", event.Error.Message)
	}
}

func TestParseLoopEvent_System(t *testing.T) {
	t.Parallel()

	line := []byte(`{"type":"system","subtype":"init","message":"bureau-agent starting"}`)
	event, err := parseLoopEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agentdriver.EventTypeSystem {
		t.Errorf("Type = %q, want %q", event.Type, agentdriver.EventTypeSystem)
	}
	if event.System.Subtype != "init" {
		t.Errorf("Subtype = %q, want %q", event.System.Subtype, "init")
	}
}

func TestParseLoopEvent_Thinking(t *testing.T) {
	t.Parallel()

	line := []byte(`{"type":"thinking","thinking_content":"Let me analyze this...","thinking_signature":"sig_abc123"}`)
	event, err := parseLoopEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agentdriver.EventTypeThinking {
		t.Errorf("Type = %q, want %q", event.Type, agentdriver.EventTypeThinking)
	}
	if event.Thinking == nil {
		t.Fatal("Thinking is nil")
	}
	if event.Thinking.Content != "Let me analyze this..." {
		t.Errorf("Content = %q", event.Thinking.Content)
	}
	if event.Thinking.Signature != "sig_abc123" {
		t.Errorf("Signature = %q, want sig_abc123", event.Thinking.Signature)
	}
}

func TestParseLoopEvent_MetricAllFields(t *testing.T) {
	t.Parallel()

	line := []byte(`{"type":"metric","input_tokens":1500,"output_tokens":200,"cache_read_tokens":500,"cache_write_tokens":100,"turn_count":1,"duration_seconds":2.5,"status":"success"}`)
	event, err := parseLoopEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agentdriver.EventTypeMetric {
		t.Errorf("Type = %q, want %q", event.Type, agentdriver.EventTypeMetric)
	}
	if event.Metric == nil {
		t.Fatal("Metric is nil")
	}
	if event.Metric.InputTokens != 1500 {
		t.Errorf("InputTokens = %d, want 1500", event.Metric.InputTokens)
	}
	if event.Metric.OutputTokens != 200 {
		t.Errorf("OutputTokens = %d, want 200", event.Metric.OutputTokens)
	}
	if event.Metric.CacheReadTokens != 500 {
		t.Errorf("CacheReadTokens = %d, want 500", event.Metric.CacheReadTokens)
	}
	if event.Metric.CacheWriteTokens != 100 {
		t.Errorf("CacheWriteTokens = %d, want 100", event.Metric.CacheWriteTokens)
	}
	if event.Metric.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", event.Metric.TurnCount)
	}
	if event.Metric.DurationSeconds != 2.5 {
		t.Errorf("DurationSeconds = %f, want 2.5", event.Metric.DurationSeconds)
	}
	if event.Metric.Status != "success" {
		t.Errorf("Status = %q, want success", event.Metric.Status)
	}
}

func TestParseLoopEvent_ToolCallServerTool(t *testing.T) {
	t.Parallel()

	line := []byte(`{"type":"tool_call","id":"srvtoolu_01","name":"tool_search","input":{"query":"weather"},"server_tool":true}`)
	event, err := parseLoopEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agentdriver.EventTypeToolCall {
		t.Errorf("Type = %q, want %q", event.Type, agentdriver.EventTypeToolCall)
	}
	if event.ToolCall == nil {
		t.Fatal("ToolCall is nil")
	}
	if !event.ToolCall.ServerTool {
		t.Error("ServerTool = false, want true")
	}
	if event.ToolCall.Name != "tool_search" {
		t.Errorf("Name = %q, want tool_search", event.ToolCall.Name)
	}
}

func TestParseLoopEvent_SystemMetadata(t *testing.T) {
	t.Parallel()

	line := []byte(`{"type":"system","subtype":"init","message":"bureau-agent starting","metadata":{"model":"claude-sonnet-4-5-20250929","tool_count":15}}`)
	event, err := parseLoopEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agentdriver.EventTypeSystem {
		t.Errorf("Type = %q, want %q", event.Type, agentdriver.EventTypeSystem)
	}
	if event.System == nil {
		t.Fatal("System is nil")
	}
	if event.System.Subtype != "init" {
		t.Errorf("Subtype = %q, want init", event.System.Subtype)
	}
	if event.System.Metadata == nil {
		t.Fatal("Metadata is nil")
	}
	// Verify metadata is valid JSON with expected fields.
	var metadata map[string]any
	if err := json.Unmarshal(event.System.Metadata, &metadata); err != nil {
		t.Fatalf("Metadata is not valid JSON: %v", err)
	}
	if metadata["model"] != "claude-sonnet-4-5-20250929" {
		t.Errorf("metadata.model = %v", metadata["model"])
	}
}

func TestParseLoopEvent_UnknownType(t *testing.T) {
	t.Parallel()

	line := []byte(`{"type":"unknown_future_event","data":"something"}`)
	event, err := parseLoopEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agentdriver.EventTypeOutput {
		t.Errorf("Type = %q, want %q", event.Type, agentdriver.EventTypeOutput)
	}
	if event.Output == nil {
		t.Fatal("Output is nil")
	}
}

func TestParseLoopEvent_InvalidJSON(t *testing.T) {
	t.Parallel()

	line := []byte(`not json at all`)
	_, err := parseLoopEvent(line)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestBuildToolCatalog(t *testing.T) {
	t.Parallel()

	// Create a minimal CLI command tree with a tool.
	server := testCommandTree()

	// Build tool catalog using the same code path as the agent.
	catalog := buildToolCatalog(server)

	// The wildcard grant should authorize the test tool.
	if len(catalog.definitions) == 0 {
		t.Fatal("expected at least one tool definition")
	}

	// Find our test tool.
	var found bool
	for i, definition := range catalog.definitions {
		if definition.Name == "test_echo" {
			found = true
			if definition.Description == "" {
				t.Error("Description is empty")
			}
			// Verify InputSchema is valid JSON.
			var schemaMap map[string]any
			if err := json.Unmarshal(definition.InputSchema, &schemaMap); err != nil {
				t.Errorf("InputSchema is not valid JSON: %v", err)
			}
			// Verify deferrable metadata is present.
			if len(catalog.deferrable) <= i {
				t.Error("deferrable slice too short")
			}
			break
		}
	}
	if !found {
		t.Errorf("test_echo tool not found in catalog; got %d tools", len(catalog.definitions))
	}
}

func TestEstimateOverheadTokens(t *testing.T) {
	t.Parallel()

	server := testCommandTree()

	// With system prompt and tools, overhead should exceed the floor.
	systemPrompt := "You are a Bureau agent. You have access to tools for managing infrastructure."
	overhead := estimateOverheadTokens(systemPrompt, server)
	if overhead < overheadFloorTokens {
		t.Errorf("overhead = %d, want >= %d (floor)", overhead, overheadFloorTokens)
	}
	// The estimate should account for the system prompt.
	if overhead <= len(systemPrompt)/4 {
		t.Errorf("overhead = %d, should be > system prompt alone (%d tokens)", overhead, len(systemPrompt)/4)
	}
}

func TestEstimateOverheadTokens_EmptyPromptReturnsFloor(t *testing.T) {
	t.Parallel()

	// Even with no grants (no authorized tools) and no system prompt,
	// the floor should apply for protocol framing.
	root := &cli.Command{Name: "empty"}
	grants := []schema.Grant{}
	server := mcp.NewServer(root, grants)

	overhead := estimateOverheadTokens("", server)
	if overhead != overheadFloorTokens {
		t.Errorf("overhead = %d, want %d (floor)", overhead, overheadFloorTokens)
	}
}

// testCommandTree creates a minimal command tree with a single
// authorized tool for testing tool definition building.
func testCommandTree() *mcp.Server {
	type echoParams struct {
		Message string `json:"message" desc:"message to echo" required:"true"`
	}

	var params echoParams
	root := &cli.Command{
		Name: "test",
		Subcommands: []*cli.Command{
			{
				Name:        "echo",
				Summary:     "Echo a message",
				Description: "Returns the input message unchanged.",
				Params:      func() any { return &params },
				Run: func(_ context.Context, args []string, _ *slog.Logger) error {
					fmt.Println(params.Message)
					return nil
				},
				RequiredGrants: []string{"command/test/echo"},
			},
		},
	}

	// Wildcard grant authorizes all tools.
	grants := []schema.Grant{
		{Actions: []string{"command/**"}},
	}

	return mcp.NewServer(root, grants)
}
