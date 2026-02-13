// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cbor

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/codec"
)

func encodeCommand() *cli.Command {
	return &cli.Command{
		Name:    "encode",
		Summary: "Convert JSON on stdin to CBOR on stdout",
		Description: `Read JSON from stdin and write the equivalent CBOR to stdout using
Bureau's Core Deterministic Encoding (RFC 8949 S4.2).

JSON integers are preserved as CBOR integers (not floats). This matters
for interoperability with Bureau services that use integer map keys and
typed numeric fields.

The output is binary. Pipe to "bureau cbor diag" or "xxd" to inspect.`,
		Usage: "bureau cbor encode",
		Examples: []cli.Example{
			{
				Description: "Encode JSON to CBOR",
				Command:     "echo '{\"action\":\"status\"}' | bureau cbor encode > request.cbor",
			},
			{
				Description: "Round-trip: encode then decode",
				Command:     "echo '{\"count\":42}' | bureau cbor encode | bureau cbor decode",
			},
		},
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("encode takes no positional arguments, got %q", args[0])
			}
			return encodeCBOR(os.Stdin, os.Stdout)
		},
	}
}

// encodeCBOR reads JSON from r and writes CBOR to w.
func encodeCBOR(r io.Reader, w io.Writer) error {
	decoder := json.NewDecoder(r)
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}

	value = convertNumbers(value)

	data, err := codec.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode CBOR: %w", err)
	}

	_, err = w.Write(data)
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
