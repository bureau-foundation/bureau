// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cbor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/codec"
)

// encodeParams holds the parameters for the "bureau cbor encode" command.
// Encode has no flags but the struct satisfies the structural requirement
// that every leaf command has a Params function.
type encodeParams struct{}

func encodeCommand() *cli.Command {
	var params encodeParams

	return &cli.Command{
		Name:    "encode",
		Summary: "Convert JSON to CBOR",
		Description: `Read JSON from stdin (or a file argument) and write the equivalent CBOR
to stdout using Bureau's Core Deterministic Encoding (RFC 8949 §4.2).

JSON integers are preserved as CBOR integers (not floats). This matters
for interoperability with Bureau services that use integer map keys and
typed numeric fields.

The output is binary. Pipe to "bureau cbor diag" or "xxd" to inspect.`,
		Usage:          "bureau cbor encode [file]",
		Params:         func() any { return &params },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/cbor/encode"},
		Examples: []cli.Example{
			{
				Description: "Encode JSON to CBOR",
				Command:     "echo '{\"action\":\"status\"}' | bureau cbor encode > request.cbor",
			},
			{
				Description: "Encode a JSON file to CBOR",
				Command:     "bureau cbor encode input.json > output.cbor",
			},
			{
				Description: "Round-trip: encode then decode",
				Command:     "echo '{\"count\":42}' | bureau cbor encode | bureau cbor decode",
			},
		},
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			data, remainingArgs, err := readInput(args, false)
			if err != nil {
				return err
			}
			if len(remainingArgs) > 0 {
				return cli.Validation("encode takes no positional arguments besides an optional file path, got %q", remainingArgs[0])
			}
			return encodeCBOR(data, os.Stdout)
		},
	}
}

// encodeCBOR encodes JSON data as CBOR and writes it to w.
func encodeCBOR(data []byte, w io.Writer) error {
	if len(data) == 0 {
		return cli.Validation("empty input: expected JSON data")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return cli.Internal("decode JSON: %w", err)
	}

	value = convertNumbers(value)

	cborData, err := codec.Marshal(value)
	if err != nil {
		return cli.Internal("encode CBOR: %w", err)
	}

	_, err = w.Write(cborData)
	return err
}

// convertNumbers recursively walks a JSON-decoded value and converts
// json.Number to int64 or float64. Without this, json.Decoder with
// UseNumber() leaves numbers as strings that the CBOR encoder would
// encode as text instead of numeric types.
func convertNumbers(v any) any {
	switch value := v.(type) {
	case json.Number:
		if integer, err := value.Int64(); err == nil {
			return integer
		}
		if float, err := value.Float64(); err == nil {
			return float
		}
		// json.Number that is neither int64 nor float64 should not
		// happen with valid JSON, but fail loudly if it does.
		panic(fmt.Sprintf("json.Number %q is neither int64 nor float64", value.String()))

	case map[string]any:
		for key, element := range value {
			value[key] = convertNumbers(element)
		}
		return value

	case []any:
		for index, element := range value {
			value[index] = convertNumbers(element)
		}
		return value

	default:
		return v
	}
}
