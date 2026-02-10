// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package environment

import "runtime"

// goarch returns GOARCH. Split out for testability — tests can override
// via the build tag mechanism or by testing currentSystem() directly.
func goarch() string { return runtime.GOARCH }
func goos() string   { return runtime.GOOS }
