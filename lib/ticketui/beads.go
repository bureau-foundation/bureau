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

// LoadBeadsFile reads a beads JSONL file and returns an IndexSource
// populated with the converted tickets. Each line in the file is an
// independent JSON object representing one ticket.
//
// See ticket.BeadsToTicketContent for the field mapping from beads
// entries to TicketContent.
func LoadBeadsFile(path string) (*IndexSource, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open beads file: %w", err)
	}
	defer file.Close()

	index := ticketindex.NewIndex()
	scanner := bufio.NewScanner(file)

	// Beads entries can be large (long descriptions, many deps).
	// Default 64KB scanner buffer may be insufficient.
	const maxLineSize = 1024 * 1024
	scanner.Buffer(make([]byte, 0, maxLineSize), maxLineSize)

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

		content := ticket.BeadsToTicketContent(entry)
		index.Put(entry.ID, content)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read beads file: %w", err)
	}

	return NewIndexSource(index), nil
}
