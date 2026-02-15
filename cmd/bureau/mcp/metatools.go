// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// Meta-tool names. These synthetic tools provide progressive discovery
// of the real tool catalog, reducing the initial tool payload from
// O(n) tool descriptions to 3 fixed entries.
const (
	metaToolList     = "bureau_tools_list"
	metaToolDescribe = "bureau_tools_describe"
	metaToolCall     = "bureau_tools_call"
)

// --- Meta-tool parameter types ---

// metaListParams are the parameters for bureau_tools_list.
type metaListParams struct {
	Category string `json:"category" desc:"filter tools by category (e.g. 'ticket', 'pipeline')"`
}

// metaDescribeParams are the parameters for bureau_tools_describe.
type metaDescribeParams struct {
	Name string `json:"name" desc:"tool name to describe (e.g. bureau_pipeline_list)" required:"true"`
}

// metaCallParams are the parameters for bureau_tools_call. The
// Arguments field is json.RawMessage so the inner tool's arguments
// pass through without intermediate deserialization.
type metaCallParams struct {
	Name      string          `json:"name"      desc:"tool name to invoke" required:"true"`
	Arguments json.RawMessage `json:"arguments" desc:"tool-specific arguments as a JSON object"`
}

// --- Meta-tool output types ---

// toolSummary is a single entry in the bureau_tools_list output.
type toolSummary struct {
	Name     string `json:"name"     desc:"tool name (e.g. bureau_pipeline_list)"`
	Title    string `json:"title"    desc:"one-line summary of what the tool does"`
	Category string `json:"category" desc:"tool category derived from command path (e.g. pipeline, ticket)"`
}

// toolDetail is the full description returned by bureau_tools_describe.
type toolDetail struct {
	Name         string           `json:"name"                   desc:"tool name"`
	Title        string           `json:"title"                  desc:"one-line summary"`
	Description  string           `json:"description"            desc:"detailed description of the tool"`
	InputSchema  any              `json:"inputSchema"            desc:"JSON Schema for tool arguments"`
	OutputSchema any              `json:"outputSchema,omitempty" desc:"JSON Schema for tool output (if declared)"`
	Annotations  *toolAnnotations `json:"annotations,omitempty"  desc:"behavioral hints"`
	Examples     []toolExample    `json:"examples,omitempty"     desc:"usage examples"`
}

// toolExample is a usage example for a tool.
type toolExample struct {
	Description string `json:"description" desc:"what this example demonstrates"`
	Command     string `json:"command"     desc:"CLI command line"`
}

// isMetaTool returns true if the name is one of the three meta-tools.
func isMetaTool(name string) bool {
	return name == metaToolList || name == metaToolDescribe || name == metaToolCall
}

