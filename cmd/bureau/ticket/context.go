// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"context"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/lib/service"
)

// resolveContextID queries the agent wrapper's context socket for the
// current ctx-* identifier. Returns empty string if the socket does
// not exist (running outside a sandbox) or the query fails. Failures
// are silent: context capture is best-effort and must not block ticket
// operations.
func resolveContextID() string {
	socketPath := os.Getenv("BUREAU_AGENT_CONTEXT_SOCKET")
	if socketPath == "" {
		return ""
	}
	if _, err := os.Stat(socketPath); err != nil {
		return ""
	}

	client := service.NewServiceClientFromToken(socketPath, nil)
	var response struct {
		ContextID string `cbor:"context_id"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Call(ctx, "current-context", nil, &response); err != nil {
		return ""
	}
	return response.ContextID
}
