// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/hex"
	"net/http"
	"testing"

	"github.com/bureau-foundation/bureau/lib/github"
	"github.com/bureau-foundation/bureau/lib/schema/forge"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

func TestDownloadReleaseAsset_MissingGrant(t *testing.T) {
	t.Parallel()

	env := newOutboundTestEnv(t, []servicetoken.Grant{
		{Actions: []string{forge.ProviderAction(forge.ProviderGitHub, forge.ActionCreateIssue)}},
	})
	defer env.cleanup()

	var response downloadReleaseAssetResponse
	err := env.client.Call(context.Background(),
		forge.ProviderAction(forge.ProviderGitHub, forge.ActionDownloadReleaseAsset),
		downloadReleaseAssetRequest{
			Repo:  "acme/widgets",
			Tag:   "v1.0.0",
			Asset: "widget.tar.gz",
		},
		&response,
	)
	if err == nil {
		t.Fatal("expected access denied error for missing grant")
	}
}

func TestDownloadReleaseAsset_ReleaseNotFound(t *testing.T) {
	t.Parallel()

	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	// No route for the release tag — mock returns 404.
	var response downloadReleaseAssetResponse
	err := env.client.Call(context.Background(),
		forge.ProviderAction(forge.ProviderGitHub, forge.ActionDownloadReleaseAsset),
		downloadReleaseAssetRequest{
			Repo:  "acme/widgets",
			Tag:   "v0.0.0",
			Asset: "widget.tar.gz",
		},
		&response,
	)
	if err == nil {
		t.Fatal("expected error for release not found")
	}
}

func TestDownloadReleaseAsset_AssetNotFoundInRelease(t *testing.T) {
	t.Parallel()

	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	env.mock.routes["GET /repos/acme/widgets/releases/tags/v1.0.0"] = mockRoute{
		Status: http.StatusOK,
		Body: github.Release{
			ID:      1,
			TagName: "v1.0.0",
			Assets: []github.ReleaseAsset{
				{ID: 100, Name: "other-file.tar.gz", Size: 1024},
			},
		},
	}

	var response downloadReleaseAssetResponse
	err := env.client.Call(context.Background(),
		forge.ProviderAction(forge.ProviderGitHub, forge.ActionDownloadReleaseAsset),
		downloadReleaseAssetRequest{
			Repo:  "acme/widgets",
			Tag:   "v1.0.0",
			Asset: "widget.tar.gz",
		},
		&response,
	)
	if err == nil {
		t.Fatal("expected error for asset not found in release")
	}
}

func TestDownloadReleaseAsset_SizeLimitExceeded(t *testing.T) {
	t.Parallel()

	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	env.mock.routes["GET /repos/acme/widgets/releases/tags/v1.0.0"] = mockRoute{
		Status: http.StatusOK,
		Body: github.Release{
			ID:      1,
			TagName: "v1.0.0",
			Assets: []github.ReleaseAsset{
				{ID: 100, Name: "widget.tar.gz", Size: 2048},
			},
		},
	}

	var response downloadReleaseAssetResponse
	err := env.client.Call(context.Background(),
		forge.ProviderAction(forge.ProviderGitHub, forge.ActionDownloadReleaseAsset),
		downloadReleaseAssetRequest{
			Repo:    "acme/widgets",
			Tag:     "v1.0.0",
			Asset:   "widget.tar.gz",
			MaxSize: 1024,
		},
		&response,
	)
	if err == nil {
		t.Fatal("expected error for size limit exceeded")
	}
}

