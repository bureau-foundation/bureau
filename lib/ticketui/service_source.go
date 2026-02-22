// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

// ServiceSource implements [Source] by connecting to the ticket service's
// subscribe stream over a CBOR Unix socket. It maintains a local
// [ticketindex.Index] populated from the stream, giving zero-latency
// query responses identical to [IndexSource].
//
// The background goroutine handles connection lifecycle: initial
// connect, subscribe handshake, frame processing, and exponential
// backoff reconnection. Callers use [LoadingState] to display
// appropriate loading indicators in the TUI.
//
// Token refresh is supported via [SetToken]; the new token takes
// effect on the next reconnection. The current connection continues
// using the token it authenticated with.
type ServiceSource struct {
	// IndexSource is embedded to reuse all Source interface methods
	// (Ready, Blocked, All, Get, etc.) and the subscriber dispatch
	// logic (Subscribe, Put, Remove). The stream handler calls Put
	// and Remove on the embedded IndexSource to update the local
	// index and dispatch events to TUI subscribers.
	IndexSource

	socketPath   string
	token        atomic.Value // stores []byte
	roomID       string
	loadingState atomic.Value // stores string

	cancel context.CancelFunc
	logger *slog.Logger
}

// serviceSubscribeFrame is the client-side CBOR decoding target for
// subscribe stream frames. Mirrors the server's subscribeFrame wire
// format.
type serviceSubscribeFrame struct {
	Type     string               `cbor:"type"`
	TicketID string               `cbor:"ticket_id,omitempty"`
	Content  ticket.TicketContent `cbor:"content,omitempty"`
	Stats    *ticketindex.Stats   `cbor:"stats,omitempty"`
	Message  string               `cbor:"message,omitempty"`
}

// Backoff parameters for reconnection after stream disconnects.
const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
)

// NewServiceSource creates a ServiceSource that connects to the ticket
// service at socketPath, subscribes to the given room, and populates
// a local index from the stream. The background goroutine starts
// immediately; call [Close] to shut it down.
func NewServiceSource(socketPath string, tokenBytes []byte, roomID string, logger *slog.Logger) *ServiceSource {
	source := &ServiceSource{
		IndexSource: IndexSource{
			index: ticketindex.NewIndex(),
		},
		socketPath: socketPath,
		roomID:     roomID,
		logger:     logger,
	}
	source.token.Store(tokenBytes)
	source.loadingState.Store("connecting")

	ctx, cancel := context.WithCancel(context.Background())
	source.cancel = cancel
	go source.streamLoop(ctx)

	return source
}

// LoadingState returns the current loading phase of the subscribe
// stream. The TUI uses this to display appropriate loading indicators:
//
//   - "connecting": not yet connected to the service
//   - "loading": connected, receiving initial snapshot
//   - "open_complete": open tickets loaded, Ready/Blocked tabs usable
//   - "caught_up": full snapshot received, live events flowing
func (source *ServiceSource) LoadingState() string {
	return source.loadingState.Load().(string)
}

// SetToken stores a new service token for use on the next
// reconnection. Does not affect the current connection.
func (source *ServiceSource) SetToken(tokenBytes []byte) {
	source.token.Store(tokenBytes)
}

// Close shuts down the background stream goroutine and releases
// resources. Safe to call multiple times.
func (source *ServiceSource) Close() {
	source.cancel()
}

// streamLoop manages the subscribe connection lifecycle with
// exponential backoff reconnection. Runs in a background goroutine
// until the context is cancelled.
func (source *ServiceSource) streamLoop(ctx context.Context) {
	backoff := initialBackoff
	for {
		source.loadingState.Store("connecting")
		err := source.runStream(ctx)
		if ctx.Err() != nil {
			return
		}
		source.logger.Warn("subscribe stream disconnected",
			"room_id", source.roomID,
			"error", err,
			"backoff", backoff,
		)

		// Clear the local index on disconnection. The next successful
		// connection delivers a complete snapshot that replaces all
		// previous state.
		source.clearIndex()

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// runStream establishes a single subscribe connection, sends the
// handshake, and processes frames until the connection ends or the
// context is cancelled. Returns the error that ended the stream.
func (source *ServiceSource) runStream(ctx context.Context) error {
	tokenBytes, _ := source.token.Load().([]byte)
	if len(tokenBytes) == 0 {
		return fmt.Errorf("no service token available")
	}

	conn, err := net.DialTimeout("unix", source.socketPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer conn.Close()

	// Close the connection when the context is cancelled. This
	// unblocks the decoder's Read call in processFrames.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	// Send subscribe request.
	request := map[string]any{
		"action": "subscribe",
		"token":  tokenBytes,
		"room":   source.roomID,
	}
	if err := codec.NewEncoder(conn).Encode(request); err != nil {
		return fmt.Errorf("sending subscribe request: %w", err)
	}

	source.loadingState.Store("loading")
	source.logger.Info("subscribe stream connected", "room_id", source.roomID)

	return source.processFrames(codec.NewDecoder(conn))
}

// processFrames reads CBOR frames from the decoder and updates the
// local index. Returns when the connection closes, an error frame is
// received, or a decode error occurs.
func (source *ServiceSource) processFrames(decoder *codec.Decoder) error {
	for {
		var frame serviceSubscribeFrame
		if err := decoder.Decode(&frame); err != nil {
			return fmt.Errorf("reading frame: %w", err)
		}

		switch frame.Type {
		case "put":
			source.Put(frame.TicketID, frame.Content)
		case "remove":
			source.Remove(frame.TicketID)
		case "open_complete":
			source.loadingState.Store("open_complete")
			source.logger.Info("subscribe stream open_complete",
				"room_id", source.roomID)
		case "caught_up":
			source.loadingState.Store("caught_up")
			source.logger.Info("subscribe stream caught_up",
				"room_id", source.roomID)
		case "heartbeat":
			// Connection liveness — no action needed.
		case "resync":
			source.clearIndex()
			source.loadingState.Store("loading")
			source.logger.Info("subscribe stream resync",
				"room_id", source.roomID)
		case "error":
			return fmt.Errorf("server error: %s", frame.Message)
		default:
			// Forward compatibility: ignore unknown frame types.
			source.logger.Debug("unknown subscribe frame type",
				"type", frame.Type, "room_id", source.roomID)
		}
	}
}

// clearIndex replaces the local index with a fresh empty one. Called
// on disconnection and resync to ensure the next snapshot starts from
// a clean slate.
func (source *ServiceSource) clearIndex() {
	source.mutex.Lock()
	source.index = ticketindex.NewIndex()
	source.mutex.Unlock()
}
