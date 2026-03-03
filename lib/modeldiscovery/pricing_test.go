// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modeldiscovery

import "testing"

func TestPerTokenToMicrodollarsPerMtok(t *testing.T) {
	tests := []struct {
		name     string
		perToken string
		want     int64
		wantErr  bool
	}{
		{"claude sonnet input", "0.000005", 5_000_000, false},
		{"claude opus output", "0.000025", 25_000_000, false},
		{"gemini pro input", "0.00000125", 1_250_000, false},
		{"cache read", "0.0000005", 500_000, false},
		{"cache write", "0.00000625", 6_250_000, false},
		{"free model", "0", 0, false},
		{"empty string", "", 0, false},
		{"dynamic pricing", "-1", 0, false},
		{"expensive model", "0.00006", 60_000_000, false},
		{"very cheap", "0.000000125", 125_000, false},

		{"invalid string", "abc", 0, true},
		{"negative price", "-0.001", 0, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := PerTokenToMicrodollarsPerMtok(test.perToken)
			if test.wantErr {
				if err == nil {
					t.Errorf("PerTokenToMicrodollarsPerMtok(%q) = %d, want error", test.perToken, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("PerTokenToMicrodollarsPerMtok(%q) error: %v", test.perToken, err)
			}
			if got != test.want {
				t.Errorf("PerTokenToMicrodollarsPerMtok(%q) = %d, want %d", test.perToken, got, test.want)
			}
		})
	}
}
