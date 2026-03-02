// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package github provides a typed Go client for the GitHub REST API.
//
// The client authenticates via GitHub App installation tokens (preferred)
// or personal access tokens. It handles rate limiting (X-RateLimit-*
// headers with automatic backoff), pagination (RFC 5988 Link headers),
// conditional requests (ETags), and structured error mapping.
//
// All requests are made over HTTPS. The client refuses non-HTTPS base URLs.
//
// This is a GitHub-specific implementation — Forgejo and other forges have
// their own clients. The connector service translates between these
// provider-specific types and Bureau's forge schema types.
package github
