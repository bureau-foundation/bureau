// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"crypto/rand"
	"encoding/hex"
)

// GenerateRequestID creates a random 16-byte hex string for correlating
// command requests with their threaded result replies. Uses crypto/rand
// for uniqueness without external dependencies.
func GenerateRequestID() (string, error) {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer[:]), nil
}
