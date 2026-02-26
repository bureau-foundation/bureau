// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

// beadsSnapshotEntry holds the raw JSON bytes and parsed content for a
// single beads entry. The raw bytes are retained for diffing: if the
// bytes haven't changed between reads, neither has the content, avoiding
// false Put events and unnecessary heat animation.
type beadsSnapshotEntry struct {
	rawJSON []byte
	content ticket.TicketContent
}

// readBeadsSnapshot reads a beads JSONL file and returns a map from
// bead ID to snapshot entry. Used by both LoadBeadsFile (one-shot) and
// WatchBeadsFile (initial load + re-reads on change).
func readBeadsSnapshot(path string) (map[string]beadsSnapshotEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open beads file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	// Beads entries can be large (long descriptions, many deps).
	// Default 64KB scanner buffer may be insufficient.
	const maxLineSize = 1024 * 1024
	scanner.Buffer(make([]byte, 0, maxLineSize), maxLineSize)

	entries := make(map[string]beadsSnapshotEntry)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry ticket.BeadsEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber, err)
		}

		if entry.ID == "" {
			return nil, fmt.Errorf("line %d: missing id field", lineNumber)
		}

		// Copy the line bytes — the scanner reuses its buffer.
		rawJSON := make([]byte, len(line))
		copy(rawJSON, line)

		entries[entry.ID] = beadsSnapshotEntry{
			rawJSON: rawJSON,
			content: ticket.BeadsToTicketContent(entry),
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read beads file: %w", err)
	}

	return entries, nil
}

// LoadBeadsFile reads a beads JSONL file and returns an IndexSource
// populated with the converted tickets. Each line in the file is an
// independent JSON object representing one ticket.
//
// For live-updating file mode, use WatchBeadsFile instead.
//
// See ticket.BeadsToTicketContent for the field mapping from beads
// entries to TicketContent.
func LoadBeadsFile(path string) (*IndexSource, error) {
	entries, err := readBeadsSnapshot(path)
	if err != nil {
		return nil, err
	}

	index := ticketindex.NewIndex()
	for id, entry := range entries {
		index.Put(id, entry.content)
	}

	return NewIndexSource(index), nil
}
