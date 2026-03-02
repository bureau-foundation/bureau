// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"
)

// CreateReviewRequest contains the fields for submitting a PR review.
type CreateReviewRequest struct {
	// Body is the review comment.
	Body string `json:"body,omitempty"`

	// Event is the review action: "APPROVE", "REQUEST_CHANGES", or "COMMENT".
	Event string `json:"event"`

	// Comments are inline comments on specific lines. Optional.
	Comments []ReviewComment `json:"comments,omitempty"`
}

// ReviewComment is an inline comment on a specific file and line.
type ReviewComment struct {
	Path     string `json:"path"`
	Position int    `json:"position,omitempty"` // diff position (deprecated, use line)
	Line     int    `json:"line,omitempty"`     // line number in the file
	Side     string `json:"side,omitempty"`     // "LEFT" or "RIGHT" for multi-line
	Body     string `json:"body"`
}

// CreateReview submits a review on a pull request.
func (client *Client) CreateReview(ctx context.Context, owner, repo string, prNumber int, request CreateReviewRequest) (*Review, error) {
	var review Review
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, prNumber)
	if err := client.post(ctx, path, request, &review); err != nil {
		return nil, fmt.Errorf("creating review on %s/%s#%d: %w", owner, repo, prNumber, err)
	}
	return &review, nil
}
