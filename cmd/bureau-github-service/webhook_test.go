// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema/forge"
)

const testWebhookSecret = "test-secret-for-hmac"

// signPayload computes the HMAC-SHA256 signature for a webhook body.
func signPayload(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// testHandler creates a WebhookHandler that collects events into a
// slice protected by a mutex.
type testHandler struct {
	handler *WebhookHandler
	mu      sync.Mutex
	events  []*forge.Event
}

func newTestHandler() *testHandler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := &testHandler{}
	handler.handler = NewWebhookHandler(
		[]byte(testWebhookSecret),
		logger,
		func(event *forge.Event) {
			handler.mu.Lock()
			defer handler.mu.Unlock()
			handler.events = append(handler.events, event)
		},
	)
	return handler
}

func (th *testHandler) lastEvent() *forge.Event {
	th.mu.Lock()
	defer th.mu.Unlock()
	if len(th.events) == 0 {
		return nil
	}
	return th.events[len(th.events)-1]
}

func (th *testHandler) eventCount() int {
	th.mu.Lock()
	defer th.mu.Unlock()
	return len(th.events)
}

// --- HTTP method enforcement ---

func TestWebhookRejectsNonPOST(t *testing.T) {
	handler := newTestHandler()

	methods := []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			request := httptest.NewRequest(method, "/webhook", nil)
			recorder := httptest.NewRecorder()
			handler.handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}

// --- HMAC signature verification ---

func TestWebhookRejectsEmptyBody(t *testing.T) {
	handler := newTestHandler()

	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(""))
	request.Header.Set("X-Hub-Signature-256", "sha256=irrelevant")
	recorder := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestWebhookRejectsInvalidSignature(t *testing.T) {
	handler := newTestHandler()

	body := `{"action":"opened"}`
	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	request.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("ab", 32))
	request.Header.Set("X-GitHub-Event", "issues")
	recorder := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestWebhookRejectsMissingEventType(t *testing.T) {
	handler := newTestHandler()

	body := `{"action":"opened"}`
	signature := signPayload([]byte(testWebhookSecret), []byte(body))
	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	request.Header.Set("X-Hub-Signature-256", signature)
	// No X-GitHub-Event header.
	recorder := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

// --- Delivery deduplication ---

func TestWebhookDeduplicatesDeliveries(t *testing.T) {
	handler := newTestHandler()

	body := buildPushPayload(t)
	signature := signPayload([]byte(testWebhookSecret), body)

	// First delivery should succeed and produce an event.
	request1 := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	request1.Header.Set("X-Hub-Signature-256", signature)
	request1.Header.Set("X-GitHub-Event", "push")
	request1.Header.Set("X-GitHub-Delivery", "delivery-abc-123")
	recorder1 := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder1, request1)

	if recorder1.Code != http.StatusOK {
		t.Fatalf("first delivery: status = %d, want %d", recorder1.Code, http.StatusOK)
	}
	if handler.eventCount() != 1 {
		t.Fatalf("first delivery: event count = %d, want 1", handler.eventCount())
	}

	// Duplicate delivery should be accepted (200) but not produce
	// another event.
	request2 := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	request2.Header.Set("X-Hub-Signature-256", signature)
	request2.Header.Set("X-GitHub-Event", "push")
	request2.Header.Set("X-GitHub-Delivery", "delivery-abc-123")
	recorder2 := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder2, request2)

	if recorder2.Code != http.StatusOK {
		t.Errorf("duplicate delivery: status = %d, want %d", recorder2.Code, http.StatusOK)
	}
	if handler.eventCount() != 1 {
		t.Errorf("duplicate delivery: event count = %d, want 1", handler.eventCount())
	}
}

// --- Ping event ---

func TestWebhookPingReturnsOK(t *testing.T) {
	handler := newTestHandler()

	body := `{"zen":"Responsive is better than fast."}`
	signature := signPayload([]byte(testWebhookSecret), []byte(body))
	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	request.Header.Set("X-Hub-Signature-256", signature)
	request.Header.Set("X-GitHub-Event", "ping")
	recorder := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if handler.eventCount() != 0 {
		t.Errorf("ping should not produce events, got %d", handler.eventCount())
	}
}

// --- Push event translation ---

