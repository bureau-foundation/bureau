// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package messaging wraps the Matrix client-server API for Bureau's
// communication and state management needs.
//
// The package provides two core types. [Client] is an unauthenticated Matrix
// client that handles registration (token-authenticated via MSC3231 UIAA
// flow) and login, returning authenticated [Session] values. Client holds
// the homeserver URL and HTTP transport, shared across all Sessions derived
// from it.
//
// [Session] wraps a Client with an access token for authenticated operations:
// room management (create, join, leave, invite, kick), messaging (send events,
// room messages with pagination, thread messages via the relations endpoint),
// state events (get/set individual events, full room state), incremental sync
// with long-polling, room alias resolution, media upload, TURN credential
// retrieval, password changes, and identity verification (WhoAmI).
//
// Sessions are lightweight (a pointer to the parent Client plus an access
// token in mmap-backed secret.Buffer memory) and safe to create in large
// numbers. The proxy process holds one Client and many Sessions, one per
// sandboxed agent. The access token is locked against swap and excluded from
// core dumps; callers must call Session.Close to release the protected memory.
//
// All API errors are returned as [*MatrixError] with the standard Matrix
// error code (M_FORBIDDEN, M_NOT_FOUND, etc.) and HTTP status code.
// [IsMatrixError] tests for a specific error code. Request URLs are built
// by string concatenation rather than url.URL to avoid double-encoding of
// path segments that contain URL-encoded characters (such as room aliases
// with slashes).
//
// Thread support is first-class: [NewTextMessage] creates a plain message,
// [NewThreadReply] creates a threaded reply with the m.thread relation type,
// and Session.ThreadMessages fetches thread contents via the relations API.
package messaging
