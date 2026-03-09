// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/github"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/forge"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// --- Credential loading ---

// createGitHubClient creates a GitHub API client from environment
// variables. Supports two authentication modes:
//
//   - App auth: BUREAU_GITHUB_APP_ID, BUREAU_GITHUB_APP_PRIVATE_KEY_FILE,
//     BUREAU_GITHUB_APP_INSTALLATION_ID
//   - Token auth: BUREAU_GITHUB_TOKEN_FILE
//
// Returns (nil, nil) if no credentials are configured — the service
// runs in webhook-only mode without outbound API access. Returns an
// error if credentials are partially configured (e.g., AppID set but
// no private key file).
func createGitHubClient(clk clock.Clock, logger *slog.Logger) (*github.Client, error) {
	appIDStr := os.Getenv("BUREAU_GITHUB_APP_ID")
	keyFile := os.Getenv("BUREAU_GITHUB_APP_PRIVATE_KEY_FILE")
	installIDStr := os.Getenv("BUREAU_GITHUB_APP_INSTALLATION_ID")
	tokenFile := os.Getenv("BUREAU_GITHUB_TOKEN_FILE")

	hasApp := appIDStr != "" || keyFile != "" || installIDStr != ""
	hasToken := tokenFile != ""

	if !hasApp && !hasToken {
		return nil, nil
	}

	if hasApp && hasToken {
		return nil, fmt.Errorf("cannot configure both GitHub App auth (BUREAU_GITHUB_APP_*) and token auth (BUREAU_GITHUB_TOKEN_FILE)")
	}

	config := github.Config{
		Clock:  clk,
		Logger: logger,
	}

	if hasApp {
		if appIDStr == "" {
			return nil, fmt.Errorf("BUREAU_GITHUB_APP_ID is required when BUREAU_GITHUB_APP_PRIVATE_KEY_FILE or BUREAU_GITHUB_APP_INSTALLATION_ID is set")
		}
		if keyFile == "" {
			return nil, fmt.Errorf("BUREAU_GITHUB_APP_PRIVATE_KEY_FILE is required when BUREAU_GITHUB_APP_ID is set")
		}
		if installIDStr == "" {
			return nil, fmt.Errorf("BUREAU_GITHUB_APP_INSTALLATION_ID is required when BUREAU_GITHUB_APP_ID is set")
		}

		appID, err := strconv.ParseInt(appIDStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing BUREAU_GITHUB_APP_ID: %w", err)
		}

		privateKey, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("reading GitHub App private key from %s: %w", keyFile, err)
		}

		installID, err := strconv.ParseInt(installIDStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing BUREAU_GITHUB_APP_INSTALLATION_ID: %w", err)
		}

		config.AppID = appID
		config.PrivateKey = privateKey
		config.InstallationID = installID
	} else {
		tokenData, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading GitHub token from %s: %w", tokenFile, err)
		}
		token := strings.TrimSpace(string(tokenData))
		if token == "" {
			return nil, fmt.Errorf("GitHub token file %s is empty", tokenFile)
		}
		config.Token = token
	}

	return github.NewClient(config)
}

// --- Helpers ---

// parseRepoSlug splits an "owner/repo" string into its components.
func parseRepoSlug(slug string) (string, string, error) {
	parts := strings.SplitN(slug, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repo format %q: expected \"owner/repo\"", slug)
	}
	owner, repo := parts[0], parts[1]
	if !isValidRepoComponent(owner) || !isValidRepoComponent(repo) {
		return "", "", fmt.Errorf("invalid repo slug %q: owner and repo must contain only alphanumeric characters, hyphens, underscores, and periods", slug)
	}
	return owner, repo, nil
}

// isValidRepoComponent checks that a GitHub owner or repo name contains
// only characters that are safe for URL path construction. GitHub
// allows [a-zA-Z0-9._-] in owner and repo names. Semantic safety
// (empty, ".", "..", leading dot) is delegated to ref.ValidatePathSegment
// so the codebase has one canonical path traversal check. Character
// validation is GitHub-specific: the allowlist prevents API injection
// via crafted repo slugs from untrusted agents.
func isValidRepoComponent(name string) bool {
	if ref.ValidatePathSegment(name, "repo component") != nil {
		return false
	}
	for _, char := range name {
		if !((char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') ||
			char == '-' || char == '_' || char == '.') {
			return false
		}
	}
	return true
}

// requireGitHubClient returns the GitHub API client or a clear error
// if outbound API access is not configured.
func (gs *GitHubService) requireGitHubClient() (*github.Client, error) {
	if gs.githubClient == nil {
		return nil, fmt.Errorf("outbound GitHub API not configured (set BUREAU_GITHUB_APP_* or BUREAU_GITHUB_TOKEN_FILE)")
	}
	return gs.githubClient, nil
}

// --- CBOR request/response types ---

type createIssueRequest struct {
	Repo      string   `json:"repo"`
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
}

type createIssueResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

type createCommentRequest struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Body   string `json:"body"`
}

