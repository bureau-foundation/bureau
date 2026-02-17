// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/authorization"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/messaging"
)

// Server is an MCP server that exposes Bureau CLI commands as tools
// over JSON-RPC 2.0 on newline-delimited stdio.
type Server struct {
	tools       []tool
	toolsByName map[string]*tool
	grants      []schema.Grant
	initialized bool
	progressive bool
}

// ServerOption configures optional server behavior.
type ServerOption func(*Server)

// WithProgressiveDisclosure enables meta-tool mode. Instead of
// exposing all discovered tools directly in tools/list, the server
// exposes three synthetic meta-tools (bureau_tools_list,
// bureau_tools_describe, bureau_tools_call) that let agents discover
// and invoke tools on demand.
//
// This reduces the initial tool catalog from O(n) tool descriptions
// (~200-500 tokens each) to 3 fixed entries (~600 tokens total),
// which is significant for agents with tight context budgets.
//
// In progressive mode, the real tools are still discovered and stored
// internally — they're accessed through the meta-tools rather than
// exposed directly.
func WithProgressiveDisclosure() ServerOption {
	return func(s *Server) {
		s.progressive = true
	}
}

// tool is a discovered CLI command exposed as an MCP tool.
type tool struct {
	name         string
	title        string
	description  string
	annotations  *toolAnnotations
	inputSchema  *cli.Schema
	outputSchema *cli.Schema
	command      *cli.Command
}

// NewServer creates an MCP server by walking the command tree to
// discover all commands with a Params function. Each such command
// becomes an MCP tool with a JSON Schema derived from its parameter
// struct.
//
// Grants control which tools are visible and callable. Only tools
// whose RequiredGrants are all satisfied by the provided grants
// appear in tools/list and are accepted by tools/call. Commands
// without RequiredGrants are hidden (default-deny). Use a wildcard
// grant (actions: ["command/**"]) for operator/development access.
func NewServer(root *cli.Command, grants []schema.Grant, options ...ServerOption) *Server {
	s := &Server{grants: grants}
	for _, option := range options {
		option(s)
	}

	discoverTools(root, nil, &s.tools)

	s.toolsByName = make(map[string]*tool, len(s.tools))
	for i := range s.tools {
		s.toolsByName[s.tools[i].name] = &s.tools[i]
	}

	return s
}

// Serve starts the MCP server reading from os.Stdin and writing to
// os.Stdout. This is the entry point for "bureau mcp serve".
func (s *Server) Serve() error {
	return s.Run(os.Stdin, os.Stdout)
}

// Run processes JSON-RPC 2.0 requests from input and writes responses
// to output until input reaches EOF. Each request occupies a single
// line (newline-delimited JSON-RPC, not Content-Length framed).
func (s *Server) Run(input io.Reader, output io.Writer) error {
	scanner := bufio.NewScanner(input)
	// MCP messages can be large (tool results with verbose output).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	encoder := json.NewEncoder(output)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			if writeErr := writeError(encoder, json.RawMessage("null"), codeParseError, "parse error: "+err.Error()); writeErr != nil {
				return cli.Internal("writing parse error response: %w", writeErr)
			}
			continue
		}

		if req.JSONRPC != "2.0" {
			if !req.isNotification() {
				if writeErr := writeError(encoder, req.ID, codeInvalidRequest, "unsupported JSON-RPC version"); writeErr != nil {
					return cli.Internal("writing version error response: %w", writeErr)
				}
			}
			continue
		}

		// Notifications have no ID and receive no response.
		if req.isNotification() {
			continue
		}

		if err := s.dispatch(encoder, &req); err != nil {
			return err
		}
	}

	return scanner.Err()
}

// dispatch routes a JSON-RPC request to the appropriate handler.
func (s *Server) dispatch(encoder *json.Encoder, req *request) error {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(encoder, req)
	case "ping":
		return s.handlePing(encoder, req)
	case "tools/list":
		if !s.initialized {
			return writeError(encoder, req.ID, codeInvalidRequest, "server not initialized (call initialize first)")
		}
		return s.handleToolsList(encoder, req)
	case "tools/call":
		if !s.initialized {
			return writeError(encoder, req.ID, codeInvalidRequest, "server not initialized (call initialize first)")
		}
		return s.handleToolsCall(encoder, req)
	default:
		return writeError(encoder, req.ID, codeMethodNotFound, "unknown method: "+req.Method)
	}
}

