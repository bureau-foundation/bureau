// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"testing"

	"github.com/spf13/pflag"
)

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"abc", "abc", 0},
		{"abc", "abd", 1}, // substitution
		{"abc", "ab", 1},  // deletion
		{"ab", "abc", 1},  // insertion
		{"abc", "bac", 2}, // transposition (counted as 2 edits)
		{"kitten", "sitting", 3},
		{"observe", "obsreve", 2},
		{"matrix", "matrx", 1},
		{"setup", "steup", 2},
		{"doctor", "docotr", 2},
	}

	for _, test := range tests {
		t.Run(test.a+"→"+test.b, func(t *testing.T) {
			got := levenshtein(test.a, test.b)
			if got != test.want {
				t.Errorf("levenshtein(%q, %q) = %d, want %d", test.a, test.b, got, test.want)
			}
		})
	}
}

func TestLevenshtein_Symmetric(t *testing.T) {
	pairs := [][2]string{
		{"abc", "abd"},
		{"hello", "helo"},
		{"setup", "steup"},
	}

	for _, pair := range pairs {
		forward := levenshtein(pair[0], pair[1])
		reverse := levenshtein(pair[1], pair[0])
		if forward != reverse {
			t.Errorf("levenshtein(%q, %q) = %d, but reverse = %d",
				pair[0], pair[1], forward, reverse)
		}
	}
}

func TestSuggestCommand(t *testing.T) {
	commands := []*Command{
		{Name: "observe"},
		{Name: "matrix"},
		{Name: "version"},
		{Name: "list"},
		{Name: "dashboard"},
	}

	tests := []struct {
		input string
		want  string
	}{
		{"obsreve", "observe"},    // typo
		{"matrx", "matrix"},       // missing letter
		{"matrixx", "matrix"},     // extra letter
		{"vrsion", "version"},     // missing letter
		{"lst", "list"},           // missing letters
		{"dashbord", "dashboard"}, // missing letter
		{"zzzzzzzzz", ""},         // nothing close
		{"m", ""},                 // too short to match well
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got := suggestCommand(test.input, commands)
			if got != test.want {
				t.Errorf("suggestCommand(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestSuggestFlag(t *testing.T) {
	makeFlagSet := func() *pflag.FlagSet {
		flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
		flagSet.String("homeserver", "", "")
		flagSet.String("token", "", "")
		flagSet.String("socket", "", "")
		flagSet.Bool("readonly", false, "")
		flagSet.Bool("json", false, "")
		return flagSet
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "close typo with double dash",
			args: []string{"--homserver"},
			want: "--homeserver",
		},
		{
			name: "close typo with single dash",
			args: []string{"-homserver"},
			want: "--homeserver",
		},
		{
			name: "readonly typo",
			args: []string{"--readnoly"},
			want: "--readonly",
		},
		{
			name: "token typo",
			args: []string{"--tken"},
			want: "--token",
		},
		{
			name: "nothing close",
			args: []string{"--zzzzzzzzz"},
			want: "",
		},
		{
			name: "no flags",
			args: []string{"positional"},
			want: "",
		},
		{
			name: "flag with equals",
			args: []string{"--homserver=http://localhost"},
			want: "--homeserver",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := suggestFlag(test.args, makeFlagSet())
			if got != test.want {
				t.Errorf("suggestFlag(%v) = %q, want %q", test.args, got, test.want)
			}
		})
	}
}
