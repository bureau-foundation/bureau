// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/tidwall/jsonc"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// maxContentSize is the maximum size of content fetched from a URL.
// Prevents abuse by limiting how much data we'll read.
const maxContentSize = 1 << 20 // 1 MB

// SourceContent holds content loaded from a remote source (flake or URL).
// The raw JSON data is ready for json.Unmarshal into a typed content struct.
//
// Origin is partially populated — FlakeRef/URL and ResolvedRev are set,
// but ContentHash is empty. The caller must compute the hash after
// unmarshaling through their typed struct (to get canonical JSON field
// ordering) and set it on Origin.ContentHash.
type SourceContent struct {
	// Data is the raw JSON content, ready for json.Unmarshal into a
	// typed struct like schema.TemplateContent or pipeline.PipelineContent.
	Data json.RawMessage

	// Origin holds provenance metadata for update tracking. Nil for
	// file sources (which don't support update tracking).
	Origin *ref.ContentOrigin

	// Source is the source type: "flake", "url", or "file".
	Source string

	// Ref is the source reference string (flake ref, URL, or file path).
	Ref string
}

// LoadFromFlake evaluates a Nix flake attribute and returns the raw JSON
// output along with origin metadata. The flakeAttr is the full attribute
// path including the flake ref (e.g., "github:owner/repo#bureauTemplate.x86_64-linux").
//
// The returned SourceContent.Origin has FlakeRef and ResolvedRev set, but
// ContentHash is empty — the caller must compute it after unmarshaling.
func LoadFromFlake(ctx context.Context, flakeRef, flakeAttr string, logger *slog.Logger) (*SourceContent, error) {
	nixPath, err := exec.LookPath("nix")
	if err != nil {
		return nil, Validation("nix not found on PATH: %w", err).
			WithHint("The Nix package manager is required for --flake. Install it with 'script/setup-nix'.")
	}

	// Evaluate the flake attribute.
	fullAttr := flakeRef + "#" + flakeAttr
	logger.Info("evaluating flake attribute", "attr", fullAttr)

	evalCmd := exec.CommandContext(ctx, nixPath, "eval", "--json", fullAttr)
	evalCmd.Stderr = os.Stderr
	evalOutput, err := evalCmd.Output()
	if err != nil {
		return nil, Internal("nix eval %s: %w", fullAttr, err).
			WithHint(fmt.Sprintf("Ensure the flake exports a %s attribute.", flakeAttr))
	}

	// Get flake metadata for the resolved revision.
	logger.Info("reading flake metadata", "flake_ref", flakeRef)

	metaCmd := exec.CommandContext(ctx, nixPath, "flake", "metadata", "--json", flakeRef)
	metaCmd.Stderr = os.Stderr
	metaOutput, err := metaCmd.Output()
	if err != nil {
		return nil, Internal("nix flake metadata %s: %w", flakeRef, err).
			WithHint("Ensure the flake reference is valid and accessible.")
	}

	var metadata struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(metaOutput, &metadata); err != nil {
		return nil, Internal("parsing flake metadata: %w", err)
	}

	return &SourceContent{
		Data: json.RawMessage(evalOutput),
		Origin: &ref.ContentOrigin{
			FlakeRef:    flakeRef,
			ResolvedRev: metadata.Revision,
			// ContentHash left empty — caller computes after unmarshal.
		},
		Source: "flake",
		Ref:    flakeRef,
	}, nil
}

// LoadFromURL fetches JSONC content from an HTTPS URL with a 1MB size limit.
// Comments and trailing commas are stripped before returning the raw JSON.
//
// The returned SourceContent.Origin has URL set, but ContentHash is empty —
// the caller must compute it after unmarshaling.
func LoadFromURL(ctx context.Context, rawURL string, logger *slog.Logger) (*SourceContent, error) {
	if !strings.HasPrefix(rawURL, "https://") {
		return nil, Validation("URL must use HTTPS: %s", rawURL)
	}

	logger.Info("fetching content", "url", rawURL)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, Internal("creating HTTP request: %w", err)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, Internal("fetching %s: %w", rawURL, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, Internal("fetching %s: HTTP %d", rawURL, response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxContentSize+1))
	if err != nil {
		return nil, Internal("reading response body: %w", err)
	}
	if len(body) > maxContentSize {
		return nil, Validation("content exceeds 1 MB size limit")
	}

	// Strip JSONC comments and trailing commas.
	stripped := jsonc.ToJSON(body)

	return &SourceContent{
		Data: json.RawMessage(stripped),
		Origin: &ref.ContentOrigin{
			URL: rawURL,
			// ContentHash left empty — caller computes after unmarshal.
		},
		Source: "url",
		Ref:    rawURL,
	}, nil
}

// LoadFromFile reads a JSONC file from disk and strips comments. Returns raw
// JSON data with no origin metadata (file sources don't support update
// tracking).
func LoadFromFile(path string) (*SourceContent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, Validation("reading %s: %w", path, err)
	}

	stripped := jsonc.ToJSON(data)

	return &SourceContent{
		Data:   json.RawMessage(stripped),
		Origin: nil,
		Source: "file",
		Ref:    path,
	}, nil
}

// ComputeContentHash computes a SHA-256 hash of content for change detection.
// The content is serialized as JSON via json.Marshal before hashing. The
// caller should pass a copy of the typed struct with its Origin field set
// to nil — the hash should reflect the content definition itself, not the
// metadata about where it came from.
func ComputeContentHash(content any) (string, error) {
	data, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("marshaling content for hash: %w", err)
	}
	hash := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", hash), nil
}

// ResolveSystem returns the Nix system triple for the current architecture,
// or the explicitly specified system if non-empty.
func ResolveSystem(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64-linux", nil
	case "arm64":
		return "aarch64-linux", nil
	default:
		return "", Validation("unsupported architecture %q; specify --system explicitly", runtime.GOARCH)
	}
}

// OriginSummary returns a one-line summary of a content origin for display.
func OriginSummary(origin *ref.ContentOrigin) string {
	switch {
	case origin.FlakeRef != "":
		if origin.ResolvedRev != "" {
			return fmt.Sprintf("flake %s (rev %s)", origin.FlakeRef, ShortRevision(origin.ResolvedRev))
		}
		return fmt.Sprintf("flake %s", origin.FlakeRef)
	case origin.URL != "":
		return fmt.Sprintf("url %s", origin.URL)
	default:
		return "unknown"
	}
}

// ShortRevision truncates a git revision to 12 characters for display.
func ShortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}
