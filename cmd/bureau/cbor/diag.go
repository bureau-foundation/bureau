// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cbor

import (
	"fmt"
	"io"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/codec"
)

// diagParams holds the parameters for the "bureau cbor diag" command.
type diagParams struct {
	HexInput bool `json:"hex_input" flag:"hex,x" desc:"treat input as hex-encoded CBOR"`
}

func diagCommand() *cli.Command {
	var params diagParams

	return &cli.Command{
		Name:    "diag",
		Summary: "Convert CBOR to diagnostic notation",
		Description: `Read CBOR from stdin (or a file argument) and write RFC 8949 Extended
Diagnostic Notation (EDN) to stdout.

Unlike JSON output, diagnostic notation preserves CBOR type information:
integer vs float, byte strings vs text strings, integer map keys, and
tagged values. This is useful for inspecting the exact wire representation
of Bureau protocol messages.

With --hex, treats input as hex-encoded CBOR rather than raw binary.

Examples of diagnostic notation:

  {"action": "status", "count": 42}       text keys, integer value
  {1: "subject", 2: "machine"}            integer keys (keyasint encoding)
  h'a1636b6579'                           byte string in hex`,
		Usage: "bureau cbor diag [-x] [file]",
		Examples: []cli.Example{
			{
				Description: "Show diagnostic notation for a CBOR file",
				Command:     "bureau cbor diag message.cbor",
			},
			{
				Description: "Decode hex-encoded CBOR to diagnostic notation",
				Command:     "echo 'a1636b657963766174' | bureau cbor diag --hex",
			},
			{
				Description: "Encode JSON and inspect the CBOR structure",
				Command:     "echo '{\"count\":42}' | bureau cbor encode | bureau cbor diag",
			},
		},
		Params: func() any { return &params },
		Run: func(args []string) error {
			data, remainingArgs, err := readInput(args, params.HexInput)
			if err != nil {
				return err
			}
			if len(remainingArgs) > 0 {
				return fmt.Errorf("diag takes no positional arguments besides an optional file path, got %q", remainingArgs[0])
			}
			return diagCBOR(data, os.Stdout)
		},
	}
}

// diagCBOR writes diagnostic notation for CBOR data to w.
func diagCBOR(data []byte, w io.Writer) error {
	if len(data) == 0 {
		return fmt.Errorf("empty input: expected CBOR data")
	}

	// Process as a sequence: diagnose each item and print on its
	// own line. For a single item this produces one line; for CBOR
	// sequences (RFC 8742) it produces one line per item.
	remaining := data
	for len(remaining) > 0 {
		notation, rest, err := codec.DiagnoseFirst(remaining)
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
