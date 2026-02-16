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

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
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

// --- Test fixtures ---

// wildcardGrants returns grants that authorize all command actions,
// used by tests that exercise non-authorization behavior.
func wildcardGrants() []schema.Grant {
	return []schema.Grant{{Actions: []string{"command/**"}}}
}

// --- Output types for test commands ---

// formatOutput is the output type for the test_format command.
type formatOutput struct {
	Value string `json:"value" desc:"the formatted value"`
}

// listItem is the output element type for the test_list command.
type listItem struct {
	Name string `json:"name" desc:"item name"`
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
		cli.JSONOutput
		Value string `json:"value" flag:"value" desc:"value to print"`
	}
	type listParams struct {
		cli.JSONOutput
		Prefix string `json:"prefix" flag:"prefix" desc:"filter prefix" default:""`
	}

	var echoP echoParams
	var failP failParams
	var formatP formatParams
	var listP listParams

	return &cli.Command{
		Name: "test",
		Subcommands: []*cli.Command{
			{
				Name:           "echo",
				Summary:        "Echo a message",
				Description:    "Echo the provided message to stdout.",
				Annotations:    cli.ReadOnly(),
				Params:         func() any { return &echoP },
				RequiredGrants: []string{"command/test/echo"},
				Run: func(args []string) error {
					fmt.Println(echoP.Message)
					return nil
				},
			},
			{
				Name:           "fail",
				Summary:        "Always fails with a reason",
				Annotations:    cli.Create(),
				Params:         func() any { return &failP },
				RequiredGrants: []string{"command/test/fail"},
				Run: func(args []string) error {
					fmt.Print("partial")
					return cli.Internal("intentional failure: %s", failP.Reason)
				},
			},
			{
				Name:           "format",
				Summary:        "Conditional JSON output",
				Annotations:    cli.Idempotent(),
				Params:         func() any { return &formatP },
				Output:         func() any { return &formatOutput{} },
				RequiredGrants: []string{"command/test/format"},
				Examples: []cli.Example{
					{
						Description: "Format a greeting",
						Command:     "bureau test format --value hello",
					},
				},
				Run: func(args []string) error {
					if done, err := formatP.EmitJSON(formatOutput{Value: formatP.Value}); done {
						return err
					}
					fmt.Printf("VALUE: %s", formatP.Value)
					return nil
				},
			},
			{
				Name:           "list",
				Summary:        "List items",
				Annotations:    cli.ReadOnly(),
				Params:         func() any { return &listP },
				Output:         func() any { return &[]listItem{} },
				RequiredGrants: []string{"command/test/list"},
				Run: func(args []string) error {
					items := []listItem{
						{Name: "alpha"},
						{Name: "beta"},
					}
					if done, err := listP.EmitJSON(items); done {
						return err
					}
					for _, item := range items {
						fmt.Println(item.Name)
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

// --- Session helpers ---
//
// These are layered from low-level (mcpSession) to high-level
// (callToolResult). Most tests should use the highest-level
// helper that covers their assertions.

// mcpSession sends a sequence of JSON-RPC messages to a fresh MCP
// server and returns the responses. Notifications produce no response.
func mcpSession(t *testing.T, root *cli.Command, grants []schema.Grant, messages ...map[string]any) []testResponse {
	t.Helper()
	return mcpSessionWithOptions(t, root, grants, nil, messages...)
}

// mcpSessionWithOptions is like mcpSession but accepts server options
// (e.g., WithProgressiveDisclosure).
func mcpSessionWithOptions(t *testing.T, root *cli.Command, grants []schema.Grant, options []ServerOption, messages ...map[string]any) []testResponse {
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
	server := NewServer(root, grants, options...)
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

// session runs an initialized MCP session against testCommandTree with
// wildcard grants. Additional messages are sent after the init handshake.
func session(t *testing.T, messages ...map[string]any) []testResponse {
	t.Helper()
	return sessionWithGrants(t, wildcardGrants(), messages...)
}

// sessionWithGrants runs an initialized MCP session against
// testCommandTree with the specified grants.
func sessionWithGrants(t *testing.T, grants []schema.Grant, messages ...map[string]any) []testResponse {
	t.Helper()
	root := testCommandTree()
	all := append(initMessages(), messages...)
	return mcpSession(t, root, grants, all...)
}

// listTools runs tools/list with wildcard grants and returns the
// parsed result.
func listTools(t *testing.T) toolsListResult {
	t.Helper()
	return listToolsWithGrants(t, wildcardGrants())
}

// listToolsWithGrants runs tools/list with the specified grants and
// returns the parsed result. Fails the test on JSON-RPC errors.
func listToolsWithGrants(t *testing.T, grants []schema.Grant) toolsListResult {
	t.Helper()
	responses := sessionWithGrants(t, grants, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})
	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("tools/list error: code=%d message=%q", resp.Error.Code, resp.Error.Message)
	}
	var result toolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal tools/list result: %v", err)
	}
	return result
}

// callTool runs tools/call with wildcard grants and returns the raw
// response, allowing callers to assert on both JSON-RPC success and
// error conditions.
func callTool(t *testing.T, name string, arguments map[string]any) testResponse {
	t.Helper()
	return callToolWithGrants(t, wildcardGrants(), name, arguments)
}

// callToolWithGrants runs tools/call with the specified grants and
// returns the raw response.
func callToolWithGrants(t *testing.T, grants []schema.Grant, name string, arguments map[string]any) testResponse {
	t.Helper()
	params := map[string]any{"name": name}
	if arguments != nil {
		params["arguments"] = arguments
	}
	responses := sessionWithGrants(t, grants, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  params,
	})
	return responses[1]
}

// callToolResult runs tools/call with wildcard grants and parses the
// result into a toolsCallResult. Fails the test on JSON-RPC errors
// (as opposed to tool-level errors indicated by isError=true).
func callToolResult(t *testing.T, name string, arguments map[string]any) toolsCallResult {
	t.Helper()
	resp := callTool(t, name, arguments)
	if resp.Error != nil {
		t.Fatalf("tools/call %q: JSON-RPC error code=%d message=%q",
			name, resp.Error.Code, resp.Error.Message)
	}
	var result toolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal tools/call %q result: %v", name, err)
	}
	return result
}

// --- Progressive mode helpers ---

// progressiveSession runs an initialized MCP session in progressive
// (meta-tool) mode against testCommandTree with wildcard grants.
func progressiveSession(t *testing.T, messages ...map[string]any) []testResponse {
	t.Helper()
	return progressiveSessionWithGrants(t, wildcardGrants(), messages...)
}

// progressiveSessionWithGrants runs an initialized MCP session in
// progressive mode with the specified grants.
func progressiveSessionWithGrants(t *testing.T, grants []schema.Grant, messages ...map[string]any) []testResponse {
	t.Helper()
	root := testCommandTree()
	all := append(initMessages(), messages...)
	return mcpSessionWithOptions(t, root, grants, []ServerOption{WithProgressiveDisclosure()}, all...)
}

// callMetaTool calls a meta-tool in progressive mode and returns the
// parsed result. Fails the test on JSON-RPC errors.
func callMetaTool(t *testing.T, name string, arguments map[string]any) toolsCallResult {
	t.Helper()
	params := map[string]any{"name": name}
	if arguments != nil {
		params["arguments"] = arguments
	}
	responses := progressiveSession(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  params,
	})
	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("tools/call %q: JSON-RPC error code=%d message=%q",
			name, resp.Error.Code, resp.Error.Message)
	}
	var result toolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal tools/call %q result: %v", name, err)
	}
	return result
}