func TestDownloadReleaseAsset_MaxSizeClampedToServerCeiling(t *testing.T) {
	t.Parallel()

	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	// Asset declared at 1 byte over the server ceiling. Without
	// clamping, the caller-specified MaxSize (2x ceiling) would
	// allow the download. With clamping, the effective limit is
	// the server ceiling and the declared size check rejects it.
	env.mock.routes["GET /repos/acme/widgets/releases/tags/v1.0.0"] = mockRoute{
		Status: http.StatusOK,
		Body: github.Release{
			ID:      1,
			TagName: "v1.0.0",
			Assets: []github.ReleaseAsset{
				{ID: 100, Name: "widget.tar.gz", Size: defaultMaxAssetSize + 1},
			},
		},
	}

	var response downloadReleaseAssetResponse
	err := env.client.Call(context.Background(),
		forge.ProviderAction(forge.ProviderGitHub, forge.ActionDownloadReleaseAsset),
		downloadReleaseAssetRequest{
			Repo:    "acme/widgets",
			Tag:     "v1.0.0",
			Asset:   "widget.tar.gz",
			MaxSize: defaultMaxAssetSize * 2,
		},
		&response,
	)
	if err == nil {
		t.Fatal("expected error: client-specified MaxSize should be clamped to server ceiling")
	}
}

func TestDownloadReleaseAsset_MissingFields(t *testing.T) {
	t.Parallel()

	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	tests := []struct {
		name    string
		request downloadReleaseAssetRequest
	}{
		{
			name: "missing repo",
			request: downloadReleaseAssetRequest{
				Tag:   "v1.0.0",
				Asset: "widget.tar.gz",
			},
		},
		{
			name: "missing tag",
			request: downloadReleaseAssetRequest{
				Repo:  "acme/widgets",
				Asset: "widget.tar.gz",
			},
		},
		{
			name: "missing asset",
			request: downloadReleaseAssetRequest{
				Repo: "acme/widgets",
				Tag:  "v1.0.0",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var response downloadReleaseAssetResponse
			err := env.client.Call(context.Background(),
				forge.ProviderAction(forge.ProviderGitHub, forge.ActionDownloadReleaseAsset),
				test.request,
				&response,
			)
			if err == nil {
				t.Fatalf("expected error for %s", test.name)
			}
		})
	}
}

func TestDownloadReleaseAsset_PathTraversalInRepo(t *testing.T) {
	t.Parallel()

	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	tests := []struct {
		name string
		repo string
	}{
		{name: "path traversal", repo: "../evil/../../admin"},
		{name: "query injection", repo: "owner/repo?admin=true"},
		{name: "fragment injection", repo: "owner/repo#evil"},
		{name: "slash in owner", repo: "ow/ner/repo"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var response downloadReleaseAssetResponse
			err := env.client.Call(context.Background(),
				forge.ProviderAction(forge.ProviderGitHub, forge.ActionDownloadReleaseAsset),
				downloadReleaseAssetRequest{
					Repo:  test.repo,
					Tag:   "v1.0.0",
					Asset: "widget.tar.gz",
				},
				&response,
			)
			if err == nil {
				t.Fatalf("expected error for repo %q", test.repo)
			}
		})
	}
}

func TestDownloadReleaseAsset_PathTraversalInTag(t *testing.T) {
	t.Parallel()

	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	// Adversarial tags that attempt to escape the
	// /repos/{owner}/{repo}/releases/tags/{tag} URL path.
	// url.PathEscape in GetReleaseByTag prevents these from
	// reaching unintended API endpoints.
	tests := []struct {
		name string
		tag  string
	}{
		{name: "path traversal", tag: "../../admin"},
		{name: "encoded slash", tag: "v1.0.0%2F..%2Fadmin"},
		{name: "null byte", tag: "v1.0.0\x00evil"},
		{name: "newline injection", tag: "v1.0.0\r\nX-Evil: true"},
		{name: "query string", tag: "v1.0.0?admin=true"},
		{name: "fragment", tag: "v1.0.0#evil"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var response downloadReleaseAssetResponse
			err := env.client.Call(context.Background(),
				forge.ProviderAction(forge.ProviderGitHub, forge.ActionDownloadReleaseAsset),
				downloadReleaseAssetRequest{
					Repo:  "acme/widgets",
					Tag:   test.tag,
					Asset: "widget.tar.gz",
				},
				&response,
			)
			if err == nil {
				t.Fatalf("expected error for adversarial tag %q", test.tag)
			}
		})
	}
}

