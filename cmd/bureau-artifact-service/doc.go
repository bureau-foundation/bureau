// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package main implements the Bureau artifact service — a content-
// addressable storage service that accepts, chunks, compresses, and
// serves artifacts over a Unix socket.
//
// The artifact service follows the same service principal pattern as
// the ticket service: Matrix account, service registration in
// #bureau/service, sync loop watching for artifact_scope events, and
// a Unix socket for client operations.
//
// # Connection protocol
//
// Unlike services that use lib/service/SocketServer (one self-delimiting
// CBOR value per connection), the artifact service uses a length-prefixed
// CBOR protocol that supports multi-phase binary streaming. Each
// connection begins with a 4-byte big-endian length prefix followed by
// a CBOR message containing at minimum an "action" field.
//
// For simple actions (exists, show, status), the response is another
// length-prefixed CBOR message. For store and fetch, the connection
// carries a binary data stream between the CBOR header and response,
// allowing artifact data of arbitrary size to be transferred without
// buffering the entire payload in memory.
//
// This protocol is defined in lib/artifact/transfer.go and documented
// in .notes/design/ARTIFACTS.md.
package main
