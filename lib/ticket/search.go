// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"regexp"
	"slices"
	"strings"

	"github.com/bureau-foundation/bureau/lib/bm25"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// BM25 field weights for ticket documents. Title is the highest-signal
// field (curated summary), labels are categorical terms, body is the
// full description, and notes are supplementary context.
const (
	weightTitle  = 5
	weightLabels = 3
	weightBody   = 2
	weightNotes  = 1
)

// Score boost magnitudes. These are separated by orders of magnitude
// from BM25 scores (which range 0-20 for typical corpora) to ensure
// clean tier separation: exact match > graph neighbor > BM25-only.
const (
	boostExactMatch        = 1e6
	boostDirectNeighbor    = 1000
	boostTextualReference  = 500
)

// Graph neighbor tiers. Direct edges (blockers, dependents, parent,
// children) are tier 1. Textual references (body/notes mention the
// ID) are tier 2.
const (
	tierDirectEdge       = 1
	tierTextualReference = 2
)

// SearchResult pairs a ticket entry with a relevance score. Score
// combines BM25 text relevance, exact-match boosting for ticket and
// artifact IDs, and graph-proximity boosting for dependency neighbors.
type SearchResult struct {
	Entry
	Score float64
}

// idPattern matches Bureau ticket IDs (tkt-hex) and artifact IDs
// (art-hex) in a query string. The prefix is one or more lowercase
// letters, the suffix is 4+ hex characters. This accommodates
// configurable prefixes while avoiding false positives on short
// tokens.
var idPattern = regexp.MustCompile(`\b([a-z]+-[a-f0-9]{4,})\b`)

