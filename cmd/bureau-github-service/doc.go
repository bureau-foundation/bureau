// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau GitHub connector service. Receives GitHub webhook events,
// translates them into provider-agnostic forge schema types, and
// dispatches them to subscribed agents via CBOR streams.
//
// Three interfaces:
//   - CBOR Unix socket (service.sock): agent operations (subscribe,
//     create-issue, etc.)
//   - HTTP Unix socket (http.sock): webhook ingestion from GitHub
//     (HMAC-SHA256 verified). Consuming principals (e.g., an inbound
//     tunnel) mount this endpoint via RequiredServices as "github:http".
//   - Matrix /sync: watches m.bureau.repository, m.bureau.forge_config,
//     m.bureau.forge_identity, and room membership changes
//
// Requires environment variables set by the launcher (BUREAU_PROXY_SOCKET,
// BUREAU_MACHINE_NAME, BUREAU_SERVER_NAME, BUREAU_PRINCIPAL_NAME,
// BUREAU_FLEET, BUREAU_SERVICE_SOCKET) plus:
//   - BUREAU_GITHUB_WEBHOOK_SECRET_FILE: path to file containing the
//     webhook HMAC secret
//   - BUREAU_GITHUB_WEBHOOK_SOCKET: Unix socket path for webhook
//     ingestion (set by the github-service template, typically
//     /run/bureau/listen/http.sock)
package main
