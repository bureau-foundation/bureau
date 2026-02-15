// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cbor

import (
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
)

// cborParams holds the parameters for the top-level "bureau cbor" command.
// This command has both Subcommands (decode, encode, diag, validate) and
// a Run fallback. When the first positional argument matches a subcommand,
// the framework routes there. Otherwise, Run handles it: no args means
// default decode; anything else is treated as a jq filter expression.
type cborParams struct {
	Compact   bool `json:"compact"    flag:"compact,c"    desc:"compact output (no indentation)"`
	RawOutput bool `json:"raw_output" flag:"raw-output,r" desc:"raw string output (passed to jq)"`
	Slurp     bool `json:"slurp"      flag:"slurp,s"      desc:"read CBOR sequence as JSON array"`
	HexInput  bool `json:"hex_input"  flag:"hex,x"        desc:"treat input as hex-encoded CBOR"`
}

// Command returns the "cbor" command group.
func Command() *cli.Command {
	var params cborParams

	return &cli.Command{
		Name:    "cbor",
		Summary: "Inspect, produce, and filter CBOR data",
		Description: `Tools for working with CBOR data from the command line.

Bureau uses CBOR with Core Deterministic Encoding as the wire format for
service sockets, artifact transfers, and service tokens. This command
provides ergonomic access to that data.

With no arguments, decodes CBOR on stdin to pretty-printed JSON on
stdout (equivalent to "bureau cbor decode").

When the first argument is not a subcommand name (encode, decode, diag,
validate), it is treated as a jq filter expression. The CBOR input is
decoded to JSON internally and piped through jq. Common jq flags (-c,
-r) are supported and passed through.

All subcommands accept an optional trailing file path argument. When
provided, input is read from the file instead of stdin. This matches jq
convention: "bureau cbor '.field' request.cbor".

With --hex, input is treated as hex-encoded CBOR rather than raw binary.
Whitespace in the hex input is ignored.`,
		Subcommands: []*cli.Command{
			decodeCommand(),
			encodeCommand(),
			diagCommand(),
			validateCommand(),
		},
		Params: func() any { return &params },
		Run: func(args []string) error {
			data, remainingArgs, err := readInput(args, params.HexInput)
			if err != nil {
				return err
			}

			if len(remainingArgs) == 0 {
				// No arguments: default to decode.
				return decodeCBOR(data, os.Stdout, params.Compact, params.Slurp)
			}

			// Remaining positional args are a jq filter expression.
			var jqArgs []string
			if params.Compact {
				jqArgs = append(jqArgs, "-c")
			}
			if params.RawOutput {
				jqArgs = append(jqArgs, "-r")
			}
			if params.Slurp {
				jqArgs = append(jqArgs, "-s")
			}
			jqArgs = append(jqArgs, remainingArgs...)

			return filterCBOR(data, jqArgs)
		},
		Examples: []cli.Example{
			{
				Description: "Decode CBOR to pretty JSON",
				Command:     "bureau cbor < message.cbor",
			},
			{
				Description: "Decode a CBOR file to JSON",
				Command:     "bureau cbor decode message.cbor",
			},
			{
				Description: "Extract a field with jq",
				Command:     "bureau cbor '.action' request.cbor",
			},
			{
				Description: "Raw string output from jq filter",
				Command:     "bureau cbor -r '.name' principal.cbor",
			},
			{
				Description: "Decode hex-encoded CBOR",
				Command:     "echo 'a163...' | bureau cbor --hex",
			},
			{
				Description: "Encode JSON to CBOR",
				Command:     "echo '{\"action\":\"status\"}' | bureau cbor encode",
			},
			{
				Description: "Validate deterministic encoding",
				Command:     "bureau cbor validate message.cbor",
			},
			{
				Description: "Inspect CBOR structure with diagnostic notation",
				Command:     "bureau cbor diag token.cbor",
			},
			{
				Description: "Round-trip: encode then decode",
				Command:     "echo '{\"count\":42}' | bureau cbor encode | bureau cbor decode",
			},
		},
	}
}
