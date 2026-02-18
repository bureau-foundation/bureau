// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package toolsearch

import (
	"github.com/bureau-foundation/bureau/lib/bm25"
)

// Field repetition weights. When building the composite document
// for a tool, each field's tokens are repeated this many times.
// This gives name tokens 3x the influence of argument descriptions
// without needing per-field BM25 (which adds implementation weight
// for marginal benefit on corpora this small).
const (
	weightName                = 3
	weightDescription         = 2
	weightArgumentName        = 2
	weightArgumentDescription = 1
)

// Document describes a tool for indexing. Field weighting is
// applied by the index: names and descriptions carry more weight
// than argument metadata.
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

// NewIndex builds a BM25 index from tool documents. Each tool's
// fields are weighted according to the package-level weight
// constants: name tokens carry 3x the influence of argument
// descriptions.
func NewIndex(documents []Document) *bm25.Index {
	bm25Documents := make([]bm25.Document, len(documents))
	for i, document := range documents {
		bm25Documents[i] = toBM25Document(document)
	}
	return bm25.New(bm25Documents)
}

// toBM25Document converts a tool Document to a generic BM25
// Document by mapping each tool field to a weighted BM25 field.
func toBM25Document(document Document) bm25.Document {
	// Pre-count total fields for capacity.
	fieldCount := 2 + len(document.ArgumentNames) + len(document.ArgumentDescriptions)
	fields := make([]bm25.Field, 0, fieldCount)

	fields = append(fields, bm25.Field{Text: document.Name, Weight: weightName})
	fields = append(fields, bm25.Field{Text: document.Description, Weight: weightDescription})

	for _, argumentName := range document.ArgumentNames {
		fields = append(fields, bm25.Field{Text: argumentName, Weight: weightArgumentName})
	}
	for _, argumentDescription := range document.ArgumentDescriptions {
		fields = append(fields, bm25.Field{Text: argumentDescription, Weight: weightArgumentDescription})
	}

	return bm25.Document{Name: document.Name, Fields: fields}
}
