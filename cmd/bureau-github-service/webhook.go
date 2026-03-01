// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema/forge"
	"github.com/bureau-foundation/bureau/lib/service"
)

// maxWebhookBodySize is the maximum size of a webhook payload we will
// accept. GitHub's documented maximum is ~25 MB for push events with
// large commit histories. 32 MB gives comfortable headroom.
const maxWebhookBodySize = 32 * 1024 * 1024

// deduplicationWindow is how long we track delivery IDs for replay
// protection. GitHub typically retries within minutes, so 1 hour is
// conservative.
const deduplicationWindow = 1 * time.Hour

// WebhookHandler processes incoming GitHub webhooks. It verifies
// HMAC-SHA256 signatures, deduplicates deliveries, deserializes
// payloads, and translates them into forge schema event types.
//
// The handler is an http.Handler suitable for use with HTTPServer
// or any standard Go HTTP server/mux.
type WebhookHandler struct {
	secret []byte
	logger *slog.Logger

	// onEvent is called for each successfully verified and translated
	// webhook event. The caller (GitHubService) wires this to the
	// subscription manager and entity mapping handlers.
	onEvent func(event *forge.Event)

	// deliveries tracks recently processed delivery IDs for replay
	// protection. Keys are X-GitHub-Delivery values; values are the
	// time the delivery was first processed.
	mu         sync.Mutex
	deliveries map[string]time.Time
}

// NewWebhookHandler creates a handler that verifies webhooks using
// the given HMAC secret. Panics if secret is empty, logger is nil,
// or onEvent is nil — a nil callback would silently discard events.
func NewWebhookHandler(secret []byte, logger *slog.Logger, onEvent func(*forge.Event)) *WebhookHandler {
	if len(secret) == 0 {
		panic("WebhookHandler: secret is required")
	}
	if logger == nil {
		panic("WebhookHandler: logger is required")
	}
	if onEvent == nil {
		panic("WebhookHandler: onEvent callback is required")
	}
	return &WebhookHandler{
		secret:     secret,
		logger:     logger,
		onEvent:    onEvent,
		deliveries: make(map[string]time.Time),
	}
}

// ServeHTTP handles a single webhook request.
func (h *WebhookHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(writer, "", http.StatusMethodNotAllowed)
		return
	}

	// Read the body first — HMAC verification requires the raw bytes.
	body, err := io.ReadAll(io.LimitReader(request.Body, maxWebhookBodySize))
	if err != nil {
		h.logger.Error("webhook: failed to read body", "error", err)
		http.Error(writer, "", http.StatusInternalServerError)
		return
	}
	if len(body) == 0 {
		http.Error(writer, "", http.StatusBadRequest)
		return
	}

	// Verify HMAC-SHA256 signature.
	signature := request.Header.Get("X-Hub-Signature-256")
	if err := service.VerifyWebhookHMAC(h.secret, body, signature); err != nil {
		h.logger.Warn("webhook: HMAC verification failed",
			"error", err,
			"remote_addr", request.RemoteAddr,
		)
		// 401 with no information disclosure.
		http.Error(writer, "", http.StatusUnauthorized)
		return
	}

	// Extract GitHub-specific headers.
	eventType := request.Header.Get("X-GitHub-Event")
	deliveryID := request.Header.Get("X-GitHub-Delivery")

	if eventType == "" {
		h.logger.Warn("webhook: missing X-GitHub-Event header")
		http.Error(writer, "", http.StatusBadRequest)
		return
	}

	// Replay protection: reject duplicate delivery IDs.
	if deliveryID != "" && h.isDuplicate(deliveryID) {
		h.logger.Debug("webhook: duplicate delivery, ignoring",
			"delivery_id", deliveryID,
			"event_type", eventType,
		)
		// Return 200 so GitHub doesn't retry.
		writer.WriteHeader(http.StatusOK)
		return
	}

	h.logger.Info("webhook received",
		"event_type", eventType,
		"delivery_id", deliveryID,
	)

	// Translate the webhook payload into a forge schema event.
	event, err := h.translateEvent(eventType, body)
	if err != nil {
		h.logger.Error("webhook: translation failed",
			"event_type", eventType,
			"delivery_id", deliveryID,
			"error", err,
		)
		// Return 200 — retrying won't fix a translation error.
		writer.WriteHeader(http.StatusOK)
		return
	}

	if event == nil {
		// Event type not handled (e.g., "ping", "installation").
		// Log and acknowledge.
		h.logger.Debug("webhook: unhandled event type, ignoring",
			"event_type", eventType,
			"delivery_id", deliveryID,
		)
		writer.WriteHeader(http.StatusOK)
		return
	}

	// Dispatch the translated event.
	h.onEvent(event)

	writer.WriteHeader(http.StatusOK)
}

