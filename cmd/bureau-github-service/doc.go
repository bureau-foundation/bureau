// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau GitHub connector service. Receives GitHub webhook events,
// translates them into provider-agnostic forge schema types, and
// dispatches them to subscribed agents via CBOR streams.
//
// Three interfaces:
//   - CBOR Unix socket: agent operations (subscribe, create-issue, etc.)
//   - HTTP listener: webhook ingestion from GitHub (HMAC-SHA256 verified)
//   - Matrix /sync: watches m.bureau.repository, m.bureau.forge_config,
//     m.bureau.forge_identity, and room membership changes
//
// Requires environment variables set by the launcher (BUREAU_PROXY_SOCKET,
// BUREAU_MACHINE_NAME, BUREAU_SERVER_NAME, BUREAU_PRINCIPAL_NAME,
// BUREAU_FLEET, BUREAU_SERVICE_SOCKET) plus:
//   - BUREAU_GITHUB_WEBHOOK_SECRET_FILE: path to file containing the
//     webhook HMAC secret
//   - BUREAU_GITHUB_WEBHOOK_LISTEN: TCP address for webhook ingestion
//     (default "127.0.0.1:9876")
package main
