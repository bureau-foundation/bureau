// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package bm25 provides relevance-ranked full-text search using the
// Okapi BM25 algorithm. The index accepts documents with named,
// weighted text fields and scores natural language queries against
// them using term-frequency and inverse-document-frequency weighting.
//
// Field weighting is achieved by repeating each field's tokens in
// proportion to its weight. This is a simple alternative to per-field
// BM25 that works well for small corpora (hundreds to low thousands
// of documents).
//
// The index is built at construction time and is immutable thereafter.
// It is safe for concurrent read access.
package bm25
