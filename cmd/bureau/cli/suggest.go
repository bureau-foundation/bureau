// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"flag"
	"strings"
)

// suggestCommand returns the name of the closest matching subcommand to
// the unknown input, or "" if nothing is close enough. "Close enough"
// means an edit distance of at most 3, which catches common typos
// (transpositions, dropped characters, extra characters).
func suggestCommand(unknown string, commands []*Command) string {
	bestName := ""
	bestDistance := 4 // threshold: only suggest if distance <= 3

	for _, command := range commands {
		distance := levenshtein(unknown, command.Name)
		if distance < bestDistance {
			bestDistance = distance
			bestName = command.Name
		}
	}

	return bestName
}

// suggestFlag looks at the args for the first unrecognized flag and returns
// the closest defined flag name, formatted with the appropriate prefix
// (-- or -). Returns "" if no good suggestion is found.
func suggestFlag(args []string, flagSet *flag.FlagSet) string {
	// Collect defined flag names.
	var defined []string
	flagSet.VisitAll(func(f *flag.Flag) {
		defined = append(defined, f.Name)
	})

	// Find the unrecognized flag in args.
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}

		// Strip prefix to get the bare name.
		name := strings.TrimLeft(arg, "-")
		if index := strings.IndexByte(name, '='); index >= 0 {
			name = name[:index]
		}

		// Check if this flag is defined.
		isDefined := false
		flagSet.VisitAll(func(f *flag.Flag) {
			if f.Name == name {
				isDefined = true
			}
		})
		if isDefined {
			continue
		}

		// Unknown flag — find closest match.
		bestName := ""
		bestDistance := 4

		for _, candidate := range defined {
			distance := levenshtein(name, candidate)
			if distance < bestDistance {
				bestDistance = distance
				bestName = candidate
			}
		}

		if bestName != "" {
			if len(bestName) == 1 {
				return "-" + bestName
			}
			return "--" + bestName
		}

		// Only check the first unrecognized flag.
		break
	}

	return ""
}

// levenshtein computes the Levenshtein edit distance between two strings.
// This is the minimum number of single-character edits (insertions, deletions,
// or substitutions) required to change one string into the other.
func levenshtein(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}

	// Use a single row of the distance matrix, updated in place.
	// This is O(min(m,n)) space instead of O(m*n).
	if len(a) > len(b) {
		a, b = b, a
	}

	previous := make([]int, len(a)+1)
	for i := range previous {
		previous[i] = i
	}

	for j := 1; j <= len(b); j++ {
		current := make([]int, len(a)+1)
		current[0] = j

		for i := 1; i <= len(a); i++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}

			deletion := previous[i] + 1
			insertion := current[i-1] + 1
			substitution := previous[i-1] + cost

			current[i] = min(deletion, min(insertion, substitution))
		}

		previous = current
	}

	return previous[len(a)]
}
