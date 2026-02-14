// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// testResponse is a JSON-RPC 2.0 response for test assertions. Result
// is kept as raw JSON so each test can unmarshal it into the expected type.
type testResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *testRPCError   `json:"error"`
}

type testRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// testCommandTree returns a command tree for MCP server tests. Each
// call creates fresh parameter variables, so tests are independent.
func testCommandTree() *cli.Command {
	type echoParams struct {
		Message string `json:"message" flag:"message" desc:"message to echo" required:"true"`
	}
	type failParams struct {
		Reason string `json:"reason" flag:"reason" desc:"failure reason" default:"boom"`
	}
	type formatParams struct {
		Value      string `json:"value" flag:"value" desc:"value to print"`
		OutputJSON bool   `json:"-" flag:"json" desc:"output as JSON"`
	}

	var echoP echoParams
	var failP failParams
	var formatP formatParams

	return &cli.Command{
		Name: "test",
		Subcommands: []*cli.Command{
			{
				Name:        "echo",
				Summary:     "Echo a message",
				Description: "Echo the provided message to stdout.",
				Flags:       func() *pflag.FlagSet { return cli.FlagsFromParams("echo", &echoP) },
				Params:      func() any { return &echoP },
				Run: func(args []string) error {
					fmt.Println(echoP.Message)
					return nil
				},
			},
			{
				Name:    "fail",
				Summary: "Always fails with a reason",
				Flags:   func() *pflag.FlagSet { return cli.FlagsFromParams("fail", &failP) },
				Params:  func() any { return &failP },
				Run: func(args []string) error {
					fmt.Print("partial")
					return fmt.Errorf("intentional failure: %s", failP.Reason)
				},
			},
			{
				Name:    "format",
				Summary: "Conditional JSON output",
				Flags:   func() *pflag.FlagSet { return cli.FlagsFromParams("format", &formatP) },
				Params:  func() any { return &formatP },
				Run: func(args []string) error {
					if formatP.OutputJSON {
						fmt.Printf(`{"value":%q}`, formatP.Value)
					} else {
						fmt.Printf("VALUE: %s", formatP.Value)
					}
					return nil
				},
			},
			{
				Name:    "noparams",
				Summary: "Not discoverable as MCP tool",
				Run:     func(args []string) error { return nil },
			},
		},
	}
}

// initMessages returns the initialize request and initialized
// notification that start every MCP session.
func initMessages() []map[string]any {
	return []map[string]any{
		{
			"jsonrpc": "2.0",
			"id":      0,
			"method":  "initialize",
			"params": map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{},
				"clientInfo":      map[string]any{"name": "test", "version": "1.0"},
			},
		},
		{
			"jsonrpc": "2.0",
			"method":  "notifications/initialized",
		},
	}
}

// mcpSession sends a sequence of JSON-RPC messages to a fresh MCP
// server and returns the responses. Notifications produce no response.
func mcpSession(t *testing.T, root *cli.Command, messages ...map[string]any) []testResponse {
	t.Helper()

	var input bytes.Buffer
	for _, msg := range messages {
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal message: %v", err)
		}
		input.Write(data)
		input.WriteByte('\n')
	}

	var output bytes.Buffer
	server := NewServer(root)
	if err := server.Run(&input, &output); err != nil {
		t.Fatalf("server.Run: %v", err)
	}

	var responses []testResponse
	scanner := bufio.NewScanner(&output)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp testResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("unmarshal response: %v\nraw: %s", err, line)
		}
		responses = append(responses, resp)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning output: %v", err)
	}

	return responses
}

func TestNewServer_ToolDiscovery(t *testing.T) {
	root := testCommandTree()
	server := NewServer(root)

	// Should discover: test_echo, test_fail, test_format.
	// Should NOT discover: test_noparams (no Params function).
	if len(server.tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(server.tools))
	}

	expected := []string{"test_echo", "test_fail", "test_format"}
	for i, name := range expected {
		if server.tools[i].name != name {
			t.Errorf("tools[%d].name = %q, want %q", i, server.tools[i].name, name)
		}
	}

	// Verify schemas were generated.
	for _, discovered := range server.tools {
		if discovered.inputSchema == nil {
			t.Errorf("tool %q has nil inputSchema", discovered.name)
		}
	}

	// Verify map lookup works.
	for _, name := range expected {
		if _, ok := server.toolsByName[name]; !ok {
			t.Errorf("toolsByName missing %q", name)
		}
	}
}

