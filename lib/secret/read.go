// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package secret

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
)

// ReadFromPath reads a secret from a file path, or from stdin if path is "-".
// The returned buffer is mmap-backed (locked into RAM, excluded from core
// dumps) and must be closed by the caller. Leading/trailing whitespace is
// trimmed before storing. Returns an error if the source is empty after
// trimming.
func ReadFromPath(path string) (*Buffer, error) {
	var data []byte

	if path == "-" {
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return nil, fmt.Errorf("reading stdin: %w", err)
			}
			return nil, fmt.Errorf("stdin is empty")
		}
		data = scanner.Bytes()
	} else {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		Zero(data)
		return nil, fmt.Errorf("secret is empty")
	}

	// NewFromBytes copies into mmap-backed memory and zeros trimmed.
	buffer, err := NewFromBytes(trimmed)
	// Zero remaining bytes (whitespace prefix/suffix) not covered by trimmed.
	Zero(data)
	if err != nil {
		return nil, err
	}
	return buffer, nil
}
