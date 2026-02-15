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
		Value      string `json:"value" flag:"value" desc:"value to print"`
		OutputJSON bool   `json:"-" flag:"json" desc:"output as JSON"`
	}
	type listParams struct {
		Prefix     string `json:"prefix" flag:"prefix" desc:"filter prefix" default:""`
		OutputJSON bool   `json:"-"      flag:"json"   desc:"output as JSON"`
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
				Flags:          func() *pflag.FlagSet { return cli.FlagsFromParams("echo", &echoP) },
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
				Flags:          func() *pflag.FlagSet { return cli.FlagsFromParams("fail", &failP) },
				Params:         func() any { return &failP },
				RequiredGrants: []string{"command/test/fail"},
				Run: func(args []string) error {
					fmt.Print("partial")
					return fmt.Errorf("intentional failure: %s", failP.Reason)
				},
			},
			{
				Name:           "format",
				Summary:        "Conditional JSON output",
				Flags:          func() *pflag.FlagSet { return cli.FlagsFromParams("format", &formatP) },
				Params:         func() any { return &formatP },
				Output:         func() any { return &formatOutput{} },
				RequiredGrants: []string{"command/test/format"},
				Run: func(args []string) error {
					if formatP.OutputJSON {
						return cli.WriteJSON(formatOutput{Value: formatP.Value})
					}
					fmt.Printf("VALUE: %s", formatP.Value)
					return nil
				},
			},
			{
				Name:           "list",
				Summary:        "List items",
				Flags:          func() *pflag.FlagSet { return cli.FlagsFromParams("list", &listP) },
				Params:         func() any { return &listP },
				Output:         func() any { return &[]listItem{} },
				RequiredGrants: []string{"command/test/list"},
				Run: func(args []string) error {
					items := []listItem{
						{Name: "alpha"},
						{Name: "beta"},
					}
					if listP.OutputJSON {
						return cli.WriteJSON(items)
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
	server := NewServer(root, grants)
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

	// Find the echo tool and verify it has no annotations.
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
	if echoTool.Annotations != nil {
		t.Errorf("test_echo should have nil annotations, got %+v", echoTool.Annotations)
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

func TestDeriveAnnotations(t *testing.T) {
	tests := []struct {
		name           string
		commandName    string
		wantNil        bool
		wantReadOnly   bool
		wantIdempotent bool
	}{
		{
			name:           "list command is read-only and idempotent",
			commandName:    "list",
			wantReadOnly:   true,
			wantIdempotent: true,
		},
		{
			name:        "non-list command has no annotations",
			commandName: "create",
			wantNil:     true,
		},
		{
			name:        "empty name has no annotations",
			commandName: "",
			wantNil:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command := &cli.Command{Name: tt.commandName}
			annotations := deriveAnnotations(command)

			if tt.wantNil {
				if annotations != nil {
					t.Fatalf("expected nil annotations for %q, got %+v",
						tt.commandName, annotations)
				}
				return
			}

			if annotations == nil {
				t.Fatalf("expected non-nil annotations for %q", tt.commandName)
			}
			if annotations.ReadOnlyHint == nil || *annotations.ReadOnlyHint != tt.wantReadOnly {
				t.Errorf("readOnlyHint = %v, want %v", annotations.ReadOnlyHint, tt.wantReadOnly)
			}
			if annotations.IdempotentHint == nil || *annotations.IdempotentHint != tt.wantIdempotent {
				t.Errorf("idempotentHint = %v, want %v", annotations.IdempotentHint, tt.wantIdempotent)
			}
			if annotations.DestructiveHint == nil || *annotations.DestructiveHint != false {
				t.Errorf("destructiveHint should be false for list commands")
			}
			if annotations.OpenWorldHint == nil || *annotations.OpenWorldHint != false {
				t.Errorf("openWorldHint should be false for list commands")
			}
		})
	}
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
				Flags:   func() *pflag.FlagSet { return cli.FlagsFromParams("ungated", &params) },
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