// --- Tool discovery tests ---

func TestNewServer_ToolDiscovery(t *testing.T) {
	root := testCommandTree()
	server := NewServer(root, wildcardGrants())

	// Should discover: test_echo, test_fail, test_format, test_list.
	// Should NOT discover: test_noparams (no Params function).
	if len(server.tools) != 4 {
		t.Fatalf("expected 4 tools, got %d", len(server.tools))
	}

	expected := []string{"test_echo", "test_fail", "test_format", "test_list"}
	for i, name := range expected {
		if server.tools[i].name != name {
			t.Errorf("tools[%d].name = %q, want %q", i, server.tools[i].name, name)
		}
	}

	for _, discovered := range server.tools {
		if discovered.inputSchema == nil {
			t.Errorf("tool %q has nil inputSchema", discovered.name)
		}
	}

	for _, discovered := range server.tools {
		if discovered.title == "" {
			t.Errorf("tool %q has empty title", discovered.name)
		}
	}

	for _, name := range expected {
		if _, ok := server.toolsByName[name]; !ok {
			t.Errorf("toolsByName missing %q", name)
		}
	}
}

// --- Protocol tests ---

func TestServer_Initialize(t *testing.T) {
	responses := session(t)
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

func TestServer_InitializeVersionNegotiation(t *testing.T) {
	// The server accepts any client version and responds with its own
	// version, per the MCP specification. Older clients simply ignore
	// fields they don't recognize.
	root := testCommandTree()
	responses := mcpSession(t, root, wildcardGrants(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "old-client"},
		},
	})

	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	if responses[0].Error != nil {
		t.Fatalf("unexpected error: code=%d message=%q",
			responses[0].Error.Code, responses[0].Error.Message)
	}

	var result initializeResult
	if err := json.Unmarshal(responses[0].Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// Server must respond with its own version, not the client's.
	if result.ProtocolVersion != protocolVersion {
		t.Errorf("protocolVersion = %q, want %q", result.ProtocolVersion, protocolVersion)
	}
}

func TestServer_Ping(t *testing.T) {
	responses := session(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "ping",
	})
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

func TestServer_NotInitialized(t *testing.T) {
	root := testCommandTree()
	// Send tools/call without initializing first.
	responses := mcpSession(t, root, wildcardGrants(), map[string]any{
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
	responses := session(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "resources/list",
	})
	resp := responses[1]
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != codeMethodNotFound {
		t.Errorf("error code = %d, want %d", resp.Error.Code, codeMethodNotFound)
	}
}

func TestServer_NotificationIgnored(t *testing.T) {
	responses := session(t, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/progress",
		"params":  map[string]any{"token": "abc"},
	})
	// Only the initialize request should produce a response.
	if len(responses) != 1 {
		t.Fatalf("expected 1 response (init only), got %d", len(responses))
	}
}

// --- Tool listing tests ---

func TestServer_ToolsList(t *testing.T) {
	result := listTools(t)

	if len(result.Tools) != 4 {
		t.Fatalf("expected 4 tools, got %d", len(result.Tools))
	}

	names := make(map[string]bool)
	for _, discovered := range result.Tools {
		names[discovered.Name] = true
	}
	for _, expected := range []string{"test_echo", "test_fail", "test_format", "test_list"} {
		if !names[expected] {
			t.Errorf("missing tool %q in tools/list", expected)
		}
	}

	for _, discovered := range result.Tools {
		if discovered.InputSchema == nil {
			t.Errorf("tool %q has nil inputSchema", discovered.Name)
		}
		if discovered.Title == "" {
			t.Errorf("tool %q has empty title", discovered.Name)
		}
	}
}

