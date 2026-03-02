// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"
	"net/url"
)

// CreateTreeRequest contains the fields for creating a git tree via
// the GitHub API. This is the first step of the API-mediated commit
// path: tree → commit → ref update.
type CreateTreeRequest struct {
	// BaseTree is the SHA of the base tree to apply changes to. If
	// empty, creates a tree from scratch (unusual).
	BaseTree string `json:"base_tree,omitempty"`

	// Entries are the tree entries to create or modify.
	Entries []CreateTreeEntry `json:"tree"`
}

// CreateTreeEntry describes a single entry in a tree creation request.
type CreateTreeEntry struct {
	// Path is the file path relative to the repository root.
	Path string `json:"path"`

	// Mode is the file mode: "100644" (regular), "100755" (executable),
	// "120000" (symlink), "160000" (submodule), "040000" (directory).
	Mode string `json:"mode"`

	// Type is the object type: "blob", "tree", or "commit".
	Type string `json:"type"`

	// Content is the file content for inline blob creation. Mutually
	// exclusive with SHA — use Content for small files (<100 KB) and
	// SHA for pre-uploaded blobs.
	Content *string `json:"content,omitempty"`

	// SHA is the blob SHA for an already-uploaded blob. Mutually
	// exclusive with Content.
	SHA *string `json:"sha,omitempty"`
}

// CreateCommitRequest contains the fields for creating a git commit
// via the GitHub API.
type CreateCommitRequest struct {
	// Message is the commit message.
	Message string `json:"message"`

	// Tree is the SHA of the tree object for this commit.
	Tree string `json:"tree"`

	// Parents are the SHAs of the parent commits.
	Parents []string `json:"parents"`
}

// CreateTree creates a git tree object in a repository.
func (client *Client) CreateTree(ctx context.Context, owner, repo string, request CreateTreeRequest) (*Tree, error) {
	var tree Tree
	path := fmt.Sprintf("/repos/%s/%s/git/trees", owner, repo)
	if err := client.post(ctx, path, request, &tree); err != nil {
		return nil, fmt.Errorf("creating tree in %s/%s: %w", owner, repo, err)
	}
	return &tree, nil
}

// CreateCommit creates a git commit object in a repository.
func (client *Client) CreateCommit(ctx context.Context, owner, repo string, request CreateCommitRequest) (*Commit, error) {
	var commit Commit
	path := fmt.Sprintf("/repos/%s/%s/git/commits", owner, repo)
	if err := client.post(ctx, path, request, &commit); err != nil {
		return nil, fmt.Errorf("creating commit in %s/%s: %w", owner, repo, err)
	}
	return &commit, nil
}

// UpdateRef updates a git reference (branch) to point to a new commit.
// The ref should be the full ref path (e.g., "heads/main" without the
// "refs/" prefix — GitHub's API adds it).
func (client *Client) UpdateRef(ctx context.Context, owner, repo, ref, sha string, force bool) (*Ref, error) {
	var result Ref
	request := struct {
		SHA   string `json:"sha"`
		Force bool   `json:"force"`
	}{SHA: sha, Force: force}

	path := fmt.Sprintf("/repos/%s/%s/git/refs/%s", owner, repo, url.PathEscape(ref))
	if err := client.patch(ctx, path, request, &result); err != nil {
		return nil, fmt.Errorf("updating ref %s in %s/%s: %w", ref, owner, repo, err)
	}
	return &result, nil
}