type createCommentResponse struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

type createReviewRequest struct {
	Repo     string                 `json:"repo"`
	Number   int                    `json:"number"`
	Body     string                 `json:"body,omitempty"`
	Event    string                 `json:"event"`
	Comments []github.ReviewComment `json:"comments,omitempty"`
}

type createReviewResponse struct {
	ID      int64  `json:"id"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
}

type mergePRRequest struct {
	Repo          string `json:"repo"`
	Number        int    `json:"number"`
	CommitTitle   string `json:"commit_title,omitempty"`
	CommitMessage string `json:"commit_message,omitempty"`
	MergeMethod   string `json:"merge_method,omitempty"`
	SHA           string `json:"sha,omitempty"`
}

type mergePRResponse struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

type triggerWorkflowRequest struct {
	Repo       string            `json:"repo"`
	WorkflowID string            `json:"workflow_id"`
	Ref        string            `json:"ref"`
	Inputs     map[string]string `json:"inputs,omitempty"`
}

type reportStatusRequest struct {
	Repo        string `json:"repo"`
	SHA         string `json:"sha"`
	State       string `json:"state"`
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
	Context     string `json:"context,omitempty"`
}

type reportStatusResponse struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
}

// --- Handler methods ---

func (gs *GitHubService) handleCreateIssue(
	ctx context.Context,
	token *servicetoken.Token,
	raw []byte,
) (any, error) {
	action := forge.ProviderAction(forge.ProviderGitHub, forge.ActionCreateIssue)
	if !servicetoken.GrantsAllow(token.Grants, action, "") {
		return nil, fmt.Errorf("access denied: missing grant for %s", action)
	}

	client, err := gs.requireGitHubClient()
	if err != nil {
		return nil, err
	}

	var request createIssueRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.Repo == "" {
		return nil, fmt.Errorf("repo is required")
	}
	if request.Title == "" {
		return nil, fmt.Errorf("title is required")
	}

	owner, repo, err := parseRepoSlug(request.Repo)
	if err != nil {
		return nil, err
	}

	issue, err := client.CreateIssue(ctx, owner, repo, github.CreateIssueRequest{
		Title:     request.Title,
		Body:      request.Body,
		Labels:    request.Labels,
		Assignees: request.Assignees,
	})
	if err != nil {
		return nil, fmt.Errorf("github API: %w", err)
	}

	return createIssueResponse{
		Number:  issue.Number,
		HTMLURL: issue.HTMLURL,
	}, nil
}

func (gs *GitHubService) handleCreateComment(
	ctx context.Context,
	token *servicetoken.Token,
	raw []byte,
) (any, error) {
	action := forge.ProviderAction(forge.ProviderGitHub, forge.ActionCreateComment)
	if !servicetoken.GrantsAllow(token.Grants, action, "") {
		return nil, fmt.Errorf("access denied: missing grant for %s", action)
	}

	client, err := gs.requireGitHubClient()
	if err != nil {
		return nil, err
	}

	var request createCommentRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.Repo == "" {
		return nil, fmt.Errorf("repo is required")
	}
	if request.Number == 0 {
		return nil, fmt.Errorf("number is required")
	}
	if request.Body == "" {
		return nil, fmt.Errorf("body is required")
	}

	owner, repo, err := parseRepoSlug(request.Repo)
	if err != nil {
		return nil, err
	}

	comment, err := client.CreateIssueComment(ctx, owner, repo, request.Number, request.Body)
	if err != nil {
		return nil, fmt.Errorf("github API: %w", err)
	}

	return createCommentResponse{
		ID:      comment.ID,
		HTMLURL: comment.HTMLURL,
	}, nil
}

func (gs *GitHubService) handleCreateReview(
	ctx context.Context,
	token *servicetoken.Token,
	raw []byte,
) (any, error) {
	action := forge.ProviderAction(forge.ProviderGitHub, forge.ActionCreateReview)
	if !servicetoken.GrantsAllow(token.Grants, action, "") {
		return nil, fmt.Errorf("access denied: missing grant for %s", action)
	}

	client, err := gs.requireGitHubClient()
	if err != nil {
		return nil, err
	}

	var request createReviewRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.Repo == "" {
		return nil, fmt.Errorf("repo is required")
	}
	if request.Number == 0 {
		return nil, fmt.Errorf("number is required")
	}
	if request.Event == "" {
		return nil, fmt.Errorf("event is required")
	}

	owner, repo, err := parseRepoSlug(request.Repo)
	if err != nil {
		return nil, err
	}

	review, err := client.CreateReview(ctx, owner, repo, request.Number, github.CreateReviewRequest{
		Body:     request.Body,
		Event:    request.Event,
		Comments: request.Comments,
	})
	if err != nil {
		return nil, fmt.Errorf("github API: %w", err)
	}

	return createReviewResponse{
		ID:      review.ID,
		State:   review.State,
		HTMLURL: review.HTMLURL,
	}, nil
}

func (gs *GitHubService) handleMergePR(
	ctx context.Context,
	token *servicetoken.Token,
	raw []byte,
) (any, error) {
	action := forge.ProviderAction(forge.ProviderGitHub, forge.ActionMergePR)
	if !servicetoken.GrantsAllow(token.Grants, action, "") {
		return nil, fmt.Errorf("access denied: missing grant for %s", action)
	}

	client, err := gs.requireGitHubClient()
	if err != nil {
		return nil, err
	}

	var request mergePRRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.Repo == "" {
		return nil, fmt.Errorf("repo is required")
	}
	if request.Number == 0 {
		return nil, fmt.Errorf("number is required")
	}

	owner, repo, err := parseRepoSlug(request.Repo)
	if err != nil {
		return nil, err
	}

	result, err := client.MergePullRequest(ctx, owner, repo, request.Number, github.MergePullRequestRequest{
		CommitTitle:   request.CommitTitle,
		CommitMessage: request.CommitMessage,
		MergeMethod:   request.MergeMethod,
		SHA:           request.SHA,
	})
	if err != nil {
		return nil, fmt.Errorf("github API: %w", err)
	}

	return mergePRResponse{
		SHA:     result.SHA,
		Merged:  result.Merged,
		Message: result.Message,
	}, nil
}

func (gs *GitHubService) handleTriggerWorkflow(
	ctx context.Context,
	token *servicetoken.Token,
	raw []byte,
) (any, error) {
	action := forge.ProviderAction(forge.ProviderGitHub, forge.ActionTriggerWorkflow)
	if !servicetoken.GrantsAllow(token.Grants, action, "") {
		return nil, fmt.Errorf("access denied: missing grant for %s", action)
	}

	client, err := gs.requireGitHubClient()
	if err != nil {
		return nil, err
	}

	var request triggerWorkflowRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.Repo == "" {
		return nil, fmt.Errorf("repo is required")
	}
	if request.WorkflowID == "" {
		return nil, fmt.Errorf("workflow_id is required")
	}
	if request.Ref == "" {
		return nil, fmt.Errorf("ref is required")
	}

	owner, repo, err := parseRepoSlug(request.Repo)
	if err != nil {
		return nil, err
	}

	err = client.DispatchWorkflow(ctx, owner, repo, request.WorkflowID, github.DispatchWorkflowRequest{
		Ref:    request.Ref,
		Inputs: request.Inputs,
	})
	if err != nil {
		return nil, fmt.Errorf("github API: %w", err)
	}

	// GitHub returns 204 No Content — no response body.
	return nil, nil
}

func (gs *GitHubService) handleReportStatus(
	ctx context.Context,
	token *servicetoken.Token,
	raw []byte,
) (any, error) {
	action := forge.ProviderAction(forge.ProviderGitHub, forge.ActionReportStatus)
	if !servicetoken.GrantsAllow(token.Grants, action, "") {
		return nil, fmt.Errorf("access denied: missing grant for %s", action)
	}

	client, err := gs.requireGitHubClient()
	if err != nil {
		return nil, err
	}

	var request reportStatusRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.Repo == "" {
		return nil, fmt.Errorf("repo is required")
	}
	if request.SHA == "" {
		return nil, fmt.Errorf("sha is required")
	}
	if request.State == "" {
		return nil, fmt.Errorf("state is required")
	}

	owner, repo, err := parseRepoSlug(request.Repo)
	if err != nil {
		return nil, err
	}

	status, err := client.CreateCommitStatus(ctx, owner, repo, request.SHA, github.CreateStatusRequest{
		State:       request.State,
		TargetURL:   request.TargetURL,
		Description: request.Description,
		Context:     request.Context,
	})
	if err != nil {
		return nil, fmt.Errorf("github API: %w", err)
	}

	return reportStatusResponse{
		ID:    status.ID,
		State: status.State,
	}, nil
}
