// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cbor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	gocbor "github.com/fxamacker/cbor/v2"
)

// toolDecMode is a CBOR decoder for the CLI tool. Unlike lib/codec's
// decoder (which sets DefaultMapType to map[string]any), this one uses
// the default map type (map[any]any) so it can decode CBOR with
// integer keys (keyasint encoding). The normalizeValue function then
// converts the result to JSON-compatible types.
var toolDecMode gocbor.DecMode

func init() {
	var err error
	toolDecMode, err = gocbor.DecOptions{}.DecMode()
	if err != nil {
		panic("cbor tool: decoder initialization failed: " + err.Error())
	}
}

func decodeCommand() *cli.Command {
	var (
		compact bool
		slurp   bool
	)

	return &cli.Command{
		Name:    "decode",
		Summary: "Convert CBOR on stdin to JSON on stdout",
		Description: `Read CBOR data from stdin and write the equivalent JSON to stdout.

By default, output is pretty-printed with 2-space indentation. Use -c
for compact single-line output.

CBOR integer map keys (from keyasint-encoded structs) appear as string
keys in JSON (e.g., "1", "2") since JSON requires string keys. Use
"bureau cbor diag" for a representation that preserves CBOR types.

With -s, reads a CBOR sequence (multiple consecutive items) and outputs
them as a JSON array.`,
		Usage: "bureau cbor decode [-c] [-s]",
		Examples: []cli.Example{
			{
				Description: "Decode a CBOR file to pretty JSON",
				Command:     "bureau cbor decode < message.cbor",
			},
			{
				Description: "Decode a CBOR sequence to a JSON array",
				Command:     "bureau cbor decode -s < sequence.cbor",
			},
		},
		Flags: cborFlags(&compact, &slurp, nil),
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("decode takes no positional arguments, got %q", args[0])
			}
			return decodeCBOR(os.Stdin, os.Stdout, compact, slurp)
		},
	}
}

// decodeCBOR reads CBOR from r and writes JSON to w.
func decodeCBOR(r io.Reader, w io.Writer, compact bool, slurp bool) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	if len(data) == 0 {
		return fmt.Errorf("empty input: expected CBOR data on stdin")
	}

	if slurp {
		return decodeSlurp(data, w, compact)
	}

	var value any
	if err := toolDecMode.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode CBOR: %w", err)
	}

	return writeJSON(w, normalizeValue(value), compact)
}

// decodeSlurp reads a CBOR sequence (multiple concatenated items) and
// outputs them as a JSON array.
func decodeSlurp(data []byte, w io.Writer, compact bool) error {
	decoder := toolDecMode.NewDecoder(bytes.NewReader(data))
	var items []any
	for {
		var value any
		if err := decoder.Decode(&value); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("decode CBOR sequence item %d: %w", len(items), err)
		}
		items = append(items, normalizeValue(value))
	}

	if len(items) == 0 {
		return fmt.Errorf("empty input: expected CBOR data on stdin")
	}

	return writeJSON(w, items, compact)
}

// normalizeValue recursively converts CBOR-decoded values to
// JSON-compatible types. The main transformation is converting
// map[any]any (from CBOR maps with integer keys) to map[string]any
// with fmt.Sprint'd keys.
func normalizeValue(v any) any {
	switch value := v.(type) {
	case map[any]any:
		result := make(map[string]any, len(value))
		for key, element := range value {
			result[fmt.Sprint(key)] = normalizeValue(element)
		}
		return result

	case map[string]any:
		for key, element := range value {
			value[key] = normalizeValue(element)
		}
		return value

	case []any:
		for index, element := range value {
			value[index] = normalizeValue(element)
		}
		return value

	default:
		return v
	}
}

// writeJSON encodes value as JSON and writes it to w with a trailing
// newline. When compact is false, output is pretty-printed with 2-space
// indentation.
func writeJSON(w io.Writer, value any, compact bool) error {
	var output []byte
	var err error
	if compact {
		output, err = json.Marshal(value)
	} else {
		output, err = json.MarshalIndent(value, "", "  ")
	}
	if err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}

	_, err = fmt.Fprintln(w, string(output))
	return err
}
