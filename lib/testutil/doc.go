// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides shared test helpers for Bureau packages.
//
// [SocketDir] creates a temporary directory in /tmp suitable for Unix
// domain sockets. This exists because Unix domain sockets have a
// 108-byte path limit (sun_path in sockaddr_un), and build systems
// like Bazel set TEST_TMPDIR to deeply nested paths that exceed this
// limit, making t.TempDir() unsuitable for socket files. The directory
// is automatically removed when the test completes.
//
// [DataBinary] resolves a pre-built test binary from Bazel's runfiles.
// Tests declare binary dependencies as data attributes in BUILD.bazel
// with $(rlocationpath ...) environment variables. DataBinary reads the
// environment variable, resolves it against RUNFILES_DIR, and returns
// an absolute path. This avoids calling "go build" from tests and
// ensures reproducible builds through Bazel's dependency graph.
//
// [RequireReceive], [RequireSend], and [RequireClosed] encapsulate the
// timeout safety valve pattern (select with time.After fallback) so
// that individual tests do not need direct time.After calls. These are
// the only place in the test suite where real wall-clock timeouts are
// used; see script/check-real-clock for the rationale.
//
// [UniqueID] generates monotonically increasing identifiers for test
// disambiguation. Use it instead of time.Now() when tests need unique
// transaction IDs, request IDs, or message bodies distinguishable in
// shared rooms.
//
// All helpers call t.Fatalf on failure rather than returning errors,
// since test setup failures are not recoverable.
//
// This package has no Bureau-internal dependencies.
package testutil