func TestWebhookTranslatePush(t *testing.T) {
	handler := newTestHandler()

	body := buildPushPayload(t)
	signature := signPayload([]byte(testWebhookSecret), body)
	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	request.Header.Set("X-Hub-Signature-256", signature)
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", "push-001")
	recorder := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	event := handler.lastEvent()
	if event == nil {
		t.Fatal("no event produced")
	}
	if event.Type != forge.EventCategoryPush {
		t.Errorf("event type = %q, want %q", event.Type, forge.EventCategoryPush)
	}
	if event.Push == nil {
		t.Fatal("event.Push is nil")
	}
	if event.Push.Repo != "octocat/Hello-World" {
		t.Errorf("repo = %q, want %q", event.Push.Repo, "octocat/Hello-World")
	}
	if event.Push.Ref != "refs/heads/main" {
		t.Errorf("ref = %q, want %q", event.Push.Ref, "refs/heads/main")
	}
	if event.Push.Sender != "octocat" {
		t.Errorf("sender = %q, want %q", event.Push.Sender, "octocat")
	}
	if len(event.Push.Commits) != 1 {
		t.Fatalf("commit count = %d, want 1", len(event.Push.Commits))
	}
	if event.Push.Commits[0].SHA != "abc123def456" {
		t.Errorf("commit SHA = %q, want %q", event.Push.Commits[0].SHA, "abc123def456")
	}
	if event.Push.Commits[0].Author != "Mona Lisa <mona@github.com>" {
		t.Errorf("commit author = %q, want %q", event.Push.Commits[0].Author, "Mona Lisa <mona@github.com>")
	}
	if event.Push.Summary == "" {
		t.Error("summary is empty")
	}
}

// --- Issues event translation ---

func TestWebhookTranslateIssues(t *testing.T) {
	handler := newTestHandler()

	payload := ghIssuesPayload{
		Action: "opened",
		Issue: ghIssue{
			Number:  42,
			Title:   "Bug in authentication",
			Body:    "Login fails with OAuth",
			HTMLURL: "https://github.com/octocat/Hello-World/issues/42",
			User:    ghUser{Login: "octocat"},
			Labels:  []ghLabel{{Name: "bug"}, {Name: "auth"}},
			State:   "open",
		},
		Repository: ghRepository{FullName: "octocat/Hello-World"},
		Sender:     ghUser{Login: "octocat"},
	}
	body := mustMarshal(t, payload)
	signature := signPayload([]byte(testWebhookSecret), body)

	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	request.Header.Set("X-Hub-Signature-256", signature)
	request.Header.Set("X-GitHub-Event", "issues")
	request.Header.Set("X-GitHub-Delivery", "issues-001")
	recorder := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	event := handler.lastEvent()
	if event == nil {
		t.Fatal("no event produced")
	}
	if event.Type != forge.EventCategoryIssues {
		t.Errorf("event type = %q, want %q", event.Type, forge.EventCategoryIssues)
	}
	if event.Issue == nil {
		t.Fatal("event.Issue is nil")
	}
	if event.Issue.Number != 42 {
		t.Errorf("number = %d, want 42", event.Issue.Number)
	}
	if event.Issue.Action != "opened" {
		t.Errorf("action = %q, want %q", event.Issue.Action, "opened")
	}
	if event.Issue.Author != "octocat" {
		t.Errorf("author = %q, want %q", event.Issue.Author, "octocat")
	}
	if len(event.Issue.Labels) != 2 {
		t.Errorf("label count = %d, want 2", len(event.Issue.Labels))
	}
	if event.Issue.Body != "Login fails with OAuth" {
		t.Errorf("body = %q, want %q", event.Issue.Body, "Login fails with OAuth")
	}
}

// --- Pull request event translation ---

