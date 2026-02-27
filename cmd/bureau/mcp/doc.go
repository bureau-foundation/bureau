// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package mcp implements a Model Context Protocol server that exposes
// Bureau CLI commands as MCP tools and Bureau service state as MCP
// resources over newline-delimited JSON-RPC 2.0 on stdin/stdout.
//
// # Tools
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
// # Progressive disclosure
//
// With [WithProgressiveDisclosure], the server exposes three
// meta-tools instead of the full tool catalog:
//
//   - bureau_tools_list: lightweight listing of tool names, summaries,
//     and categories, with optional category filtering and BM25 search.
//   - bureau_tools_describe: full tool description including
//     inputSchema, outputSchema, annotations, and examples.
//   - bureau_tools_call: invoke a tool by name with arguments.
//
// This reduces the initial tool payload from O(n) tool descriptions
// to 3 fixed entries, which matters for agents with tight context
// budgets. Authorization grants still apply: meta-tools only expose
// and execute tools the principal is authorized to use.
//
// # Resources
//
// The server exposes Bureau service state as MCP resources via the
// [ResourceProvider] interface. Resources use bureau:// URIs:
//
//   - bureau://identity: principal identity, grants, and services.
//     Static for the sandbox lifetime. No subscription support.
//   - bureau://tickets/{room_alias}: tickets in a room.
//   - bureau://tickets/{room_alias}/ready: actionable tickets.
//   - bureau://tickets/{room_alias}/blocked: blocked tickets.
//
// Resources support subscriptions (resources/subscribe). The ticket
// provider streams changes from the ticket service's subscribe
// protocol and translates mutations into notifications/resources/updated
// notifications. The agent's MCP client receives these notifications
// and can re-read the resource to get updated state.
//
// Resource providers are discovered at startup by probing for
// well-known service sockets in the sandbox. Providers that are not
// available (missing socket or token) are silently skipped.
//
// # Authorization
//
// Tools use "command/" grant action patterns (e.g., "command/ticket/list").
// Resources use "resource/" grant action patterns (e.g., "resource/ticket/**").
// The two namespaces are independent: a principal can have resource access
// without tool access, and vice versa. Service tokens independently enforce
// their own grants on the socket side, providing defense in depth.
//
// This package implements the 2025-11-25 MCP protocol specification.
package mcp