// metaToolDescriptions returns the toolDescription entries for the
// three meta-tools. Schemas are generated from the param/output
// structs using the same reflection machinery as real tools.
func metaToolDescriptions() []toolDescription {
	// bureau_tools_list uses ParamsSchema and OutputSchema since its
	// types are simple structs with only string fields.
	listInputSchema, _ := cli.ParamsSchema(&metaListParams{})
	listOutputSchema, _ := cli.OutputSchema(&[]toolSummary{})

	describeInputSchema, _ := cli.ParamsSchema(&metaDescribeParams{})

	// bureau_tools_describe output schema is built programmatically
	// because toolDetail contains interface{} fields (InputSchema,
	// OutputSchema) that the struct-tag schema reflection cannot
	// represent.
	describeOutputSchema := &cli.Schema{
		Type: "object",
		Properties: map[string]*cli.Schema{
			"name":         {Type: "string", Description: "tool name"},
			"title":        {Type: "string", Description: "one-line summary"},
			"description":  {Type: "string", Description: "detailed description of the tool"},
			"inputSchema":  {Type: "object", Description: "JSON Schema for tool arguments"},
			"outputSchema": {Type: "object", Description: "JSON Schema for tool output (if declared)"},
			"annotations":  {Type: "object", Description: "behavioral hints (read-only, destructive, etc.)"},
			"examples": {
				Type:        "array",
				Description: "usage examples",
				Items: &cli.Schema{
					Type: "object",
					Properties: map[string]*cli.Schema{
						"description": {Type: "string", Description: "what this example demonstrates"},
						"command":     {Type: "string", Description: "CLI command line"},
					},
				},
			},
		},
		Required: []string{"name", "title", "description", "inputSchema"},
	}

	// bureau_tools_call schema is built programmatically because the
	// Arguments field is json.RawMessage (opaque JSON passthrough),
	// which the struct-tag schema reflection doesn't handle.
	callInputSchema := &cli.Schema{
		Type: "object",
		Properties: map[string]*cli.Schema{
			"name": {
				Type:        "string",
				Description: "tool name to invoke (e.g. bureau_pipeline_list)",
			},
			"arguments": {
				Type:        "object",
				Description: "tool-specific arguments as a JSON object",
			},
		},
		Required: []string{"name"},
	}

	readOnlyAnnotations := &toolAnnotations{
		ReadOnlyHint:    boolPtr(true),
		DestructiveHint: boolPtr(false),
		IdempotentHint:  boolPtr(true),
		OpenWorldHint:   boolPtr(false),
	}

	return []toolDescription{
		{
			Name:  metaToolList,
			Title: "List available Bureau tools",
			Description: "Return a lightweight listing of all available Bureau tools " +
				"with names, one-line summaries, and categories. Use the category " +
				"parameter to filter by tool group (e.g. 'ticket', 'pipeline'). " +
				"Call bureau_tools_describe with a tool name to get full details " +
				"including input/output schemas before invoking a tool.",
			InputSchema:  listInputSchema,
			OutputSchema: listOutputSchema,
			Annotations:  readOnlyAnnotations,
		},
		{
			Name:  metaToolDescribe,
			Title: "Describe a Bureau tool's schema and usage",
			Description: "Return the full description of a Bureau tool including its " +
				"input schema (argument types, required fields, defaults), output " +
				"schema (if declared), behavioral annotations, and usage examples. " +
				"Use bureau_tools_list first to discover tool names.",
			InputSchema:  describeInputSchema,
			OutputSchema: describeOutputSchema,
			Annotations:  readOnlyAnnotations,
		},
		{
			Name:  metaToolCall,
			Title: "Invoke a Bureau tool",
			Description: "Invoke a Bureau tool by name with the specified arguments. " +
				"The arguments object must match the tool's input schema — use " +
				"bureau_tools_describe to check required fields and types. " +
				"Returns the tool's output as text content.",
			InputSchema: callInputSchema,
			// No outputSchema: the output type varies per inner tool.
			// No annotations: safety characteristics depend on the inner
			// tool, so we leave the MCP defaults (potentially destructive,
			// not idempotent, open world).
		},
	}
}

// dispatchMetaTool routes a meta-tool invocation to the appropriate
// handler.
func (s *Server) dispatchMetaTool(encoder *json.Encoder, req *request, name string, arguments json.RawMessage) error {
	switch name {
	case metaToolList:
		return s.handleMetaList(encoder, req, arguments)
	case metaToolDescribe:
		return s.handleMetaDescribe(encoder, req, arguments)
	case metaToolCall:
		return s.handleMetaCall(encoder, req, arguments)
	default:
		return writeError(encoder, req.ID, codeInvalidParams, "unknown tool: "+name)
	}
}

// handleMetaList implements bureau_tools_list: returns authorized
// tools as lightweight summaries filtered by optional category.
func (s *Server) handleMetaList(encoder *json.Encoder, req *request, arguments json.RawMessage) error {
	var params metaListParams
	if len(arguments) > 0 && string(arguments) != "null" {
		if err := json.Unmarshal(arguments, &params); err != nil {
			return writeError(encoder, req.ID, codeInvalidParams, "invalid arguments: "+err.Error())
		}
	}

	var summaries []toolSummary
	for i := range s.tools {
		t := &s.tools[i]
		if !s.toolAuthorized(t) {
			continue
		}
		category := toolCategory(t.name)
		if params.Category != "" && category != params.Category {
			continue
		}
		summaries = append(summaries, toolSummary{
			Name:     t.name,
			Title:    t.title,
			Category: category,
		})
	}
	if summaries == nil {
		summaries = []toolSummary{}
	}

	return writeMetaToolResult(encoder, req.ID, summaries)
}

