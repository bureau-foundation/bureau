// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package model provides CLI commands for interacting with the Bureau
// model service. Commands connect to the model service's Unix socket
// to send completion and embedding requests, list available models,
// and inspect quota usage.
//
// Inside a sandbox, the socket and token paths default to the standard
// provisioned locations (/run/bureau/service/model.sock and
// /run/bureau/service/token/model.token). Outside a sandbox, use
// --service mode (requires "bureau login") or explicit --socket and
// --token-file flags.
package model
