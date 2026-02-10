// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import "testing"

// testCredentials creates a MapCredentialSource for use in tests. Calls
// t.Fatal if buffer allocation fails. The source is registered for cleanup
// when the test ends.
func testCredentials(t *testing.T, values map[string]string) *MapCredentialSource {
	t.Helper()
	source, err := NewMapCredentialSource(values)
	if err != nil {
		t.Fatalf("testCredentials: %v", err)
	}
	t.Cleanup(func() { source.Close() })
	return source
}
