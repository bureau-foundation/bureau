// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"testing"
)

func TestParseLabelArgs(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantUpdates  map[string]string
		wantRemovals []string
		wantErr      bool
	}{
		{
			name:        "single update",
			args:        []string{"gpu=h100"},
			wantUpdates: map[string]string{"gpu": "h100"},
		},
		{
			name:        "multiple updates",
			args:        []string{"gpu=h100", "tier=production", "region=us-east"},
			wantUpdates: map[string]string{"gpu": "h100", "tier": "production", "region": "us-east"},
		},
		{
			name:         "single removal",
			args:         []string{"gpu="},
			wantUpdates:  map[string]string{},
			wantRemovals: []string{"gpu"},
		},
		{
			name:         "mixed updates and removals",
			args:         []string{"gpu=h100", "tier=", "region=us-east"},
			wantUpdates:  map[string]string{"gpu": "h100", "region": "us-east"},
			wantRemovals: []string{"tier"},
		},
		{
			name:        "value containing equals sign",
			args:        []string{"config=key=value"},
			wantUpdates: map[string]string{"config": "key=value"},
		},
		{
			name:        "value with multiple equals signs",
			args:        []string{"expr=a=b=c=d"},
			wantUpdates: map[string]string{"expr": "a=b=c=d"},
		},
		{
			name:    "missing equals sign",
			args:    []string{"gpu"},
			wantErr: true,
		},
		{
			name:    "empty key",
			args:    []string{"=value"},
			wantErr: true,
		},
		{
			name:    "just equals sign",
			args:    []string{"="},
			wantErr: true,
		},
		{
			name:    "error stops parsing",
			args:    []string{"gpu=h100", "bad", "tier=prod"},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			updates, removals, err := parseLabelArgs(test.args)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(updates) != len(test.wantUpdates) {
				t.Fatalf("updates: got %d entries, want %d", len(updates), len(test.wantUpdates))
			}
			for key, wantValue := range test.wantUpdates {
				gotValue, exists := updates[key]
				if !exists {
					t.Errorf("updates missing key %q", key)
					continue
				}
				if gotValue != wantValue {
					t.Errorf("updates[%q] = %q, want %q", key, gotValue, wantValue)
				}
			}

			if len(removals) != len(test.wantRemovals) {
				t.Fatalf("removals: got %v, want %v", removals, test.wantRemovals)
			}
			for index, wantKey := range test.wantRemovals {
				if removals[index] != wantKey {
					t.Errorf("removals[%d] = %q, want %q", index, removals[index], wantKey)
				}
			}
		})
	}
}
