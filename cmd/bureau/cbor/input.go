// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cbor

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"unicode"
)

// readInput resolves input data from either a file (the last element
// of args, if it names a regular file on disk) or stdin.
//
// When hexMode is true, the raw bytes are treated as hex-encoded CBOR:
// whitespace is stripped and the hex is decoded to binary.
//
// Returns the input bytes and the args with any consumed file path
// removed. The caller is responsible for validating that the returned
// args are acceptable (e.g., no unexpected positional arguments).
func readInput(args []string, hexMode bool) ([]byte, []string, error) {
	var data []byte
	remainingArgs := args

	if length := len(args); length > 0 {
		candidate := args[length-1]
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			data, err = os.ReadFile(candidate)
			if err != nil {
				return nil, nil, fmt.Errorf("read %s: %w", candidate, err)
			}
			remainingArgs = args[:length-1]
		}
	}

	if data == nil {
		var err error
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, nil, fmt.Errorf("read stdin: %w", err)
		}
	}

	if hexMode {
		decoded, err := decodeHexInput(data)
		if err != nil {
			return nil, nil, err
		}
		data = decoded
	}

	return data, remainingArgs, nil
}

// decodeHexInput strips whitespace from hex-encoded input and decodes
// it to binary bytes. Whitespace between hex digit pairs is allowed
// (e.g., "a1 63 6b 65 79" or "a1636b6579").
func decodeHexInput(data []byte) ([]byte, error) {
	cleaned := bytes.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, data)

	if len(cleaned) == 0 {
		return nil, fmt.Errorf("empty input after stripping whitespace from hex")
	}

	decoded := make([]byte, hex.DecodedLen(len(cleaned)))
	count, err := hex.Decode(decoded, cleaned)
	if err != nil {
		return nil, fmt.Errorf("decode hex: %w", err)
	}
	return decoded[:count], nil
}
