// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/version"
)

// Server is an MCP server that exposes Bureau CLI commands as tools
// over JSON-RPC 2.0 on newline-delimited stdio.
type Server struct {
	tools       []tool
	toolsByName map[string]*tool
	initialized bool
}

// tool is a discovered CLI command exposed as an MCP tool.
type tool struct {
	name        string
	description string
	inputSchema *cli.Schema
	command     *cli.Command
}

// NewServer creates an MCP server by walking the command tree to
// discover all commands with a Params function. Each such command
// becomes an MCP tool with a JSON Schema derived from its parameter
// struct.
func NewServer(root *cli.Command) *Server {
	s := &Server{}

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
				return fmt.Errorf("writing parse error response: %w", writeErr)
			}
			continue
		}

		if req.JSONRPC != "2.0" {
			if !req.isNotification() {
				if writeErr := writeError(encoder, req.ID, codeInvalidRequest, "unsupported JSON-RPC version"); writeErr != nil {
					return fmt.Errorf("writing version error response: %w", writeErr)
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

	if params.ProtocolVersion != protocolVersion {
		return writeError(encoder, req.ID, codeInvalidRequest,
			fmt.Sprintf("unsupported protocol version %q (server supports %q)",
				params.ProtocolVersion, protocolVersion))
	}

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
	descriptions := make([]toolDescription, len(s.tools))
	for i, t := range s.tools {
		descriptions[i] = toolDescription{
			Name:        t.name,
			Description: t.description,
			InputSchema: t.inputSchema,
		}
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

	t, ok := s.toolsByName[params.Name]
	if !ok {
		return writeError(encoder, req.ID, codeInvalidParams, "unknown tool: "+params.Name)
	}

	output, runErr := s.executeTool(t, params.Arguments)

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
	}
	// MCP requires at least one content block in the result.
	if len(result.Content) == 0 {
		result.Content = []contentBlock{{Type: "text", Text: ""}}
	}

	return writeResult(encoder, req.ID, result)
}

// executeTool runs a CLI command as an MCP tool, capturing stdout.
// Parameters are zeroed, defaults applied from flag tags, JSON
// arguments overlaid, and JSON output mode forced before execution.
func (s *Server) executeTool(t *tool, arguments json.RawMessage) (string, error) {
	// Get the closure-captured params pointer and zero it. This
	// prevents state from a previous call from bleeding through.
	params := t.command.Params()
	reflect.ValueOf(params).Elem().SetZero()

	// Apply defaults from struct tags. Calling Flags() triggers flag
	// registration, and pflag sets *target = defaultValue during
	// registration. This reuses the existing default-parsing logic
	// rather than duplicating it.
	if t.command.Flags != nil {
		t.command.Flags()
	}

	// Overlay with the JSON arguments from the MCP client.
	if len(arguments) > 0 && string(arguments) != "null" {
		if err := json.Unmarshal(arguments, params); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}

	// Force JSON output when the command supports it. Table formatting
	// is for human terminals; MCP tool output should be structured.
	enableJSONOutput(params)

	return captureRun(t.command.Run)
}

// enableJSONOutput sets the JSON output flag to true if the params
// struct has a bool field tagged flag:"json". Commands with this
// convention produce structured JSON instead of tabwriter tables.
func enableJSONOutput(params any) {
	value := reflect.ValueOf(params).Elem()
	structType := value.Type()
	for i := range structType.NumField() {
		field := structType.Field(i)
		if field.Tag.Get("flag") == "json" && field.Type.Kind() == reflect.Bool {
			value.Field(i).SetBool(true)
			return
		}
	}
}

// captureRun executes a Run function while capturing its stdout. A
// goroutine reads from the pipe concurrently to prevent deadlock
// when the command produces more output than the OS pipe buffer.
func captureRun(run func([]string) error) (string, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return "", fmt.Errorf("creating output pipe: %w", err)
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
		return "", fmt.Errorf("reading captured output: %w", captured.err)
	}

	return string(captured.data), runErr
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
		schema, err := cli.ParamsSchema(command.Params())
		if err != nil {
			fmt.Fprintf(os.Stderr, "mcp: skipping %s: schema error: %v\n",
				strings.Join(current, "_"), err)
		} else {
			*tools = append(*tools, tool{
				name:        strings.Join(current, "_"),
				description: toolDescriptionText(command),
				inputSchema: schema,
				command:     command,
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
