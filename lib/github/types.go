// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import "time"

// User is a GitHub user reference. Appears in issue authors, PR authors,
// reviewers, assignees, etc.
type User struct {
	Login   string `json:"login"`
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

// Label is a GitHub issue/PR label.
type Label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Branch is a git branch reference on a pull request.
type Branch struct {
	Ref string `json:"ref"` // branch name
	SHA string `json:"sha"` // head commit SHA
}

// Issue is a GitHub issue.
type Issue struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"` // "open" or "closed"
	HTMLURL   string     `json:"html_url"`
	User      User       `json:"user"`
	Labels    []Label    `json:"labels"`
	Assignees []User     `json:"assignees"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"`
}

// Comment is a GitHub issue or PR comment.
type Comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	User      User      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PullRequest is a GitHub pull request.
type PullRequest struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"` // "open" or "closed"
	HTMLURL   string     `json:"html_url"`
	User      User       `json:"user"`
	Head      Branch     `json:"head"`
	Base      Branch     `json:"base"`
	Draft     bool       `json:"draft"`
	Merged    bool       `json:"merged"`
	Labels    []Label    `json:"labels"`
	Assignees []User     `json:"assignees"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	MergedAt  *time.Time `json:"merged_at"`
}

// Review is a GitHub pull request review.
type Review struct {
	ID          int64     `json:"id"`
	State       string    `json:"state"` // "APPROVED", "CHANGES_REQUESTED", "COMMENTED"
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	User        User      `json:"user"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// Tree is a GitHub git tree object.
type Tree struct {
	SHA       string      `json:"sha"`
	Truncated bool        `json:"truncated"`
	Entries   []TreeEntry `json:"tree"`
}

// TreeEntry is a single entry in a git tree.
type TreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"` // "100644", "100755", "120000", "160000", "040000"
	Type string `json:"type"` // "blob", "tree", "commit"
	SHA  string `json:"sha"`
	Size int64  `json:"size,omitempty"`
}

// Commit is a GitHub git commit object.
type Commit struct {
	SHA     string       `json:"sha"`
	Message string       `json:"message"`
	Tree    CommitTree   `json:"tree"`
	Parents []CommitRef  `json:"parents"`
	HTMLURL string       `json:"html_url"`
	Author  CommitAuthor `json:"author"`
}

// CommitTree is a reference to the tree in a commit.
type CommitTree struct {
	SHA string `json:"sha"`
}

// CommitRef is a reference to a parent commit.
type CommitRef struct {
	SHA string `json:"sha"`
}

// CommitAuthor is the author/committer metadata on a commit.
type CommitAuthor struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	Date  time.Time `json:"date"`
}

// Ref is a GitHub git reference (branch or tag).
type Ref struct {
	Ref    string    `json:"ref"` // "refs/heads/main"
	Object RefObject `json:"object"`
}

// RefObject is the object a ref points to.
type RefObject struct {
	SHA  string `json:"sha"`
	Type string `json:"type"` // "commit"
}

// WorkflowRun is a GitHub Actions workflow run.
type WorkflowRun struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`     // "queued", "in_progress", "completed"
	Conclusion string    `json:"conclusion"` // "success", "failure", "cancelled", ""
	HeadSHA    string    `json:"head_sha"`
	HeadBranch string    `json:"head_branch"`
	HTMLURL    string    `json:"html_url"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Webhook is a GitHub webhook configuration.
type Webhook struct {
	ID     int64         `json:"id"`
	Active bool          `json:"active"`
	Events []string      `json:"events"`
	Config WebhookConfig `json:"config"`
}

// WebhookConfig holds the webhook endpoint configuration.
type WebhookConfig struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Secret      string `json:"secret,omitempty"` // masked in responses
	InsecureSSL string `json:"insecure_ssl"`
}

// CommitStatus is a GitHub commit status.
type CommitStatus struct {
	ID          int64     `json:"id"`
	State       string    `json:"state"` // "error", "failure", "pending", "success"
	TargetURL   string    `json:"target_url"`
	Description string    `json:"description"`
	Context     string    `json:"context"`
	CreatedAt   time.Time `json:"created_at"`
}

// App is a GitHub App.
type App struct {
	ID          int64             `json:"id"`
	Slug        string            `json:"slug"`
	Name        string            `json:"name"`
	HTMLURL     string            `json:"html_url"`
	Permissions map[string]string `json:"permissions"`
	Events      []string          `json:"events"`
}

// Installation is a GitHub App installation.
type Installation struct {
	ID                  int64  `json:"id"`
	Account             User   `json:"account"`
	AppID               int64  `json:"app_id"`
	TargetType          string `json:"target_type"`          // "Organization" or "User"
	RepositorySelection string `json:"repository_selection"` // "all" or "selected"
}
