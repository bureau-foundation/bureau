// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package toolserver defines the interface for Bureau tool discovery
// and execution. The interface decouples the agent loop from the
// concrete MCP tool server, allowing the core agent logic to depend
// on a stable abstraction in lib/ rather than on the CLI binary's
// MCP implementation in cmd/bureau/mcp/.
//
// The MCP server in cmd/bureau/mcp/ implements this interface. The
// agent's driver code (cmd/bureau-agent/driver.go) constructs the
// concrete MCP server from the CLI command tree and authorization
// grants, then passes it to the agent loop as a [Server]. The loop
// code never imports from cmd/ — it depends only on this interface.
package toolserver

import "encoding/json"

// ToolExport describes a tool for callers that need tool metadata
// without going through the MCP JSON-RPC protocol. The native Bureau
// agent uses this to build LLM tool definitions from the same
// discovery and authorization logic as the MCP server.
type ToolExport struct {
	// Name is the underscore-joined command path (e.g., "bureau_ticket_list").
	Name string

	// Description is the human-readable tool description.
	Description string

	// InputSchema is the JSON Schema for the tool's parameters,
	// serialized as JSON.
	InputSchema json.RawMessage

	// Deferrable is true when the tool can be deferred for on-demand
	// discovery via tool search. Read-only query tools (list, show,
	// search, status) are NOT deferrable — they stay in context
	// always. Everything else is deferrable.
	Deferrable bool
}

// MetaToolDefinition describes a progressive disclosure meta-tool.
// Used when the tool catalog is too large to send inline and the
// provider doesn't support server-side tool search.
type MetaToolDefinition struct {
	// Name is the meta-tool name (e.g., "bureau_tools_list").
	Name string

	// Description is the human-readable description.
	Description string

	// InputSchema is the JSON Schema for the meta-tool's parameters,
	// serialized as JSON.
	InputSchema json.RawMessage
}

// Server provides tool discovery, authorization filtering, and
// execution. The MCP server implements this interface; the agent loop
// depends on it rather than on the concrete MCP types.
type Server interface {
	// AuthorizedTools returns metadata for all tools that pass
	// authorization checks. Used by the native agent to build the LLM
	// tool catalog from the same discovery and grant-filtering logic
	// as the MCP JSON-RPC tools/list endpoint.
	AuthorizedTools() []ToolExport

	// CallTool executes a tool by name with the given JSON arguments.
	// Returns the captured output text and whether the tool reported
	// an error.
	//
	// Meta-tools (bureau_tools_list, bureau_tools_describe,
	// bureau_tools_call) are dispatched through the same handlers as
	// the JSON-RPC path, allowing the native agent loop to use
	// progressive disclosure without going through JSON-RPC framing.
	//
	// A non-nil error return indicates an infrastructure failure
	// (unknown tool, authorization denied) — not a tool execution
	// failure. Tool execution failures are indicated by isError=true
	// with the error message included in the output string.
	CallTool(name string, arguments json.RawMessage) (output string, isError bool, err error)

	// MetaToolDefinitions returns LLM tool definitions for the
	// progressive disclosure meta-tools. Used by the native agent
	// loop when progressive disclosure is selected for non-Anthropic
	// providers.
	MetaToolDefinitions() []MetaToolDefinition
}
