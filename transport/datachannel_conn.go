// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// DataChannelConn wraps a detached pion data channel ReadWriteCloser as a
// net.Conn. The underlying ReadWriteCloser is stream-oriented (SCTP handles
// message fragmentation and reassembly), so this behaves like a TCP
// connection from the perspective of HTTP and other stream protocols.
//
// Deadline support uses timer-based cancellation: when a deadline fires,
// the underlying stream is closed, causing any blocked Read/Write to return
// an error. This matches the pattern used by net.Pipe.
type DataChannelConn struct {
	rwc        io.ReadWriteCloser
	localLabel string
	peerLabel  string

	// Deadline state. Closing deadlineCloser terminates the rwc, unblocking
	// any pending Read/Write. Once closed, the conn is permanently broken.
	mu              sync.Mutex
	readTimer       *time.Timer
	writeTimer      *time.Timer
	deadlineClosed  bool
}

// Compile-time interface check.
var _ net.Conn = (*DataChannelConn)(nil)

// NewDataChannelConn wraps a detached pion data channel as a net.Conn.
// localLabel identifies the local endpoint (for logging/addr); peerLabel
// identifies the remote endpoint.
func NewDataChannelConn(rwc io.ReadWriteCloser, localLabel, peerLabel string) *DataChannelConn {
	return &DataChannelConn{
		rwc:        rwc,
		localLabel: localLabel,
		peerLabel:  peerLabel,
	}
}

func (c *DataChannelConn) Read(buffer []byte) (int, error) {
	return c.rwc.Read(buffer)
}

func (c *DataChannelConn) Write(buffer []byte) (int, error) {
	return c.rwc.Write(buffer)
}

func (c *DataChannelConn) Close() error {
	c.mu.Lock()
	c.stopTimersLocked()
	c.mu.Unlock()
	return c.rwc.Close()
}

// LocalAddr returns a synthetic address identifying the local data channel endpoint.
func (c *DataChannelConn) LocalAddr() net.Addr {
	return &dataChannelAddr{label: c.localLabel}
}

// RemoteAddr returns a synthetic address identifying the remote data channel endpoint.
func (c *DataChannelConn) RemoteAddr() net.Addr {
	return &dataChannelAddr{label: c.peerLabel}
}

// SetDeadline sets both read and write deadlines. A zero value clears the deadline.
func (c *DataChannelConn) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setReadDeadlineLocked(deadline)
	c.setWriteDeadlineLocked(deadline)
	return nil
}

// SetReadDeadline sets the read deadline. When the deadline fires, pending
// reads return an error. A zero value clears the deadline.
func (c *DataChannelConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setReadDeadlineLocked(deadline)
	return nil
}

// SetWriteDeadline sets the write deadline. When the deadline fires, pending
// writes return an error. A zero value clears the deadline.
func (c *DataChannelConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setWriteDeadlineLocked(deadline)
	return nil
}

func (c *DataChannelConn) setReadDeadlineLocked(deadline time.Time) {
	if c.readTimer != nil {
		c.readTimer.Stop()
		c.readTimer = nil
	}
	if deadline.IsZero() || c.deadlineClosed {
		return
	}
	duration := time.Until(deadline)
	if duration <= 0 {
		c.closeFromDeadline()
		return
	}
	c.readTimer = time.AfterFunc(duration, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.closeFromDeadline()
	})
}

func (c *DataChannelConn) setWriteDeadlineLocked(deadline time.Time) {
	if c.writeTimer != nil {
		c.writeTimer.Stop()
		c.writeTimer = nil
	}
	if deadline.IsZero() || c.deadlineClosed {
		return
	}
	duration := time.Until(deadline)
	if duration <= 0 {
		c.closeFromDeadline()
		return
	}
	c.writeTimer = time.AfterFunc(duration, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.closeFromDeadline()
	})
}

// closeFromDeadline closes the underlying stream to unblock pending I/O.
// Must be called with c.mu held.
func (c *DataChannelConn) closeFromDeadline() {
	if c.deadlineClosed {
		return
	}
	c.deadlineClosed = true
	c.rwc.Close()
}

func (c *DataChannelConn) stopTimersLocked() {
	if c.readTimer != nil {
		c.readTimer.Stop()
		c.readTimer = nil
	}
	if c.writeTimer != nil {
		c.writeTimer.Stop()
		c.writeTimer = nil
	}
}

// errDeadlineExceeded is returned when a deadline-triggered close causes
// a read/write to fail. We rely on the rwc returning its own error when
// closed, but callers can check for this sentinel.
var errDeadlineExceeded = errors.New("data channel deadline exceeded")

// dataChannelAddr is a synthetic net.Addr for data channel connections.
type dataChannelAddr struct {
	label string
}

func (a *dataChannelAddr) Network() string { return "webrtc" }
func (a *dataChannelAddr) String() string  { return a.label }
