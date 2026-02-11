// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package proxy provides credential injection for sandboxed agent processes.
//
// The proxy is Bureau's universal agent API: any process that can make HTTP
// requests to a Unix socket is a Bureau agent. The proxy holds secrets (API
// keys, Matrix tokens) that the sandbox never sees, injecting them into
// outbound requests on the agent's behalf.
//
// [Server] listens on two Unix sockets: an agent-facing socket for service
// requests, and an admin-facing socket for daemon configuration (service
// directory updates, observation forwarding setup). It optionally also
// listens on TCP for use behind the bridge package in network-isolated
// sandboxes.
//
// Two service types are supported. [CLIService] executes host binaries with
// credentials injected as environment variables or temporary files, returning
// stdout/stderr to the agent. [HTTPService] reverse-proxies HTTP requests to
// upstream APIs with credentials injected as HTTP headers, supporting both
// request/response and SSE streaming. Services are configured via YAML
// ([Config]) or registered programmatically (the built-in Matrix proxy is
// auto-registered when Matrix credentials are present).
//
// [Handler] dispatches requests by path: /cli/{service} for CLI services,
// /http/{service}/... for HTTP services, and /services for the filtered
// service directory. Matrix API requests are subject to MatrixPolicy
// enforcement, which gates room creation, joins, and invites independently.
// The service directory filters entries by each sandbox's visibility policy
// ([Filter]), preventing agents from enumerating the organization's full
// service inventory.
//
// Credential sources ([CredentialSource]) form a chain: environment variables,
// files, systemd credentials, stdin pipe (production credential delivery from
// the launcher), and in-memory maps. The pipe source reads a JSON credential
// payload from stdin, stores tokens in mmap-backed secret.Buffer memory
// (locked against swap, excluded from core dumps), and zeroes the raw input
// buffer after parsing.
//
// Observation requests from inside the sandbox are transparently forwarded
// to the daemon via [observeProxy], which rewrites the JSON handshake to
// inject the sandbox's Matrix credentials for authorization.
package proxy