func TestDownloadReleaseAsset_ActualSizeExceedsDeclared(t *testing.T) {
	t.Parallel()

	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	// Release metadata declares asset as 50 bytes, but the actual
	// download serves 200 bytes. This simulates a compromised GitHub
	// API or MITM that lies about asset size. The io.LimitReader
	// safety net at download time prevents reading more than maxSize+1
	// bytes regardless of what GitHub declared.
	env.mock.routes["GET /repos/acme/widgets/releases/tags/v1.0.0"] = mockRoute{
		Status: http.StatusOK,
		Body: github.Release{
			ID:      1,
			TagName: "v1.0.0",
			Assets: []github.ReleaseAsset{
				{ID: 100, Name: "widget.tar.gz", Size: 50},
			},
		},
	}
	env.mock.routes["GET /repos/acme/widgets/releases/assets/100"] = mockRoute{
		Status:  http.StatusOK,
		RawBody: make([]byte, 200),
	}

	var response downloadReleaseAssetResponse
	err := env.client.Call(context.Background(),
		forge.ProviderAction(forge.ProviderGitHub, forge.ActionDownloadReleaseAsset),
		downloadReleaseAssetRequest{
			Repo:    "acme/widgets",
			Tag:     "v1.0.0",
			Asset:   "widget.tar.gz",
			MaxSize: 100, // Declared size (50) passes this check
		},
		&response,
	)
	if err == nil {
		t.Fatal("expected error: actual content (200 bytes) exceeds MaxSize (100) despite declared size (50) passing the check")
	}
}

func TestDownloadReleaseAsset_MaxSizeZeroAndNegativeUseDefault(t *testing.T) {
	t.Parallel()

	env := newOutboundTestEnv(t, defaultGrants())
	defer env.cleanup()

	assetContent := []byte("hello world")
	env.mock.routes["GET /repos/acme/widgets/releases/tags/v1.0.0"] = mockRoute{
		Status: http.StatusOK,
		Body: github.Release{
			ID:      1,
			TagName: "v1.0.0",
			Assets: []github.ReleaseAsset{
				{ID: 100, Name: "widget.tar.gz", Size: int64(len(assetContent))},
			},
		},
	}
	env.mock.routes["GET /repos/acme/widgets/releases/assets/100"] = mockRoute{
		Status:  http.StatusOK,
		RawBody: assetContent,
	}

	tests := []struct {
		name    string
		maxSize int64
	}{
		{name: "zero uses default", maxSize: 0},
		{name: "negative uses default", maxSize: -1},
		{name: "negative large uses default", maxSize: -999},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var response downloadReleaseAssetResponse
			err := env.client.Call(context.Background(),
				forge.ProviderAction(forge.ProviderGitHub, forge.ActionDownloadReleaseAsset),
				downloadReleaseAssetRequest{
					Repo:    "acme/widgets",
					Tag:     "v1.0.0",
					Asset:   "widget.tar.gz",
					MaxSize: test.maxSize,
				},
				&response,
			)
			if err != nil {
				t.Fatalf("MaxSize=%d should use default (256MB), not reject: %v", test.maxSize, err)
			}
			if response.Size != int64(len(assetContent)) {
				t.Errorf("response.Size = %d, want %d", response.Size, len(assetContent))
			}
			if response.SHA256 == "" {
				t.Error("expected non-empty SHA256")
			}
		})
	}
}

func TestComputeSHA256(t *testing.T) {
	t.Parallel()

	// SHA-256 of empty string is a well-known value.
	digest := computeSHA256([]byte(""))
	expected := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	got := hex.EncodeToString(digest)
	if got != expected {
		t.Errorf("computeSHA256(\"\") = %s, want %s", got, expected)
	}

	// SHA-256 of "hello" — another well-known value.
	digest = computeSHA256([]byte("hello"))
	expected = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	got = hex.EncodeToString(digest)
	if got != expected {
		t.Errorf("computeSHA256(\"hello\") = %s, want %s", got, expected)
	}
}
