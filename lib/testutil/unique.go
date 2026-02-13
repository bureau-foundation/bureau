// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"fmt"
	"sync/atomic"
)

var uniqueCounter atomic.Uint64

// UniqueID returns a string of the form "prefix-N" where N is a
// monotonically increasing integer. Use this instead of time.Now() when
// tests need unique identifiers for transaction IDs, request IDs, or
// message bodies that must be distinguishable in shared rooms.
//
//	txnID := testutil.UniqueID("txn")         // "txn-1", "txn-2", ...
//	msg := testutil.UniqueID("hello-from-b")  // "hello-from-b-3", ...
func UniqueID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, uniqueCounter.Add(1))
}
