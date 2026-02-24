// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    []string
		wantErr bool
	}{
		{
			name:    "no arguments",
			args:    nil,
			wantErr: true,
		},
		{
			name:    "empty arguments",
			args:    []string{},
			wantErr: true,
		},
		{
			name:    "only separator",
			args:    []string{"--"},
			wantErr: true,
		},
		{
			name: "command without separator",
			args: []string{"/bin/sh", "-c", "echo hello"},
			want: []string{"/bin/sh", "-c", "echo hello"},
		},
		{
			name: "command with separator",
			args: []string{"--", "/bin/sh", "-c", "echo hello"},
			want: []string{"/bin/sh", "-c", "echo hello"},
		},
		{
			name: "single command",
			args: []string{"/path/to/sandbox.sh"},
			want: []string{"/path/to/sandbox.sh"},
		},
		{
			name: "command starting with dash",
			args: []string{"--", "--version"},
			want: []string{"--version"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseArgs(test.args)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("got %d args, want %d: %v vs %v", len(got), len(test.want), got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Errorf("arg[%d] = %q, want %q", i, got[i], test.want[i])
				}
			}
		})
	}
}