func (s *Server) handleInitialize(encoder *json.Encoder, req *request) error {
	if len(req.Params) == 0 {
		return writeError(encoder, req.ID, codeInvalidParams, "params required for initialize")
	}

	var params initializeParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return writeError(encoder, req.ID, codeInvalidParams, "invalid initialize params: "+err.Error())
	}

	// The MCP specification says the server responds with its own
	// protocol version and the client decides whether it can proceed.
	// We do not reject clients that request a different version —
	// all MCP versions are additive, so older clients will simply
	// ignore fields they don't recognize.
	s.initialized = true

	return writeResult(encoder, req.ID, initializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities: serverCapabilities{
			Tools: &toolCapability{},
		},
		ServerInfo: serverInfo{
			Name:    "bureau",
			Version: version.Short(),
		},
	})
}

func (s *Server) handlePing(encoder *json.Encoder, req *request) error {
	return writeResult(encoder, req.ID, map[string]any{})
}

func (s *Server) handleToolsList(encoder *json.Encoder, req *request) error {
	if s.progressive {
		return writeResult(encoder, req.ID, toolsListResult{
			Tools: metaToolDescriptions(),
		})
	}

	var descriptions []toolDescription
	for _, t := range s.tools {
		if !s.toolAuthorized(&t) {
			continue
		}
		descriptions = append(descriptions, toolDescription{
			Name:         t.name,
			Title:        t.title,
			Description:  t.description,
			InputSchema:  t.inputSchema,
			OutputSchema: t.outputSchema,
			Annotations:  t.annotations,
		})
	}
	if descriptions == nil {
		descriptions = []toolDescription{}
	}
	return writeResult(encoder, req.ID, toolsListResult{Tools: descriptions})
}

func (s *Server) handleToolsCall(encoder *json.Encoder, req *request) error {
	if len(req.Params) == 0 {
		return writeError(encoder, req.ID, codeInvalidParams, "params required for tools/call")
	}

	var params toolsCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return writeError(encoder, req.ID, codeInvalidParams, "invalid tools/call params: "+err.Error())
	}

	// In progressive mode, only meta-tools are callable directly.
	// Real tools must be invoked through bureau_tools_call.
	if s.progressive {
		if isMetaTool(params.Name) {
			return s.dispatchMetaTool(encoder, req, params.Name, params.Arguments)
		}
		return writeError(encoder, req.ID, codeInvalidParams,
			fmt.Sprintf("tool %q is not directly callable in progressive mode; use %s", params.Name, metaToolCall))
	}

	t, ok := s.toolsByName[params.Name]
	if !ok {
		return writeError(encoder, req.ID, codeInvalidParams, "unknown tool: "+params.Name)
	}

	if !s.toolAuthorized(t) {
		return writeError(encoder, req.ID, codeInvalidParams, "tool not authorized: "+params.Name)
	}

	output, runErr := s.executeTool(t, params.Arguments)
	result := buildToolResult(output, runErr)

	// When the tool declares an output schema and the call succeeded,
	// parse the captured JSON output into structuredContent. Per the
	// MCP specification, tools with outputSchema MUST return both
	// structuredContent (typed JSON) and a text content block
	// (serialized JSON for backward compatibility).
	if t.outputSchema != nil && !result.IsError && output != "" {
		var structured any
		if parseErr := json.Unmarshal([]byte(output), &structured); parseErr != nil {
			// The command declared an output schema but produced
			// output that doesn't parse as JSON. This is a bug in
			// the command, not a runtime error. Surface it as a
			// tool error so it's visible to both the agent and the
			// operator.
			result.IsError = true
			result.Content = append(result.Content, contentBlock{
				Type: "text",
				Text: fmt.Sprintf("output schema violation: command produced non-JSON output: %v", parseErr),
			})
		} else {
			result.StructuredContent = structured
		}
	}

	return writeResult(encoder, req.ID, result)
}

// buildToolResult assembles a toolsCallResult from captured output
// and an optional run error. This is the common base for both direct
// tool calls and meta-tool call-through results.
func buildToolResult(output string, runErr error) toolsCallResult {
	result := toolsCallResult{}
	if output != "" {
		result.Content = append(result.Content, contentBlock{
			Type: "text",
			Text: output,
		})
	}
	if runErr != nil {
		result.IsError = true
		result.Content = append(result.Content, contentBlock{
			Type: "text",
			Text: runErr.Error(),
		})
		result.ErrorInfo = classifyError(runErr)
	}
	// MCP requires at least one content block in the result.
	if len(result.Content) == 0 {
		result.Content = []contentBlock{{Type: "text", Text: ""}}
	}
	return result
}

// classifyError extracts structured error metadata from an error.
// It checks for ToolError first (the primary path after migration),
// then falls back to known error types (MatrixError, context errors)
// for defense in depth.
func classifyError(err error) *errorInfo {
	var toolErr *cli.ToolError
	if errors.As(err, &toolErr) {
		return &errorInfo{
			Category:  string(toolErr.Category),
			Retryable: toolErr.Category == cli.CategoryTransient,
		}
	}

	// Fallback: classify known library error types that might not
	// be wrapped in a ToolError.
	var matrixErr *messaging.MatrixError
	if errors.As(err, &matrixErr) {
		return classifyMatrixError(matrixErr)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &errorInfo{Category: string(cli.CategoryTransient), Retryable: true}
	}

	return &errorInfo{Category: string(cli.CategoryInternal), Retryable: false}
}

