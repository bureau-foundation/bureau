// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package log

import (
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// testSource returns a non-zero ref.Entity for test fixtures.
func testSource() ref.Entity {
	source, err := ref.ParseEntityUserID("@bureau/fleet/prod/agent/builder:bureau.local")
	if err != nil {
		panic("invalid test entity: " + err.Error())
	}
	return source
}

func TestLogContentValidate(t *testing.T) {
	content := LogContent{
		Version:    1,
		SessionID:  "sess-abc123",
		Source:     testSource(),
		Format:     LogFormatRaw,
		Status:     LogStatusActive,
		TotalBytes: 4096,
		Chunks: []LogChunk{
			{Ref: "abc123def456", Sequence: 0, Size: 4096, Timestamp: 1740000000000000000},
		},
	}
	if err := content.Validate(); err != nil {
		t.Errorf("valid LogContent failed validation: %v", err)
	}
}

func TestLogContentValidateEmpty(t *testing.T) {
	// A log with no chunks is valid (just created, no output yet).
	content := LogContent{
		Version:   1,
		SessionID: "sess-abc123",
		Source:    testSource(),
		Format:    LogFormatRaw,
		Status:    LogStatusActive,
	}
	if err := content.Validate(); err != nil {
		t.Errorf("valid LogContent with no chunks failed validation: %v", err)
	}
}

func TestLogContentValidateErrors(t *testing.T) {
	valid := func() LogContent {
		return LogContent{
			Version:    1,
			SessionID:  "sess-abc123",
			Source:     testSource(),
			Format:     LogFormatRaw,
			Status:     LogStatusActive,
			TotalBytes: 1024,
			Chunks: []LogChunk{
				{Ref: "deadbeef", Sequence: 0, Size: 1024, Timestamp: 1740000000000000000},
			},
		}
	}

	tests := []struct {
		name    string
		modify  func(*LogContent)
		wantErr string
	}{
		{
			name:    "version zero",
			modify:  func(c *LogContent) { c.Version = 0 },
			wantErr: "version must be >= 1",
		},
		{
			name:    "missing session ID",
			modify:  func(c *LogContent) { c.SessionID = "" },
			wantErr: "session_id is required",
		},
		{
			name:    "zero source",
			modify:  func(c *LogContent) { c.Source = ref.Entity{} },
			wantErr: "source is required",
		},
		{
			name:    "missing format",
			modify:  func(c *LogContent) { c.Format = "" },
			wantErr: "format is required",
		},
		{
			name:    "unsupported format",
			modify:  func(c *LogContent) { c.Format = "jsonl" },
			wantErr: `unsupported format "jsonl"`,
		},
		{
			name:    "missing status",
			modify:  func(c *LogContent) { c.Status = "" },
			wantErr: "status is required",
		},
		{
			name:    "unsupported status",
			modify:  func(c *LogContent) { c.Status = "archived" },
			wantErr: `unsupported status "archived"`,
		},
		{
			name:    "negative total bytes",
			modify:  func(c *LogContent) { c.TotalBytes = -1 },
			wantErr: "total_bytes must be >= 0",
		},
		{
			name: "chunk with empty ref",
			modify: func(c *LogContent) {
				c.Chunks = []LogChunk{{Ref: "", Sequence: 0, Size: 1024, Timestamp: 1740000000000000000}}
			},
			wantErr: "log chunk 0: ref is required",
		},
		{
			name: "chunk with zero size",
			modify: func(c *LogContent) {
				c.Chunks = []LogChunk{{Ref: "deadbeef", Sequence: 0, Size: 0, Timestamp: 1740000000000000000}}
			},
			wantErr: "log chunk 0: size must be > 0",
		},
		{
			name: "chunk with negative size",
			modify: func(c *LogContent) {
				c.Chunks = []LogChunk{{Ref: "deadbeef", Sequence: 0, Size: -512, Timestamp: 1740000000000000000}}
			},
			wantErr: "log chunk 0: size must be > 0",
		},
		{
			name: "second chunk invalid",
			modify: func(c *LogContent) {
				c.Chunks = append(c.Chunks, LogChunk{Ref: "", Sequence: 100, Size: 512, Timestamp: 1740000001000000000})
			},
			wantErr: "log chunk 1: ref is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := valid()
			test.modify(&content)
			err := content.Validate()
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), test.wantErr)
			}
		})
	}
}

func TestLogContentValidateAllStatuses(t *testing.T) {
	for _, status := range []LogStatus{LogStatusActive, LogStatusComplete, LogStatusRotating} {
		t.Run(string(status), func(t *testing.T) {
			content := LogContent{
				Version:   1,
				SessionID: "sess-abc123",
				Source:    testSource(),
				Format:    LogFormatRaw,
				Status:    status,
			}
			if err := content.Validate(); err != nil {
				t.Errorf("status %q should be valid, got: %v", status, err)
			}
		})
	}
}

func TestLogContentCanModify(t *testing.T) {
	t.Run("current version", func(t *testing.T) {
		content := LogContent{Version: LogContentVersion}
		if err := content.CanModify(); err != nil {
			t.Errorf("CanModify should succeed for current version: %v", err)
		}
	})

	t.Run("older version", func(t *testing.T) {
		content := LogContent{Version: 1}
		if err := content.CanModify(); err != nil {
			t.Errorf("CanModify should succeed for older version: %v", err)
		}
	})

	t.Run("newer version", func(t *testing.T) {
		content := LogContent{Version: LogContentVersion + 1}
		err := content.CanModify()
		if err == nil {
			t.Fatal("CanModify should fail for newer version")
		}
		if !strings.Contains(err.Error(), "modification would lose fields") {
			t.Errorf("error = %q, want mention of field loss", err.Error())
		}
		if !strings.Contains(err.Error(), "upgrade before modifying") {
			t.Errorf("error = %q, want upgrade guidance", err.Error())
		}
	})
}

func TestLogChunkValidate(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		chunk := LogChunk{Ref: "abc123", Sequence: 42, Size: 1024, Timestamp: 1740000000000000000}
		if err := chunk.Validate(0); err != nil {
			t.Errorf("valid chunk failed validation: %v", err)
		}
	})

	t.Run("empty ref", func(t *testing.T) {
		chunk := LogChunk{Ref: "", Sequence: 0, Size: 1024, Timestamp: 1740000000000000000}
		err := chunk.Validate(3)
		if err == nil {
			t.Fatal("expected error for empty ref")
		}
		if !strings.Contains(err.Error(), "log chunk 3: ref is required") {
			t.Errorf("error = %q, want chunk index in message", err.Error())
		}
	})

	t.Run("zero size", func(t *testing.T) {
		chunk := LogChunk{Ref: "abc123", Sequence: 0, Size: 0, Timestamp: 1740000000000000000}
		err := chunk.Validate(0)
		if err == nil {
			t.Fatal("expected error for zero size")
		}
		if !strings.Contains(err.Error(), "size must be > 0") {
			t.Errorf("error = %q, want size error", err.Error())
		}
	})
}
