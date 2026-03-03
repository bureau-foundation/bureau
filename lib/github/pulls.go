// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// ListPullRequestsOptions controls filtering and pagination for
// ListPullRequests.
type ListPullRequestsOptions struct {
	State   string // "open", "closed", "all" (default: "open")
	Sort    string // "created", "updated", "popularity", "long-running"
	PerPage int    // results per page (max 100, default 30)
}

func (options ListPullRequestsOptions) queryParams() string {
	values := url.Values{}
	if options.State != "" {
		values.Set("state", options.State)
	}
	if options.Sort != "" {
		values.Set("sort", options.Sort)
	}
	if options.PerPage > 0 {
		values.Set("per_page", strconv.Itoa(options.PerPage))
	}
	return values.Encode()
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

// MergePullRequest merges a pull request. The merge method ("merge",
// "squash", "rebase") is specified in the request. Returns the merge
// commit SHA and status. GitHub returns 405 Method Not Allowed if the
// PR is not mergeable (conflicts, status checks pending, etc.).
func (client *Client) MergePullRequest(ctx context.Context, owner, repo string, number int, request MergePullRequestRequest) (*PullRequestMergeResult, error) {
	var result PullRequestMergeResult
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", owner, repo, number)
	if err := client.put(ctx, path, request, &result); err != nil {
		return nil, fmt.Errorf("merging PR %s/%s#%d: %w", owner, repo, number, err)
	}
	return &result, nil
}

// ListPullRequests returns a paginated iterator over pull requests in
// a repository.
func (client *Client) ListPullRequests(ctx context.Context, owner, repo string, options ListPullRequestsOptions) *PageIterator[PullRequest] {
	basePath := fmt.Sprintf("/repos/%s/%s/pulls", owner, repo)
	return list[PullRequest](client, buildListPath(basePath, options))
}
