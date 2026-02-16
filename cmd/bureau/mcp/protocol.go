// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import "encoding/json"

// protocolVersion is the MCP protocol version implemented by this server.
// The server responds with this version during initialization regardless
// of what version the client requests, per the MCP specification: the
// client then decides whether it can work with the server's version.
const protocolVersion = "2025-11-25"

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
// Its presence in capabilities signals tool support; listChanged
// declares whether the server sends notifications/tools/list_changed.
type toolCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// serverInfo identifies the MCP server.
type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// toolsListResult is the result for tools/list. NextCursor supports
// pagination per the MCP specification; when empty it is omitted.
type toolsListResult struct {
	Tools      []toolDescription `json:"tools"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

// toolDescription describes a single tool for the tools/list response.
// Title is the human-readable display name; Annotations carry behavioral
// hints; OutputSchema declares the JSON Schema for structured results;
// Icons are optional sized images for client UIs.
type toolDescription struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"`
	Description  string           `json:"description"`
	InputSchema  any              `json:"inputSchema"`
	OutputSchema any              `json:"outputSchema,omitempty"`
	Annotations  *toolAnnotations `json:"annotations,omitempty"`
	Icons        []toolIcon       `json:"icons,omitempty"`
}

// toolAnnotations provides behavioral hints about a tool. All fields
// are optional pointers; when nil the MCP-specified defaults apply:
// readOnly=false, destructive=true, idempotent=false, openWorld=true.
type toolAnnotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

// toolIcon is a sized icon that clients can display for a tool.
type toolIcon struct {
	Source   string   `json:"src"`
	MIMEType string   `json:"mimeType"`
	Sizes    []string `json:"sizes,omitempty"`
}

// toolsCallParams is the client's tools/call request parameters.
type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// toolsCallResult is the server's tools/call response. StructuredContent
// carries typed JSON when the tool declares an outputSchema; per the spec,
// the serialized JSON is also included in a text content block for
// backward compatibility. ErrorInfo is a Bureau extension that provides
// structured error metadata alongside the human-readable text in content
// blocks, enabling agents to make programmatic recovery decisions.
type toolsCallResult struct {
	Content           []contentBlock `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
	ErrorInfo         *errorInfo     `json:"errorInfo,omitempty"`
}

// errorInfo carries structured error metadata when IsError is true.
// This is a Bureau extension to the MCP protocol: clients that don't
// understand errorInfo ignore the field, while Bureau-aware agents use
// it to decide whether to retry, fix input, or escalate.
type errorInfo struct {
	// Category classifies the error. One of: validation, not_found,
	// forbidden, conflict, transient, internal.
	Category string `json:"category"`

	// Retryable indicates whether repeating the same call might succeed.
	// True for transient errors (network, timeout, rate limit); false
	// for validation, not_found, forbidden, and internal errors.
	Retryable bool `json:"retryable"`
}

// contentBlock is an MCP content block within a tool result.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