func TestWebhookTranslatePullRequest(t *testing.T) {
	handler := newTestHandler()

	payload := ghPullRequestPayload{
		Action: "opened",
		PullRequest: ghPullRequest{
			Number:  99,
			Title:   "Add feature X",
			Body:    "Implements feature X",
			HTMLURL: "https://github.com/octocat/Hello-World/pull/99",
			User:    ghUser{Login: "contributor"},
			Head:    ghBranch{Ref: "feature-x", SHA: "head-sha-abc"},
			Base:    ghBranch{Ref: "main", SHA: "base-sha-def"},
			Draft:   false,
			State:   "open",
			Merged:  false,
		},
		Repository: ghRepository{FullName: "octocat/Hello-World"},
		Sender:     ghUser{Login: "contributor"},
	}
	body := mustMarshal(t, payload)
	signature := signPayload([]byte(testWebhookSecret), body)

	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	request.Header.Set("X-Hub-Signature-256", signature)
	request.Header.Set("X-GitHub-Event", "pull_request")
	request.Header.Set("X-GitHub-Delivery", "pr-001")
	recorder := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	event := handler.lastEvent()
	if event == nil {
		t.Fatal("no event produced")
	}
	if event.Type != forge.EventCategoryPullRequest {
		t.Errorf("event type = %q, want %q", event.Type, forge.EventCategoryPullRequest)
	}
	if event.PullRequest == nil {
		t.Fatal("event.PullRequest is nil")
	}
	if event.PullRequest.Number != 99 {
		t.Errorf("number = %d, want 99", event.PullRequest.Number)
	}
	if event.PullRequest.Action != "opened" {
		t.Errorf("action = %q, want %q", event.PullRequest.Action, "opened")
	}
	if event.PullRequest.HeadRef != "feature-x" {
		t.Errorf("head ref = %q, want %q", event.PullRequest.HeadRef, "feature-x")
	}
	if event.PullRequest.HeadSHA != "head-sha-abc" {
		t.Errorf("head SHA = %q, want %q", event.PullRequest.HeadSHA, "head-sha-abc")
	}
}

// --- PR merge detection ---

func TestWebhookTranslatePullRequestMerge(t *testing.T) {
	handler := newTestHandler()

	payload := ghPullRequestPayload{
		Action: "closed",
		PullRequest: ghPullRequest{
			Number:  99,
			Title:   "Add feature X",
			HTMLURL: "https://github.com/octocat/Hello-World/pull/99",
			User:    ghUser{Login: "contributor"},
			Head:    ghBranch{Ref: "feature-x", SHA: "head-sha-abc"},
			Base:    ghBranch{Ref: "main", SHA: "base-sha-def"},
			State:   "closed",
			Merged:  true,
		},
		Repository: ghRepository{FullName: "octocat/Hello-World"},
		Sender:     ghUser{Login: "contributor"},
	}
	body := mustMarshal(t, payload)
	signature := signPayload([]byte(testWebhookSecret), body)

	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	request.Header.Set("X-Hub-Signature-256", signature)
	request.Header.Set("X-GitHub-Event", "pull_request")
	request.Header.Set("X-GitHub-Delivery", "pr-merge-001")
	recorder := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	event := handler.lastEvent()
	if event == nil {
		t.Fatal("no event produced")
	}
	if event.PullRequest == nil {
		t.Fatal("event.PullRequest is nil")
	}
	// GitHub sends action "closed" for merges. The translator should
	// detect the merged flag and use the forge schema's "merged" action.
	if event.PullRequest.Action != string(forge.PullRequestMerged) {
		t.Errorf("action = %q, want %q", event.PullRequest.Action, forge.PullRequestMerged)
	}
}

// --- Pull request review translation ---

func TestWebhookTranslateReview(t *testing.T) {
	handler := newTestHandler()

	payload := ghPullRequestReviewPayload{
		Action: "submitted",
		Review: ghReview{
			State:   "approved",
			Body:    "Looks good to me!",
			HTMLURL: "https://github.com/octocat/Hello-World/pull/99#pullrequestreview-1",
			User:    ghUser{Login: "reviewer"},
		},
		PullRequest: ghPullRequest{
			Number:  99,
			HTMLURL: "https://github.com/octocat/Hello-World/pull/99",
		},
		Repository: ghRepository{FullName: "octocat/Hello-World"},
		Sender:     ghUser{Login: "reviewer"},
	}
	body := mustMarshal(t, payload)
	signature := signPayload([]byte(testWebhookSecret), body)

	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	request.Header.Set("X-Hub-Signature-256", signature)
	request.Header.Set("X-GitHub-Event", "pull_request_review")
	request.Header.Set("X-GitHub-Delivery", "review-001")
	recorder := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	event := handler.lastEvent()
	if event == nil {
		t.Fatal("no event produced")
	}
	if event.Type != forge.EventCategoryReview {
		t.Errorf("event type = %q, want %q", event.Type, forge.EventCategoryReview)
	}
	if event.Review == nil {
		t.Fatal("event.Review is nil")
	}
	if event.Review.Reviewer != "reviewer" {
		t.Errorf("reviewer = %q, want %q", event.Review.Reviewer, "reviewer")
	}
	if event.Review.State != "approved" {
		t.Errorf("state = %q, want %q", event.Review.State, "approved")
	}
	if event.Review.PRNumber != 99 {
		t.Errorf("PR number = %d, want 99", event.Review.PRNumber)
	}
}