func TestServer_ToolsListAnnotations(t *testing.T) {
	result := listTools(t)

	// Find the list tool and verify it has annotations.
	var listTool *toolDescription
	for i := range result.Tools {
		if result.Tools[i].Name == "test_list" {
			listTool = &result.Tools[i]
			break
		}
	}
	if listTool == nil {
		t.Fatal("test_list not found in tools/list response")
	}
	if listTool.Annotations == nil {
		t.Fatal("test_list should have annotations")
	}
	if listTool.Annotations.ReadOnlyHint == nil || !*listTool.Annotations.ReadOnlyHint {
		t.Error("test_list should have readOnlyHint=true")
	}

	// Find the echo tool and verify it has explicit ReadOnly annotations.
	var echoTool *toolDescription
	for i := range result.Tools {
		if result.Tools[i].Name == "test_echo" {
			echoTool = &result.Tools[i]
			break
		}
	}
	if echoTool == nil {
		t.Fatal("test_echo not found in tools/list response")
	}
	if echoTool.Annotations == nil {
		t.Fatal("test_echo should have annotations (explicit ReadOnly)")
	}
	if echoTool.Annotations.ReadOnlyHint == nil || !*echoTool.Annotations.ReadOnlyHint {
		t.Error("test_echo should have readOnlyHint=true")
	}
}

// --- Tool call tests ---

func TestServer_ToolsCall(t *testing.T) {
	result := callToolResult(t, "test_echo", map[string]any{"message": "hello world"})

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
	// Call fail with no arguments — Reason should default to "boom"
	// via the Flags() default registration.
	result := callToolResult(t, "test_fail", map[string]any{})

	if !result.IsError {
		t.Error("expected isError=true (tool returns error)")
	}

	var errorText string
	for _, block := range result.Content {
		if strings.Contains(block.Text, "intentional failure") {
			errorText = block.Text
		}
	}
	if errorText == "" {
		t.Fatal("no error content block found")
	}
	if !strings.Contains(errorText, "boom") {
		t.Errorf("error text = %q, want it to contain 'boom' (default)", errorText)
	}
}

func TestServer_ToolsCallJSONOutput(t *testing.T) {
	result := callToolResult(t, "test_format", map[string]any{"value": "hello"})

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
	result := callToolResult(t, "test_fail", map[string]any{"reason": "test error"})

	if !result.IsError {
		t.Error("expected isError=true")
	}
	// Should have two content blocks: partial stdout and the error.
	if len(result.Content) < 2 {
		t.Fatalf("expected at least 2 content blocks, got %d", len(result.Content))
	}
	if result.Content[0].Text != "partial" {
		t.Errorf("first content = %q, want %q", result.Content[0].Text, "partial")
	}
	if !strings.Contains(result.Content[1].Text, "test error") {
		t.Errorf("error content = %q, want it to contain 'test error'", result.Content[1].Text)
	}

	// ErrorInfo should carry structured error metadata.
	if result.ErrorInfo == nil {
		t.Fatal("errorInfo should be present on tool error")
	}
	if result.ErrorInfo.Category != string(cli.CategoryInternal) {
		t.Errorf("errorInfo.category = %q, want %q", result.ErrorInfo.Category, cli.CategoryInternal)
	}
	if result.ErrorInfo.Retryable {
		t.Error("errorInfo.retryable should be false for internal errors")
	}
}

