// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"
)

// CreateStatusRequest contains the fields for creating a commit status.
type CreateStatusRequest struct {
	// State is the status state: "error", "failure", "pending", or "success".
	State string `json:"state"`

	// TargetURL is the URL to associate with this status. Shown as
	// "Details" link in the GitHub UI.
	TargetURL string `json:"target_url,omitempty"`

	// Description is a short human-readable description of the status.
	// Max 140 characters.
	Description string `json:"description,omitempty"`

	// Context is the status context identifier. Multiple statuses can
	// exist for the same SHA with different contexts (e.g.,
	// "ci/bureau-pipeline", "review/code-review").
	Context string `json:"context,omitempty"`
}

// CreateCommitStatus creates a status on a commit. The sha is the full
// 40-character commit SHA.
func (client *Client) CreateCommitStatus(ctx context.Context, owner, repo, sha string, request CreateStatusRequest) (*CommitStatus, error) {
	var status CommitStatus
	path := fmt.Sprintf("/repos/%s/%s/statuses/%s", owner, repo, sha)
	if err := client.post(ctx, path, request, &status); err != nil {
		return nil, fmt.Errorf("creating status on %s/%s@%s: %w", owner, repo, sha[:minInt(len(sha), 8)], err)
	}
	return &status, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