// --- Review filtering (only "submitted" action) ---

func TestWebhookReviewIgnoresNonSubmitted(t *testing.T) {
	handler := newTestHandler()

	payload := ghPullRequestReviewPayload{
		Action: "edited",
		Review: ghReview{
			State: "approved",
			User:  ghUser{Login: "reviewer"},
		},
		PullRequest: ghPullRequest{Number: 99},
		Repository:  ghRepository{FullName: "octocat/Hello-World"},
		Sender:      ghUser{Login: "reviewer"},
	}
	body := mustMarshal(t, payload)
	signature := signPayload([]byte(testWebhookSecret), body)

	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	request.Header.Set("X-Hub-Signature-256", signature)
	request.Header.Set("X-GitHub-Event", "pull_request_review")
	request.Header.Set("X-GitHub-Delivery", "review-edit-001")
	recorder := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if handler.eventCount() != 0 {
		t.Errorf("edited review should not produce events, got %d", handler.eventCount())
	}
}

// --- Issue comment translation ---

func TestWebhookTranslateIssueComment(t *testing.T) {
	handler := newTestHandler()

	payload := ghIssueCommentPayload{
		Action: "created",
		Issue: ghIssue{
			Number:  42,
			Title:   "Bug report",
			HTMLURL: "https://github.com/octocat/Hello-World/issues/42",
			User:    ghUser{Login: "octocat"},
		},
		Comment: ghComment{
			Body:    "I can reproduce this.",
			HTMLURL: "https://github.com/octocat/Hello-World/issues/42#issuecomment-1",
			User:    ghUser{Login: "helper"},
		},
		Repository: ghRepository{FullName: "octocat/Hello-World"},
		Sender:     ghUser{Login: "helper"},
	}
	body := mustMarshal(t, payload)
	signature := signPayload([]byte(testWebhookSecret), body)

	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	request.Header.Set("X-Hub-Signature-256", signature)
	request.Header.Set("X-GitHub-Event", "issue_comment")
	request.Header.Set("X-GitHub-Delivery", "comment-001")
	recorder := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	event := handler.lastEvent()
	if event == nil {
		t.Fatal("no event produced")
	}
	if event.Type != forge.EventCategoryComment {
		t.Errorf("event type = %q, want %q", event.Type, forge.EventCategoryComment)
	}
	if event.Comment == nil {
		t.Fatal("event.Comment is nil")
	}
	if event.Comment.Author != "helper" {
		t.Errorf("author = %q, want %q", event.Comment.Author, "helper")
	}
	if event.Comment.EntityType != "issue" {
		t.Errorf("entity type = %q, want %q", event.Comment.EntityType, "issue")
	}
	if event.Comment.EntityNumber != 42 {
		t.Errorf("entity number = %d, want 42", event.Comment.EntityNumber)
	}
}

// --- PR comment detection ---

