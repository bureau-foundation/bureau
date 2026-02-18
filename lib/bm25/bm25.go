// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package bm25

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// BM25 parameters (Okapi variant, standard values).
const (
	paramK1      = 1.2
	paramB       = 0.75
	paramEpsilon = 0.25
)

// tokenPattern splits text into alphanumeric runs.
var tokenPattern = regexp.MustCompile(`[a-z0-9]+`)

// Field is a weighted text field for BM25 indexing. The Weight
// controls how many times this field's tokens are repeated in the
// composite document (higher = more influence on ranking). A weight
// of 0 or negative causes the field to be skipped.
type Field struct {
	Text   string
	Weight int
}

// Document is a named collection of weighted text fields. Name is
// used for result identification only — it is not scored unless
// explicitly included as a Field.
type Document struct {
	Name   string
	Fields []Field
}

// Result is a single search hit with its relevance score.
type Result struct {
	// Name is the document name (as provided at index construction).
	Name string

	// Score is the relevance score. Higher is more relevant. The
	// scale depends on the corpus — BM25 scores are typically in
	// the range 0–20 but are not bounded.
	Score float64
}

// Index is a BM25 (Okapi) index over documents. The index is built
// at construction time and is immutable thereafter. It is safe for
// concurrent read access.
type Index struct {
	// documents stores the original documents for name retrieval.
	documents []Document

	// documentTermFrequencies[i][term] is the term frequency in
	// the composite document for document i.
	documentTermFrequencies []map[string]int

	// documentLengths[i] is the total token count for document i.
	documentLengths []int

	// averageDocumentLength is the mean of documentLengths.
	averageDocumentLength float64

	// inverseDocumentFrequency[term] is the precomputed IDF score
	// for each term in the corpus.
	inverseDocumentFrequency map[string]float64
}

// New creates a BM25 index from the given documents. Index
// construction is O(total tokens across all documents) and takes
// sub-millisecond for typical corpora (hundreds of documents).
func New(documents []Document) *Index {
	index := &Index{
		documents:                documents,
		documentTermFrequencies:  make([]map[string]int, len(documents)),
		documentLengths:          make([]int, len(documents)),
		inverseDocumentFrequency: make(map[string]float64),
	}

	// Track how many documents contain each term (for IDF).
	documentFrequency := make(map[string]int)

	var totalLength int

	for i, document := range documents {
		tokens := buildCompositeTokens(document)
		index.documentLengths[i] = len(tokens)
		totalLength += len(tokens)

		termFrequency := make(map[string]int)
		seen := make(map[string]bool)
		for _, token := range tokens {
			termFrequency[token]++
			if !seen[token] {
				seen[token] = true
				documentFrequency[token]++
			}
		}
		index.documentTermFrequencies[i] = termFrequency
	}

	if len(documents) > 0 {
		index.averageDocumentLength = float64(totalLength) / float64(len(documents))
	}

	// Precompute IDF for each term. Terms that appear in every
	// document get a small positive score (epsilon) rather than
	// zero, so they still contribute a tiny amount to ranking.
	documentCount := float64(len(documents))
	for term, frequency := range documentFrequency {
		idf := math.Log(1 + (documentCount-float64(frequency)+0.5)/(float64(frequency)+0.5))
		if idf < 0 {
			idf = paramEpsilon
		}
		index.inverseDocumentFrequency[term] = idf
	}

	return index
}

// Search returns up to limit documents ranked by BM25 relevance to
// the query. Returns an empty slice if the query produces no tokens
// or matches no documents.
func (index *Index) Search(query string, limit int) []Result {
	queryTokens := Tokenize(query)
	if len(queryTokens) == 0 {
		return nil
	}

	type scored struct {
		index int
		score float64
	}
	var hits []scored

	for i := range index.documents {
		score := index.score(i, queryTokens)
		if score > 0 {
			hits = append(hits, scored{index: i, score: score})
		}
	}

	sort.Slice(hits, func(a, b int) bool {
		return hits[a].score > hits[b].score
	})

	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}

	results := make([]Result, len(hits))
	for i, hit := range hits {
		results[i] = Result{
			Name:  index.documents[hit.index].Name,
			Score: hit.score,
		}
	}
	return results
}

// score computes the BM25 score for a single document against the
// query tokens.
func (index *Index) score(documentIndex int, queryTokens []string) float64 {
	termFrequency := index.documentTermFrequencies[documentIndex]
	documentLength := float64(index.documentLengths[documentIndex])

	var score float64
	for _, token := range queryTokens {
		idf, exists := index.inverseDocumentFrequency[token]
		if !exists {
			continue
		}

		frequency := float64(termFrequency[token])
		if frequency == 0 {
			continue
		}

		// BM25 term score: IDF * (tf * (k1 + 1)) / (tf + k1 * (1 - b + b * dl/avgdl))
		numerator := frequency * (paramK1 + 1)
		denominator := frequency + paramK1*(1-paramB+paramB*documentLength/index.averageDocumentLength)
		score += idf * numerator / denominator
	}

	return score
}

// buildCompositeTokens creates a weighted token sequence from a
// document by repeating each field's tokens according to the field
// weight. This is a simple alternative to per-field BM25 that works
// well for small corpora.
func buildCompositeTokens(document Document) []string {
	var tokens []string

	for _, field := range document.Fields {
		if field.Weight <= 0 {
			continue
		}
		fieldTokens := Tokenize(field.Text)
		for i := 0; i < field.Weight; i++ {
			tokens = append(tokens, fieldTokens...)
		}
	}

	return tokens
}

// Tokenize splits text into lowercase alphanumeric tokens, discarding
// tokens shorter than 2 characters. This catches "a", "I", and other
// noise words that don't contribute to relevance ranking.
func Tokenize(text string) []string {
	lower := strings.ToLower(text)
	matches := tokenPattern.FindAllString(lower, -1)

	// Filter short tokens in place.
	tokens := matches[:0]
	for _, match := range matches {
		if len(match) >= 2 {
			tokens = append(tokens, match)
		}
	}
	return tokens
}
