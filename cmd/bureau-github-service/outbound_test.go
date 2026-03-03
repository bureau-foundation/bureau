// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/forgesub"
	"github.com/bureau-foundation/bureau/lib/github"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/forge"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// outboundClockEpoch is the fixed time used by the fake clock in
// outbound action tests. Token timestamps and service clock share
// this epoch.
var outboundClockEpoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// --- Mock GitHub HTTP server ---

// recordedRequest captures an HTTP request to the mock GitHub server.
type recordedRequest struct {
	Method string
	Path   string
	Body   string
}

// mockGitHubServer creates an httptest TLS server that records requests
// and dispatches canned responses by method+path. Callers register
// responses via the returned routes map before making requests.
//
// Route keys are "METHOD /path" (e.g., "POST /repos/acme/widgets/issues").
// Each route is a mockRoute with a status code and response body.
type mockRoute struct {
	Status int
	Body   any // JSON-encoded into response
}

type mockGitHub struct {
	server   *httptest.Server
	routes   map[string]mockRoute
	requests []recordedRequest
	mu       sync.Mutex
}

func newMockGitHub(t *testing.T) *mockGitHub {
	t.Helper()
	mock := &mockGitHub{
		routes: make(map[string]mockRoute),
	}

	mock.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		bodyBytes, _ := io.ReadAll(request.Body)

		mock.mu.Lock()
		mock.requests = append(mock.requests, recordedRequest{
			Method: request.Method,
			Path:   request.URL.Path,
			Body:   string(bodyBytes),
		})
		mock.mu.Unlock()

		key := request.Method + " " + request.URL.Path
		route, found := mock.routes[key]
		if !found {
			writer.WriteHeader(http.StatusNotFound)
			json.NewEncoder(writer).Encode(map[string]string{
				"message": "no mock route for " + key,
			})
			return
		}

		writer.WriteHeader(route.Status)
		if route.Body != nil {
			json.NewEncoder(writer).Encode(route.Body)
		}
	}))

	t.Cleanup(mock.server.Close)
	return mock
}

func (mock *mockGitHub) lastRequest() recordedRequest {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.requests) == 0 {
		return recordedRequest{}
	}
	return mock.requests[len(mock.requests)-1]
}

// --- Test infrastructure ---

// outboundTestEnv holds the test server, client, and mock GitHub.
type outboundTestEnv struct {
	client  *service.ServiceClient
	mock    *mockGitHub
	cleanup func()
}