func TestServer_Initialize(t *testing.T) {
	root := testCommandTree()
	responses := mcpSession(t, root, initMessages()...)

	// Only the initialize request produces a response.
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}

	resp := responses[0]
	if resp.Error != nil {
		t.Fatalf("unexpected error: code=%d message=%q", resp.Error.Code, resp.Error.Message)
	}

	var result initializeResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if result.ProtocolVersion != protocolVersion {
		t.Errorf("protocolVersion = %q, want %q", result.ProtocolVersion, protocolVersion)
	}
	if result.ServerInfo.Name != "bureau" {
		t.Errorf("serverInfo.name = %q, want %q", result.ServerInfo.Name, "bureau")
	}
	if result.Capabilities.Tools == nil {
		t.Error("capabilities.tools is nil, expected non-nil")
	}
}

func TestServer_InitializeWrongVersion(t *testing.T) {
	root := testCommandTree()
	responses := mcpSession(t, root, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "1999-01-01",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test"},
		},
	})

	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	if responses[0].Error == nil {
		t.Fatal("expected error response for wrong protocol version")
	}
	if !strings.Contains(responses[0].Error.Message, "unsupported protocol version") {
		t.Errorf("error message = %q, want it to contain 'unsupported protocol version'",
			responses[0].Error.Message)
	}
}

func TestServer_Ping(t *testing.T) {
	root := testCommandTree()
	messages := append(initMessages(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "ping",
	})

	responses := mcpSession(t, root, messages...)
	if len(responses) != 2 {
		t.Fatalf("expected 2 responses (init + ping), got %d", len(responses))
	}

	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if string(resp.Result) != "{}" {
		t.Errorf("ping result = %s, want {}", resp.Result)
	}
}

func TestServer_ToolsList(t *testing.T) {
	root := testCommandTree()
	messages := append(initMessages(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})

	responses := mcpSession(t, root, messages...)
	if len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(responses))
	}

	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	var result toolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if len(result.Tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(result.Tools))
	}

	names := make(map[string]bool)
	for _, discovered := range result.Tools {
		names[discovered.Name] = true
	}
	for _, expected := range []string{"test_echo", "test_fail", "test_format"} {
		if !names[expected] {
			t.Errorf("missing tool %q in tools/list", expected)
		}
	}

	// Verify each tool has a non-nil inputSchema.
	for _, discovered := range result.Tools {
		if discovered.InputSchema == nil {
			t.Errorf("tool %q has nil inputSchema", discovered.Name)
		}
	}
}

func TestServer_ToolsCall(t *testing.T) {
	root := testCommandTree()
	messages := append(initMessages(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "test_echo",
			"arguments": map[string]any{"message": "hello world"},
		},
	})

	responses := mcpSession(t, root, messages...)
	if len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(responses))
	}

	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	var result toolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if result.IsError {
		t.Error("isError should be false")
	}
	if len(result.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(result.Content))
	}
	// fmt.Println adds a trailing newline.
	if result.Content[0].Text != "hello world\n" {
		t.Errorf("content text = %q, want %q", result.Content[0].Text, "hello world\n")
	}
}

func TestServer_ToolsCallDefaults(t *testing.T) {
	root := testCommandTree()
	// Call fail with no arguments — Reason should default to "boom"
	// via the Flags() default registration.
	messages := append(initMessages(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "test_fail",
			"arguments": map[string]any{},
		},
	})

	responses := mcpSession(t, root, messages...)
	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %v", resp.Error)
	}

	var result toolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if !result.IsError {
		t.Error("expected isError=true (tool returns error)")
	}

	// Find the error content block.
	var errorText string
	for _, block := range result.Content {
		if strings.Contains(block.Text, "intentional failure") {
			errorText = block.Text
		}
	}
	if errorText == "" {
		t.Fatal("no error content block found")
	}
	// Default "boom" should be applied via Flags() default registration.
	if !strings.Contains(errorText, "boom") {
		t.Errorf("error text = %q, want it to contain 'boom' (default)", errorText)
	}
}

