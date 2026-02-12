// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package netutil

import (
	"errors"
	"io"
	"net"
	"syscall"
)

// IsExpectedCloseError reports whether err is a normal connection termination:
// EOF, closed connection, broken pipe, or connection reset. These errors occur
// during normal bidirectional bridge teardown when one side disconnects and the
// other side's in-flight read or write fails as a result.
//
// Bridges that use full-close (closing the entire connection rather than
// half-close via CloseWrite) produce ECONNRESET and EPIPE instead of EOF on
// the surviving side. All four are expected and should not be logged as errors.
func IsExpectedCloseError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.EPIPE || errno == syscall.ECONNRESET
	}
	return false
}
