// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref

// ContentOrigin records where a content definition (template, pipeline, etc.)
// was published from, enabling update commands to find and fetch newer
// versions. Content published from local files or inline in setup code has
// no origin.
//
// This type lives in lib/ref (rather than lib/schema) because both
// lib/schema and lib/schema/pipeline need it, and lib/schema already
// imports lib/schema/pipeline — placing it here avoids an import cycle.
type ContentOrigin struct {
	// FlakeRef is the Nix flake reference used to publish this content
	// (e.g., "github:bureau-foundation/bureau-discord/v1.2.0"). Empty
	// when the content was published from a URL or local file.
	FlakeRef string `json:"flake_ref,omitempty"`

	// URL is the HTTP(S) URL the content was fetched from. Empty when
	// the content was published from a flake or local file.
	URL string `json:"url,omitempty"`

	// ResolvedRev is the git commit hash that the flake reference
	// resolved to at publish time. Enables "N commits behind"
	// diagnostics during update checks. Empty for non-flake origins.
	ResolvedRev string `json:"resolved_rev,omitempty"`

	// ContentHash is the SHA-256 hash of the content at publish time.
	// Used to detect whether re-evaluation or re-fetching produces
	// different content without diffing the full definition.
	ContentHash string `json:"content_hash"`
}