func TestServer_ToolsCallUnknownTool(t *testing.T) {
	resp := callTool(t, "nonexistent_tool", nil)
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

// --- Unit tests for internal helpers ---

func TestEnableJSONOutput(t *testing.T) {
	type params struct {
		cli.JSONOutput
		Name string `json:"name" flag:"name" desc:"name"`
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
	// Should not panic when the struct doesn't implement JSONOutputter.
	enableJSONOutput(p)
}

func TestResolveAnnotations(t *testing.T) {
	t.Run("explicit annotations take precedence", func(t *testing.T) {
		command := &cli.Command{
			Name:        "list",
			Annotations: cli.Destructive(),
		}
		annotations := resolveAnnotations(command)
		if annotations == nil {
			t.Fatal("expected non-nil annotations")
		}
		// Even though the name is "list", the explicit Destructive()
		// annotation should override the read-only heuristic.
		if annotations.DestructiveHint == nil || !*annotations.DestructiveHint {
			t.Error("destructiveHint should be true (explicit annotation)")
		}
		if annotations.ReadOnlyHint == nil || *annotations.ReadOnlyHint {
			t.Error("readOnlyHint should be false (explicit annotation)")
		}
	})

	t.Run("heuristic fallback for list", func(t *testing.T) {
		command := &cli.Command{Name: "list"}
		annotations := resolveAnnotations(command)
		if annotations == nil {
			t.Fatal("expected non-nil annotations for list command")
		}
		if annotations.ReadOnlyHint == nil || !*annotations.ReadOnlyHint {
			t.Error("readOnlyHint should be true")
		}
		if annotations.IdempotentHint == nil || !*annotations.IdempotentHint {
			t.Error("idempotentHint should be true")
		}
		if annotations.DestructiveHint == nil || *annotations.DestructiveHint {
			t.Error("destructiveHint should be false")
		}
	})

	t.Run("no annotations for unknown command", func(t *testing.T) {
		command := &cli.Command{Name: "create"}
		annotations := resolveAnnotations(command)
		if annotations != nil {
			t.Fatalf("expected nil annotations, got %+v", annotations)
		}
	})

	t.Run("ReadOnly preset", func(t *testing.T) {
		command := &cli.Command{
			Name:        "show",
			Annotations: cli.ReadOnly(),
		}
		annotations := resolveAnnotations(command)
		if annotations == nil {
			t.Fatal("expected non-nil annotations")
		}
		if !*annotations.ReadOnlyHint {
			t.Error("readOnlyHint should be true")
		}
		if *annotations.DestructiveHint {
			t.Error("destructiveHint should be false")
		}
		if !*annotations.IdempotentHint {
			t.Error("idempotentHint should be true")
		}
		if *annotations.OpenWorldHint {
			t.Error("openWorldHint should be false")
		}
	})

	t.Run("Create preset", func(t *testing.T) {
		command := &cli.Command{
			Name:        "create",
			Annotations: cli.Create(),
		}
		annotations := resolveAnnotations(command)
		if annotations == nil {
			t.Fatal("expected non-nil annotations")
		}
		if *annotations.ReadOnlyHint {
			t.Error("readOnlyHint should be false")
		}
		if *annotations.DestructiveHint {
			t.Error("destructiveHint should be false")
		}
		if *annotations.IdempotentHint {
			t.Error("idempotentHint should be false")
		}
	})

	t.Run("Idempotent preset", func(t *testing.T) {
		command := &cli.Command{
			Name:        "update",
			Annotations: cli.Idempotent(),
		}
		annotations := resolveAnnotations(command)
		if annotations == nil {
			t.Fatal("expected non-nil annotations")
		}
		if *annotations.ReadOnlyHint {
			t.Error("readOnlyHint should be false")
		}
		if *annotations.DestructiveHint {
			t.Error("destructiveHint should be false")
		}
		if !*annotations.IdempotentHint {
			t.Error("idempotentHint should be true")
		}
	})

	t.Run("Destructive preset", func(t *testing.T) {
		command := &cli.Command{
			Name:        "delete",
			Annotations: cli.Destructive(),
		}
		annotations := resolveAnnotations(command)
		if annotations == nil {
			t.Fatal("expected non-nil annotations")
		}
		if *annotations.ReadOnlyHint {
			t.Error("readOnlyHint should be false")
		}
		if !*annotations.DestructiveHint {
			t.Error("destructiveHint should be true")
		}
		if *annotations.IdempotentHint {
			t.Error("idempotentHint should be false")
		}
	})
}

// --- Grant filtering tests ---

func TestServer_GrantFiltering_WildcardShowsAll(t *testing.T) {
	result := listTools(t)
	if len(result.Tools) != 4 {
		t.Errorf("wildcard grant should show all 4 tools, got %d", len(result.Tools))
	}
}

func TestServer_GrantFiltering_EmptyGrantsHidesAll(t *testing.T) {
	result := listToolsWithGrants(t, []schema.Grant{})
	if len(result.Tools) != 0 {
		t.Errorf("empty grants should show 0 tools, got %d", len(result.Tools))
	}
}

func TestServer_GrantFiltering_SpecificGrant(t *testing.T) {
	grants := []schema.Grant{{Actions: []string{"command/test/echo"}}}
	result := listToolsWithGrants(t, grants)

	if len(result.Tools) != 1 {
		t.Fatalf("expected 1 tool with echo grant, got %d", len(result.Tools))
	}
	if result.Tools[0].Name != "test_echo" {
		t.Errorf("expected test_echo, got %q", result.Tools[0].Name)
	}
}

func TestServer_GrantFiltering_GlobGrant(t *testing.T) {
	grants := []schema.Grant{{Actions: []string{"command/test/*"}}}
	result := listToolsWithGrants(t, grants)
	if len(result.Tools) != 4 {
		t.Errorf("command/test/* grant should show all 4 tools, got %d", len(result.Tools))
	}
}

func TestServer_GrantFiltering_MultipleGrants(t *testing.T) {
	grants := []schema.Grant{
		{Actions: []string{"command/test/echo"}},
		{Actions: []string{"command/test/list"}},
	}
	result := listToolsWithGrants(t, grants)

	if len(result.Tools) != 2 {
		t.Fatalf("expected 2 tools with echo+list grants, got %d", len(result.Tools))
	}
	names := make(map[string]bool)
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}
	if !names["test_echo"] {
		t.Error("expected test_echo to be visible")
	}
	if !names["test_list"] {
		t.Error("expected test_list to be visible")
	}
}

func TestServer_GrantFiltering_DefaultDenyNoRequiredGrants(t *testing.T) {
	// A command with Params+Run but no RequiredGrants should be
	// hidden even with a wildcard grant — default-deny.
	type stubParams struct {
		Value string `json:"value" flag:"value" desc:"test value"`
	}
	var params stubParams

	root := &cli.Command{
		Name: "test",
		Subcommands: []*cli.Command{
			{
				Name:    "ungated",
				Summary: "No required grants declared",
				Params:  func() any { return &params },
				Run:     func(args []string) error { return nil },
			},
		},
	}

	messages := append(initMessages(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})

	responses := mcpSession(t, root, wildcardGrants(), messages...)
	var result toolsListResult
	if err := json.Unmarshal(responses[1].Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if len(result.Tools) != 0 {
		t.Errorf("ungated command should be hidden, got %d tools: %v",
			len(result.Tools), result.Tools)
	}
}

func TestServer_GrantFiltering_CallBlockedWithoutGrant(t *testing.T) {
	// Grant only list, not echo — calling echo should fail.
	grants := []schema.Grant{{Actions: []string{"command/test/list"}}}
	resp := callToolWithGrants(t, grants, "test_echo", map[string]any{"message": "hello"})

	if resp.Error == nil {
		t.Fatal("expected error calling unauthorized tool")
	}
	if !strings.Contains(resp.Error.Message, "not authorized") {
		t.Errorf("error message = %q, want it to contain 'not authorized'",
			resp.Error.Message)
	}
}

func TestServer_GrantFiltering_CallAllowedWithGrant(t *testing.T) {
	grants := []schema.Grant{{Actions: []string{"command/test/echo"}}}
	resp := callToolWithGrants(t, grants, "test_echo", map[string]any{"message": "authorized"})

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
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "authorized") {
		t.Errorf("expected output containing 'authorized', got %v", result.Content)
	}
}

// --- Output schema tests ---

func TestServer_ToolsListOutputSchema_Present(t *testing.T) {
	result := listTools(t)

	// test_format has Output (object schema).
	var formatTool *toolDescription
	for i := range result.Tools {
		if result.Tools[i].Name == "test_format" {
			formatTool = &result.Tools[i]
			break
		}
	}
	if formatTool == nil {
		t.Fatal("test_format not found in tools/list response")
	}
	if formatTool.OutputSchema == nil {
		t.Fatal("test_format should have outputSchema (Output is declared)")
	}

	// After JSON round-trip, OutputSchema is map[string]any.
	outputSchema, ok := formatTool.OutputSchema.(map[string]any)
	if !ok {
		t.Fatalf("outputSchema type = %T, want map[string]any", formatTool.OutputSchema)
	}
	if outputSchema["type"] != "object" {
		t.Errorf("outputSchema.type = %v, want %q", outputSchema["type"], "object")
	}
	properties, ok := outputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("outputSchema.properties type = %T, want map[string]any", outputSchema["properties"])
	}
	if properties["value"] == nil {
		t.Error("outputSchema missing 'value' property")
	}
}

