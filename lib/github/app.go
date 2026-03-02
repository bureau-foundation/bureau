// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"
)

// GetApp returns the authenticated GitHub App's metadata. Uses the
// App's JWT authentication (not an installation token).
func (client *Client) GetApp(ctx context.Context) (*App, error) {
	var app App
	if err := client.get(ctx, "/app", &app); err != nil {
		return nil, fmt.Errorf("getting app info: %w", err)
	}
	return &app, nil
}

// ListInstallations returns a paginated iterator over the authenticated
// App's installations.
func (client *Client) ListInstallations(ctx context.Context) *PageIterator[Installation] {
	return list[Installation](client, "/app/installations")
}
