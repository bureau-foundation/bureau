// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// DataChannelConn wraps a detached pion data channel ReadWriteCloser as a
// net.Conn with non-destructive read deadline support.
//
// The key design constraint: http.Server calls SetReadDeadline(aLongTimeAgo)
// during Hijack() to abort its internal background reader, then the Hijack
// caller reuses the same connection for raw streaming. Read deadlines must
// NOT close the underlying stream — they must only unblock pending Reads
// without destroying the connection.
//
// Architecture:
//   - A read pump goroutine continuously reads from the underlying rwc into
//     a buffered channel, decoupling Read() from rwc.Read().
//   - Read() selects on the pump channel and a deadline notification channel.
//     When a read deadline fires, Read returns os.ErrDeadlineExceeded without
//     touching the underlying stream. Clearing the deadline (zero time) allows
//     subsequent Reads to succeed normally.
//   - Deadline notification uses a close-and-replace channel pattern: every
//     call to SetReadDeadline closes the current readDeadlineChanged channel
//     and creates a new one. This wakes any goroutine blocked in Read's
//     select, which then re-checks the readExpired flag. This avoids a race
//     where Read captures a stale reference to a per-deadline cancel channel
//     (the pattern used by net.Pipe but unsuitable here because http.Server
//     clears and re-sets deadlines across goroutine boundaries during Hijack).
//   - Write deadlines use a flag pre-check before calling rwc.Write. If a
//     write is blocked inside rwc.Write when the deadline timer fires, the
//     timer closes the rwc to unblock it. Write deadlines only fire on
//     genuine timeouts (minutes), and when they fire the connection is dead.
type DataChannelConn struct {
	rwc        io.ReadWriteCloser
	localLabel string
	peerLabel  string

	// Read pump: a goroutine reads from rwc and delivers results here.
	// Buffer capacity 4 allows the pump to read ahead slightly.
	pumpChannel chan pumpResult
	pumpOnce    sync.Once

	// Leftover bytes from a pump result larger than the caller's Read
	// buffer. Only accessed from Read(), which is serialized by callers
	// (net.Conn does not require concurrent Read safety).
	remainder []byte

	// Close signaling.
	closedChannel chan struct{}
	closeOnce     sync.Once

	// Deadline state, protected by mu.
	mu                  sync.Mutex
	readExpired         bool
	readTimer           *time.Timer
	readDeadlineChanged chan struct{} // closed+replaced on every SetReadDeadline
	writeExpired        bool
	writeTimer          *time.Timer
}

// pumpResult carries one read from the underlying stream.
type pumpResult struct {
	data []byte
	err  error
}

// Compile-time interface check.
var _ net.Conn = (*DataChannelConn)(nil)

// NewDataChannelConn wraps a detached pion data channel as a net.Conn.
// localLabel identifies the local endpoint (for logging/addr); peerLabel
// identifies the remote endpoint.
func NewDataChannelConn(rwc io.ReadWriteCloser, localLabel, peerLabel string) *DataChannelConn {
	return &DataChannelConn{
		rwc:                 rwc,
		localLabel:          localLabel,
		peerLabel:           peerLabel,
		pumpChannel:         make(chan pumpResult, 4),
		closedChannel:       make(chan struct{}),
		readDeadlineChanged: make(chan struct{}),
	}
}

// startReadPump launches the background goroutine that reads from the
// underlying stream and delivers results to the pump channel. Called
// once via pumpOnce on the first Read.
func (c *DataChannelConn) startReadPump() {
	go func() {
		for {
			buffer := make([]byte, 32*1024)
			bytesRead, err := c.rwc.Read(buffer)

			result := pumpResult{err: err}
			if bytesRead > 0 {
				result.data = buffer[:bytesRead]
			}

			select {
			case c.pumpChannel <- result:
			case <-c.closedChannel:
				return
			}

			if err != nil {
				return
			}
		}
	}()
}

// Read reads data from the data channel. If a read deadline is set and
// fires before data arrives, Read returns os.ErrDeadlineExceeded without
// closing the underlying stream. Subsequent reads (after the deadline is
// cleared or extended) will receive any data that arrived in the interim.
func (c *DataChannelConn) Read(buffer []byte) (int, error) {
	// Drain leftover from a previous oversized pump result.
	if len(c.remainder) > 0 {
		n := copy(buffer, c.remainder)
		if n == len(c.remainder) {
			c.remainder = nil
		} else {
			c.remainder = c.remainder[n:]
		}
		return n, nil
	}

	c.pumpOnce.Do(c.startReadPump)

	for {
		// Non-blocking check: deliver data immediately if available,
		// regardless of deadline state. This matches net.Conn semantics
		// where available data takes priority over expired deadlines.
		select {
		case result := <-c.pumpChannel:
			return c.deliverPumpResult(buffer, result)
		default:
		}

		// No data immediately available. Check deadline, then block.
		c.mu.Lock()
		expired := c.readExpired
		changed := c.readDeadlineChanged
		c.mu.Unlock()

		if expired {
			return 0, os.ErrDeadlineExceeded
		}

		select {
		case result := <-c.pumpChannel:
			return c.deliverPumpResult(buffer, result)
		case <-changed:
			// Deadline state changed (set, cleared, or fired).
			// Loop back to re-check readExpired.
			continue
		case <-c.closedChannel:
			return 0, net.ErrClosed
		}
	}
}

