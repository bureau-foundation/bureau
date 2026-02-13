// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package codec provides Bureau's standard CBOR encoding configuration.
//
// Bureau uses two serialization formats with a clear boundary:
//
//   - JSON for external interfaces: Matrix Client-Server API, HTTP
//     proxy endpoints, CLI output, and the sandbox filesystem contract
//     (payload.json, identity.json, trigger.json).
//   - CBOR for internal protocols: daemon↔launcher IPC, service socket
//     communication, on-disk state files (watchdog, exec state), and
//     service identity tokens.
//
// This package provides the shared CBOR encoding and decoding modes so
// that every Bureau package encodes identically without duplicating
// configuration. The encoder uses Core Deterministic Encoding (RFC 8949
// §4.2): sorted map keys, smallest integer encoding, no
// indefinite-length items. Same logical data always produces identical
// bytes.
//
// For buffer-oriented operations (files, tokens):
//
//	data, err := codec.Marshal(value)
//	err = codec.Unmarshal(data, &value)
//
// For stream-oriented operations (sockets, IPC):
//
//	encoder := codec.NewEncoder(conn)
//	decoder := codec.NewDecoder(conn)
//
// Struct tag convention: types used exclusively with CBOR use `cbor`
// struct tags. Types that serve both JSON and CBOR use `json` struct
// tags — fxamacker/cbor falls back to json tags when cbor tags are
// absent.
package codec
