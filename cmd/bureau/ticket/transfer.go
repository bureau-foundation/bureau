// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
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
		RequiredGrants: []string{"command/ticket/export"},
		Run: func(args []string) error {
			if params.Room == "" {
				return fmt.Errorf("--room is required")
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
					return fmt.Errorf("creating output file: %w", err)
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
					return fmt.Errorf("writing entry %s: %w", entry.ID, err)
				}
			}

			if err := writer.Flush(); err != nil {
				return fmt.Errorf("flushing output: %w", err)
			}

			if params.File != "" && params.File != "-" {
				fmt.Fprintf(os.Stderr, "exported %d tickets from %s to %s\n", len(entries), params.Room, params.File)
			}

			return nil
		},
	}
}

// --- import ---

type importParams struct {
	TicketConnection
	Room        string `json:"room" flag:"room,r" desc:"target room ID" required:"true"`
	File        string `json:"-"    flag:"file,f" desc:"input JSONL file (- for stdin)" required:"true"`
	ResetStatus bool   `json:"-"    flag:"reset-status" desc:"set all imported tickets to open status"`
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
				Description: "Import with status reset (all tickets become open)",
				Command:     "bureau ticket import --room '!abc:bureau.local' --file tickets.jsonl --reset-status",
			},
		},
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/ticket/import"},
		Run: func(args []string) error {
			if params.Room == "" {
				return fmt.Errorf("--room is required")
			}
			if params.File == "" {
				return fmt.Errorf("--file is required")
			}

			// Read and parse the JSONL input.
			input := os.Stdin
			if params.File != "-" {
				file, err := os.Open(params.File)
				if err != nil {
					return fmt.Errorf("opening input file: %w", err)
				}
				defer file.Close()
				input = file
			}

			type importEntry struct {
				ID      string               `json:"id"`
				Content schema.TicketContent `json:"content"`
			}

			var entries []importEntry
			scanner := bufio.NewScanner(input)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			lineNumber := 0

			for scanner.Scan() {
				lineNumber++
				line := scanner.Bytes()
				if len(line) == 0 {
					continue
				}

				var entry importEntry
				if err := json.Unmarshal(line, &entry); err != nil {
					return fmt.Errorf("line %d: invalid JSON: %w", lineNumber, err)
				}
				if entry.ID == "" {
					return fmt.Errorf("line %d: missing required field: id", lineNumber)
				}
				if entry.Content.Title == "" {
					return fmt.Errorf("line %d (%s): missing required field: content.title", lineNumber, entry.ID)
				}

				if params.ResetStatus {
					entry.Content.Status = "open"
					entry.Content.Assignee = ""
					entry.Content.ClosedAt = ""
					entry.Content.CloseReason = ""
				}

				entries = append(entries, entry)
			}
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("reading input: %w", err)
			}

			if len(entries) == 0 {
				return fmt.Errorf("no tickets found in input")
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
