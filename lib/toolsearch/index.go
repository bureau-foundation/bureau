// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package toolsearch

// Index searches a corpus of tool documents and returns ranked
// results. Implementations may use different ranking algorithms
// (BM25, embeddings, etc.) but all share this query interface.
type Index interface {
	// Search returns up to limit documents ranked by relevance to
	// the query. Results are sorted by descending score. Returns
	// an empty slice if the query matches no documents.
	Search(query string, limit int) []Result
}

// Result is a single search hit with its relevance score.
type Result struct {
	// Name is the document name (tool name or command path).
	Name string

	// Score is the relevance score. Higher is more relevant.
	// The scale depends on the index implementation — BM25 scores
	// are typically in the range 0–20 but not bounded.
	Score float64
}

// Document describes a tool for indexing. Field weighting is
// applied by the index implementation: names and descriptions
// carry more weight than argument metadata.
type Document struct {
	// Name is the tool's unique identifier (e.g., "bureau_ticket_create"
	// for MCP tools, "ticket create" for CLI commands).
	Name string

	// Description is the tool's human-readable description. Both
	// the one-line summary and the detailed description should be
	// concatenated here for maximum searchability.
	Description string

	// ArgumentNames are the parameter/flag names the tool accepts
	// (e.g., ["project", "title", "body"]).
	ArgumentNames []string

	// ArgumentDescriptions are the human-readable descriptions for
	// each argument, parallel to ArgumentNames.
	ArgumentDescriptions []string
}
