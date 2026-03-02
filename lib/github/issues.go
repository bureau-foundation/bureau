// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"
)

// CreateIssueRequest contains the fields for creating a new issue.
type CreateIssueRequest struct {
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
}

// UpdateIssueRequest contains the fields for updating an issue. Only
// non-nil fields are sent in the PATCH request.
type UpdateIssueRequest struct {
	Title     *string  `json:"title,omitempty"`
	Body      *string  `json:"body,omitempty"`
	State     *string  `json:"state,omitempty"` // "open" or "closed"
	Labels    []string `json:"labels,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
}

// ListIssuesOptions controls filtering and pagination for ListIssues.
type ListIssuesOptions struct {
	State   string // "open", "closed", "all" (default: "open")
	Sort    string // "created", "updated", "comments" (default: "created")
	PerPage int    // results per page (max 100, default 30)
}

func (options ListIssuesOptions) queryParams() string {
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
		return query[:len(query)-1] // trim trailing &
	}
	return ""
}

// CreateIssue creates a new issue in a repository.
func (client *Client) CreateIssue(ctx context.Context, owner, repo string, request CreateIssueRequest) (*Issue, error) {
	var issue Issue
	path := fmt.Sprintf("/repos/%s/%s/issues", owner, repo)
	if err := client.post(ctx, path, request, &issue); err != nil {
		return nil, fmt.Errorf("creating issue in %s/%s: %w", owner, repo, err)
	}
	return &issue, nil
}

// GetIssue retrieves a single issue by number.
func (client *Client) GetIssue(ctx context.Context, owner, repo string, number int) (*Issue, error) {
	var issue Issue
	path := fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, number)
	if err := client.get(ctx, path, &issue); err != nil {
		return nil, fmt.Errorf("getting issue %s/%s#%d: %w", owner, repo, number, err)
	}
	return &issue, nil
}

// UpdateIssue updates an existing issue.
func (client *Client) UpdateIssue(ctx context.Context, owner, repo string, number int, request UpdateIssueRequest) (*Issue, error) {
	var issue Issue
	path := fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, number)
	if err := client.patch(ctx, path, request, &issue); err != nil {
		return nil, fmt.Errorf("updating issue %s/%s#%d: %w", owner, repo, number, err)
	}
	return &issue, nil
}

// ListIssues returns a paginated iterator over issues in a repository.
func (client *Client) ListIssues(ctx context.Context, owner, repo string, options ListIssuesOptions) *PageIterator[Issue] {
	basePath := fmt.Sprintf("/repos/%s/%s/issues", owner, repo)
	return list[Issue](client, buildListPath(basePath, options))
}

// CreateIssueComment creates a comment on an issue or pull request.
func (client *Client) CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) (*Comment, error) {
	var comment Comment
	request := struct {
		Body string `json:"body"`
	}{Body: body}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, number)
	if err := client.post(ctx, path, request, &comment); err != nil {
		return nil, fmt.Errorf("creating comment on %s/%s#%d: %w", owner, repo, number, err)
	}
	return &comment, nil
}
