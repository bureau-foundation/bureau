// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cbor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/codec"
)

// validateParams holds the parameters for the "bureau cbor validate" command.
type validateParams struct {
	Slurp    bool `json:"slurp"     flag:"slurp,s" desc:"validate each item in a CBOR sequence independently"`
	HexInput bool `json:"hex_input" flag:"hex,x"   desc:"treat input as hex-encoded CBOR"`
}

func validateCommand() *cli.Command {
	var params validateParams

	return &cli.Command{
		Name:    "validate",
		Summary: "Check whether CBOR uses Bureau's Core Deterministic Encoding",
		Description: `Read CBOR data and verify it matches Bureau's Core Deterministic
Encoding profile (RFC 8949 §4.2). Exits 0 with "valid" if the input
is deterministic, exits 1 with a diagnostic message if not.

Validation works by decoding the input and re-encoding with Bureau's
encoder, then comparing the bytes. This catches unsorted map keys,
non-minimal integer encoding, indefinite-length items, and other
deviations from Bureau's profile.

With -s, validates each item in a CBOR sequence independently.`,
		Usage: "bureau cbor validate [-s] [-x] [file]",
		Examples: []cli.Example{
			{
				Description: "Validate CBOR from a pipeline",
				Command:     "echo '{\"count\":42}' | bureau cbor encode | bureau cbor validate",
			},
			{
				Description: "Validate a CBOR file",
				Command:     "bureau cbor validate message.cbor",
			},
			{
				Description: "Validate hex-encoded CBOR",
				Command:     "echo 'a1636b657963766174' | bureau cbor validate --hex",
			},
		},
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/cbor/validate"},
		Run: func(args []string) error {
			data, remainingArgs, err := readInput(args, params.HexInput)
			if err != nil {
				return err
			}
			if len(remainingArgs) > 0 {
				return fmt.Errorf("validate takes no positional arguments besides an optional file path, got %q", remainingArgs[0])
			}
			return validateCBOR(data, os.Stdout, params.Slurp)
		},
	}
}

// validateCBOR checks whether data is valid Bureau Core Deterministic
// CBOR by decoding and re-encoding, then comparing bytes.
func validateCBOR(data []byte, w io.Writer, slurp bool) error {
	if len(data) == 0 {
		return fmt.Errorf("empty input: expected CBOR data")
	}

	if slurp {
		return validateSequence(data, w)
	}

	return validateSingle(data, w)
}

func validateSingle(data []byte, w io.Writer) error {
	var value any
	if err := toolDecMode.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode CBOR: %w", err)
	}

	reencoded, err := codec.Marshal(value)
	if err != nil {
		return fmt.Errorf("re-encode CBOR: %w", err)
	}

	if bytes.Equal(data, reencoded) {
		fmt.Fprintln(w, "valid")
		return nil
	}

	return describeMismatch(data, reencoded)
}

func validateSequence(data []byte, w io.Writer) error {
	decoder := toolDecMode.NewDecoder(bytes.NewReader(data))
	var reencoded bytes.Buffer
	var count int
	for {
		var value any
		if err := decoder.Decode(&value); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("decode CBOR sequence item %d: %w", count, err)
		}

		itemBytes, err := codec.Marshal(value)
		if err != nil {
			return fmt.Errorf("re-encode CBOR sequence item %d: %w", count, err)
		}
		reencoded.Write(itemBytes)
		count++
	}

	if count == 0 {
		return fmt.Errorf("empty input: expected CBOR data")
	}

	if bytes.Equal(data, reencoded.Bytes()) {
		fmt.Fprintln(w, "valid")
		return nil
	}

	return describeMismatch(data, reencoded.Bytes())
}

func describeMismatch(original, reencoded []byte) error {
	offset := 0
	minLength := len(original)
	if len(reencoded) < minLength {
		minLength = len(reencoded)
	}
	for offset < minLength {
		if original[offset] != reencoded[offset] {
			break
		}
		offset++
	}

	return fmt.Errorf("not deterministic: first difference at byte %d (original %d bytes, re-encoded %d bytes)",
		offset, len(original), len(reencoded))
}
