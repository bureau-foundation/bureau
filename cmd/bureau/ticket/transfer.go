// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	ticketschema "github.com/bureau-foundation/bureau/lib/schema/ticket"
)

// --- export ---

type exportParams struct {
	TicketConnection
	Room   string `json:"room"   flag:"room,r" desc:"room ID to export" required:"true"`
	File   string `json:"-"      flag:"file,f" desc:"output file (default: stdout)"`
	Status string `json:"status" flag:"status,s" desc:"filter by status (open, in_progress, blocked, closed)"`
}

func exportCommand() *cli.Command {
	var params exportParams

	return &cli.Command{
		Name:    "export",
		Summary: "Export tickets as JSONL",
		Description: `Export all tickets from a room as line-delimited JSON (JSONL).
Each line is a JSON object with "id", "room", and "content" fields,
where content is the full ticket including status, timestamps, notes,
gates, and all other fields.

Output goes to stdout by default, or to a file with --file. The
output can be piped to other tools or imported into another room
with "bureau ticket import".

Use --status to export only tickets in a particular state (e.g.
--status closed for archival of completed work).`,
		Usage: "bureau ticket export --room ROOM [flags]",
		Examples: []cli.Example{
			{
				Description: "Export all tickets to a file",
				Command:     "bureau ticket export --room '!abc:bureau.local' --file tickets.jsonl",
			},
			{
				Description: "Export open tickets to stdout",
				Command:     "bureau ticket export --room '!abc:bureau.local' --status open",
			},
			{
				Description: "Pipe to another room (migration)",
				Command:     "bureau ticket export --room '!old:bureau.local' | bureau ticket import --room '!new:bureau.local' --file -",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]ticketEntry{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/ticket/export"},
		Run: func(args []string) error {
			if params.Room == "" {
				return cli.Validation("--room is required")
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			fields := map[string]any{"room": params.Room}
			if params.Status != "" {
				fields["status"] = params.Status
			}

			var entries []ticketEntry
			if err := client.Call(ctx, "list", fields, &entries); err != nil {
				return err
			}

			// Determine output destination.
			output := os.Stdout
			if params.File != "" && params.File != "-" {
				file, err := os.Create(params.File)
				if err != nil {
					return cli.Internal("creating output file: %w", err)
				}
				defer file.Close()
				output = file
			}

			writer := bufio.NewWriter(output)
			encoder := json.NewEncoder(writer)
			encoder.SetEscapeHTML(false)

			for _, entry := range entries {
				// Ensure each entry has the room ID set for
				// round-trip fidelity — the list response may
				// omit it for room-scoped queries.
				if entry.Room == "" {
					entry.Room = params.Room
				}
				if err := encoder.Encode(entry); err != nil {
					return cli.Internal("writing entry %s: %w", entry.ID, err)
				}
			}

			if err := writer.Flush(); err != nil {
				return cli.Internal("flushing output: %w", err)
			}

			if params.File != "" && params.File != "-" {
				fmt.Fprintf(os.Stderr, "exported %d tickets from %s to %s\n", len(entries), params.Room, params.File)
			}

			return nil
		},
	}
}

// --- import ---

// importEntry is the per-ticket structure for both the export JSONL
// format and the service-side import request.
type importEntry struct {
	ID      string                     `json:"id"`
	Content ticketschema.TicketContent `json:"content"`
}

type importParams struct {
	TicketConnection
	Room        string `json:"room"   flag:"room,r"       desc:"target room ID" required:"true"`
	File        string `json:"-"      flag:"file,f"       desc:"input JSONL file (- for stdin)" required:"true"`
	Beads       bool   `json:"-"      flag:"beads"        desc:"parse input as beads JSONL format (renames IDs to tkt-*)"`
	BeadsPrefix string `json:"-"      flag:"beads-prefix" desc:"source prefix to replace when using --beads (default: bd)"`
	ResetStatus bool   `json:"-"      flag:"reset-status" desc:"set all imported tickets to open status"`
}

func importCommand() *cli.Command {
	var params importParams

	return &cli.Command{
		Name:    "import",
		Summary: "Import tickets from a JSONL file",
		Description: `Import tickets from a JSONL file into a room. Each line must be a
JSON object with "id" and "content" fields (the format produced by
"bureau ticket export").

Tickets are imported with their original IDs preserved. Status,
timestamps, notes, gates, and all other content fields are kept
as-is. Use --reset-status to force all imported tickets to "open"
status instead.

Importing into the same room as the source is an upsert — existing
tickets with matching IDs are overwritten. Importing into a different
room creates the tickets with the same IDs in the new room.

Use --beads to import from a beads JSONL file (the format produced
by beads_rust / br / bd). Beads entries are converted to Bureau
ticket format, with IDs renamed from the beads prefix (default "bd")
to "tkt". All internal references — blocked_by, parent, and textual
references in titles, bodies, and notes — are also renamed. Each
imported ticket records its beads origin for provenance tracking.

All tickets are validated before any are written. If any ticket is
invalid, none are imported.`,
		Usage: "bureau ticket import --room ROOM --file FILE [flags]",
		Examples: []cli.Example{
			{
				Description: "Import from a file",
				Command:     "bureau ticket import --room '!abc:bureau.local' --file tickets.jsonl",
			},
			{
				Description: "Import from stdin",
				Command:     "bureau ticket import --room '!abc:bureau.local' --file -",
			},
			{
				Description: "Import from a beads file",
				Command:     "bureau ticket import --room '!abc:bureau.local' --file .beads/issues.jsonl --beads",
			},
			{
				Description: "Import beads with custom prefix (non-default beads project)",
				Command:     "bureau ticket import --room '!abc:bureau.local' --file .beads/issues.jsonl --beads --beads-prefix proj",
			},
			{
				Description: "Import with status reset (all tickets become open)",
				Command:     "bureau ticket import --room '!abc:bureau.local' --file tickets.jsonl --reset-status",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &importResult{} },
		Annotations:    cli.Create(),
		RequiredGrants: []string{"command/ticket/import"},
		Run: func(args []string) error {
			if params.Room == "" {
				return cli.Validation("--room is required")
			}
			if params.File == "" {
				return cli.Validation("--file is required")
			}
			if params.BeadsPrefix != "" && !params.Beads {
				return cli.Validation("--beads-prefix requires --beads")
			}

			// Read and parse the JSONL input.
			input := os.Stdin
			if params.File != "-" {
				file, err := os.Open(params.File)
				if err != nil {
					return cli.Internal("opening input file: %w", err)
				}
				defer file.Close()
				input = file
			}

			scanner := bufio.NewScanner(input)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

			var entries []importEntry
			var parseErr error
			if params.Beads {
				entries, parseErr = parseBeadsInput(scanner)
			} else {
				entries, parseErr = parseExportInput(scanner)
			}
			if parseErr != nil {
				return parseErr
			}

			if len(entries) == 0 {
				return cli.Validation("no tickets found in input")
			}

			// Apply beads ID renaming after parsing. Done as a
			// separate pass so that all entries are available for
			// validation before modification.
			if params.Beads {
				sourcePrefix := params.BeadsPrefix
				if sourcePrefix == "" {
					sourcePrefix = "bd"
				}
				const targetPrefix = "tkt"

				for i := range entries {
					originalID := entries[i].ID
					entries[i].ID, entries[i].Content = ticketschema.RenameBeadsIDs(
						entries[i].ID, entries[i].Content,
						sourcePrefix, targetPrefix,
					)
					entries[i].Content.Origin = &ticketschema.TicketOrigin{
						Source:      "beads",
						ExternalRef: originalID,
					}
				}

				fmt.Fprintf(os.Stderr, "converted %d beads entries (%s-* → %s-*)\n",
					len(entries), sourcePrefix, targetPrefix)
			}

			if params.ResetStatus {
				for i := range entries {
					entries[i].Content.Status = "open"
					entries[i].Content.Assignee = ref.UserID{}
					entries[i].Content.ClosedAt = ""
					entries[i].Content.CloseReason = ""
				}
			}

			// Build the import request. The service-side type uses
			// its own importEntry with cbor tags, but the json tags
			// are compatible through the CBOR codec fallback.
			tickets := make([]map[string]any, len(entries))
			for i, entry := range entries {
				tickets[i] = map[string]any{
					"id":      entry.ID,
					"content": entry.Content,
				}
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext()
			defer cancel()

			var result importResult
			if err := client.Call(ctx, "import", map[string]any{
				"room":    params.Room,
				"tickets": tickets,
			}, &result); err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "imported %d tickets into %s\n", result.Imported, result.Room)
			return nil
		},
	}
}

// parseExportInput parses JSONL in the export format: each line is
// {"id": "...", "content": {...}}.
func parseExportInput(scanner *bufio.Scanner) ([]importEntry, error) {
	var entries []importEntry
	lineNumber := 0

	for scanner.Scan() {
		lineNumber++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry importEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, cli.Validation("line %d: invalid JSON: %w", lineNumber, err)
		}
		if entry.ID == "" {
			return nil, cli.Validation("line %d: missing required field: id", lineNumber)
		}
		if entry.Content.Title == "" {
			return nil, cli.Validation("line %d (%s): missing required field: content.title", lineNumber, entry.ID)
		}

		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, cli.Internal("reading input: %w", err)
	}

	return entries, nil
}

// parseBeadsInput parses JSONL in the beads format: each line is a
// flat JSON object with fields like "id", "title", "status",
// "issue_type", etc. Converts each entry to the import format using
// the shared beads-to-ticket conversion.
func parseBeadsInput(scanner *bufio.Scanner) ([]importEntry, error) {
	var entries []importEntry
	lineNumber := 0

	for scanner.Scan() {
		lineNumber++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var beads ticketschema.BeadsEntry
		if err := json.Unmarshal(line, &beads); err != nil {
			return nil, cli.Validation("line %d: invalid JSON: %w", lineNumber, err)
		}
		if beads.ID == "" {
			return nil, cli.Validation("line %d: missing required field: id", lineNumber)
		}
		if beads.Title == "" {
			return nil, cli.Validation("line %d (%s): missing required field: title", lineNumber, beads.ID)
		}
		if beads.Status == "" {
			return nil, cli.Validation("line %d (%s): missing required field: status", lineNumber, beads.ID)
		}
		if beads.IssueType == "" {
			return nil, cli.Validation("line %d (%s): missing required field: issue_type", lineNumber, beads.ID)
		}

		content := ticketschema.BeadsToTicketContent(beads)
		entries = append(entries, importEntry{
			ID:      beads.ID,
			Content: content,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, cli.Internal("reading input: %w", err)
	}

	return entries, nil
}