func TestWebhookTranslatePRComment(t *testing.T) {
	handler := newTestHandler()

	payload := ghIssueCommentPayload{
		Action: "created",
		Issue: ghIssue{
			Number: 99,
			Title:  "Add feature X",
			// The URL contains "/pull/" which signals this is a PR comment.
			HTMLURL: "https://github.com/octocat/Hello-World/pull/99",
			User:    ghUser{Login: "contributor"},
		},
		Comment: ghComment{
			Body:    "Please update the tests.",
			HTMLURL: "https://github.com/octocat/Hello-World/pull/99#issuecomment-2",
			User:    ghUser{Login: "reviewer"},
		},
		Repository: ghRepository{FullName: "octocat/Hello-World"},
		Sender:     ghUser{Login: "reviewer"},
	}
	body := mustMarshal(t, payload)
	signature := signPayload([]byte(testWebhookSecret), body)

	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	request.Header.Set("X-Hub-Signature-256", signature)
	request.Header.Set("X-GitHub-Event", "issue_comment")
	request.Header.Set("X-GitHub-Delivery", "pr-comment-001")
	recorder := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	event := handler.lastEvent()
	if event == nil {
		t.Fatal("no event produced")
	}
	if event.Comment == nil {
		t.Fatal("event.Comment is nil")
	}
	if event.Comment.EntityType != "pull_request" {
		t.Errorf("entity type = %q, want %q", event.Comment.EntityType, "pull_request")
	}
}

// --- Comment filtering (only "created" action) ---

func TestWebhookCommentIgnoresEdited(t *testing.T) {
	handler := newTestHandler()

	payload := ghIssueCommentPayload{
		Action: "edited",
		Issue: ghIssue{
			Number:  42,
			HTMLURL: "https://github.com/octocat/Hello-World/issues/42",
		},
		Comment: ghComment{
			Body: "Updated comment text.",
			User: ghUser{Login: "helper"},
		},
		Repository: ghRepository{FullName: "octocat/Hello-World"},
		Sender:     ghUser{Login: "helper"},
	}
	body := mustMarshal(t, payload)
	signature := signPayload([]byte(testWebhookSecret), body)

	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	request.Header.Set("X-Hub-Signature-256", signature)
	request.Header.Set("X-GitHub-Event", "issue_comment")
	request.Header.Set("X-GitHub-Delivery", "comment-edit-001")
	recorder := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if handler.eventCount() != 0 {
		t.Errorf("edited comment should not produce events, got %d", handler.eventCount())
	}
}

// --- Workflow run translation ---

func TestWebhookTranslateWorkflowRun(t *testing.T) {
	handler := newTestHandler()

	payload := ghWorkflowRunPayload{
		Action: "completed",
		WorkflowRun: ghWorkflowRun{
			ID:           12345,
			Name:         "CI",
			Status:       "completed",
			Conclusion:   "success",
			HeadSHA:      "sha-abc",
			HeadBranch:   "main",
			HTMLURL:      "https://github.com/octocat/Hello-World/actions/runs/12345",
			PullRequests: []ghPR{{Number: 99}},
		},
		Repository: ghRepository{FullName: "octocat/Hello-World"},
		Sender:     ghUser{Login: "github-actions"},
	}
	body := mustMarshal(t, payload)
	signature := signPayload([]byte(testWebhookSecret), body)

	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	request.Header.Set("X-Hub-Signature-256", signature)
	request.Header.Set("X-GitHub-Event", "workflow_run")
	request.Header.Set("X-GitHub-Delivery", "workflow-001")
	recorder := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	event := handler.lastEvent()
	if event == nil {
		t.Fatal("no event produced")
	}
	if event.Type != forge.EventCategoryCIStatus {
		t.Errorf("event type = %q, want %q", event.Type, forge.EventCategoryCIStatus)
	}
	if event.CIStatus == nil {
		t.Fatal("event.CIStatus is nil")
	}
	if event.CIStatus.RunID != "12345" {
		t.Errorf("run ID = %q, want %q", event.CIStatus.RunID, "12345")
	}
	if event.CIStatus.Workflow != "CI" {
		t.Errorf("workflow = %q, want %q", event.CIStatus.Workflow, "CI")
	}
	if event.CIStatus.Conclusion != "success" {
		t.Errorf("conclusion = %q, want %q", event.CIStatus.Conclusion, "success")
	}
	if event.CIStatus.PRNumber != 99 {
		t.Errorf("PR number = %d, want 99", event.CIStatus.PRNumber)
	}
	if event.CIStatus.HeadSHA != "sha-abc" {
		t.Errorf("head SHA = %q, want %q", event.CIStatus.HeadSHA, "sha-abc")
	}
}

// --- Unknown event type ---

