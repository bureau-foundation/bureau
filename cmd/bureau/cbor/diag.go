// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cbor

import (
	"fmt"
	"io"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/fxamacker/cbor/v2"
)

func diagCommand() *cli.Command {
	return &cli.Command{
		Name:    "diag",
		Summary: "Convert CBOR on stdin to diagnostic notation",
		Description: `Read CBOR from stdin and write RFC 8949 Extended Diagnostic Notation
(EDN) to stdout.

Unlike JSON output, diagnostic notation preserves CBOR type information:
integer vs float, byte strings vs text strings, integer map keys, and
tagged values. This is useful for inspecting the exact wire representation
of Bureau protocol messages.

Examples of diagnostic notation:

  {"action": "status", "count": 42}       text keys, integer value
  {1: "subject", 2: "machine"}            integer keys (keyasint encoding)
  h'a1636b6579'                           byte string in hex`,
		Usage: "bureau cbor diag",
		Examples: []cli.Example{
			{
				Description: "Show diagnostic notation for a CBOR file",
				Command:     "bureau cbor diag < message.cbor",
			},
			{
				Description: "Encode JSON and inspect the CBOR structure",
				Command:     "echo '{\"count\":42}' | bureau cbor encode | bureau cbor diag",
			},
		},
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("diag takes no positional arguments, got %q", args[0])
			}
			return diagCBOR(os.Stdin, os.Stdout)
		},
	}
}

// diagCBOR reads CBOR from r and writes diagnostic notation to w.
func diagCBOR(r io.Reader, w io.Writer) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	if len(data) == 0 {
		return fmt.Errorf("empty input: expected CBOR data on stdin")
	}

	// Process as a sequence: diagnose each item and print on its
	// own line. For a single item this produces one line; for CBOR
	// sequences (RFC 8742) it produces one line per item.
	remaining := data
	for len(remaining) > 0 {
		notation, rest, err := cbor.DiagnoseFirst(remaining)
		if err != nil {
			offset := len(data) - len(remaining)
			return fmt.Errorf("diagnose CBOR at byte %d: %w", offset, err)
		}
		if _, err := fmt.Fprintln(w, notation); err != nil {
			return err
		}
		remaining = rest
	}

	return nil
}
