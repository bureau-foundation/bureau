// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import "encoding/json"

// protocolVersion is the MCP protocol version supported by this server.
const protocolVersion = "2024-11-05"

// JSON-RPC 2.0 standard error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// request is a JSON-RPC 2.0 request or notification. Notifications
// are distinguished by having no ID field (isNotification returns true).
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification returns true if this request has no ID, indicating
// it is a JSON-RPC 2.0 notification that expects no response.
func (r *request) isNotification() bool {
	return len(r.ID) == 0
}

// response is a JSON-RPC 2.0 response. Exactly one of Result or Error
// is set. Result uses omitempty so it is absent in error responses.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// --- MCP protocol types ---

// initializeParams is the client's initialize request parameters.
type initializeParams struct {
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    any        `json:"capabilities"`
	ClientInfo      clientInfo `json:"clientInfo"`
}

// clientInfo identifies the MCP client.
type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// initializeResult is the server's initialize response.
type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      serverInfo         `json:"serverInfo"`
}

// serverCapabilities declares what the server supports.
type serverCapabilities struct {
	Tools *toolCapability `json:"tools,omitempty"`
}

// toolCapability indicates the server supports tool operations.
// Empty struct — presence alone signals the capability.
type toolCapability struct{}

// serverInfo identifies the MCP server.
type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// toolsListResult is the result for tools/list.
type toolsListResult struct {
	Tools []toolDescription `json:"tools"`
}

// toolDescription describes a single tool for the tools/list response.
type toolDescription struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// toolsCallParams is the client's tools/call request parameters.
type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// toolsCallResult is the server's tools/call response.
type toolsCallResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// contentBlock is an MCP content block within a tool result.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
