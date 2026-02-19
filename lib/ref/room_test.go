// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref

import (
	"strings"
	"testing"
)

func TestParseRoomID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:  "valid simple",
			input: "!abc123:bureau.local",
		},
		{
			name:  "valid with port in server",
			input: "!opaque:localhost:6167",
		},
		{
			name:  "valid long opaque part",
			input: "!YTRkZjEwNjUtNzU4ZC00ZjFk:matrix.example.com",
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: "empty room ID",
		},
		{
			name:    "missing bang prefix",
			input:   "abc123:bureau.local",
			wantErr: "must start with '!'",
		},
		{
			name:    "wrong prefix sigil",
			input:   "#room:bureau.local",
			wantErr: "must start with '!'",
		},
		{
			name:    "missing colon and server",
			input:   "!abc123",
			wantErr: "missing ':server' suffix",
		},
		{
			name:    "empty local part",
			input:   "!:bureau.local",
			wantErr: "empty local part",
		},
		{
			name:    "empty server name",
			input:   "!abc123:",
			wantErr: "empty server name",
		},
		{
			name:    "bang only",
			input:   "!",
			wantErr: "missing ':server' suffix",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			roomID, err := ParseRoomID(test.input)
			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseRoomID(%q) succeeded, want error containing %q", test.input, test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("ParseRoomID(%q) error = %q, want error containing %q", test.input, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRoomID(%q) unexpected error: %v", test.input, err)
			}
			if roomID.String() != test.input {
				t.Errorf("String() = %q, want %q", roomID.String(), test.input)
			}
			if roomID.IsZero() {
				t.Error("IsZero() = true for valid RoomID")
			}
		})
	}
}

func TestRoomIDZeroValue(t *testing.T) {
	var zero RoomID
	if !zero.IsZero() {
		t.Error("zero value: IsZero() = false, want true")
	}
	if zero.String() != "" {
		t.Errorf("zero value: String() = %q, want empty", zero.String())
	}
}
