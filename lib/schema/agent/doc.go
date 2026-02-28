// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package agent defines schema types for Bureau agent session lifecycle,
// context management, context commit chains, and metrics aggregation.
// Session and metrics types are stored as Matrix state events in machine
// config rooms. Context commit metadata is stored as CBOR artifacts in
// the CAS, tagged as "ctx/<commitID>".
package agent