// deliverPumpResult copies data from a pump result into the caller's buffer,
// saving any overflow in c.remainder for the next Read call.
func (c *DataChannelConn) deliverPumpResult(buffer []byte, result pumpResult) (int, error) {
	if result.err != nil && len(result.data) == 0 {
		return 0, result.err
	}
	n := copy(buffer, result.data)
	if n < len(result.data) {
		c.remainder = make([]byte, len(result.data)-n)
		copy(c.remainder, result.data[n:])
	}
	// Return data without error even if the pump reported one. The error
	// will surface on the next Read when the pump channel is drained.
	// This matches the io.Reader contract: return data when available.
	return n, nil
}

// Write writes data to the data channel. If a write deadline has expired,
// Write returns os.ErrDeadlineExceeded immediately. Otherwise it writes
// directly to the underlying stream (SCTP writes are typically non-blocking
// as data is queued in the SCTP association's send buffer).
func (c *DataChannelConn) Write(buffer []byte) (int, error) {
	c.mu.Lock()
	expired := c.writeExpired
	c.mu.Unlock()

	if expired {
		return 0, os.ErrDeadlineExceeded
	}

	return c.rwc.Write(buffer)
}

// Close shuts down the connection, stops deadline timers, and closes the
// underlying stream. The read pump goroutine exits when it sees the
// closed stream or the closedChannel signal.
func (c *DataChannelConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closedChannel)
	})
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
// Reads return os.ErrDeadlineExceeded. A zero value clears the deadline,
// allowing subsequent Reads to succeed. The underlying stream is never
// closed by a read deadline — this is critical for http.Server's Hijack
// pattern where SetReadDeadline(past) aborts a background read and then
// the caller reuses the connection for raw streaming.
func (c *DataChannelConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setReadDeadlineLocked(deadline)
	return nil
}

// SetWriteDeadline sets the write deadline. A zero value clears the deadline.
// If a write is blocked inside rwc.Write when the deadline timer fires,
// the timer closes the underlying stream to unblock it. This is acceptable
// because write timeouts (minutes) indicate a genuinely dead connection.
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

	if deadline.IsZero() {
		// Clear: allow subsequent Reads to proceed.
		c.readExpired = false
		c.signalReadDeadlineChangedLocked()
		return
	}

	duration := time.Until(deadline)
	if duration <= 0 {
		// Already expired. This is the path taken by http.Server's
		// Hijack → abortPendingRead, which sets the deadline to a
		// time far in the past to interrupt the background reader.
		c.readExpired = true
		c.signalReadDeadlineChangedLocked()
		return
	}

	// Future deadline: clear expired flag and arm a timer.
	c.readExpired = false
	c.readTimer = time.AfterFunc(duration, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.readExpired = true
		c.signalReadDeadlineChangedLocked()
	})
	c.signalReadDeadlineChangedLocked()
}

// signalReadDeadlineChangedLocked wakes any goroutine blocked in Read's
// select by closing the current notification channel and replacing it.
// Must be called with c.mu held.
func (c *DataChannelConn) signalReadDeadlineChangedLocked() {
	close(c.readDeadlineChanged)
	c.readDeadlineChanged = make(chan struct{})
}

func (c *DataChannelConn) setWriteDeadlineLocked(deadline time.Time) {
	if c.writeTimer != nil {
		c.writeTimer.Stop()
		c.writeTimer = nil
	}

	c.writeExpired = false

	if deadline.IsZero() {
		return
	}

	duration := time.Until(deadline)
	if duration <= 0 {
		c.writeExpired = true
		return
	}

	c.writeTimer = time.AfterFunc(duration, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.writeExpired = true
		// Close the underlying stream to unblock any blocked Write.
		// A write timeout firing means the connection is dead.
		c.rwc.Close()
	})
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

// dataChannelAddr is a synthetic net.Addr for data channel connections.
type dataChannelAddr struct {
	label string
}

func (a *dataChannelAddr) Network() string { return "webrtc" }
func (a *dataChannelAddr) String() string  { return a.label }