func TestServer_ToolsListOutputSchema_ArrayType(t *testing.T) {
	result := listTools(t)

	// test_list has Output (array of listItem).
	var listToolDesc *toolDescription
	for i := range result.Tools {
		if result.Tools[i].Name == "test_list" {
			listToolDesc = &result.Tools[i]
			break
		}
	}
	if listToolDesc == nil {
		t.Fatal("test_list not found in tools/list response")
	}
	if listToolDesc.OutputSchema == nil {
		t.Fatal("test_list should have outputSchema (Output is declared)")
	}

	// After JSON round-trip, OutputSchema is map[string]any.
	outputSchema, ok := listToolDesc.OutputSchema.(map[string]any)
	if !ok {
		t.Fatalf("outputSchema type = %T, want map[string]any", listToolDesc.OutputSchema)
	}
	if outputSchema["type"] != "array" {
		t.Errorf("outputSchema.type = %v, want %q", outputSchema["type"], "array")
	}
	items, ok := outputSchema["items"].(map[string]any)
	if !ok {
		t.Fatalf("outputSchema.items type = %T, want map[string]any", outputSchema["items"])
	}
	if items["type"] != "object" {
		t.Errorf("items.type = %v, want %q", items["type"], "object")
	}
	itemProperties, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("items.properties type = %T, want map[string]any", items["properties"])
	}
	if itemProperties["name"] == nil {
		t.Error("outputSchema.items missing 'name' property")
	}
}

func TestServer_ToolsListOutputSchema_Absent(t *testing.T) {
	result := listTools(t)

	// test_echo has no Output — outputSchema should be absent.
	var echoTool *toolDescription
	for i := range result.Tools {
		if result.Tools[i].Name == "test_echo" {
			echoTool = &result.Tools[i]
			break
		}
	}
	if echoTool == nil {
		t.Fatal("test_echo not found in tools/list response")
	}
	if echoTool.OutputSchema != nil {
		t.Errorf("test_echo should have nil outputSchema, got %v", echoTool.OutputSchema)
	}
}

func TestServer_StructuredContent_ObjectOutput(t *testing.T) {
	// test_format declares Output (formatOutput) and produces JSON
	// when --json is forced. The MCP server should parse it and
	// return structuredContent alongside the text content block.
	resp := callTool(t, "test_format", map[string]any{"value": "hello"})
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %v", resp.Error)
	}

	var result toolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if result.IsError {
		t.Errorf("expected isError=false, got true; content: %v", result.Content)
	}
	if result.StructuredContent == nil {
		t.Fatal("structuredContent should be present for tool with outputSchema")
	}

	// structuredContent should be the parsed JSON object.
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent type = %T, want map[string]any", result.StructuredContent)
	}
	if structured["value"] != "hello" {
		t.Errorf("structuredContent.value = %v, want %q", structured["value"], "hello")
	}

	// The text content block should also be present (backward compat).
	if len(result.Content) == 0 {
		t.Fatal("expected at least one text content block")
	}
	if !strings.Contains(result.Content[0].Text, `"value"`) {
		t.Errorf("text content = %q, expected JSON containing 'value'", result.Content[0].Text)
	}
}

func TestServer_StructuredContent_ArrayOutput(t *testing.T) {
	resp := callTool(t, "test_list", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %v", resp.Error)
	}

	var result toolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if result.IsError {
		t.Errorf("expected isError=false, got true; content: %v", result.Content)
	}
	if result.StructuredContent == nil {
		t.Fatal("structuredContent should be present for tool with outputSchema")
	}

	// structuredContent should be the parsed JSON array.
	items, ok := result.StructuredContent.([]any)
	if !ok {
		t.Fatalf("structuredContent type = %T, want []any", result.StructuredContent)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items in structuredContent, got %d", len(items))
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] type = %T, want map[string]any", items[0])
	}
	if first["name"] != "alpha" {
		t.Errorf("items[0].name = %v, want %q", first["name"], "alpha")
	}
}

