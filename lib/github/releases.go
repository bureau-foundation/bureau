// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// GetReleaseByTag retrieves a release by its tag name.
func (client *Client) GetReleaseByTag(ctx context.Context, owner, repo, tag string) (*Release, error) {
	var release Release
	path := fmt.Sprintf("/repos/%s/%s/releases/tags/%s", owner, repo, url.PathEscape(tag))
	if err := client.get(ctx, path, &release); err != nil {
		return nil, fmt.Errorf("getting release %q in %s/%s: %w", tag, owner, repo, err)
	}
	return &release, nil
}

// DownloadReleaseAsset downloads a release asset's binary content.
// The caller is responsible for closing the returned ReadCloser.
//
// GitHub returns a 302 redirect to a short-lived URL for the asset
// content. The http.Client follows the redirect automatically.
// The Accept header must be set to "application/octet-stream" to
// request the binary content instead of the asset metadata JSON.
func (client *Client) DownloadReleaseAsset(ctx context.Context, owner, repo string, assetID int64) (io.ReadCloser, error) {
	path := fmt.Sprintf("/repos/%s/%s/releases/assets/%d", owner, repo, assetID)
	url := client.baseURL + path

	// Build the request manually to set the Accept header for binary
	// content. The standard doRaw sets Accept: application/vnd.github+json
	// which returns the asset metadata JSON instead of the binary.
	if err := client.rateLimit.wait(ctx); err != nil {
		return nil, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("github: creating request: %w", err)
	}

	authHeader, err := client.auth.AuthorizationHeader(ctx)
	if err != nil {
		return nil, fmt.Errorf("github: authentication: %w", err)
	}
	request.Header.Set("Authorization", authHeader)
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("X-GitHub-Api-Version", githubAPIVersion)

	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("downloading asset %d in %s/%s: %w", assetID, owner, repo, err)
	}

	client.rateLimit.update(response.Header)

	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return nil, parseAPIError(response)
	}

	return response.Body, nil
}