func newOutboundTestEnv(t *testing.T, grants []servicetoken.Grant) *outboundTestEnv {
	t.Helper()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}

	testClock := clock.Fake(outboundClockEpoch)
	authConfig := &service.AuthConfig{
		PublicKey: publicKey,
		Audience:  "github",
		Blacklist: servicetoken.NewBlacklist(),
		Clock:     testClock,
	}

	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "github.sock")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := service.NewSocketServer(socketPath, logger, authConfig)

	mock := newMockGitHub(t)

	githubClient, err := github.NewClient(github.Config{
		BaseURL:    mock.server.URL,
		Token:      "test-token",
		HTTPClient: mock.server.Client(),
		Clock:      testClock,
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("creating GitHub client: %v", err)
	}

	githubService := &GitHubService{
		manager:      forgesub.NewManager(testClock, logger),
		githubClient: githubClient,
		clock:        testClock,
		logger:       logger,
	}
	githubService.registerActions(server)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()
	outboundWaitForSocket(t, socketPath)

	token := &servicetoken.Token{
		Subject:   ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
		Machine:   outboundMustParseMachine("@bureau/fleet/prod/machine/test:bureau.local"),
		Audience:  "github",
		Grants:    grants,
		ID:        "test-token",
		IssuedAt:  outboundClockEpoch.Add(-5 * time.Minute).Unix(),
		ExpiresAt: outboundClockEpoch.Add(5 * time.Minute).Unix(),
	}
	tokenBytes, err := servicetoken.Mint(privateKey, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	client := service.NewServiceClientFromToken(socketPath, tokenBytes)

	return &outboundTestEnv{
		client: client,
		mock:   mock,
		cleanup: func() {
			cancel()
			wg.Wait()
		},
	}
}

// outboundWaitForSocket polls until the socket is accepting connections.
func outboundWaitForSocket(t *testing.T, path string) {
	t.Helper()
	for range 500 {
		conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(time.Millisecond) //nolint:realclock — polling for socket readiness
	}
	t.Fatalf("socket %s not accepting connections within timeout", path)
}

// defaultGrants returns grants that cover all outbound GitHub actions.
func defaultGrants() []servicetoken.Grant {
	return []servicetoken.Grant{{Actions: []string{"github/*"}}}
}

// --- Tests ---

func TestCreateIssueAction(t *testing.T) {
	t.Parallel()
	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	env.mock.routes["POST /repos/acme/widgets/issues"] = mockRoute{
		Status: http.StatusCreated,
		Body:   github.Issue{Number: 42, HTMLURL: "https://github.com/acme/widgets/issues/42"},
	}

	var response createIssueResponse
	err := env.client.Call(context.Background(),
		forge.ProviderAction(forge.ProviderGitHub, forge.ActionCreateIssue),
		map[string]any{
			"repo":      "acme/widgets",
			"title":     "Bug in parser",
			"body":      "The parser crashes on empty input",
			"labels":    []string{"bug"},
			"assignees": []string{"alice"},
		},
		&response,
	)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if response.Number != 42 {
		t.Errorf("response.Number = %d, want 42", response.Number)
	}
	if response.HTMLURL != "https://github.com/acme/widgets/issues/42" {
		t.Errorf("response.HTMLURL = %q, want %q", response.HTMLURL, "https://github.com/acme/widgets/issues/42")
	}

	recorded := env.mock.lastRequest()
	if recorded.Method != "POST" {
		t.Errorf("recorded method = %q, want POST", recorded.Method)
	}
	if recorded.Path != "/repos/acme/widgets/issues" {
		t.Errorf("recorded path = %q, want /repos/acme/widgets/issues", recorded.Path)
	}
	if !strings.Contains(recorded.Body, `"title":"Bug in parser"`) {
		t.Errorf("recorded body missing title: %s", recorded.Body)
	}
}

func TestCreateCommentAction(t *testing.T) {
	t.Parallel()
	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	env.mock.routes["POST /repos/acme/widgets/issues/42/comments"] = mockRoute{
		Status: http.StatusCreated,
		Body:   github.Comment{ID: 999, HTMLURL: "https://github.com/acme/widgets/issues/42#issuecomment-999"},
	}

	var response createCommentResponse
	err := env.client.Call(context.Background(),
		forge.ProviderAction(forge.ProviderGitHub, forge.ActionCreateComment),
		map[string]any{
			"repo":   "acme/widgets",
			"number": 42,
			"body":   "This is a comment",
		},
		&response,
	)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if response.ID != 999 {
		t.Errorf("response.ID = %d, want 999", response.ID)
	}

	recorded := env.mock.lastRequest()
	if recorded.Path != "/repos/acme/widgets/issues/42/comments" {
		t.Errorf("recorded path = %q, want /repos/acme/widgets/issues/42/comments", recorded.Path)
	}
	if !strings.Contains(recorded.Body, `"body":"This is a comment"`) {
		t.Errorf("recorded body missing comment text: %s", recorded.Body)
	}
}

func TestCreateReviewAction(t *testing.T) {
	t.Parallel()
	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	env.mock.routes["POST /repos/acme/widgets/pulls/7/reviews"] = mockRoute{
		Status: http.StatusOK,
		Body:   github.Review{ID: 555, State: "APPROVED", HTMLURL: "https://github.com/acme/widgets/pull/7#pullrequestreview-555"},
	}

	var response createReviewResponse
	err := env.client.Call(context.Background(),
		forge.ProviderAction(forge.ProviderGitHub, forge.ActionCreateReview),
		map[string]any{
			"repo":   "acme/widgets",
			"number": 7,
			"body":   "LGTM",
			"event":  "APPROVE",
		},
		&response,
	)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if response.ID != 555 {
		t.Errorf("response.ID = %d, want 555", response.ID)
	}
	if response.State != "APPROVED" {
		t.Errorf("response.State = %q, want APPROVED", response.State)
	}

	recorded := env.mock.lastRequest()
	if recorded.Path != "/repos/acme/widgets/pulls/7/reviews" {
		t.Errorf("recorded path = %q, want /repos/acme/widgets/pulls/7/reviews", recorded.Path)
	}
	if !strings.Contains(recorded.Body, `"event":"APPROVE"`) {
		t.Errorf("recorded body missing event: %s", recorded.Body)
	}
}

func TestMergePRAction(t *testing.T) {
	t.Parallel()
	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	env.mock.routes["PUT /repos/acme/widgets/pulls/7/merge"] = mockRoute{
		Status: http.StatusOK,
		Body:   github.PullRequestMergeResult{SHA: "abc123merge", Merged: true, Message: "Pull Request successfully merged"},
	}

	var response mergePRResponse
	err := env.client.Call(context.Background(),
		forge.ProviderAction(forge.ProviderGitHub, forge.ActionMergePR),
		map[string]any{
			"repo":           "acme/widgets",
			"number":         7,
			"commit_title":   "Merge PR #7",
			"commit_message": "Approved by bureau",
			"merge_method":   "squash",
			"sha":            "headsha123",
		},
		&response,
	)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if !response.Merged {
		t.Errorf("response.Merged = false, want true")
	}
	if response.SHA != "abc123merge" {
		t.Errorf("response.SHA = %q, want %q", response.SHA, "abc123merge")
	}

	recorded := env.mock.lastRequest()
	if recorded.Method != "PUT" {
		t.Errorf("recorded method = %q, want PUT", recorded.Method)
	}
	if recorded.Path != "/repos/acme/widgets/pulls/7/merge" {
		t.Errorf("recorded path = %q, want /repos/acme/widgets/pulls/7/merge", recorded.Path)
	}
	if !strings.Contains(recorded.Body, `"merge_method":"squash"`) {
		t.Errorf("recorded body missing merge_method: %s", recorded.Body)
	}
}

func TestTriggerWorkflowAction(t *testing.T) {
	t.Parallel()
	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	env.mock.routes["POST /repos/acme/widgets/actions/workflows/ci.yml/dispatches"] = mockRoute{
		Status: http.StatusNoContent,
		Body:   nil,
	}

	err := env.client.Call(context.Background(),
		forge.ProviderAction(forge.ProviderGitHub, forge.ActionTriggerWorkflow),
		map[string]any{
			"repo":        "acme/widgets",
			"workflow_id": "ci.yml",
			"ref":         "refs/heads/main",
			"inputs":      map[string]string{"suite": "full"},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	recorded := env.mock.lastRequest()
	if recorded.Path != "/repos/acme/widgets/actions/workflows/ci.yml/dispatches" {
		t.Errorf("recorded path = %q, want /repos/acme/widgets/actions/workflows/ci.yml/dispatches", recorded.Path)
	}
	if !strings.Contains(recorded.Body, `"ref":"refs/heads/main"`) {
		t.Errorf("recorded body missing ref: %s", recorded.Body)
	}
}

func TestReportStatusAction(t *testing.T) {
	t.Parallel()
	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	env.mock.routes["POST /repos/acme/widgets/statuses/abc123def456"] = mockRoute{
		Status: http.StatusCreated,
		Body:   github.CommitStatus{ID: 1, State: "success"},
	}

	var response reportStatusResponse
	err := env.client.Call(context.Background(),
		forge.ProviderAction(forge.ProviderGitHub, forge.ActionReportStatus),
		map[string]any{
			"repo":        "acme/widgets",
			"sha":         "abc123def456",
			"state":       "success",
			"target_url":  "https://bureau.example.com/run/123",
			"description": "All checks passed",
			"context":     "ci/bureau",
		},
		&response,
	)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if response.ID != 1 {
		t.Errorf("response.ID = %d, want 1", response.ID)
	}
	if response.State != "success" {
		t.Errorf("response.State = %q, want %q", response.State, "success")
	}

	recorded := env.mock.lastRequest()
	if recorded.Path != "/repos/acme/widgets/statuses/abc123def456" {
		t.Errorf("recorded path = %q, want /repos/acme/widgets/statuses/abc123def456", recorded.Path)
	}
	if !strings.Contains(recorded.Body, `"state":"success"`) {
		t.Errorf("recorded body missing state: %s", recorded.Body)
	}
}

// --- Error cases ---

func TestOutboundRequiresGrant(t *testing.T) {
	t.Parallel()

	// Token with no grants at all.
	env := newOutboundTestEnv(t, nil)
	defer env.cleanup()

	actions := []string{
		forge.ActionCreateIssue,
		forge.ActionCreateComment,
		forge.ActionCreateReview,
		forge.ActionMergePR,
		forge.ActionTriggerWorkflow,
		forge.ActionReportStatus,
	}

	for _, action := range actions {
		t.Run(action, func(t *testing.T) {
			err := env.client.Call(context.Background(),
				forge.ProviderAction(forge.ProviderGitHub, action),
				map[string]any{"repo": "acme/widgets"},
				nil,
			)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var serviceErr *service.ServiceError
			if !errors.As(err, &serviceErr) {
				t.Fatalf("expected *ServiceError, got %T: %v", err, err)
			}
			if !strings.Contains(serviceErr.Message, "access denied") {
				t.Errorf("error message = %q, want to contain 'access denied'", serviceErr.Message)
			}
		})
	}
}

func TestOutboundRequiresClient(t *testing.T) {
	t.Parallel()

	// Build a test env with a nil githubClient.
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}

	testClock := clock.Fake(outboundClockEpoch)
	authConfig := &service.AuthConfig{
		PublicKey: publicKey,
		Audience:  "github",
		Blacklist: servicetoken.NewBlacklist(),
		Clock:     testClock,
	}

	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "github.sock")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := service.NewSocketServer(socketPath, logger, authConfig)

	githubService := &GitHubService{
		manager:      forgesub.NewManager(testClock, logger),
		githubClient: nil, // no outbound API
		clock:        testClock,
		logger:       logger,
	}
	githubService.registerActions(server)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(ctx)
	}()
	outboundWaitForSocket(t, socketPath)
	defer func() {
		cancel()
		wg.Wait()
	}()

	token := &servicetoken.Token{
		Subject:   ref.MustParseUserID("@bureau/fleet/prod/agent/tester:bureau.local"),
		Machine:   outboundMustParseMachine("@bureau/fleet/prod/machine/test:bureau.local"),
		Audience:  "github",
		Grants:    defaultGrants(),
		ID:        "test-token",
		IssuedAt:  outboundClockEpoch.Add(-5 * time.Minute).Unix(),
		ExpiresAt: outboundClockEpoch.Add(5 * time.Minute).Unix(),
	}
	tokenBytes, err := servicetoken.Mint(privateKey, token)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	client := service.NewServiceClientFromToken(socketPath, tokenBytes)

	err = client.Call(context.Background(),
		forge.ProviderAction(forge.ProviderGitHub, forge.ActionCreateIssue),
		map[string]any{
			"repo":  "acme/widgets",
			"title": "Test issue",
		},
		nil,
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "outbound GitHub API not configured") {
		t.Errorf("error = %q, want to contain 'outbound GitHub API not configured'", err.Error())
	}
}