func TestServer_StructuredContent_AbsentWithoutOutputSchema(t *testing.T) {
	// test_echo has no Output declaration — structuredContent should
	// not appear in the result.
	resp := callTool(t, "test_echo", map[string]any{"message": "hello"})
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %v", resp.Error)
	}

	var result toolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if result.StructuredContent != nil {
		t.Errorf("structuredContent should be nil for tool without outputSchema, got %v",
			result.StructuredContent)
	}
}

func TestServer_StructuredContent_AbsentOnError(t *testing.T) {
	// test_fail has no Output, but even if it did, structuredContent
	// should not appear when isError=true.
	resp := callTool(t, "test_fail", map[string]any{"reason": "oops"})
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
	if result.StructuredContent != nil {
		t.Errorf("structuredContent should be nil on error, got %v",
			result.StructuredContent)
	}
}

// --- Progressive mode tests ---

func TestServer_Progressive_ToolsListOnlyMetaTools(t *testing.T) {
	responses := progressiveSession(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})
	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("tools/list error: %v", resp.Error)
	}

	var result toolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// Progressive mode exposes exactly 3 meta-tools.
	if len(result.Tools) != 3 {
		t.Fatalf("expected 3 meta-tools, got %d", len(result.Tools))
	}

	names := make(map[string]bool)
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}
	for _, expected := range []string{metaToolList, metaToolDescribe, metaToolCall} {
		if !names[expected] {
			t.Errorf("missing meta-tool %q", expected)
		}
	}

	// Meta-tools should have inputSchema.
	for _, tool := range result.Tools {
		if tool.InputSchema == nil {
			t.Errorf("meta-tool %q has nil inputSchema", tool.Name)
		}
	}

	// bureau_tools_list and bureau_tools_describe should have outputSchema.
	for _, tool := range result.Tools {
		if tool.Name == metaToolCall {
			if tool.OutputSchema != nil {
				t.Errorf("%s should have nil outputSchema (dynamic output)", metaToolCall)
			}
			continue
		}
		if tool.OutputSchema == nil {
			t.Errorf("meta-tool %q should have outputSchema", tool.Name)
		}
	}
}

func TestServer_Progressive_MetaToolAnnotations(t *testing.T) {
	responses := progressiveSession(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})

	var result toolsListResult
	if err := json.Unmarshal(responses[1].Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	for _, tool := range result.Tools {
		if tool.Name == metaToolCall {
			// bureau_tools_call has no annotations (inherits MCP defaults
			// because safety depends on the inner tool).
			if tool.Annotations != nil {
				t.Errorf("%s should have nil annotations, got %+v", metaToolCall, tool.Annotations)
			}
			continue
		}
		// bureau_tools_list and bureau_tools_describe are read-only.
		if tool.Annotations == nil {
			t.Fatalf("%s should have annotations", tool.Name)
		}
		if tool.Annotations.ReadOnlyHint == nil || !*tool.Annotations.ReadOnlyHint {
			t.Errorf("%s should have readOnlyHint=true", tool.Name)
		}
	}
}

func TestServer_Progressive_ToolsList(t *testing.T) {
	result := callMetaTool(t, metaToolList, nil)

	if result.IsError {
		t.Errorf("expected isError=false, got true; content: %v", result.Content)
	}
	if result.StructuredContent == nil {
		t.Fatal("structuredContent should be present")
	}

	// Parse the structured content as a list of tool summaries.
	items, ok := result.StructuredContent.([]any)
	if !ok {
		t.Fatalf("structuredContent type = %T, want []any", result.StructuredContent)
	}

	// With wildcard grants, all 4 test tools should be listed.
	if len(items) != 4 {
		t.Fatalf("expected 4 tools, got %d", len(items))
	}

	// Check that each item has name, title, and category.
	for _, item := range items {
		summary, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("item type = %T, want map[string]any", item)
		}
		if summary["name"] == nil || summary["title"] == nil {
			t.Errorf("tool summary missing fields: %v", summary)
		}
		// Category is the second segment of the tool name. For test
		// tools like "test_echo", category is "echo".
		if summary["category"] == nil || summary["category"] == "" {
			t.Errorf("tool %v has empty category", summary["name"])
		}
	}
}

func TestServer_Progressive_ToolsListCategoryFilter(t *testing.T) {
	// Test tools have names like test_echo, test_fail, etc.
	// Category "echo" matches only test_echo.
	result := callMetaTool(t, metaToolList, map[string]any{
		"category": "echo",
	})

	if result.IsError {
		t.Errorf("expected isError=false")
	}

	items, ok := result.StructuredContent.([]any)
	if !ok {
		t.Fatalf("structuredContent type = %T, want []any", result.StructuredContent)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 tool for category 'echo', got %d", len(items))
	}
	first := items[0].(map[string]any)
	if first["name"] != "test_echo" {
		t.Errorf("name = %v, want %q", first["name"], "test_echo")
	}
}

func TestServer_Progressive_ToolsListCategoryFilterNoMatch(t *testing.T) {
	result := callMetaTool(t, metaToolList, map[string]any{
		"category": "nonexistent",
	})

	if result.IsError {
		t.Errorf("expected isError=false")
	}

	items, ok := result.StructuredContent.([]any)
	if !ok {
		t.Fatalf("structuredContent type = %T, want []any", result.StructuredContent)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 tools for nonexistent category, got %d", len(items))
	}
}

