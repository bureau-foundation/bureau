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
// # Struct Tag Rules
//
// The struct tag on a type documents its serialization format:
//
//   - `cbor` tag: this type is ONLY ever serialized as CBOR. It will
//     never be marshaled to JSON or interact with CLI tooling.
//     Examples: daemon↔launcher IPC messages, on-disk CBOR state
//     files, internal protocol envelopes (auth revocations, etc.).
//   - `json` tag: this type may be serialized as BOTH JSON and CBOR.
//     fxamacker/cbor v2 reads `json` tags as fallback when `cbor`
//     tags are absent, so a single `json` tag controls field naming
//     and omitempty for both formats. Examples: service socket
//     protocol types (which CLI and MCP tools consume), types used
//     in CLI --json output, types shared between Matrix (JSON) and
//     socket (CBOR) protocols.
//
// Never use both `cbor` and `json` tags on the same field. The tag
// choice documents the contract — doubling up is noise that obscures
// whether a type participates in JSON serialization.
package codec