// Search performs BM25-ranked full-text search with exact-match
// boosting for ticket/artifact IDs and graph expansion for dependency
// neighbors.
//
// The query is tokenized for BM25 matching and also scanned for
// ticket/artifact ID patterns (e.g., "tkt-a3f9", "art-cafe"). When
// IDs are found:
//   - The ticket with that exact ID gets an exact-match boost (always
//     ranked first)
//   - Graph neighbors (blockers, dependents, parent, children) get a
//     direct-edge boost
//   - Tickets that textually reference the ID get a reference boost
//
// Filter narrows results using the same AND semantics as [Index.List].
// Limit controls the maximum number of results returned (0 means no
// limit). Results are sorted by score descending, with tiebreakers on
// priority (ascending) and creation time (ascending).
func (idx *Index) Search(query string, filter Filter, limit int) []SearchResult {
	if idx.searchDirty || idx.searchIndex == nil {
		idx.rebuildSearch()
	}

	// Extract ticket/artifact IDs from the query and compute boost
	// scores for exact matches and graph neighbors.
	referencedIDs := extractIDs(query)

	boosts := make(map[string]float64)
	for _, referenceID := range referencedIDs {
		if _, exists := idx.tickets[referenceID]; exists {
			boosts[referenceID] = boostExactMatch
		}

		for neighborID, tier := range idx.graphNeighbors(referenceID) {
			var boost float64
			switch tier {
			case tierDirectEdge:
				boost = boostDirectNeighbor
			case tierTextualReference:
				boost = boostTextualReference
			}
			if boost > boosts[neighborID] {
				boosts[neighborID] = boost
			}
		}
	}

	// BM25 search over all tickets (no limit — we filter afterward).
	bm25Results := idx.searchIndex.Search(query, 0)

	// Build a map of BM25 scores for merging with boosted tickets.
	bm25Scores := make(map[string]float64, len(bm25Results))
	for _, result := range bm25Results {
		bm25Scores[result.Name] = result.Score
	}

	// Merge BM25 results with boosted tickets. Every BM25 match
	// gets its boost added (zero if no boost). Boosted tickets
	// with no BM25 match are included with boost-only score.
	scored := make(map[string]float64, len(bm25Scores)+len(boosts))
	for ticketID, bm25Score := range bm25Scores {
		scored[ticketID] = bm25Score + boosts[ticketID]
	}
	for ticketID, boost := range boosts {
		if _, hasBM25 := scored[ticketID]; !hasBM25 {
			scored[ticketID] = boost
		}
	}

	// Filter and collect results.
	var results []SearchResult
	for ticketID, score := range scored {
		content, exists := idx.tickets[ticketID]
		if !exists {
			continue
		}
		if !idx.matchesFilter(&content, &filter) {
			continue
		}
		results = append(results, SearchResult{
			Entry: Entry{ID: ticketID, Content: content},
			Score: score,
		})
	}

	// Sort by score descending, with tiebreakers on priority
	// (ascending — P0 first) and creation time (ascending — oldest
	// first).
	slices.SortFunc(results, func(a, b SearchResult) int {
		if a.Score != b.Score {
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		if a.Content.Priority != b.Content.Priority {
			return a.Content.Priority - b.Content.Priority
		}
		return strings.Compare(a.Content.CreatedAt, b.Content.CreatedAt)
	})

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	return results
}

// rebuildSearch constructs a new BM25 index from all tickets in the
// index. Called lazily on the first Search() call and after any
// Put/Remove mutation.
func (idx *Index) rebuildSearch() {
	documents := make([]bm25.Document, 0, len(idx.tickets))
	for ticketID, content := range idx.tickets {
		documents = append(documents, ticketDocument(ticketID, &content))
	}
	idx.searchIndex = bm25.New(documents)
	idx.searchDirty = false
}

// ticketDocument converts a ticket into a BM25 document with weighted
// fields. The ticket ID is used as the document Name for result
// identification.
func ticketDocument(ticketID string, content *schema.TicketContent) bm25.Document {
	fields := make([]bm25.Field, 0, 4)
	fields = append(fields, bm25.Field{Text: content.Title, Weight: weightTitle})
	fields = append(fields, bm25.Field{Text: content.Body, Weight: weightBody})

	if len(content.Labels) > 0 {
		fields = append(fields, bm25.Field{
			Text:   strings.Join(content.Labels, " "),
			Weight: weightLabels,
		})
	}

	if len(content.Notes) > 0 {
		var noteText strings.Builder
		for i := range content.Notes {
			if i > 0 {
				noteText.WriteByte(' ')
			}
			noteText.WriteString(content.Notes[i].Body)
		}
		fields = append(fields, bm25.Field{
			Text:   noteText.String(),
			Weight: weightNotes,
		})
	}

	return bm25.Document{Name: ticketID, Fields: fields}
}

// extractIDs returns all ticket-like and artifact-like ID references
// found in the query string. IDs match the pattern "prefix-hex" where
// prefix is one or more lowercase letters and hex is 4+ hex digits
// (e.g., "tkt-a3f9", "art-cafe1234"). Results are deduplicated.
func extractIDs(query string) []string {
	matches := idPattern.FindAllString(strings.ToLower(query), -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(matches))
	unique := make([]string, 0, len(matches))
	for _, match := range matches {
		if _, exists := seen[match]; !exists {
			seen[match] = struct{}{}
			unique = append(unique, match)
		}
	}
	return unique
}

// graphNeighbors collects the dependency neighborhood for a ticket ID.
// Returns a map of neighbor ticket ID to proximity tier: tierDirectEdge
// for structural relationships (blockers, dependents, parent, children)
// and tierTextualReference for tickets that mention the ID in their
// content.
func (idx *Index) graphNeighbors(ticketID string) map[string]int {
	neighbors := make(map[string]int)

	// Structural neighbors require the ticket to exist in this
	// index. When it doesn't (artifact IDs, cross-room references),
	// we skip structural expansion but still scan for textual
	// references below.
	if content, exists := idx.tickets[ticketID]; exists {
		// Direct blockers (this ticket's blocked_by).
		for _, blockerID := range content.BlockedBy {
			neighbors[blockerID] = tierDirectEdge
		}

		// Direct dependents (tickets that this ticket blocks).
		if dependents, exists := idx.blocks[ticketID]; exists {
			for dependentID := range dependents {
				neighbors[dependentID] = tierDirectEdge
			}
		}

		// Parent epic.
		if content.Parent != "" {
			neighbors[content.Parent] = tierDirectEdge
		}

		// Children (if this ticket is an epic/parent).
		if childIDs, exists := idx.children[ticketID]; exists {
			for childID := range childIDs {
				neighbors[childID] = tierDirectEdge
			}
		}
	}

	// Textual references: scan all tickets for mentions of ticketID
	// in body, notes, blocked_by, gate TicketID, or attachment Ref.
	// This runs regardless of whether ticketID exists as a ticket,
	// which handles artifact IDs and cross-room ticket references.
	for otherID, otherContent := range idx.tickets {
		if otherID == ticketID {
			continue
		}
		if _, already := neighbors[otherID]; already {
			continue
		}
		if ticketReferences(ticketID, &otherContent) {
			neighbors[otherID] = tierTextualReference
		}
	}

	return neighbors
}

// ticketReferences returns true if the ticket content textually
// references the given ticket ID in body, notes, blocked_by entries,
// gate TicketID fields, or attachment Ref fields.
func ticketReferences(ticketID string, content *schema.TicketContent) bool {
	if strings.Contains(content.Body, ticketID) {
		return true
	}
	for i := range content.Notes {
		if strings.Contains(content.Notes[i].Body, ticketID) {
			return true
		}
	}
	for _, dependency := range content.BlockedBy {
		if dependency == ticketID {
			return true
		}
	}
	for i := range content.Gates {
		if content.Gates[i].TicketID == ticketID {
			return true
		}
	}
	for i := range content.Attachments {
		if content.Attachments[i].Ref == ticketID {
			return true
		}
	}
	return false
}
