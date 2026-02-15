// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package mcp implements a Model Context Protocol server that exposes
// Bureau CLI commands as MCP tools over newline-delimited JSON-RPC 2.0
// on stdin/stdout.
//
// The server discovers tools by walking the CLI command tree and
// collecting leaf commands that have a [cli.Command.Params] function.
// Each discovered command becomes an MCP tool with inputSchema
// generated from the parameter struct's tags via [cli.ParamsSchema].
// Commands that declare [cli.Command.Output] also get an outputSchema
// reflected from the output type via [cli.OutputSchema], and their
// results include structuredContent (parsed JSON) alongside the text
// content block.
//
// Tool names are underscore-joined command paths (e.g.,
// "bureau_pipeline_list" for "bureau pipeline list"). Arguments are
// JSON objects matching the parameter struct's json tags, with
// validation enforced by the JSON Schema.
//
// Tools are filtered by the principal's authorization grants, fetched
// from the credential proxy at startup. Commands must declare
// [cli.Command.RequiredGrants] to be visible; commands without grants
// are hidden (default-deny). This prevents sandboxed agents from
// seeing or invoking tools they are not authorized to use.
//
// This package implements the 2025-11-25 MCP protocol specification.
package mcp
