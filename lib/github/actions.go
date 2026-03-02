// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// DispatchWorkflowRequest contains the fields for triggering a GitHub
// Actions workflow.
type DispatchWorkflowRequest struct {
	// Ref is the git reference to run the workflow on (branch, tag, or SHA).
	Ref string `json:"ref"`

	// Inputs are the workflow input parameters. Keys must match the
	// workflow's workflow_dispatch input definitions.
	Inputs map[string]string `json:"inputs,omitempty"`
}

// DispatchWorkflow triggers a GitHub Actions workflow via the
// workflow_dispatch event. The workflowID can be the workflow file name
// (e.g., "ci.yml") or the workflow's numeric ID.
//
// Returns nil on success. GitHub returns 204 No Content — there is no
// response body. The resulting workflow run must be discovered via
// webhooks or polling.
func (client *Client) DispatchWorkflow(ctx context.Context, owner, repo, workflowID string, request DispatchWorkflowRequest) error {
	path := fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/dispatches", owner, repo, workflowID)

	body, _, err := client.do(ctx, http.MethodPost, path, request)
	if err != nil {
		return fmt.Errorf("dispatching workflow %s in %s/%s: %w", workflowID, owner, repo, err)
	}
	// GitHub returns 204 No Content on success. The do() method treats
	// 2xx as success, so body will be empty.
	_ = body
	return nil
}

// GetWorkflowRun retrieves a single workflow run by ID.
func (client *Client) GetWorkflowRun(ctx context.Context, owner, repo string, runID int64) (*WorkflowRun, error) {
	var run WorkflowRun
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d", owner, repo, runID)
	if err := client.get(ctx, path, &run); err != nil {
		return nil, fmt.Errorf("getting workflow run %d in %s/%s: %w", runID, owner, repo, err)
	}
	return &run, nil
}

// DownloadWorkflowLogs downloads the logs for a workflow run as a zip
// archive. The caller is responsible for closing the returned ReadCloser.
//
// GitHub returns a 302 redirect to a short-lived URL for the log
// archive. The http.Client follows the redirect automatically.
func (client *Client) DownloadWorkflowLogs(ctx context.Context, owner, repo string, runID int64) (io.ReadCloser, error) {
	url := client.baseURL + fmt.Sprintf("/repos/%s/%s/actions/runs/%d/logs", owner, repo, runID)

	response, err := client.doRaw(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("downloading logs for run %d in %s/%s: %w", runID, owner, repo, err)
	}

	// doRaw already updated rate limit state from the response headers.

	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return nil, parseAPIError(response)
	}

	return response.Body, nil
}