func TestServer_Progressive_ToolsListGrantFiltering(t *testing.T) {
	// Only grant echo — bureau_tools_list should only show echo.
	grants := []schema.Grant{{Actions: []string{"command/test/echo"}}}
	responses := progressiveSessionWithGrants(t, grants, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": metaToolList,
		},
	})
	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	var result toolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	items, ok := result.StructuredContent.([]any)
	if !ok {
		t.Fatalf("structuredContent type = %T, want []any", result.StructuredContent)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 tool with echo grant, got %d", len(items))
	}
	first := items[0].(map[string]any)
	if first["name"] != "test_echo" {
		t.Errorf("expected test_echo, got %v", first["name"])
	}
}

func TestServer_Progressive_ToolsDescribe(t *testing.T) {
	result := callMetaTool(t, metaToolDescribe, map[string]any{
		"name": "test_format",
	})

	if result.IsError {
		t.Errorf("expected isError=false, got true; content: %v", result.Content)
	}
	if result.StructuredContent == nil {
		t.Fatal("structuredContent should be present")
	}

	detail, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent type = %T, want map[string]any", result.StructuredContent)
	}

	if detail["name"] != "test_format" {
		t.Errorf("name = %v, want %q", detail["name"], "test_format")
	}
	if detail["title"] != "Conditional JSON output" {
		t.Errorf("title = %v, want %q", detail["title"], "Conditional JSON output")
	}
	if detail["inputSchema"] == nil {
		t.Error("inputSchema should be present")
	}
	if detail["outputSchema"] == nil {
		t.Error("outputSchema should be present (test_format declares Output)")
	}

	// Check that examples are included.
	examples, ok := detail["examples"].([]any)
	if !ok {
		t.Fatalf("examples type = %T, want []any", detail["examples"])
	}
	if len(examples) != 1 {
		t.Fatalf("expected 1 example, got %d", len(examples))
	}
	example := examples[0].(map[string]any)
	if example["command"] != "bureau test format --value hello" {
		t.Errorf("example command = %v, want %q", example["command"], "bureau test format --value hello")
	}
}

func TestServer_Progressive_ToolsDescribeUnknown(t *testing.T) {
	responses := progressiveSession(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      metaToolDescribe,
			"arguments": map[string]any{"name": "nonexistent_tool"},
		},
	})
	resp := responses[1]
	if resp.Error == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(resp.Error.Message, "unknown tool") {
		t.Errorf("error message = %q, want it to contain 'unknown tool'", resp.Error.Message)
	}
}

func TestServer_Progressive_ToolsDescribeUnauthorized(t *testing.T) {
	// Grant only echo, try to describe format.
	grants := []schema.Grant{{Actions: []string{"command/test/echo"}}}
	responses := progressiveSessionWithGrants(t, grants, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      metaToolDescribe,
			"arguments": map[string]any{"name": "test_format"},
		},
	})
	resp := responses[1]
	if resp.Error == nil {
		t.Fatal("expected error for unauthorized tool")
	}
	if !strings.Contains(resp.Error.Message, "not authorized") {
		t.Errorf("error message = %q, want it to contain 'not authorized'", resp.Error.Message)
	}
}

func TestServer_Progressive_ToolsCall(t *testing.T) {
	result := callMetaTool(t, metaToolCall, map[string]any{
		"name":      "test_echo",
		"arguments": map[string]any{"message": "hello from meta"},
	})

	if result.IsError {
		t.Errorf("expected isError=false, got true; content: %v", result.Content)
	}
	if len(result.Content) == 0 {
		t.Fatal("expected at least one content block")
	}
	if !strings.Contains(result.Content[0].Text, "hello from meta") {
		t.Errorf("content = %q, want it to contain 'hello from meta'", result.Content[0].Text)
	}

	// bureau_tools_call has no outputSchema, so structuredContent
	// should not be present even if the inner tool has one.
	if result.StructuredContent != nil {
		t.Errorf("structuredContent should be nil for meta-tool call-through, got %v",
			result.StructuredContent)
	}
}

func TestServer_Progressive_ToolsCallError(t *testing.T) {
	result := callMetaTool(t, metaToolCall, map[string]any{
		"name":      "test_fail",
		"arguments": map[string]any{"reason": "meta-fail"},
	})

	if !result.IsError {
		t.Error("expected isError=true")
	}
	var errorFound bool
	for _, block := range result.Content {
		if strings.Contains(block.Text, "meta-fail") {
			errorFound = true
		}
	}
	if !errorFound {
		t.Errorf("expected error content containing 'meta-fail', got %v", result.Content)
	}

	// ErrorInfo should propagate through progressive mode too.
	if result.ErrorInfo == nil {
		t.Fatal("errorInfo should be present on meta-tool error")
	}
	if result.ErrorInfo.Category != "internal" {
		t.Errorf("errorInfo.category = %q, want %q", result.ErrorInfo.Category, "internal")
	}
}

func TestServer_Progressive_ToolsCallUnknown(t *testing.T) {
	responses := progressiveSession(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      metaToolCall,
			"arguments": map[string]any{"name": "nonexistent_tool"},
		},
	})
	resp := responses[1]
	if resp.Error == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(resp.Error.Message, "unknown tool") {
		t.Errorf("error message = %q, want it to contain 'unknown tool'", resp.Error.Message)
	}
}

func TestServer_Progressive_ToolsCallUnauthorized(t *testing.T) {
	// Grant only echo, try to call format through meta-tool.
	grants := []schema.Grant{{Actions: []string{"command/test/echo"}}}
	responses := progressiveSessionWithGrants(t, grants, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      metaToolCall,
			"arguments": map[string]any{"name": "test_format", "arguments": map[string]any{"value": "x"}},
		},
	})
	resp := responses[1]
	if resp.Error == nil {
		t.Fatal("expected error for unauthorized tool")
	}
	if !strings.Contains(resp.Error.Message, "not authorized") {
		t.Errorf("error message = %q, want it to contain 'not authorized'", resp.Error.Message)
	}
}