// classifyMatrixError maps a Matrix homeserver error to an error category.
func classifyMatrixError(err *messaging.MatrixError) *errorInfo {
	switch err.Code {
	case messaging.ErrCodeForbidden, messaging.ErrCodeUnknownToken:
		return &errorInfo{Category: string(cli.CategoryForbidden), Retryable: false}
	case messaging.ErrCodeNotFound:
		return &errorInfo{Category: string(cli.CategoryNotFound), Retryable: false}
	case messaging.ErrCodeLimitExceeded:
		return &errorInfo{Category: string(cli.CategoryTransient), Retryable: true}
	case messaging.ErrCodeUserInUse, messaging.ErrCodeRoomInUse, messaging.ErrCodeExclusive:
		return &errorInfo{Category: string(cli.CategoryConflict), Retryable: false}
	case messaging.ErrCodeInvalidParam, messaging.ErrCodeMissingParam:
		return &errorInfo{Category: string(cli.CategoryValidation), Retryable: false}
	default:
		return &errorInfo{Category: string(cli.CategoryInternal), Retryable: false}
	}
}

// toolAuthorized returns true if the principal's grants satisfy all of
// the tool's required grants. Commands without RequiredGrants are hidden
// (default-deny): every MCP-eligible command must declare its grants.
func (s *Server) toolAuthorized(t *tool) bool {
	if len(t.command.RequiredGrants) == 0 {
		return false
	}
	for _, action := range t.command.RequiredGrants {
		if !authorization.GrantsAllow(s.grants, action, "") {
			return false
		}
	}
	return true
}

// executeTool runs a CLI command as an MCP tool, capturing stdout.
// Parameters are zeroed, defaults applied from flag tags, JSON
// arguments overlaid, and JSON output mode forced before execution.
func (s *Server) executeTool(t *tool, arguments json.RawMessage) (string, error) {
	// Get the closure-captured params pointer and zero it. This
	// prevents state from a previous call from bleeding through.
	params := t.command.Params()
	reflect.ValueOf(params).Elem().SetZero()

	// Apply defaults from struct tags. FlagSet() triggers flag
	// registration (either from the explicit Flags function or
	// auto-derived from Params), and pflag sets *target = defaultValue
	// during registration. This reuses the existing default-parsing
	// logic rather than duplicating it.
	t.command.FlagSet()

	// Overlay with the JSON arguments from the MCP client.
	if len(arguments) > 0 && string(arguments) != "null" {
		if err := json.Unmarshal(arguments, params); err != nil {
			return "", cli.Validation("invalid arguments: %w", err)
		}
	}

	// Force JSON output when the command supports it. Table formatting
	// is for human terminals; MCP tool output should be structured.
	enableJSONOutput(params)

	return captureRun(t.command.Run)
}

// enableJSONOutput forces JSON output mode on params structs that
// embed [cli.JSONOutput]. Commands produce structured JSON instead
// of tabwriter tables when invoked as MCP tools.
func enableJSONOutput(params any) {
	if j, ok := params.(cli.JSONOutputter); ok {
		j.SetJSONOutput(true)
	}
}

// captureRun executes a Run function while capturing its stdout. A
// goroutine reads from the pipe concurrently to prevent deadlock
// when the command produces more output than the OS pipe buffer.
func captureRun(run func([]string) error) (string, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return "", cli.Internal("creating output pipe: %w", err)
	}

	saved := os.Stdout
	os.Stdout = writer

	type capturedOutput struct {
		data []byte
		err  error
	}
	done := make(chan capturedOutput, 1)
	go func() {
		data, readErr := io.ReadAll(reader)
		done <- capturedOutput{data, readErr}
	}()

	runErr := run(nil)

	// Restore stdout before closing the pipe so that any
	// subsequent writes go to the real output destination.
	os.Stdout = saved
	writer.Close()

	captured := <-done
	reader.Close()

	if captured.err != nil {
		return "", cli.Internal("reading captured output: %w", captured.err)
	}

	return string(captured.data), runErr
}

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
}

// AuthorizedTools returns tool metadata for all tools that pass
// authorization checks. Used by the native agent to build the LLM
// tool catalog from the same discovery and grant-filtering logic
// as the MCP JSON-RPC tools/list endpoint.
func (s *Server) AuthorizedTools() []ToolExport {
	var exports []ToolExport
	for i := range s.tools {
		t := &s.tools[i]
		if !s.toolAuthorized(t) {
			continue
		}
		schemaJSON, err := json.Marshal(t.inputSchema)
		if err != nil {
			// Schema generation is deterministic — if it marshaled
			// once during discovery, it will marshal again. A failure
			// here would indicate a programming error, not a runtime
			// condition.
			continue
		}
		exports = append(exports, ToolExport{
			Name:        t.name,
			Description: t.description,
			InputSchema: schemaJSON,
		})
	}
	return exports
}

