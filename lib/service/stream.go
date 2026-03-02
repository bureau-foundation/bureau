// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"fmt"
	"net"

	"github.com/bureau-foundation/bureau/lib/codec"
)

// ServiceStream wraps a net.Conn for typed bidirectional CBOR
// messaging. Created by stream handlers (server side) via
// NewServiceStream, or by ServiceClient.OpenStream (client side).
//
// One goroutine may call Send while another calls Recv; however,
// concurrent Sends or concurrent Recvs are not safe. The typical
// pattern is one reader goroutine calling Recv in a loop and one
// writer goroutine calling Send.
//
// The stream does not manage the connection's lifecycle — the caller
// (or the socket server) is responsible for closing the underlying
// connection. For server-side handlers, the socket server closes the
// connection after the handler returns. For client-side streams
// obtained via OpenStream, the caller must call Close.
type ServiceStream struct {
	conn    net.Conn
	encoder *codec.Encoder
	decoder *codec.Decoder
}

// NewServiceStream wraps a connection for bidirectional CBOR
// streaming. Use this in AuthStreamFunc handlers to get typed
// Send/Recv instead of manually creating encoder/decoder pairs.
//
// The connection must already be established and authenticated. On
// the server side, the socket server handles authentication before
// calling the stream handler. On the client side, OpenStream handles
// the handshake before returning the stream.
func NewServiceStream(conn net.Conn) *ServiceStream {
	return &ServiceStream{
		conn:    conn,
		encoder: codec.NewEncoder(conn),
		decoder: codec.NewDecoder(conn),
	}
}

// Send encodes a value as CBOR and writes it to the stream. Returns
// an error if encoding fails or the connection is broken.
//
// For server-side handlers: use Send(Response{OK: true}) as the
// first message to acknowledge the stream to OpenStream clients.
// Use Send(Response{OK: false, Error: "..."}) to reject the stream.
func (stream *ServiceStream) Send(value any) error {
	if err := stream.encoder.Encode(value); err != nil {
		return fmt.Errorf("stream send: %w", err)
	}
	return nil
}

// Recv reads the next CBOR value from the stream into target. Blocks
// until a complete CBOR value arrives, the connection closes, or an
// error occurs.
//
// When the peer closes their write side (CloseWrite or connection
// close), Recv returns io.EOF. Use netutil.IsExpectedCloseError to
// distinguish clean shutdown from unexpected errors.
func (stream *ServiceStream) Recv(target any) error {
	if err := stream.decoder.Decode(target); err != nil {
		return fmt.Errorf("stream recv: %w", err)
	}
	return nil
}

// CloseWrite signals to the peer that no more messages will be sent
// from this side. The peer's next Recv will see EOF. The caller may
// still call Recv to read remaining messages from the peer.
//
// Only works on Unix connections. For other connection types, this
// is a no-op (the caller should Close instead).
func (stream *ServiceStream) CloseWrite() error {
	if unixConn, ok := stream.conn.(*net.UnixConn); ok {
		return unixConn.CloseWrite()
	}
	return nil
}

// Close closes the underlying connection entirely (both directions).
// After Close, both Send and Recv will return errors.
func (stream *ServiceStream) Close() error {
	return stream.conn.Close()
}

// Conn returns the underlying net.Conn. Use this for advanced
// operations like setting deadlines, context-driven shutdown (close
// the conn from another goroutine to unblock a blocking Recv), or
// accessing connection metadata.
func (stream *ServiceStream) Conn() net.Conn {
	return stream.conn
}

// SendAck sends a successful stream acknowledgment. This is the
// standard first message from a stream handler to signal that the
// stream is established and data will follow. OpenStream clients
// read this ack before returning the stream to the caller.
func (stream *ServiceStream) SendAck() error {
	return stream.Send(Response{OK: true})
}

// SendError sends a stream rejection. This is the standard first
// message from a stream handler when the request is valid but the
// handler cannot service it (e.g., invalid model alias, quota
// exceeded). OpenStream clients read this and return a ServiceError.
func (stream *ServiceStream) SendError(message string) error {
	return stream.Send(Response{OK: false, Error: message})
}