func TestWebhookUnknownEventReturnsOK(t *testing.T) {
	handler := newTestHandler()

	body := `{"action":"some_action"}`
	signature := signPayload([]byte(testWebhookSecret), []byte(body))
	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	request.Header.Set("X-Hub-Signature-256", signature)
	request.Header.Set("X-GitHub-Event", "some_future_event")
	request.Header.Set("X-GitHub-Delivery", "unknown-001")
	recorder := httptest.NewRecorder()
	handler.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if handler.eventCount() != 0 {
		t.Errorf("unknown event type should not produce events, got %d", handler.eventCount())
	}
}

// --- Push summary formatting ---

func TestFormatPushSummary(t *testing.T) {
	tests := []struct {
		name     string
		payload  ghPushPayload
		expected string
	}{
		{
			name: "single_commit",
			payload: ghPushPayload{
				Ref:        "refs/heads/main",
				Repository: ghRepository{FullName: "octocat/Hello-World"},
				Sender:     ghUser{Login: "octocat"},
				Commits:    []ghCommit{{ID: "abc"}},
			},
			expected: "[octocat/Hello-World] octocat pushed 1 commit to main",
		},
		{
			name: "multiple_commits",
			payload: ghPushPayload{
				Ref:        "refs/heads/feature",
				Repository: ghRepository{FullName: "octocat/Hello-World"},
				Sender:     ghUser{Login: "octocat"},
				Commits:    []ghCommit{{ID: "a"}, {ID: "b"}, {ID: "c"}},
			},
			expected: "[octocat/Hello-World] octocat pushed 3 commits to feature",
		},
		{
			name: "tag_ref",
			payload: ghPushPayload{
				Ref:        "refs/tags/v1.0.0",
				Repository: ghRepository{FullName: "octocat/Hello-World"},
				Sender:     ghUser{Login: "octocat"},
				Commits:    []ghCommit{{ID: "a"}},
			},
			expected: "[octocat/Hello-World] octocat pushed 1 commit to refs/tags/v1.0.0",
		},
		{
			name: "zero_commits",
			payload: ghPushPayload{
				Ref:        "refs/heads/new-branch",
				Repository: ghRepository{FullName: "octocat/Hello-World"},
				Sender:     ghUser{Login: "octocat"},
				Commits:    nil,
			},
			expected: "[octocat/Hello-World] octocat pushed 0 commits to new-branch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatPushSummary(tt.payload)
			if result != tt.expected {
				t.Errorf("formatPushSummary() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// --- Constructor validation ---

func TestNewWebhookHandlerPanics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	callback := func(*forge.Event) {}

	tests := []struct {
		name    string
		secret  []byte
		logger  *slog.Logger
		onEvent func(*forge.Event)
	}{
		{
			name:    "nil_secret",
			secret:  nil,
			logger:  logger,
			onEvent: callback,
		},
		{
			name:    "empty_secret",
			secret:  []byte{},
			logger:  logger,
			onEvent: callback,
		},
		{
			name:    "nil_logger",
			secret:  []byte("secret"),
			logger:  nil,
			onEvent: callback,
		},
		{
			name:    "nil_callback",
			secret:  []byte("secret"),
			logger:  logger,
			onEvent: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Error("NewWebhookHandler did not panic")
				}
			}()
			NewWebhookHandler(tt.secret, tt.logger, tt.onEvent)
		})
	}
}

// --- Test helpers ---

// buildPushPayload creates a valid push webhook payload for testing.
func buildPushPayload(t *testing.T) []byte {
	t.Helper()
	payload := ghPushPayload{
		Ref:    "refs/heads/main",
		Before: "0000000000000000000000000000000000000000",
		After:  "abc123def456",
		Repository: ghRepository{
			FullName: "octocat/Hello-World",
			HTMLURL:  "https://github.com/octocat/Hello-World",
		},
		Sender: ghUser{Login: "octocat", ID: 1},
		Commits: []ghCommit{
			{
				ID:        "abc123def456",
				Message:   "Update README",
				Timestamp: "2026-01-15T10:00:00Z",
				URL:       "https://github.com/octocat/Hello-World/commit/abc123def456",
				Author:    ghAuthor{Name: "Mona Lisa", Email: "mona@github.com", Username: "octocat"},
				Added:     []string{"new-file.txt"},
				Modified:  []string{"README.md"},
			},
		},
		CompareURL: "https://github.com/octocat/Hello-World/compare/000...abc",
	}
	return mustMarshal(t, payload)
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return data
}