// CallTool executes a tool by name with the given JSON arguments,
// using the same parameter zeroing, default application, JSON overlay,
// and stdout capture as the MCP tools/call endpoint. Returns the
// captured output text and whether the tool reported an error.
//
// A non-nil error return indicates an infrastructure failure (unknown
// tool, authorization denied) — not a tool execution failure. Tool
// execution failures are indicated by isError=true with the error
// message included in the output string.
func (s *Server) CallTool(name string, arguments json.RawMessage) (output string, isError bool, err error) {
	t, ok := s.toolsByName[name]
	if !ok {
		return "", false, fmt.Errorf("unknown tool: %s", name)
	}
	if !s.toolAuthorized(t) {
		return "", false, fmt.Errorf("tool not authorized: %s", name)
	}

	out, runErr := s.executeTool(t, arguments)
	if runErr != nil {
		// Combine captured output and error message into a single
		// string, matching the behavior of buildToolResult for MCP.
		if out != "" {
			out += "\n"
		}
		out += runErr.Error()
		return out, true, nil
	}
	return out, false, nil
}

// discoverTools walks the command tree recursively, collecting leaf
// commands that have both Params and Run as MCP tools.
func discoverTools(command *cli.Command, path []string, tools *[]tool) {
	// Build the current path with a fresh slice to avoid aliasing
	// across recursive calls that share the same path prefix.
	current := make([]string, len(path)+1)
	copy(current, path)
	current[len(path)] = command.Name

	if command.Params != nil && command.Run != nil {
		toolName := strings.Join(current, "_")

		inputSchema, err := cli.ParamsSchema(command.Params())
		if err != nil {
			fmt.Fprintf(os.Stderr, "mcp: skipping %s: input schema error: %v\n",
				toolName, err)
		} else {
			var outputSchema *cli.Schema
			if command.Output != nil {
				outSchema, outErr := cli.OutputSchema(command.Output())
				if outErr != nil {
					fmt.Fprintf(os.Stderr, "mcp: %s: output schema error: %v\n",
						toolName, outErr)
				} else {
					outputSchema = outSchema
				}
			}

			*tools = append(*tools, tool{
				name:         toolName,
				title:        command.Summary,
				description:  toolDescriptionText(command),
				annotations:  resolveAnnotations(command),
				inputSchema:  inputSchema,
				outputSchema: outputSchema,
				command:      command,
			})
		}
	}

	for _, sub := range command.Subcommands {
		discoverTools(sub, current, tools)
	}
}

// toolDescriptionText returns the best available description for a
// command, preferring the detailed Description over the Summary.
func toolDescriptionText(command *cli.Command) string {
	if command.Description != "" {
		return command.Description
	}
	return command.Summary
}

// resolveAnnotations translates a command's behavioral annotations into
// MCP protocol hints. When a command declares explicit [cli.ToolAnnotations],
// those are used directly. Otherwise, the function falls back to
// name-based inference as a safety net for commands that haven't been
// updated yet — "list" commands are assumed read-only and idempotent.
//
// Returns nil when no annotations are available (neither explicit nor
// inferred), letting MCP clients apply the spec defaults (destructive,
// non-idempotent, open-world).
func resolveAnnotations(command *cli.Command) *toolAnnotations {
	if command.Annotations != nil {
		return &toolAnnotations{
			ReadOnlyHint:    command.Annotations.ReadOnly,
			DestructiveHint: command.Annotations.Destructive,
			IdempotentHint:  command.Annotations.Idempotent,
			OpenWorldHint:   command.Annotations.OpenWorld,
		}
	}

	// Heuristic fallback for commands that haven't declared annotations.
	switch command.Name {
	case "list":
		return &toolAnnotations{
			ReadOnlyHint:    boolPtr(true),
			DestructiveHint: boolPtr(false),
			IdempotentHint:  boolPtr(true),
			OpenWorldHint:   boolPtr(false),
		}
	default:
		return nil
	}
}

func boolPtr(value bool) *bool {
	return &value
}

// writeResult sends a JSON-RPC 2.0 success response.
func writeResult(encoder *json.Encoder, id json.RawMessage, result any) error {
	return encoder.Encode(response{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

// writeError sends a JSON-RPC 2.0 error response.
func writeError(encoder *json.Encoder, id json.RawMessage, code int, message string) error {
	return encoder.Encode(response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: message},
	})
}