// isDuplicate checks and records a delivery ID. Returns true if the
// delivery was already processed within the deduplication window.
// Periodically prunes expired entries.
func (h *WebhookHandler) isDuplicate(deliveryID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()

	// Prune expired entries every time we check. The map is small
	// (one entry per webhook over the last hour) so this is cheap.
	for id, receivedAt := range h.deliveries {
		if now.Sub(receivedAt) > deduplicationWindow {
			delete(h.deliveries, id)
		}
	}

	if _, exists := h.deliveries[deliveryID]; exists {
		return true
	}
	h.deliveries[deliveryID] = now
	return false
}

// translateEvent converts a raw GitHub webhook payload into a typed
// forge schema Event. Returns nil for event types that don't map to
// forge events (ping, installation, etc.).
func (h *WebhookHandler) translateEvent(eventType string, body []byte) (*forge.Event, error) {
	switch eventType {
	case "push":
		return h.translatePush(body)
	case "issues":
		return h.translateIssues(body)
	case "pull_request":
		return h.translatePullRequest(body)
	case "pull_request_review":
		return h.translatePullRequestReview(body)
	case "issue_comment":
		return h.translateIssueComment(body)
	case "workflow_run":
		return h.translateWorkflowRun(body)
	case "ping":
		// GitHub sends a ping event when a webhook is first
		// configured. Not a forge event — acknowledge silently.
		return nil, nil
	case "installation":
		// App installation events. Not a forge event.
		return nil, nil
	default:
		// Unrecognized event type. Return nil (not an error) so we
		// don't reject webhooks for event types GitHub adds in the
		// future. The caller logs this as "unhandled."
		return nil, nil
	}
}

// --- Per-event-type translators ---

func (h *WebhookHandler) translatePush(body []byte) (*forge.Event, error) {
	var payload ghPushPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parsing push payload: %w", err)
	}

	commits := make([]forge.Commit, len(payload.Commits))
	for i, ghc := range payload.Commits {
		commits[i] = forge.Commit{
			SHA:       ghc.ID,
			Message:   ghc.Message,
			Author:    ghc.Author.Name + " <" + ghc.Author.Email + ">",
			Timestamp: ghc.Timestamp,
			URL:       ghc.URL,
			Added:     ghc.Added,
			Modified:  ghc.Modified,
			Removed:   ghc.Removed,
		}
	}

	summary := formatPushSummary(payload)

	return &forge.Event{
		Type: forge.EventCategoryPush,
		Push: &forge.PushEvent{
			Provider: string(forge.ProviderGitHub),
			Repo:     payload.Repository.FullName,
			Ref:      payload.Ref,
			Before:   payload.Before,
			After:    payload.After,
			Commits:  commits,
			Sender:   payload.Sender.Login,
			Summary:  summary,
			URL:      payload.CompareURL,
		},
	}, nil
}

func (h *WebhookHandler) translateIssues(body []byte) (*forge.Event, error) {
	var payload ghIssuesPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parsing issues payload: %w", err)
	}

	labels := make([]string, len(payload.Issue.Labels))
	for i, label := range payload.Issue.Labels {
		labels[i] = label.Name
	}

	summary := fmt.Sprintf("[%s] %s #%d: %s",
		payload.Repository.FullName,
		payload.Action,
		payload.Issue.Number,
		payload.Issue.Title,
	)

	return &forge.Event{
		Type: forge.EventCategoryIssues,
		Issue: &forge.IssueEvent{
			Provider: string(forge.ProviderGitHub),
			Repo:     payload.Repository.FullName,
			Number:   payload.Issue.Number,
			Action:   payload.Action,
			Title:    payload.Issue.Title,
			Author:   payload.Issue.User.Login,
			Labels:   labels,
			Summary:  summary,
			URL:      payload.Issue.HTMLURL,
		},
	}, nil
}

func (h *WebhookHandler) translatePullRequest(body []byte) (*forge.Event, error) {
	var payload ghPullRequestPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parsing pull_request payload: %w", err)
	}

	// GitHub uses "closed" for both close and merge. Translate
	// to the forge schema's separate "merged" action when the PR
	// was merged.
	action := payload.Action
	if action == "closed" && payload.PullRequest.Merged {
		action = string(forge.PullRequestMerged)
	}

	summary := fmt.Sprintf("[%s] %s PR #%d: %s",
		payload.Repository.FullName,
		action,
		payload.PullRequest.Number,
		payload.PullRequest.Title,
	)

	return &forge.Event{
		Type: forge.EventCategoryPullRequest,
		PullRequest: &forge.PullRequestEvent{
			Provider: string(forge.ProviderGitHub),
			Repo:     payload.Repository.FullName,
			Number:   payload.PullRequest.Number,
			Action:   action,
			Title:    payload.PullRequest.Title,
			Author:   payload.PullRequest.User.Login,
			HeadRef:  payload.PullRequest.Head.Ref,
			BaseRef:  payload.PullRequest.Base.Ref,
			HeadSHA:  payload.PullRequest.Head.SHA,
			Draft:    payload.PullRequest.Draft,
			Summary:  summary,
			URL:      payload.PullRequest.HTMLURL,
		},
	}, nil
}

