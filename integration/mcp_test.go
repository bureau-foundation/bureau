// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// TestMCPServer verifies that the MCP server correctly filters tools
// based on authorization grants propagated through the full stack:
// authorization policy → daemon → proxy → MCP server.
//
// The test deploys a principal with a grant for command/pipeline/list
// but NOT command/template/list, then launches "bureau mcp serve" as a
// subprocess connected to the principal's proxy socket. It verifies
// that tools/list reflects the grant and that tools/call enforces
// authorization.
func TestMCPServer(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	fleetRoomID := createFleetRoom(t, admin)

	machine := newTestMachine(t, "machine/mcp")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		FleetRoomID:    fleetRoomID,
	})

	// Deploy a principal with a targeted authorization policy: grant
	// only command/pipeline/list. The daemon resolves this into grants
	// and pushes them to the proxy, where the MCP server fetches them
	// via GET /v1/grants.
	agent := registerPrincipal(t, "test/mcp-agent", "mcp-agent-password")
	proxySockets := deployPrincipals(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{{
			Account: agent,
			Authorization: &schema.AuthorizationPolicy{
				Grants: []schema.Grant{
					{Actions: []string{"command/pipeline/list"}},
				},
			},
		}},
	})

	proxySocket := proxySockets[agent.Localpart]
	bureauBinary := resolvedBinary(t, "BUREAU_BINARY")

	t.Run("ToolsListFilteredByGrants", func(t *testing.T) {
		result := mcpToolsList(t, bureauBinary, proxySocket)

		// With only command/pipeline/list granted, exactly one tool
		// should be visible: bureau_pipeline_list. All other commands
		// either lack RequiredGrants (hidden by default-deny) or have
		// ungranted requirements.
		var names []string
		for _, tool := range result.Tools {
			names = append(names, tool.Name)
		}

		if len(result.Tools) != 1 {
			t.Fatalf("expected 1 tool, got %d: %v", len(result.Tools), names)
		}
		if result.Tools[0].Name != "bureau_pipeline_list" {
			t.Errorf("expected bureau_pipeline_list, got %q", result.Tools[0].Name)
		}
	})

	t.Run("ToolsCallUnauthorized", func(t *testing.T) {
		// Calling a tool the principal lacks grants for should fail
		// with a JSON-RPC error before the command is invoked.
		resp := mcpCallTool(t, bureauBinary, proxySocket, "bureau_template_list", nil)
		if resp.Error == nil {
			t.Fatal("expected JSON-RPC error calling unauthorized tool")
		}
		if !strings.Contains(resp.Error.Message, "not authorized") {
			t.Errorf("error message = %q, want it to contain 'not authorized'",
				resp.Error.Message)
		}
	})

	t.Run("ToolsCallAuthorized", func(t *testing.T) {
		// Calling an authorized tool should pass the grant check and
		// invoke the command. The command itself will fail (no Matrix
		// operator session inside the sandbox), but the MCP response
		// is a tool-level error (isError=true), not a JSON-RPC error.
		// This proves the authorization gate was passed.
		resp := mcpCallTool(t, bureauBinary, proxySocket, "bureau_pipeline_list",
			map[string]any{"room": "bureau/pipeline"})
		if resp.Error != nil {
			t.Fatalf("unexpected JSON-RPC error: code=%d message=%q",
				resp.Error.Code, resp.Error.Message)
		}

		var result mcpCallResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			t.Fatalf("unmarshal tools/call result: %v", err)
		}

		// The command was invoked (grant check passed). It returns an
		// error because there's no operator session, but that's the
		// command's problem, not the authorization system's.
		if !result.IsError {
			// If it somehow succeeded (e.g., the command found a
			// session), that's fine too — authorization passed either way.
			t.Log("tool execution succeeded (unexpected but acceptable)")
		}
	})
}

// --- MCP protocol helpers for integration tests ---

// mcpResponse is a JSON-RPC 2.0 response parsed from the MCP server's stdout.
type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *mcpRPCError    `json:"error"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpListResult is the parsed result of a tools/list response.
type mcpListResult struct {
	Tools []mcpTool `json:"tools"`
}

type mcpTool struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// mcpCallResult is the parsed result of a tools/call response.
type mcpCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// mcpSession launches "bureau mcp serve" as a subprocess connected to the
// given proxy socket, sends JSON-RPC messages to its stdin, and returns
// all responses. The subprocess receives EOF after all messages are sent
// and is waited to exit.
func mcpSession(t *testing.T, bureauBinary, proxySocket string, messages ...map[string]any) []mcpResponse {
	t.Helper()

	var stdin bytes.Buffer
	for _, msg := range messages {
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal message: %v", err)
		}
		stdin.Write(data)
		stdin.WriteByte('\n')
	}

	cmd := exec.Command(bureauBinary, "mcp", "serve")
	cmd.Stdin = &stdin
	cmd.Env = append(os.Environ(), "BUREAU_PROXY_SOCKET="+proxySocket)

	output, err := cmd.Output()
	if err != nil {
		// Include stderr in the error for debugging.
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("bureau mcp serve failed: %v\nstderr: %s", err, exitErr.Stderr)
		}
		t.Fatalf("bureau mcp serve failed: %v", err)
	}

	var responses []mcpResponse
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp mcpResponse
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

// mcpInitMessages returns the initialize request and initialized
// notification that start every MCP session.
func mcpInitMessages() []map[string]any {
	return []map[string]any{
		{
			"jsonrpc": "2.0",
			"id":      0,
			"method":  "initialize",
			"params": map[string]any{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]any{},
				"clientInfo":      map[string]any{"name": "integration-test", "version": "1.0"},
			},
		},
		{
			"jsonrpc": "2.0",
			"method":  "notifications/initialized",
		},
	}
}

// mcpToolsList launches an MCP session and returns the parsed tools/list result.
func mcpToolsList(t *testing.T, bureauBinary, proxySocket string) mcpListResult {
	t.Helper()

	messages := append(mcpInitMessages(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})

	responses := mcpSession(t, bureauBinary, proxySocket, messages...)
	if len(responses) < 2 {
		t.Fatalf("expected at least 2 responses (init + tools/list), got %d", len(responses))
	}

	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("tools/list error: code=%d message=%q", resp.Error.Code, resp.Error.Message)
	}

	var result mcpListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal tools/list result: %v", err)
	}
	return result
}

// mcpCallTool launches an MCP session with a single tools/call and returns
// the raw response, allowing callers to assert on both success and error.
func mcpCallTool(t *testing.T, bureauBinary, proxySocket, name string, arguments map[string]any) mcpResponse {
	t.Helper()

	params := map[string]any{"name": name}
	if arguments != nil {
		params["arguments"] = arguments
	}

	messages := append(mcpInitMessages(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  params,
	})

	responses := mcpSession(t, bureauBinary, proxySocket, messages...)
	if len(responses) < 2 {
		t.Fatalf("expected at least 2 responses (init + tools/call), got %d", len(responses))
	}

	return responses[1]
}
