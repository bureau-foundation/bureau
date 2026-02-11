// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-proxy is the per-sandbox credential injection proxy. It holds
// a principal's Matrix access token and external API keys, serving them
// to the sandboxed process via HTTP on a unix socket. The proxy is the
// universal agent API: any process that can HTTP to localhost is a
// Bureau agent.
package main