func TestInvalidRepoSlug(t *testing.T) {
	t.Parallel()
	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	slugs := []string{"noslash", "", "/empty-owner", "empty-repo/"}
	for _, slug := range slugs {
		t.Run(slug, func(t *testing.T) {
			err := env.client.Call(context.Background(),
				forge.ProviderAction(forge.ProviderGitHub, forge.ActionCreateIssue),
				map[string]any{
					"repo":  slug,
					"title": "Test",
				},
				nil,
			)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			// Empty string hits "repo is required"; others hit parseRepoSlug error.
			if slug == "" {
				if !strings.Contains(err.Error(), "repo is required") {
					t.Errorf("error = %q, want to contain 'repo is required'", err.Error())
				}
			} else {
				if !strings.Contains(err.Error(), "invalid repo format") {
					t.Errorf("error = %q, want to contain 'invalid repo format'", err.Error())
				}
			}
		})
	}
}

func TestMissingRequiredFields(t *testing.T) {
	t.Parallel()
	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	tests := []struct {
		name    string
		action  string
		fields  map[string]any
		wantErr string
	}{
		{
			name:    "create_issue_no_title",
			action:  forge.ActionCreateIssue,
			fields:  map[string]any{"repo": "acme/widgets"},
			wantErr: "title is required",
		},
		{
			name:    "create_comment_no_body",
			action:  forge.ActionCreateComment,
			fields:  map[string]any{"repo": "acme/widgets", "number": 1},
			wantErr: "body is required",
		},
		{
			name:    "create_comment_no_number",
			action:  forge.ActionCreateComment,
			fields:  map[string]any{"repo": "acme/widgets", "body": "hello"},
			wantErr: "number is required",
		},
		{
			name:    "create_review_no_event",
			action:  forge.ActionCreateReview,
			fields:  map[string]any{"repo": "acme/widgets", "number": 7},
			wantErr: "event is required",
		},
		{
			name:    "merge_pr_no_number",
			action:  forge.ActionMergePR,
			fields:  map[string]any{"repo": "acme/widgets"},
			wantErr: "number is required",
		},
		{
			name:    "trigger_workflow_no_workflow_id",
			action:  forge.ActionTriggerWorkflow,
			fields:  map[string]any{"repo": "acme/widgets", "ref": "main"},
			wantErr: "workflow_id is required",
		},
		{
			name:    "trigger_workflow_no_ref",
			action:  forge.ActionTriggerWorkflow,
			fields:  map[string]any{"repo": "acme/widgets", "workflow_id": "ci.yml"},
			wantErr: "ref is required",
		},
		{
			name:    "report_status_no_sha",
			action:  forge.ActionReportStatus,
			fields:  map[string]any{"repo": "acme/widgets", "state": "success"},
			wantErr: "sha is required",
		},
		{
			name:    "report_status_no_state",
			action:  forge.ActionReportStatus,
			fields:  map[string]any{"repo": "acme/widgets", "sha": "abc123"},
			wantErr: "state is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := env.client.Call(context.Background(),
				forge.ProviderAction(forge.ProviderGitHub, tt.action),
				tt.fields,
				nil,
			)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// --- Helpers ---

// outboundMustParseMachine parses a raw machine Matrix user ID for test use.
func outboundMustParseMachine(raw string) ref.Machine {
	machine, err := ref.ParseMachineUserID(raw)
	if err != nil {
		panic(fmt.Sprintf("ParseMachineUserID(%q): %v", raw, err))
	}
	return machine
}

func TestParseRepoSlug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		slug      string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"acme/widgets", "acme", "widgets", false},
		{"bureau-foundation/bureau", "bureau-foundation", "bureau", false},
		{"noslash", "", "", true},
		{"", "", "", true},
		{"/empty-owner", "", "", true},
		{"empty-repo/", "", "", true},
		{"org/repo/extra", "org", "repo/extra", false}, // splitN preserves extra slashes
	}

	for _, tt := range tests {
		t.Run(tt.slug, func(t *testing.T) {
			owner, repo, err := parseRepoSlug(tt.slug)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseRepoSlug(%q): expected error, got nil", tt.slug)
				}
				return
			}
			if err != nil {
				t.Errorf("parseRepoSlug(%q): unexpected error: %v", tt.slug, err)
				return
			}
			if owner != tt.wantOwner {
				t.Errorf("owner = %q, want %q", owner, tt.wantOwner)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
		})
	}
}