func (h *WebhookHandler) translatePullRequestReview(body []byte) (*forge.Event, error) {
	var payload ghPullRequestReviewPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parsing pull_request_review payload: %w", err)
	}

	// Only translate "submitted" actions — "edited" and "dismissed"
	// are less actionable for Bureau's review gate model.
	if payload.Action != "submitted" {
		return nil, nil
	}

	summary := fmt.Sprintf("[%s] %s reviewed PR #%d: %s",
		payload.Repository.FullName,
		payload.Review.User.Login,
		payload.PullRequest.Number,
		payload.Review.State,
	)

	return &forge.Event{
		Type: forge.EventCategoryReview,
		Review: &forge.ReviewEvent{
			Provider: string(forge.ProviderGitHub),
			Repo:     payload.Repository.FullName,
			PRNumber: payload.PullRequest.Number,
			Reviewer: payload.Review.User.Login,
			State:    payload.Review.State,
			Body:     payload.Review.Body,
			Summary:  summary,
			URL:      payload.Review.HTMLURL,
		},
	}, nil
}

func (h *WebhookHandler) translateIssueComment(body []byte) (*forge.Event, error) {
	var payload ghIssueCommentPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parsing issue_comment payload: %w", err)
	}

	// Only translate "created" comments. "edited" and "deleted" are
	// lower priority — agents reacting to comments care about new
	// comments, not edits to old ones.
	if payload.Action != "created" {
		return nil, nil
	}

	// GitHub's issue_comment fires for both issues and PRs. The
	// payload has a "pull_request" key on the issue object when
	// the comment is on a PR, but the minimal ghIssue struct
	// doesn't include it. Use a heuristic: if the issue URL
	// contains "/pull/", it's a PR comment.
	entityType := "issue"
	if strings.Contains(payload.Issue.HTMLURL, "/pull/") {
		entityType = "pull_request"
	}

	summary := fmt.Sprintf("[%s] %s commented on #%d",
		payload.Repository.FullName,
		payload.Comment.User.Login,
		payload.Issue.Number,
	)

	return &forge.Event{
		Type: forge.EventCategoryComment,
		Comment: &forge.CommentEvent{
			Provider:     string(forge.ProviderGitHub),
			Repo:         payload.Repository.FullName,
			EntityType:   entityType,
			EntityNumber: payload.Issue.Number,
			Author:       payload.Comment.User.Login,
			Body:         payload.Comment.Body,
			Summary:      summary,
			URL:          payload.Comment.HTMLURL,
		},
	}, nil
}

func (h *WebhookHandler) translateWorkflowRun(body []byte) (*forge.Event, error) {
	var payload ghWorkflowRunPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parsing workflow_run payload: %w", err)
	}

	// Determine the associated PR number, if any.
	var prNumber int
	if len(payload.WorkflowRun.PullRequests) > 0 {
		prNumber = payload.WorkflowRun.PullRequests[0].Number
	}

	summary := fmt.Sprintf("[%s] workflow %q %s",
		payload.Repository.FullName,
		payload.WorkflowRun.Name,
		payload.WorkflowRun.Status,
	)
	if payload.WorkflowRun.Conclusion != "" {
		summary += ": " + payload.WorkflowRun.Conclusion
	}

	return &forge.Event{
		Type: forge.EventCategoryCIStatus,
		CIStatus: &forge.CIStatusEvent{
			Provider:   string(forge.ProviderGitHub),
			Repo:       payload.Repository.FullName,
			RunID:      strconv.FormatInt(payload.WorkflowRun.ID, 10),
			Workflow:   payload.WorkflowRun.Name,
			Status:     payload.WorkflowRun.Status,
			Conclusion: payload.WorkflowRun.Conclusion,
			HeadSHA:    payload.WorkflowRun.HeadSHA,
			Branch:     payload.WorkflowRun.HeadBranch,
			PRNumber:   prNumber,
			URL:        payload.WorkflowRun.HTMLURL,
			Summary:    summary,
		},
	}, nil
}

// --- Summary formatting ---

func formatPushSummary(payload ghPushPayload) string {
	branch := payload.Ref
	if strings.HasPrefix(branch, "refs/heads/") {
		branch = strings.TrimPrefix(branch, "refs/heads/")
	}

	commitCount := len(payload.Commits)
	commitWord := "commit"
	if commitCount != 1 {
		commitWord = "commits"
	}

	return fmt.Sprintf("[%s] %s pushed %d %s to %s",
		payload.Repository.FullName,
		payload.Sender.Login,
		commitCount,
		commitWord,
		branch,
	)
}
