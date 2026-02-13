// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package cbor implements the "bureau cbor" subcommands for inspecting,
// producing, and filtering CBOR data from the command line.
//
// Bureau uses CBOR with Core Deterministic Encoding (RFC 8949 S4.2) as
// the wire format for service sockets, artifact transfer, and service
// tokens. This tool understands that profile and provides ergonomic
// access to CBOR data without third-party dependencies.
//
// Subcommands:
//
//   - decode: convert CBOR on stdin to JSON on stdout.
//   - encode: convert JSON on stdin to CBOR on stdout using Bureau's
//     Core Deterministic Encoding.
//   - diag: convert CBOR on stdin to RFC 8949 Extended Diagnostic
//     Notation on stdout.
//
// When the first positional argument is not a subcommand name, it is
// treated as a jq filter expression. The tool decodes CBOR to JSON
// internally and pipes the result through jq. With no arguments at
// all, bureau cbor acts as an alias for bureau cbor decode.
package cbor
