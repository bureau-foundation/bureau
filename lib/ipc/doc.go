// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package ipc defines the CBOR-encoded message types for the
// daemon↔launcher Unix socket protocol. Both cmd/bureau-daemon and
// cmd/bureau-launcher import this package so the wire types are
// defined once rather than mirrored.
package ipc
