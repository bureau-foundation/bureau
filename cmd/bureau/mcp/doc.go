// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package mcp implements a Model Context Protocol server that exposes
// Bureau CLI commands as MCP tools over newline-delimited JSON-RPC 2.0
// on stdin/stdout.
//
// The server discovers tools by walking the CLI command tree and
// collecting leaf commands that have a [cli.Command.Params] function.
// Each discovered command becomes an MCP tool with a JSON Schema
// generated from the parameter struct's tags via [cli.ParamsSchema].
//
// Tool names are underscore-joined command paths (e.g.,
// "bureau_pipeline_list" for "bureau pipeline list"). Arguments are
// JSON objects matching the parameter struct's json tags, with
// validation enforced by the JSON Schema.
//
// This package implements the 2024-11-05 MCP protocol specification.
package mcp
