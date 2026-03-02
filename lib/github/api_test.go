// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateIssue(t *testing.T) {
	var receivedBody CreateIssueRequest
	var receivedPath, receivedMethod string

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedPath = request.URL.Path
		receivedMethod = request.Method
		json.NewDecoder(request.Body).Decode(&receivedBody)

		writer.WriteHeader(http.StatusCreated)
		json.NewEncoder(writer).Encode(Issue{
			Number:  42,
			Title:   "Test Issue",
			HTMLURL: "https://github.com/owner/repo/issues/42",
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	issue, err := client.CreateIssue(context.Background(), "owner", "repo", CreateIssueRequest{
		Title:  "Test Issue",
		Body:   "Description",
		Labels: []string{"bug"},
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	if receivedMethod != "POST" {
		t.Errorf("method = %s, want POST", receivedMethod)
	}
	if receivedPath != "/repos/owner/repo/issues" {
		t.Errorf("path = %s, want /repos/owner/repo/issues", receivedPath)
	}
	if receivedBody.Title != "Test Issue" {
		t.Errorf("request.Title = %q, want %q", receivedBody.Title, "Test Issue")
	}
	if issue.Number != 42 {
		t.Errorf("issue.Number = %d, want 42", issue.Number)
	}
}

func TestGetPullRequest(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/owner/repo/pulls/7" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		json.NewEncoder(writer).Encode(PullRequest{
			Number: 7,
			Title:  "Fix bug",
			Head:   Branch{Ref: "fix-branch", SHA: "abc123"},
			Base:   Branch{Ref: "main", SHA: "def456"},
			Draft:  false,
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	pullRequest, err := client.GetPullRequest(context.Background(), "owner", "repo", 7)
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}

	if pullRequest.Number != 7 {
		t.Errorf("Number = %d, want 7", pullRequest.Number)
	}
	if pullRequest.Head.Ref != "fix-branch" {
		t.Errorf("Head.Ref = %q, want %q", pullRequest.Head.Ref, "fix-branch")
	}
}

func TestCreateReview(t *testing.T) {
	var receivedBody CreateReviewRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/owner/repo/pulls/7/reviews" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		json.NewDecoder(request.Body).Decode(&receivedBody)

		writer.WriteHeader(http.StatusOK)
		json.NewEncoder(writer).Encode(Review{
			ID:    1,
			State: "APPROVED",
			Body:  "LGTM",
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	review, err := client.CreateReview(context.Background(), "owner", "repo", 7, CreateReviewRequest{
		Body:  "LGTM",
		Event: "APPROVE",
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}

	if receivedBody.Event != "APPROVE" {
		t.Errorf("request.Event = %q, want %q", receivedBody.Event, "APPROVE")
	}
	if review.State != "APPROVED" {
		t.Errorf("review.State = %q, want %q", review.State, "APPROVED")
	}
}

func TestCreateTree(t *testing.T) {
	var receivedBody CreateTreeRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/owner/repo/git/trees" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		json.NewDecoder(request.Body).Decode(&receivedBody)

		writer.WriteHeader(http.StatusCreated)
		json.NewEncoder(writer).Encode(Tree{
			SHA: "tree-sha-123",
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	content := "hello world"
	tree, err := client.CreateTree(context.Background(), "owner", "repo", CreateTreeRequest{
		BaseTree: "base-sha",
		Entries: []CreateTreeEntry{
			{Path: "README.md", Mode: "100644", Type: "blob", Content: &content},
		},
	})
	if err != nil {
		t.Fatalf("CreateTree: %v", err)
	}

	if receivedBody.BaseTree != "base-sha" {
		t.Errorf("BaseTree = %q, want %q", receivedBody.BaseTree, "base-sha")
	}
	if len(receivedBody.Entries) != 1 {
		t.Fatalf("expected 1 tree entry, got %d", len(receivedBody.Entries))
	}
	if tree.SHA != "tree-sha-123" {
		t.Errorf("tree.SHA = %q, want %q", tree.SHA, "tree-sha-123")
	}
}

func TestCreateCommit(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/owner/repo/git/commits" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		writer.WriteHeader(http.StatusCreated)
		json.NewEncoder(writer).Encode(Commit{
			SHA:     "commit-sha-456",
			Message: "Add README",
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	commit, err := client.CreateCommit(context.Background(), "owner", "repo", CreateCommitRequest{
		Message: "Add README",
		Tree:    "tree-sha-123",
		Parents: []string{"parent-sha"},
	})
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}

	if commit.SHA != "commit-sha-456" {
		t.Errorf("commit.SHA = %q, want %q", commit.SHA, "commit-sha-456")
	}
}

func TestUpdateRef(t *testing.T) {
	var receivedBody struct {
		SHA   string `json:"sha"`
		Force bool   `json:"force"`
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.URL.Path, "/repos/owner/repo/git/refs/") {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		if request.Method != "PATCH" {
			t.Errorf("method = %s, want PATCH", request.Method)
		}
		json.NewDecoder(request.Body).Decode(&receivedBody)

		json.NewEncoder(writer).Encode(Ref{
			Ref:    "refs/heads/main",
			Object: RefObject{SHA: "commit-sha-456", Type: "commit"},
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	ref, err := client.UpdateRef(context.Background(), "owner", "repo", "heads/main", "commit-sha-456", false)
	if err != nil {
		t.Fatalf("UpdateRef: %v", err)
	}

	if receivedBody.SHA != "commit-sha-456" {
		t.Errorf("request.SHA = %q, want %q", receivedBody.SHA, "commit-sha-456")
	}
	if receivedBody.Force != false {
		t.Errorf("request.Force = %v, want false", receivedBody.Force)
	}
	if ref.Object.SHA != "commit-sha-456" {
		t.Errorf("ref.Object.SHA = %q, want %q", ref.Object.SHA, "commit-sha-456")
	}
}

func TestDispatchWorkflow(t *testing.T) {
	var receivedBody DispatchWorkflowRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/owner/repo/actions/workflows/ci.yml/dispatches" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		json.NewDecoder(request.Body).Decode(&receivedBody)

		// GitHub returns 204 No Content on success.
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := newTestClient(t, server)
	err := client.DispatchWorkflow(context.Background(), "owner", "repo", "ci.yml", DispatchWorkflowRequest{
		Ref:    "refs/heads/main",
		Inputs: map[string]string{"test_suite": "full"},
	})
	if err != nil {
		t.Fatalf("DispatchWorkflow: %v", err)
	}

	if receivedBody.Ref != "refs/heads/main" {
		t.Errorf("request.Ref = %q, want %q", receivedBody.Ref, "refs/heads/main")
	}
	if receivedBody.Inputs["test_suite"] != "full" {
		t.Errorf("request.Inputs[test_suite] = %q, want %q", receivedBody.Inputs["test_suite"], "full")
	}
}

func TestCreateCommitStatus(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/owner/repo/statuses/abc123def456" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		writer.WriteHeader(http.StatusCreated)
		json.NewEncoder(writer).Encode(CommitStatus{
			ID:      1,
			State:   "success",
			Context: "ci/bureau",
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	status, err := client.CreateCommitStatus(context.Background(), "owner", "repo", "abc123def456", CreateStatusRequest{
		State:       "success",
		Description: "All checks passed",
		Context:     "ci/bureau",
	})
	if err != nil {
		t.Fatalf("CreateCommitStatus: %v", err)
	}

	if status.State != "success" {
		t.Errorf("status.State = %q, want %q", status.State, "success")
	}
}

func TestCreateRepoWebhook(t *testing.T) {
	var receivedBody CreateWebhookRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/owner/repo/hooks" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		json.NewDecoder(request.Body).Decode(&receivedBody)

		writer.WriteHeader(http.StatusCreated)
		json.NewEncoder(writer).Encode(Webhook{
			ID:     12345,
			Active: true,
			Events: []string{"push", "pull_request"},
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	webhook, err := client.CreateRepoWebhook(context.Background(), "owner", "repo", CreateWebhookRequest{
		Events: []string{"push", "pull_request"},
		Config: CreateWebhookConfig{
			URL:         "https://bureau.example.com/webhook",
			ContentType: "json",
			Secret:      "webhook-secret",
		},
	})
	if err != nil {
		t.Fatalf("CreateRepoWebhook: %v", err)
	}

	if receivedBody.Config.URL != "https://bureau.example.com/webhook" {
		t.Errorf("request.Config.URL = %q, want %q", receivedBody.Config.URL, "https://bureau.example.com/webhook")
	}
	if webhook.ID != 12345 {
		t.Errorf("webhook.ID = %d, want 12345", webhook.ID)
	}
}

func TestPageIterator(t *testing.T) {
	page := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		page++
		switch page {
		case 1:
			nextURL := "https://" + request.Host + "/repos/owner/repo/issues?page=2"
			writer.Header().Set("Link", `<`+nextURL+`>; rel="next"`)
			json.NewEncoder(writer).Encode([]Issue{
				{Number: 1, Title: "First"},
				{Number: 2, Title: "Second"},
			})
		case 2:
			// Last page: no Link header.
			json.NewEncoder(writer).Encode([]Issue{
				{Number: 3, Title: "Third"},
			})
		default:
			t.Errorf("unexpected page %d", page)
			writer.WriteHeader(500)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server)
	issues, err := client.ListIssues(context.Background(), "owner", "repo", ListIssuesOptions{}).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(issues) != 3 {
		t.Fatalf("expected 3 issues, got %d", len(issues))
	}
	if issues[0].Number != 1 || issues[1].Number != 2 || issues[2].Number != 3 {
		t.Errorf("issues = %v, want numbers 1,2,3", issues)
	}
}

func TestDownloadWorkflowLogs(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/owner/repo/actions/runs/12345/logs" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/zip")
		writer.Write([]byte("PK\x03\x04fake-zip-content"))
	}))
	defer server.Close()

	client := newTestClient(t, server)
	reader, err := client.DownloadWorkflowLogs(context.Background(), "owner", "repo", 12345)
	if err != nil {
		t.Fatalf("DownloadWorkflowLogs: %v", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading logs: %v", err)
	}
	if !strings.HasPrefix(string(data), "PK") {
		t.Errorf("expected zip content, got %q", string(data[:4]))
	}
}
