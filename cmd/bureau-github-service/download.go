// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema/forge"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// defaultMaxAssetSize is the maximum download size for release assets
// (256 MB). This bounds memory consumption since the service holds the
// full artifact in memory for digest computation and CBOR encoding.
// Callers can override via the max_size request field.
const defaultMaxAssetSize int64 = 256 * 1024 * 1024

// downloadReleaseAssetRequest is the CBOR request body for the
// "github/download-release-asset" socket action.
type downloadReleaseAssetRequest struct {
	// Repo is the "owner/repo" slug.
	Repo string `json:"repo"`

	// Tag is the release tag name (e.g., "v1.2.3").
	Tag string `json:"tag"`

	// Asset is the release asset filename to download.
	Asset string `json:"asset"`

	// MaxSize is an optional size limit in bytes. When zero, the
	// default (256 MB) is used. Clamped to the server-side ceiling
	// of 256 MB to prevent memory exhaustion — callers cannot
	// request a larger limit than the default.
	MaxSize int64 `json:"max_size,omitempty"`
}

// downloadReleaseAssetResponse is the CBOR response body for the
// "github/download-release-asset" socket action.
type downloadReleaseAssetResponse struct {
	// Content is the raw asset bytes.
	Content []byte `json:"content"`

	// Size is the byte count of the downloaded content.
	Size int64 `json:"size"`

	// SHA256 is the hex-encoded SHA-256 digest of the content.
	SHA256 string `json:"sha256"`

	// Provenance is the provenance verification result. Nil when no
	// provenance policy is configured for the fleet.
	Provenance *provenanceResult `json:"provenance,omitempty"`
}

// handleDownloadReleaseAsset downloads a release asset from GitHub,
// computes its SHA-256 digest, verifies provenance attestations if
// configured, and returns the content with verification metadata.
//
// The service is the trust boundary: when enforcement is "require",
// the agent never sees content from unverified artifacts. The
// download is rejected before the content reaches the response.
func (gs *GitHubService) handleDownloadReleaseAsset(
	ctx context.Context,
	token *servicetoken.Token,
	raw []byte,
) (any, error) {
	action := forge.ProviderAction(forge.ProviderGitHub, forge.ActionDownloadReleaseAsset)
	if !servicetoken.GrantsAllow(token.Grants, action, "") {
		return nil, fmt.Errorf("access denied: missing grant for %s", action)
	}

	client, err := gs.requireGitHubClient()
	if err != nil {
		return nil, err
	}

	var request downloadReleaseAssetRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	if request.Repo == "" {
		return nil, fmt.Errorf("repo is required")
	}
	if request.Tag == "" {
		return nil, fmt.Errorf("tag is required")
	}
	if request.Asset == "" {
		return nil, fmt.Errorf("asset is required")
	}

	owner, repo, err := parseRepoSlug(request.Repo)
	if err != nil {
		return nil, err
	}

	maxSize := request.MaxSize
	if maxSize <= 0 || maxSize > defaultMaxAssetSize {
		maxSize = defaultMaxAssetSize
	}

	// Fetch the release to find the asset ID.
	release, err := client.GetReleaseByTag(ctx, owner, repo, request.Tag)
	if err != nil {
		return nil, fmt.Errorf("getting release %q: %w", request.Tag, err)
	}

	var assetID int64
	var assetSize int64
	found := false
	for _, asset := range release.Assets {
		if asset.Name == request.Asset {
			assetID = asset.ID
			assetSize = asset.Size
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("asset %q not found in release %q (release has %d assets)",
			request.Asset, request.Tag, len(release.Assets))
	}

	// Check the declared asset size against the limit before
	// downloading. GitHub reports the size in the release metadata.
	if assetSize > maxSize {
		return nil, fmt.Errorf("asset %q is %d bytes, exceeds max_size %d",
			request.Asset, assetSize, maxSize)
	}

	// Download the asset content.
	body, err := client.DownloadReleaseAsset(ctx, owner, repo, assetID)
	if err != nil {
		return nil, fmt.Errorf("downloading asset %q: %w", request.Asset, err)
	}
	defer body.Close()

	// Read with a size limit to guard against GitHub reporting a
	// smaller size than the actual content. LimitReader ensures we
	// never allocate more than maxSize+1 bytes.
	content, err := io.ReadAll(io.LimitReader(body, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading asset content: %w", err)
	}
	if int64(len(content)) > maxSize {
		return nil, fmt.Errorf("asset %q actual size exceeds max_size %d",
			request.Asset, maxSize)
	}

	label := fmt.Sprintf("%s/%s@%s/%s", owner, repo, request.Tag, request.Asset)

	// Compute the SHA-256 digest.
	digest := computeSHA256(content)

	// Verify provenance if a verifier is configured.
	provenanceResultValue, allowed := gs.verifyArtifactProvenance(
		ctx, client, owner, repo, digest, label)
	if !allowed {
		return nil, fmt.Errorf("provenance verification failed for %s: %s",
			label, provenanceResultValue.Error)
	}

	return downloadReleaseAssetResponse{
		Content:    content,
		Size:       int64(len(content)),
		SHA256:     hex.EncodeToString(digest),
		Provenance: provenanceResultValue,
	}, nil
}
