// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestParsePaneDeadLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		wantCode int
		wantOK   bool
	}{
		{
			name:     "normal exit code 0",
			input:    "some output\nPane is dead (status 0, Mon Feb 23 12:00:00 2026)",
			wantCode: 0,
			wantOK:   true,
		},
		{
			name:     "normal exit code 42",
			input:    "build failed\nPane is dead (status 42, Mon Feb 23 12:00:00 2026)",
			wantCode: 42,
			wantOK:   true,
		},
		{
			name:     "normal exit code 1",
			input:    "Pane is dead (status 1, Mon Feb 23 12:00:00 2026)",
			wantCode: 1,
			wantOK:   true,
		},
		{
			name:     "signal death SIGTERM",
			input:    "running...\nPane is dead (signal 15, Mon Feb 23 12:00:00 2026)",
			wantCode: 128 + 15,
			wantOK:   true,
		},
		{
			name:     "signal death SIGKILL",
			input:    "Pane is dead (signal 9, Mon Feb 23 12:00:00 2026)",
			wantCode: 128 + 9,
			wantOK:   true,
		},
		{
			name:     "signal death SIGSEGV",
			input:    "segfault\nPane is dead (signal 11, Mon Feb 23 12:00:00 2026)",
			wantCode: 128 + 11,
			wantOK:   true,
		},
		{
			name:     "no pane dead line",
			input:    "just some normal output\nno dead pane here",
			wantCode: 0,
			wantOK:   false,
		},
		{
			name:     "empty output",
			input:    "",
			wantCode: 0,
			wantOK:   false,
		},
		{
			name:     "status line in middle of output",
			input:    "line 1\nline 2\nPane is dead (status 127, Mon Feb 23 12:00:00 2026)\n",
			wantCode: 127,
			wantOK:   true,
		},
		{
			// Signal takes precedence over status. In practice tmux
			// only emits one, but the function should handle both.
			name:     "signal takes precedence over status",
			input:    "Pane is dead (signal 15, Mon Feb 23 12:00:00 2026)\nPane is dead (status 0, Mon Feb 23 12:00:00 2026)",
			wantCode: 128 + 15,
			wantOK:   true,
		},
		{
			name:     "exit code 255",
			input:    "Pane is dead (status 255, Mon Feb 23 12:00:00 2026)",
			wantCode: 255,
			wantOK:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			code, ok := parsePaneDeadLine(tt.input)
			if ok != tt.wantOK {
				t.Errorf("parsePaneDeadLine() ok = %v, want %v", ok, tt.wantOK)
			}
			if code != tt.wantCode {
				t.Errorf("parsePaneDeadLine() code = %d, want %d", code, tt.wantCode)
			}
		})
	}
}
