// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package toolsearch

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// BM25 parameters (Okapi variant, standard values).
const (
	bm25K1      = 1.2
	bm25B       = 0.75
	bm25Epsilon = 0.25
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

// tokenPattern splits text into alphanumeric runs.
var tokenPattern = regexp.MustCompile(`[a-z0-9]+`)

// BM25Index is a BM25 (Okapi) index over tool documents. The index
// is built at construction time and is immutable thereafter. It is
// safe for concurrent read access.
type BM25Index struct {
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

// NewBM25Index creates a BM25 index from the given documents. Index
// construction is O(total tokens across all documents) and takes
// sub-millisecond for typical tool corpora (hundreds of documents).
func NewBM25Index(documents []Document) *BM25Index {
	index := &BM25Index{
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
			idf = bm25Epsilon
		}
		index.inverseDocumentFrequency[term] = idf
	}

	return index
}

// Search returns up to limit documents ranked by BM25 relevance to
// the query. Returns an empty slice if the query produces no tokens
// or matches no documents.
func (index *BM25Index) Search(query string, limit int) []Result {
	queryTokens := tokenize(query)
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
func (index *BM25Index) score(documentIndex int, queryTokens []string) float64 {
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
		numerator := frequency * (bm25K1 + 1)
		denominator := frequency + bm25K1*(1-bm25B+bm25B*documentLength/index.averageDocumentLength)
		score += idf * numerator / denominator
	}

	return score
}

// buildCompositeTokens creates a weighted token sequence from a
// document by repeating each field's tokens according to the field
// weight constants. This is a simple alternative to per-field BM25
// that works well for small corpora.
func buildCompositeTokens(document Document) []string {
	var tokens []string

	nameTokens := tokenize(document.Name)
	for i := 0; i < weightName; i++ {
		tokens = append(tokens, nameTokens...)
	}

	descriptionTokens := tokenize(document.Description)
	for i := 0; i < weightDescription; i++ {
		tokens = append(tokens, descriptionTokens...)
	}

	for _, argumentName := range document.ArgumentNames {
		argumentTokens := tokenize(argumentName)
		for i := 0; i < weightArgumentName; i++ {
			tokens = append(tokens, argumentTokens...)
		}
	}

	for _, argumentDescription := range document.ArgumentDescriptions {
		argumentTokens := tokenize(argumentDescription)
		for i := 0; i < weightArgumentDescription; i++ {
			tokens = append(tokens, argumentTokens...)
		}
	}

	return tokens
}

// tokenize splits text into lowercase alphanumeric tokens, discarding
// tokens shorter than 2 characters. This catches "a", "I", and other
// noise words that don't contribute to relevance ranking.
func tokenize(text string) []string {
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
