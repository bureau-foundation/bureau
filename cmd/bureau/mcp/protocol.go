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
	Tools     *toolCapability     `json:"tools,omitempty"`
	Resources *resourceCapability `json:"resources,omitempty"`
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

	// Hint is actionable guidance for the caller: what command to run,
	// what flag to pass, what prerequisite to check. Empty when no
	// specific guidance is available.
	Hint string `json:"hint,omitempty"`
}

// contentBlock is an MCP content block within a tool result.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// --- MCP resource protocol types ---

// resourceCapability indicates the server supports resource operations.
// Subscribe declares whether the server supports resource subscriptions
// (resources/subscribe and notifications/resources/updated). ListChanged
// declares whether the server sends notifications/resources/list_changed
// when the set of available resources changes.
type resourceCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// resourceDescription describes a single concrete resource for the
// resources/list response. Each resource has a unique URI that clients
// use for resources/read and resources/subscribe. Annotations carry
// audience and priority hints per the MCP specification.
type resourceDescription struct {
	URI         string              `json:"uri"`
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	MIMEType    string              `json:"mimeType,omitempty"`
	Annotations *resourceAnnotation `json:"annotations,omitempty"`
}

// resourceTemplate describes a parameterized resource using an RFC 6570
// URI template. Clients fill in template parameters to construct concrete
// resource URIs for resources/read. Templates appear in resources/list
// alongside concrete resource descriptions.
type resourceTemplate struct {
	URITemplate string              `json:"uriTemplate"`
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	MIMEType    string              `json:"mimeType,omitempty"`
	Annotations *resourceAnnotation `json:"annotations,omitempty"`
}

// resourceAnnotation carries audience and priority hints. Audience
// indicates who the resource is for ("user", "assistant", or both).
// Priority ranges from 0.0 (optional/background) to 1.0 (required).
type resourceAnnotation struct {
	Audience []string `json:"audience,omitempty"`
	Priority float64  `json:"priority,omitempty"`
}

// resourcesListResult is the result for resources/list. Includes both
// concrete resources (with final URIs) and resource templates (with
// URI templates that clients fill in with parameters).
type resourcesListResult struct {
	Resources         []resourceDescription `json:"resources"`
	ResourceTemplates []resourceTemplate    `json:"resourceTemplates,omitempty"`
	NextCursor        string                `json:"nextCursor,omitempty"`
}

// resourcesReadParams is the client's resources/read request parameters.
type resourcesReadParams struct {
	URI string `json:"uri"`
}

// resourcesReadResult is the server's resources/read response.
type resourcesReadResult struct {
	Contents []resourceContent `json:"contents"`
}

// resourceContent carries the content of a single resource. For Bureau
// resources, the Text field contains JSON-serialized structured data
// with a MIMEType of "application/json".
type resourceContent struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

// resourcesSubscribeParams is the client's resources/subscribe request.
type resourcesSubscribeParams struct {
	URI string `json:"uri"`
}

// resourcesUnsubscribeParams is the client's resources/unsubscribe request.
type resourcesUnsubscribeParams struct {
	URI string `json:"uri"`
}

// notification is a JSON-RPC 2.0 notification (no ID, no response
// expected). Used for server-initiated messages like
// notifications/resources/updated and notifications/tools/list_changed.
type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// resourceUpdatedParams is the params for the
// notifications/resources/updated notification. Sent to clients that
// have subscribed to a resource via resources/subscribe when the
// resource's content changes.
type resourceUpdatedParams struct {
	URI string `json:"uri"`
}
