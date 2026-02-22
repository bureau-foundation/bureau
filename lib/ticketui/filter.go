// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"slices"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/junegunn/fzf/src/util"

	"github.com/bureau-foundation/bureau/lib/ticket"
)

// FilterResult holds a matched entry with its fuzzy match metadata.
// Produced by [FilterModel.ApplyFuzzy] and consumed by the list
// renderer for character-level match highlighting.
type FilterResult struct {
	Entry          ticket.Entry
	Score          int   // Best fuzzy score across all searchable fields.
	TitlePositions []int // Matched rune positions in the title (for highlighting).
}

// FilterModel implements fzf fuzzy matching across multiple ticket
// fields: ID, title, labels, assignee, type, status, and parent epic
// title. The filter composes with tabs: the tab chooses the base set
// (Ready/Blocked/All), and the filter narrows it client-side without
// round-tripping to the source.
type FilterModel struct {
	// Input is the current filter query text.
	Input string

	// Active is true when the filter input has keyboard focus
	// (the user pressed / to start typing).
	Active bool
}

// MatchesEntry returns true if the entry matches the current filter.
// An empty filter matches everything. Matching is case-insensitive
// substring against each searchable field — if any field contains
// the query, the entry matches.
func (filter *FilterModel) MatchesEntry(entry ticket.Entry, source Source) bool {
	if filter.Input == "" {
		return true
	}

	query := strings.ToLower(filter.Input)

	// Match against ticket ID.
	if strings.Contains(strings.ToLower(entry.ID), query) {
		return true
	}

	// Match against title.
	if strings.Contains(strings.ToLower(entry.Content.Title), query) {
		return true
	}

	// Match against labels.
	for _, label := range entry.Content.Labels {
		if strings.Contains(strings.ToLower(label), query) {
			return true
		}
	}

	// Match against assignee.
	if !entry.Content.Assignee.IsZero() &&
		strings.Contains(strings.ToLower(entry.Content.Assignee.String()), query) {
		return true
	}

	// Match against type.
	if strings.Contains(strings.ToLower(entry.Content.Type), query) {
		return true
	}

	// Match against status.
	if strings.Contains(strings.ToLower(entry.Content.Status), query) {
		return true
	}

	// Match against parent epic title.
	if entry.Content.Parent != "" {
		parentContent, exists := source.Get(entry.Content.Parent)
		if exists && strings.Contains(strings.ToLower(parentContent.Title), query) {
			return true
		}
	}

	return false
}

// Apply filters a slice of entries, returning only those that match
// the current filter text.
func (filter *FilterModel) Apply(entries []ticket.Entry, source Source) []ticket.Entry {
	if filter.Input == "" {
		return entries
	}

	var result []ticket.Entry
	for _, entry := range entries {
		if filter.MatchesEntry(entry, source) {
			result = append(result, entry)
		}
	}
	return result
}

// ApplyFuzzy filters entries using fzf fuzzy matching and returns
// results sorted by match quality (best first). Each result includes
// the fuzzy score and the character positions of matches within the
// title (for highlighting in the list view). When the filter input
// is empty, returns all entries with zero scores and no positions.
func (filter *FilterModel) ApplyFuzzy(entries []ticket.Entry, source Source) []FilterResult {
	if filter.Input == "" {
		results := make([]FilterResult, len(entries))
		for index, entry := range entries {
			results[index] = FilterResult{Entry: entry}
		}
		return results
	}

	pattern := []rune(strings.ToLower(filter.Input))
	slab := util.MakeSlab(256, 256)
	var results []FilterResult

	for _, entry := range entries {
		bestScore := 0

		// Match against each searchable field, keep the best score.
		for _, field := range searchableFields(entry, source) {
			match := fuzzyMatch(field, pattern, slab)
			if match.Score > bestScore {
				bestScore = match.Score
			}
		}

		if bestScore <= 0 {
			continue
		}

		// Match against title separately to get character positions
		// for highlighting. The title match may have a different
		// (lower) score than the best field, but we want the
		// positions specific to what appears in the list row.
		var titlePositions []int
		titleMatch := fuzzyMatch(entry.Content.Title, pattern, slab)
		if titleMatch.Score > 0 {
			titlePositions = titleMatch.Positions
		}

		results = append(results, FilterResult{
			Entry:          entry,
			Score:          bestScore,
			TitlePositions: titlePositions,
		})
	}

	// Sort by score descending so the best matches appear first.
	slices.SortFunc(results, func(a, b FilterResult) int {
		return b.Score - a.Score
	})

	return results
}

// searchableFields returns the strings that should be searched for a
// ticket entry. These are the same fields that MatchesEntry checks:
// ticket ID, title, labels (joined), assignee, type, status, and
// parent epic title.
func searchableFields(entry ticket.Entry, source Source) []string {
	fields := []string{
		entry.ID,
		entry.Content.Title,
		strings.Join(entry.Content.Labels, " "),
		entry.Content.Assignee.String(),
		entry.Content.Type,
		entry.Content.Status,
	}

	if entry.Content.Parent != "" {
		parentContent, exists := source.Get(entry.Content.Parent)
		if exists {
			fields = append(fields, parentContent.Title)
		}
	}

	return fields
}

// HandleRune processes a character typed while the filter is active.
// Returns true if the input changed.
func (filter *FilterModel) HandleRune(character rune) bool {
	filter.Input += string(character)
	return true
}

// HandleBackspace removes the last character from the filter input.
// Returns true if the input changed.
func (filter *FilterModel) HandleBackspace() bool {
	if len(filter.Input) == 0 {
		return false
	}
	runes := []rune(filter.Input)
	filter.Input = string(runes[:len(runes)-1])
	return true
}

// Clear resets the filter input and deactivates it.
func (filter *FilterModel) Clear() {
	filter.Input = ""
	filter.Active = false
}

// View renders the filter bar. When active, shows the input with a
// cursor. When inactive with text, shows the filter text. When
// inactive with no text, returns empty string (hidden).
func (filter *FilterModel) View(theme Theme, width int) string {
	if !filter.Active && filter.Input == "" {
		return ""
	}

	style := lipgloss.NewStyle().
		Foreground(theme.NormalText).
		Width(width)

	if filter.Active {
		cursor := lipgloss.NewStyle().
			Foreground(theme.HeaderForeground).
			Bold(true).
			Render("▎")
		return style.Render(" / " + filter.Input + cursor)
	}

	// Inactive but has text — show the filter as a subtle indicator.
	dimStyle := lipgloss.NewStyle().
		Foreground(theme.FaintText).
		Width(width)
	return dimStyle.Render(" filter: " + filter.Input)
}
