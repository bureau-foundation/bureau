// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// defaultProxySocket is the well-known Unix socket path where the
// credential proxy listens inside a sandbox.
const defaultProxySocket = "/run/bureau/proxy.sock"

// fetchGrants retrieves the principal's authorization grants from the
// proxy socket. The proxy holds pre-resolved grants from the daemon;
// this call returns them so the MCP server can filter its tool list.
func fetchGrants() ([]schema.Grant, error) {
	socketPath := os.Getenv("BUREAU_PROXY_SOCKET")
	if socketPath == "" {
		socketPath = defaultProxySocket
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}

	response, err := client.Get("http://localhost/v1/grants")
	if err != nil {
		return nil, fmt.Errorf("fetching grants from proxy at %s: %w", socketPath, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy returned %s for GET /v1/grants", response.Status)
	}

	var grants []schema.Grant
	if err := json.NewDecoder(response.Body).Decode(&grants); err != nil {
		return nil, fmt.Errorf("decoding grants response: %w", err)
	}

	return grants, nil
}
