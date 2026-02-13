// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package cbor implements the "bureau cbor" subcommands for inspecting,
// producing, filtering, and validating CBOR data from the command line.
//
// Bureau uses CBOR with Core Deterministic Encoding (RFC 8949 §4.2) as
// the wire format for service sockets, artifact transfer, and service
// tokens. This tool understands that profile and provides ergonomic
// access to CBOR data without third-party dependencies.
//
// Subcommands:
//
//   - decode: convert CBOR to JSON.
//   - encode: convert JSON to CBOR using Bureau's Core Deterministic Encoding.
//   - diag: convert CBOR to RFC 8949 Extended Diagnostic Notation.
//   - validate: verify CBOR uses Bureau's Core Deterministic Encoding.
//
// All subcommands accept input from stdin or from a file path argument.
// The --hex flag treats input as hex-encoded CBOR for debugging wire
// dumps.
//
// When the first positional argument is not a subcommand name, it is
// treated as a jq filter expression. The tool decodes CBOR to JSON
// internally and pipes the result through jq. With no arguments at
// all, bureau cbor acts as an alias for bureau cbor decode.
package cbor