// handleMetaDescribe implements bureau_tools_describe: returns the
// full description, schemas, annotations, and examples for a named
// tool.
func (s *Server) handleMetaDescribe(encoder *json.Encoder, req *request, arguments json.RawMessage) error {
	var params metaDescribeParams
	if len(arguments) > 0 && string(arguments) != "null" {
		if err := json.Unmarshal(arguments, &params); err != nil {
			return writeError(encoder, req.ID, codeInvalidParams, "invalid arguments: "+err.Error())
		}
	}
	if params.Name == "" {
		return writeError(encoder, req.ID, codeInvalidParams, "name is required")
	}

	t, ok := s.toolsByName[params.Name]
	if !ok {
		return writeError(encoder, req.ID, codeInvalidParams, "unknown tool: "+params.Name)
	}
	if !s.toolAuthorized(t) {
		return writeError(encoder, req.ID, codeInvalidParams, "tool not authorized: "+params.Name)
	}

	detail := toolDetail{
		Name:         t.name,
		Title:        t.title,
		Description:  t.description,
		InputSchema:  t.inputSchema,
		OutputSchema: t.outputSchema,
		Annotations:  t.annotations,
	}

	// Include CLI examples from the command.
	if t.command != nil {
		for _, example := range t.command.Examples {
			detail.Examples = append(detail.Examples, toolExample{
				Description: example.Description,
				Command:     example.Command,
			})
		}
	}

	return writeMetaToolResult(encoder, req.ID, detail)
}

// handleMetaCall implements bureau_tools_call: executes a tool by
// name with the provided arguments. The result passes through
// transparently — the inner tool's content blocks and error status
// are returned directly.
//
// No structuredContent is included because bureau_tools_call has no
// outputSchema (the output type varies per inner tool). Per the MCP
// specification, tools without outputSchema do not return
// structuredContent. The inner tool's JSON output is available in
// the text content block.
func (s *Server) handleMetaCall(encoder *json.Encoder, req *request, arguments json.RawMessage) error {
	var params metaCallParams
	if len(arguments) > 0 && string(arguments) != "null" {
		if err := json.Unmarshal(arguments, &params); err != nil {
			return writeError(encoder, req.ID, codeInvalidParams, "invalid arguments: "+err.Error())
		}
	}
	if params.Name == "" {
		return writeError(encoder, req.ID, codeInvalidParams, "name is required")
	}

	// Prevent recursive meta-tool calls.
	if isMetaTool(params.Name) {
		return writeError(encoder, req.ID, codeInvalidParams,
			fmt.Sprintf("meta-tools cannot be called through %s", metaToolCall))
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
	return writeResult(encoder, req.ID, result)
}

// writeMetaToolResult marshals a value to JSON and returns it as both
// a text content block and structuredContent. Used by meta-tools that
// declare output schemas (bureau_tools_list, bureau_tools_describe).
func writeMetaToolResult(encoder *json.Encoder, id json.RawMessage, value any) error {
	jsonBytes, err := json.Marshal(value)
	if err != nil {
		return writeError(encoder, id, codeInternalError, "marshaling result: "+err.Error())
	}

	return writeResult(encoder, id, toolsCallResult{
		Content: []contentBlock{{
			Type: "text",
			Text: string(jsonBytes),
		}},
		StructuredContent: value,
	})
}

// toolCategory extracts the category from a tool name. The category
// is the second segment of the underscore-joined path: "pipeline"
// from "bureau_pipeline_list", "ticket" from "bureau_ticket_create".
// For single-segment tools like "bureau_quickstart", the category
// equals the tool's own action name.
func toolCategory(name string) string {
	parts := strings.Split(name, "_")
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}
