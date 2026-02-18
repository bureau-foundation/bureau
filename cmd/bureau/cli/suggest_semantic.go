// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/lib/toolsearch"
)

// SemanticSuggestion is a command found by BM25 relevance ranking.
type SemanticSuggestion struct {
	// Path is the full command path (e.g., "bureau matrix user create").
	Path string

	// Summary is the command's one-line summary.
	Summary string

	// Score is the BM25 relevance score. Higher is more relevant.
	Score float64
}

// SuggestSemantic searches the command tree rooted at root using BM25
// relevance ranking, returning up to limit results ordered by
// descending score. Unlike suggestCommand (Levenshtein edit distance
// on command names among immediate siblings), SuggestSemantic matches
// natural language queries against command names, summaries,
// descriptions, and flag metadata across the entire tree.
//
// The BM25 index is built on every call. For Bureau's command tree
// (~50-80 leaf commands), index construction takes sub-millisecond.
func SuggestSemantic(query string, root *Command, limit int) []SemanticSuggestion {
	var documents []toolsearch.Document
	var summaries []string

	walkLeafCommands(root, "", func(path string, command *Command) {
		description := command.Summary
		if command.Description != "" {
			description += " " + command.Description
		}

		var argumentNames []string
		var argumentDescriptions []string
		if flagSet := command.FlagSet(); flagSet != nil {
			flagSet.VisitAll(func(flag *pflag.Flag) {
				argumentNames = append(argumentNames, flag.Name)
				if flag.Usage != "" {
					argumentDescriptions = append(argumentDescriptions, flag.Usage)
				}
			})
		}

		documents = append(documents, toolsearch.Document{
			Name:                 path,
			Description:          description,
			ArgumentNames:        argumentNames,
			ArgumentDescriptions: argumentDescriptions,
		})
		summaries = append(summaries, command.Summary)
	})

	index := toolsearch.NewIndex(documents)
	results := index.Search(query, limit)

	// Map document names to positions for summary lookup.
	documentPositions := make(map[string]int, len(documents))
	for i, document := range documents {
		documentPositions[document.Name] = i
	}

	suggestions := make([]SemanticSuggestion, len(results))
	for i, result := range results {
		summary := ""
		if position, ok := documentPositions[result.Name]; ok {
			summary = summaries[position]
		}
		suggestions[i] = SemanticSuggestion{
			Path:    result.Name,
			Summary: summary,
			Score:   result.Score,
		}
	}
	return suggestions
}

// walkLeafCommands recursively visits every command in the tree that
// has a Run function. Commands with both Run and Subcommands (the
// fall-through pattern) are included. The path is the space-joined
// command path from the root (e.g., "bureau matrix user create").
func walkLeafCommands(command *Command, prefix string, callback func(path string, command *Command)) {
	path := command.Name
	if prefix != "" {
		path = prefix + " " + command.Name
	}

	if command.Run != nil {
		callback(path, command)
	}

	for _, sub := range command.Subcommands {
		walkLeafCommands(sub, path, callback)
	}
}

// formatSemanticSuggestions formats a "did you mean" error message
// with BM25-ranked command suggestions and a help pointer.
func formatSemanticSuggestions(unknown string, suggestions []SemanticSuggestion, parentName string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "unknown command %q\n\nDid you mean:\n", unknown)
	writer := tabwriter.NewWriter(&builder, 2, 0, 2, ' ', 0)
	for _, suggestion := range suggestions {
		if suggestion.Summary != "" {
			fmt.Fprintf(writer, "  %s\t%s\n", suggestion.Path, suggestion.Summary)
		} else {
			fmt.Fprintf(writer, "  %s\n", suggestion.Path)
		}
	}
	writer.Flush()
	fmt.Fprintf(&builder, "\nRun '%s --help' for usage.", parentName)
	return builder.String()
}
