// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package principal handles Bureau principal identity: validation of
// hierarchical localparts, construction of Matrix user IDs and room
// aliases, and bidirectional mapping between Matrix identities and
// filesystem paths.
//
// Bureau uses Matrix localparts with "/" separators to create a
// hierarchical namespace that maps 1:1 to filesystem paths:
//
//	@iree/amdgpu/pm:bureau.local  ->  /run/bureau/principal/iree/amdgpu/pm.sock
//
// [ValidateLocalpart] enforces the invariants that make this mapping
// safe: no ".." traversal, no hidden segments, no empty segments, only
// the Matrix localpart charset (a-z, 0-9, ., _, =, -, /), and at most
// 80 characters (derived from the 108-byte Unix socket path limit).
//
// Two socket path namespaces are provided:
//
//   - [SocketPath] -- /run/bureau/principal/, visible inside sandboxes
//   - [AdminSocketPath] -- /run/bureau/admin/, daemon-only
//
// [MatchPattern] and [MatchAnyPattern] provide glob-based access
// control matching: "*" (single segment), "**" (recursive), and
// interior patterns like "iree/**/pm". Malformed patterns deny by
// default rather than propagating errors.
//
// This package has no dependencies on other Bureau packages.
package principal
