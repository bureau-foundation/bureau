// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

// GitHub webhook payload types. These are minimal structs that extract
// only the fields Bureau needs for translation into forge schema types.
// They do not attempt to model the complete GitHub API — webhook
// payloads contain hundreds of fields that are irrelevant to Bureau's
// event schema.
//
// JSON field names match GitHub's webhook payload documentation.

// ghUser is a GitHub user reference. Appears in sender, author,
// assignee, reviewer, etc.
type ghUser struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

// ghRepository is a GitHub repository reference.
type ghRepository struct {
	FullName string `json:"full_name"` // "owner/repo"
	HTMLURL  string `json:"html_url"`
}

// ghCommit is a commit within a push event.
type ghCommit struct {
	ID        string   `json:"id"` // full SHA
	Message   string   `json:"message"`
	Timestamp string   `json:"timestamp"` // ISO 8601
	URL       string   `json:"url"`       // web URL
	Author    ghAuthor `json:"author"`
	Added     []string `json:"added"`
	Modified  []string `json:"modified"`
	Removed   []string `json:"removed"`
}

// ghAuthor is the git author metadata from a commit (not the GitHub
// user — the author field in a push event commit is the git author
// string, not a GitHub user object).
type ghAuthor struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Username string `json:"username"` // GitHub username, may be empty
}

// ghPushPayload is the webhook payload for a "push" event.
type ghPushPayload struct {
	Ref        string       `json:"ref"`    // "refs/heads/main"
	Before     string       `json:"before"` // previous HEAD SHA
	After      string       `json:"after"`  // new HEAD SHA
	Repository ghRepository `json:"repository"`
	Sender     ghUser       `json:"sender"`
	Commits    []ghCommit   `json:"commits"`
	CompareURL string       `json:"compare"` // web URL to diff
}

// ghLabel is a GitHub issue/PR label.
type ghLabel struct {
	Name string `json:"name"`
}

// ghIssue is a GitHub issue within an issues event payload.
type ghIssue struct {
	Number  int       `json:"number"`
	Title   string    `json:"title"`
	Body    string    `json:"body"`
	HTMLURL string    `json:"html_url"`
	User    ghUser    `json:"user"`
	Labels  []ghLabel `json:"labels"`
	State   string    `json:"state"` // "open" or "closed"
}

// ghIssuesPayload is the webhook payload for an "issues" event.
type ghIssuesPayload struct {
	Action     string       `json:"action"` // opened, closed, reopened, labeled, assigned, edited, ...
	Issue      ghIssue      `json:"issue"`
	Repository ghRepository `json:"repository"`
	Sender     ghUser       `json:"sender"`
}

// ghBranch is a git branch reference on a pull request.
type ghBranch struct {
	Ref string `json:"ref"` // branch name
	SHA string `json:"sha"` // head commit SHA
}

// ghPullRequest is a GitHub pull request within a pull_request event.
type ghPullRequest struct {
	Number  int      `json:"number"`
	Title   string   `json:"title"`
	Body    string   `json:"body"`
	HTMLURL string   `json:"html_url"`
	User    ghUser   `json:"user"`
	Head    ghBranch `json:"head"`
	Base    ghBranch `json:"base"`
	Draft   bool     `json:"draft"`
	State   string   `json:"state"`  // "open" or "closed"
	Merged  bool     `json:"merged"` // true if the PR was merged (only on close)
}

// ghPullRequestPayload is the webhook payload for a "pull_request"
// event.
type ghPullRequestPayload struct {
	Action      string        `json:"action"` // opened, closed, synchronize, review_requested, ...
	PullRequest ghPullRequest `json:"pull_request"`
	Repository  ghRepository  `json:"repository"`
	Sender      ghUser        `json:"sender"`
}

// ghReview is a GitHub pull request review.
type ghReview struct {
	State   string `json:"state"` // approved, changes_requested, commented
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    ghUser `json:"user"`
}

// ghPullRequestReviewPayload is the webhook payload for a
// "pull_request_review" event.
type ghPullRequestReviewPayload struct {
	Action      string        `json:"action"` // submitted, edited, dismissed
	Review      ghReview      `json:"review"`
	PullRequest ghPullRequest `json:"pull_request"`
	Repository  ghRepository  `json:"repository"`
	Sender      ghUser        `json:"sender"`
}

// ghComment is a GitHub issue or PR comment.
type ghComment struct {
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    ghUser `json:"user"`
}

// ghIssueCommentPayload is the webhook payload for an
// "issue_comment" event. Despite the name, this fires for both
// issue comments and PR comments (PR top-level comments, not
// review comments).
type ghIssueCommentPayload struct {
	Action     string       `json:"action"` // created, edited, deleted
	Issue      ghIssue      `json:"issue"`
	Comment    ghComment    `json:"comment"`
	Repository ghRepository `json:"repository"`
	Sender     ghUser       `json:"sender"`
}

// ghWorkflowRun is a GitHub Actions workflow run.
type ghWorkflowRun struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`       // workflow name
	Status       string `json:"status"`     // queued, in_progress, completed
	Conclusion   string `json:"conclusion"` // success, failure, cancelled, ""
	HeadSHA      string `json:"head_sha"`
	HeadBranch   string `json:"head_branch"`
	HTMLURL      string `json:"html_url"`
	PullRequests []ghPR `json:"pull_requests"`
}

// ghPR is a minimal PR reference in workflow run events.
type ghPR struct {
	Number int `json:"number"`
}

// ghWorkflowRunPayload is the webhook payload for a "workflow_run"
// event.
type ghWorkflowRunPayload struct {
	Action      string        `json:"action"` // requested, in_progress, completed
	WorkflowRun ghWorkflowRun `json:"workflow_run"`
	Repository  ghRepository  `json:"repository"`
	Sender      ghUser        `json:"sender"`
}