func TestServer_Progressive_ToolsCallRecursionPrevented(t *testing.T) {
	// Trying to call bureau_tools_list through bureau_tools_call
	// should be rejected to prevent recursion.
	responses := progressiveSession(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      metaToolCall,
			"arguments": map[string]any{"name": metaToolList},
		},
	})
	resp := responses[1]
	if resp.Error == nil {
		t.Fatal("expected error for recursive meta-tool call")
	}
	if !strings.Contains(resp.Error.Message, "meta-tools cannot be called") {
		t.Errorf("error message = %q, want it to contain 'meta-tools cannot be called'",
			resp.Error.Message)
	}
}

func TestServer_Progressive_DirectToolBlocked(t *testing.T) {
	// In progressive mode, calling a real tool directly (not through
	// bureau_tools_call) should be rejected.
	responses := progressiveSession(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "test_echo",
			"arguments": map[string]any{"message": "direct"},
		},
	})
	resp := responses[1]
	if resp.Error == nil {
		t.Fatal("expected error for direct tool call in progressive mode")
	}
	if !strings.Contains(resp.Error.Message, "not directly callable in progressive mode") {
		t.Errorf("error message = %q, want it to mention progressive mode", resp.Error.Message)
	}
}

// --- Error classification tests ---

func TestServer_ErrorInfo_PresentOnError(t *testing.T) {
	result := callToolResult(t, "test_fail", map[string]any{"reason": "classify me"})

	if !result.IsError {
		t.Fatal("expected isError=true")
	}
	if result.ErrorInfo == nil {
		t.Fatal("errorInfo should be present when isError=true")
	}
	// The test_fail command returns cli.Internal, so category should
	// be "internal" and retryable should be false.
	if result.ErrorInfo.Category != "internal" {
		t.Errorf("category = %q, want %q", result.ErrorInfo.Category, "internal")
	}
	if result.ErrorInfo.Retryable {
		t.Error("retryable should be false for internal errors")
	}
}

func TestServer_ErrorInfo_AbsentOnSuccess(t *testing.T) {
	result := callToolResult(t, "test_echo", map[string]any{"message": "success"})

	if result.IsError {
		t.Fatal("expected isError=false")
	}
	if result.ErrorInfo != nil {
		t.Errorf("errorInfo should be nil on success, got %+v", result.ErrorInfo)
	}
}

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantCategory  string
		wantRetryable bool
	}{
		{
			name:          "ToolError validation",
			err:           cli.Validation("--room is required"),
			wantCategory:  "validation",
			wantRetryable: false,
		},
		{
			name:          "ToolError not_found",
			err:           cli.NotFound("ticket %s not found", "tkt-123"),
			wantCategory:  "not_found",
			wantRetryable: false,
		},
		{
			name:          "ToolError forbidden",
			err:           cli.Forbidden("access denied"),
			wantCategory:  "forbidden",
			wantRetryable: false,
		},
		{
			name:          "ToolError conflict",
			err:           cli.Conflict("already exists"),
			wantCategory:  "conflict",
			wantRetryable: false,
		},
		{
			name:          "ToolError transient",
			err:           cli.Transient("connection refused"),
			wantCategory:  "transient",
			wantRetryable: true,
		},
		{
			name:          "ToolError internal",
			err:           cli.Internal("unexpected: %s", "oops"),
			wantCategory:  "internal",
			wantRetryable: false,
		},
		{
			name:          "plain error defaults to internal",
			err:           fmt.Errorf("something broke"),
			wantCategory:  "internal",
			wantRetryable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := classifyError(tt.err)
			if info.Category != tt.wantCategory {
				t.Errorf("category = %q, want %q", info.Category, tt.wantCategory)
			}
			if info.Retryable != tt.wantRetryable {
				t.Errorf("retryable = %v, want %v", info.Retryable, tt.wantRetryable)
			}
		})
	}
}

func TestClassifyError_WrappedToolError(t *testing.T) {
	// A ToolError wrapped in fmt.Errorf should still be classified
	// by the inner ToolError's category.
	inner := cli.Validation("bad input")
	wrapped := fmt.Errorf("outer context: %w", inner)

	info := classifyError(wrapped)
	if info.Category != "validation" {
		t.Errorf("category = %q, want %q", info.Category, "validation")
	}
}

// --- Unit tests for meta-tool helpers ---

func TestToolCategory(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		want     string
	}{
		{"three segments", "bureau_pipeline_list", "pipeline"},
		{"four segments", "bureau_ticket_dep_add", "ticket"},
		{"two segments", "bureau_quickstart", "quickstart"},
		{"single segment", "standalone", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolCategory(tt.toolName)
			if got != tt.want {
				t.Errorf("toolCategory(%q) = %q, want %q", tt.toolName, got, tt.want)
			}
		})
	}
}

func TestIsMetaTool(t *testing.T) {
	if !isMetaTool(metaToolList) {
		t.Errorf("isMetaTool(%q) = false, want true", metaToolList)
	}
	if !isMetaTool(metaToolDescribe) {
		t.Errorf("isMetaTool(%q) = false, want true", metaToolDescribe)
	}
	if !isMetaTool(metaToolCall) {
		t.Errorf("isMetaTool(%q) = false, want true", metaToolCall)
	}
	if isMetaTool("test_echo") {
		t.Error("isMetaTool(test_echo) = true, want false")
	}
}
