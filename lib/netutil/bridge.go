// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package netutil

import (
	"io"
	"net"
)

// bridgeCopyResult holds the outcome of one direction of a bidirectional copy.
type bridgeCopyResult struct {
	bytesCopied int64
	err         error
}

// BridgeReaders copies data bidirectionally between two connections using the
// provided readers. The readers may differ from the connections when buffered
// data has already been consumed (e.g., after a JSON handshake on the
// connection). Each direction copies from one reader into the opposite
// connection's writer.
//
// Returns when either direction finishes. Both connections are closed before
// returning to unblock the surviving goroutine. Returns the error from the
// direction that terminated first, or nil if termination was due to normal
// connection closure (EOF, peer disconnect, broken pipe, connection reset).
func BridgeReaders(connectionA net.Conn, readerA io.Reader, connectionB net.Conn, readerB io.Reader) error {
	done := make(chan bridgeCopyResult, 2)

	go func() {
		bytesCopied, err := io.Copy(connectionB, readerA)
		done <- bridgeCopyResult{bytesCopied, err}
	}()

	go func() {
		bytesCopied, err := io.Copy(connectionA, readerB)
		done <- bridgeCopyResult{bytesCopied, err}
	}()

	// Wait for one direction to finish, then close both to unblock the other.
	first := <-done
	connectionA.Close()
	connectionB.Close()
	<-done

	if first.err != nil && !IsExpectedCloseError(first.err) {
		return first.err
	}
	return nil
}

// BridgeConnections copies bytes bidirectionally between two connections.
// This is a convenience wrapper around BridgeReaders for the common case
// where no buffered data exists — each connection is used as both reader
// and writer.
func BridgeConnections(a, b net.Conn) error {
	return BridgeReaders(a, a, b, b)
}
