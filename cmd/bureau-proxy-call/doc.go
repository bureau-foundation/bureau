// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-proxy-call is a one-shot HTTP client for the bureau-proxy
// unix socket. It sends a single request and streams the response to
// stdout, enabling shell scripts and agent runtimes to interact with
// proxy endpoints without a persistent HTTP client.
package main