func TestServer_ToolsCallJSONOutput(t *testing.T) {
	root := testCommandTree()
	messages := append(initMessages(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "test_format",
			"arguments": map[string]any{"value": "hello"},
		},
	})

	responses := mcpSession(t, root, messages...)
	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	var result toolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if result.IsError {
		t.Error("expected isError=false")
	}
	if len(result.Content) == 0 {
		t.Fatal("no content blocks")
	}

	// enableJSONOutput should have set OutputJSON=true, producing
	// JSON instead of "VALUE: hello".
	output := result.Content[0].Text
	if strings.Contains(output, "VALUE:") {
		t.Errorf("got table output %q, expected JSON (enableJSONOutput should force JSON mode)", output)
	}
	if !strings.Contains(output, `"value"`) {
		t.Errorf("output %q does not look like JSON", output)
	}
}

func TestServer_ToolsCallError(t *testing.T) {
	root := testCommandTree()
	messages := append(initMessages(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "test_fail",
			"arguments": map[string]any{"reason": "test error"},
		},
	})

	responses := mcpSession(t, root, messages...)
	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %v", resp.Error)
	}

	var result toolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if !result.IsError {
		t.Error("expected isError=true")
	}
	// Should have two content blocks: partial stdout and the error.
	if len(result.Content) < 2 {
		t.Fatalf("expected at least 2 content blocks, got %d", len(result.Content))
	}

	// First block: partial stdout from the command.
	if result.Content[0].Text != "partial" {
		t.Errorf("first content = %q, want %q", result.Content[0].Text, "partial")
	}
	// Second block: the error message.
	if !strings.Contains(result.Content[1].Text, "test error") {
		t.Errorf("error content = %q, want it to contain 'test error'", result.Content[1].Text)
	}
}

func TestServer_ToolsCallUnknownTool(t *testing.T) {
	root := testCommandTree()
	messages := append(initMessages(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "nonexistent_tool",
		},
	})

	responses := mcpSession(t, root, messages...)
	resp := responses[1]
	if resp.Error == nil {
		t.Fatal("expected error for unknown tool")
	}
	if resp.Error.Code != codeInvalidParams {
		t.Errorf("error code = %d, want %d", resp.Error.Code, codeInvalidParams)
	}
	if !strings.Contains(resp.Error.Message, "unknown tool") {
		t.Errorf("error message = %q, want it to contain 'unknown tool'", resp.Error.Message)
	}
}

func TestServer_NotInitialized(t *testing.T) {
	root := testCommandTree()
	// Send tools/call without initializing first.
	responses := mcpSession(t, root, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "test_echo",
			"arguments": map[string]any{"message": "hello"},
		},
	})

	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	if responses[0].Error == nil {
		t.Fatal("expected error for pre-init tools/call")
	}
	if !strings.Contains(responses[0].Error.Message, "not initialized") {
		t.Errorf("error message = %q, want it to contain 'not initialized'",
			responses[0].Error.Message)
	}
}

func TestServer_UnknownMethod(t *testing.T) {
	root := testCommandTree()
	messages := append(initMessages(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "resources/list",
	})

	responses := mcpSession(t, root, messages...)
	resp := responses[1]
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != codeMethodNotFound {
		t.Errorf("error code = %d, want %d", resp.Error.Code, codeMethodNotFound)
	}
}

func TestServer_NotificationIgnored(t *testing.T) {
	root := testCommandTree()
	// Initialize, then send a notification. The notification should
	// produce no response.
	messages := append(initMessages(), map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/progress",
		"params":  map[string]any{"token": "abc"},
	})

	responses := mcpSession(t, root, messages...)
	// Only the initialize request should produce a response.
	if len(responses) != 1 {
		t.Fatalf("expected 1 response (init only), got %d", len(responses))
	}
}

func TestEnableJSONOutput(t *testing.T) {
	type params struct {
		Name       string `json:"name" flag:"name" desc:"name"`
		OutputJSON bool   `json:"-" flag:"json" desc:"JSON output"`
	}

	p := &params{Name: "test"}
	enableJSONOutput(p)
	if !p.OutputJSON {
		t.Error("expected OutputJSON to be true after enableJSONOutput")
	}
}

func TestEnableJSONOutput_NoFlag(t *testing.T) {
	type params struct {
		Name string `json:"name" flag:"name" desc:"name"`
	}

	p := &params{Name: "test"}
	// Should not panic when there's no json flag field.
	enableJSONOutput(p)
}
