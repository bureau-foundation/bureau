// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"
)

// ListPullRequestsOptions controls filtering and pagination for
// ListPullRequests.
type ListPullRequestsOptions struct {
	State   string // "open", "closed", "all" (default: "open")
	Sort    string // "created", "updated", "popularity", "long-running"
	PerPage int    // results per page (max 100, default 30)
}

func (options ListPullRequestsOptions) queryParams() string {
	query := ""
	if options.State != "" {
		query += "state=" + options.State + "&"
	}
	if options.Sort != "" {
		query += "sort=" + options.Sort + "&"
	}
	if options.PerPage > 0 {
		query += fmt.Sprintf("per_page=%d&", options.PerPage)
	}
	if query != "" {
		return query[:len(query)-1]
	}
	return ""
}

// GetPullRequest retrieves a single pull request by number.
func (client *Client) GetPullRequest(ctx context.Context, owner, repo string, number int) (*PullRequest, error) {
	var pullRequest PullRequest
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number)
	if err := client.get(ctx, path, &pullRequest); err != nil {
		return nil, fmt.Errorf("getting PR %s/%s#%d: %w", owner, repo, number, err)
	}
	return &pullRequest, nil
}

// ListPullRequests returns a paginated iterator over pull requests in
// a repository.
func (client *Client) ListPullRequests(ctx context.Context, owner, repo string, options ListPullRequestsOptions) *PageIterator[PullRequest] {
	basePath := fmt.Sprintf("/repos/%s/%s/pulls", owner, repo)
	return list[PullRequest](client, buildListPath(basePath, options))
}
