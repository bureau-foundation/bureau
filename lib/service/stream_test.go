// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// --- ServiceStream unit tests ---

// These tests exercise ServiceStream's Send/Recv/CloseWrite using raw
// socket pairs, without a SocketServer. This validates the stream
// wrapper in isolation from the socket server's auth and dispatch.

func TestServiceStreamSendRecv(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	clientStream := NewServiceStream(clientConn)
	serverStream := NewServiceStream(serverConn)

	type Message struct {
		Text     string `cbor:"text"`
		Sequence int    `cbor:"sequence"`
	}

	// net.Pipe is synchronous: writes block until the peer reads.
	// Send in a goroutine so the main goroutine can Recv.
	sendDone := make(chan error, 1)
	go func() {
		if err := clientStream.Send(Message{Text: "hello", Sequence: 1}); err != nil {
			sendDone <- err
			return
		}
		if err := clientStream.Send(Message{Text: "world", Sequence: 2}); err != nil {
			sendDone <- err
			return
		}
		sendDone <- nil
	}()

	var message1, message2 Message
	if err := serverStream.Recv(&message1); err != nil {
		t.Fatalf("Recv message1: %v", err)
	}
	if message1.Text != "hello" || message1.Sequence != 1 {
		t.Errorf("message1 = %+v, want {Text:hello Sequence:1}", message1)
	}

	if err := serverStream.Recv(&message2); err != nil {
		t.Fatalf("Recv message2: %v", err)
	}
	if message2.Text != "world" || message2.Sequence != 2 {
		t.Errorf("message2 = %+v, want {Text:world Sequence:2}", message2)
	}

	if err := <-sendDone; err != nil {
		t.Fatalf("Send error: %v", err)
	}
}

func TestServiceStreamBidirectional(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	clientStream := NewServiceStream(clientConn)
	serverStream := NewServiceStream(serverConn)

	type Request struct {
		Value int `cbor:"value"`
	}
	type Reply struct {
		Doubled int `cbor:"doubled"`
	}

	// Server goroutine: read requests, double the value, send back.
	serverDone := make(chan error, 1)
	go func() {
		for {
			var request Request
			if err := serverStream.Recv(&request); err != nil {
				// Client closed — expected after all exchanges.
				serverDone <- nil
				return
			}
			if err := serverStream.Send(Reply{Doubled: request.Value * 2}); err != nil {
				serverDone <- err
				return
			}
		}
	}()

	for i := 1; i <= 3; i++ {
		if err := clientStream.Send(Request{Value: i}); err != nil {
			t.Fatalf("Send request %d: %v", i, err)
		}
		var reply Reply
		if err := clientStream.Recv(&reply); err != nil {
			t.Fatalf("Recv reply %d: %v", i, err)
		}
		if reply.Doubled != i*2 {
			t.Errorf("reply %d: Doubled = %d, want %d", i, reply.Doubled, i*2)
		}
	}

	clientStream.Close()

	if err := <-serverDone; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func TestServiceStreamCloseWrite(t *testing.T) {
	// CloseWrite requires a real Unix socket — net.Pipe does not
	// support half-close. Create a Unix socket pair via listener.
	dir := t.TempDir()
	socketPath := dir + "/test.sock"

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()

	serverConnChannel := make(chan net.Conn, 1)
	go func() {
		conn, acceptError := listener.Accept()
		if acceptError != nil {
			return
		}
		serverConnChannel <- conn
	}()

	clientConn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer clientConn.Close()

	serverConn := <-serverConnChannel
	defer serverConn.Close()

	clientStream := NewServiceStream(clientConn)
	serverStream := NewServiceStream(serverConn)

	// Client sends a message then half-closes the write side.
	if err := clientStream.Send(map[string]string{"message": "last"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := clientStream.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	// Server receives the message.
	var message map[string]string
	if err := serverStream.Recv(&message); err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if message["message"] != "last" {
		t.Errorf("message = %v, want {message: last}", message)
	}

	// Server's next Recv should return EOF from the half-close. The
	// CBOR decoder returns io.EOF when it encounters EOF at a value
	// boundary (no partial data).
	var extra map[string]string
	recvError := serverStream.Recv(&extra)
	if recvError == nil {
		t.Fatal("expected error after CloseWrite, got nil")
	}
	if !errors.Is(recvError, io.EOF) {
		t.Errorf("expected io.EOF after CloseWrite, got: %v", recvError)
	}

	// The server can still send back — only the client's write side
	// is closed. The read side remains open.
	if err := serverStream.Send(map[string]string{"acknowledgment": "received"}); err != nil {
		t.Fatalf("server Send after client CloseWrite: %v", err)
	}

	var acknowledgment map[string]string
	if err := clientStream.Recv(&acknowledgment); err != nil {
		t.Fatalf("client Recv after own CloseWrite: %v", err)
	}
	if acknowledgment["acknowledgment"] != "received" {
		t.Errorf("acknowledgment = %v, want {acknowledgment: received}", acknowledgment)
	}
}

func TestServiceStreamSendAck(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	serverStream := NewServiceStream(serverConn)

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- serverStream.SendAck()
	}()

	// Client reads the ack as a Response.
	var response Response
	if err := codec.NewDecoder(clientConn).Decode(&response); err != nil {
		t.Fatalf("decoding ack: %v", err)
	}
	if !response.OK {
		t.Errorf("ack OK = false, want true")
	}
	if response.Error != "" {
		t.Errorf("ack Error = %q, want empty", response.Error)
	}

	if err := <-sendDone; err != nil {
		t.Fatalf("SendAck error: %v", err)
	}
}

func TestServiceStreamSendError(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	serverStream := NewServiceStream(serverConn)

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- serverStream.SendError("quota exceeded")
	}()

	var response Response
	if err := codec.NewDecoder(clientConn).Decode(&response); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if response.OK {
		t.Error("error response OK = true, want false")
	}
	if response.Error != "quota exceeded" {
		t.Errorf("error response Error = %q, want 'quota exceeded'", response.Error)
	}

	if err := <-sendDone; err != nil {
		t.Fatalf("SendError error: %v", err)
	}
}

func TestServiceStreamConnReturnsUnderlyingConnection(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	stream := NewServiceStream(clientConn)
	if stream.Conn() != clientConn {
		t.Error("Conn() did not return the underlying connection")
	}
}

// --- OpenStream integration tests ---

// These tests exercise the full OpenStream flow: client handshake with
// a SocketServer using HandleAuthStream and the standard SendAck/
// SendError protocol.

func TestOpenStreamSuccessfulHandshake(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, privateKey := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	// Handler accepts the stream and sends one event.
	server.HandleAuthStream("subscribe", func(ctx context.Context, token *servicetoken.Token, raw []byte, conn net.Conn) {
		stream := NewServiceStream(conn)
		if err := stream.SendAck(); err != nil {
			return
		}
		stream.Send(map[string]any{
			"event":   "hello",
			"subject": token.Subject.String(),
		})
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var serverGroup sync.WaitGroup
	serverGroup.Add(1)
	go func() {
		defer serverGroup.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	tokenBytes := mintTestToken(t, privateKey, "agent/viewer")
	client := NewServiceClientFromToken(socketPath, tokenBytes)

	stream, err := client.OpenStream(ctx, "subscribe", nil)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close()

	var frame map[string]any
	if err := stream.Recv(&frame); err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if frame["event"] != "hello" {
		t.Errorf("event = %v, want hello", frame["event"])
	}
	wantSubject := "@bureau/fleet/test/agent/viewer:test.local"
	if frame["subject"] != wantSubject {
		t.Errorf("subject = %v, want %s", frame["subject"], wantSubject)
	}

	cancel()
	serverGroup.Wait()
}

func TestOpenStreamRejectedHandshake(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, privateKey := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	// Handler rejects the stream with an application-level error.
	server.HandleAuthStream("subscribe", func(ctx context.Context, token *servicetoken.Token, raw []byte, conn net.Conn) {
		stream := NewServiceStream(conn)
		stream.SendError("quota exceeded for this account")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var serverGroup sync.WaitGroup
	serverGroup.Add(1)
	go func() {
		defer serverGroup.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	tokenBytes := mintTestToken(t, privateKey, "agent/viewer")
	client := NewServiceClientFromToken(socketPath, tokenBytes)

	_, err := client.OpenStream(ctx, "subscribe", nil)
	if err == nil {
		t.Fatal("expected error from rejected stream")
	}

	var serviceError *ServiceError
	if !errors.As(err, &serviceError) {
		t.Fatalf("expected *ServiceError, got %T: %v", err, err)
	}
	if serviceError.Action != "subscribe" {
		t.Errorf("action = %q, want subscribe", serviceError.Action)
	}
	if serviceError.Message != "quota exceeded for this account" {
		t.Errorf("message = %q, want 'quota exceeded for this account'", serviceError.Message)
	}

	cancel()
	serverGroup.Wait()
}

func TestOpenStreamBidirectional(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, privateKey := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	type Request struct {
		Value int `cbor:"value"`
	}
	type Reply struct {
		Doubled int `cbor:"doubled"`
	}

	// Server handler: ack the stream, then echo doubled values until
	// the client disconnects.
	server.HandleAuthStream("compute", func(ctx context.Context, token *servicetoken.Token, raw []byte, conn net.Conn) {
		stream := NewServiceStream(conn)
		if err := stream.SendAck(); err != nil {
			return
		}
		for {
			var request Request
			if err := stream.Recv(&request); err != nil {
				return
			}
			if err := stream.Send(Reply{Doubled: request.Value * 2}); err != nil {
				return
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var serverGroup sync.WaitGroup
	serverGroup.Add(1)
	go func() {
		defer serverGroup.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	tokenBytes := mintTestToken(t, privateKey, "agent/compute")
	client := NewServiceClientFromToken(socketPath, tokenBytes)

	stream, err := client.OpenStream(ctx, "compute", nil)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	for i := 1; i <= 5; i++ {
		if err := stream.Send(Request{Value: i}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
		var reply Reply
		if err := stream.Recv(&reply); err != nil {
			t.Fatalf("Recv %d: %v", i, err)
		}
		if reply.Doubled != i*2 {
			t.Errorf("reply %d: Doubled = %d, want %d", i, reply.Doubled, i*2)
		}
	}

	// Close the client stream before cancelling the server. The
	// handler loops in Recv, which doesn't respect context
	// cancellation — it blocks on the underlying connection. Closing
	// the stream breaks the connection, causing the handler's Recv
	// to return an error and exit its loop. Without this, cancel +
	// Wait deadlocks: Wait blocks on the handler, which blocks on
	// Recv, which waits for data from the client.
	stream.Close()

	cancel()
	serverGroup.Wait()
}

func TestOpenStreamAuthFailure(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, _ := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	server.HandleAuthStream("subscribe", func(ctx context.Context, token *servicetoken.Token, raw []byte, conn net.Conn) {
		t.Error("handler should not be called without a valid token")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var serverGroup sync.WaitGroup
	serverGroup.Add(1)
	go func() {
		defer serverGroup.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	// Unauthenticated client — no token. The server rejects via the
	// standard auth path (writeError with a Response{OK: false}),
	// which OpenStream reads as a rejected handshake.
	client := NewServiceClientFromToken(socketPath, nil)

	_, err := client.OpenStream(ctx, "subscribe", nil)
	if err == nil {
		t.Fatal("expected error for unauthenticated OpenStream")
	}

	var serviceError *ServiceError
	if !errors.As(err, &serviceError) {
		t.Fatalf("expected *ServiceError, got %T: %v", err, err)
	}
	if serviceError.Action != "subscribe" {
		t.Errorf("action = %q, want subscribe", serviceError.Action)
	}

	cancel()
	serverGroup.Wait()
}

func TestOpenStreamConnectionRefused(t *testing.T) {
	client := NewServiceClientFromToken("/tmp/nonexistent-bureau-stream-test.sock", nil)

	_, err := client.OpenStream(context.Background(), "subscribe", nil)
	if err == nil {
		t.Fatal("expected error for connection refused")
	}

	// Connection failure should NOT be a ServiceError — it's a
	// transport-level error.
	var serviceError *ServiceError
	if errors.As(err, &serviceError) {
		t.Fatal("connection failure should not be *ServiceError")
	}
}

func TestOpenStreamWithFields(t *testing.T) {
	socketPath := testSocketPath(t)
	authConfig, privateKey := testAuthConfig(t)
	server := NewSocketServer(socketPath, testLogger(), authConfig)

	// Handler reads extra fields from the initial handshake request,
	// then echoes them back over the stream.
	server.HandleAuthStream("tail", func(ctx context.Context, token *servicetoken.Token, raw []byte, conn net.Conn) {
		var request struct {
			Filter string `cbor:"filter"`
		}
		if err := codec.Unmarshal(raw, &request); err != nil {
			stream := NewServiceStream(conn)
			stream.SendError("invalid request")
			return
		}

		stream := NewServiceStream(conn)
		if err := stream.SendAck(); err != nil {
			return
		}
		stream.Send(map[string]string{"filter": request.Filter})
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var serverGroup sync.WaitGroup
	serverGroup.Add(1)
	go func() {
		defer serverGroup.Done()
		server.Serve(ctx)
	}()
	waitForSocket(t, socketPath)

	tokenBytes := mintTestToken(t, privateKey, "agent/observer")
	client := NewServiceClientFromToken(socketPath, tokenBytes)

	stream, err := client.OpenStream(ctx, "tail", map[string]any{
		"filter": "bureau/fleet/*/agent/*",
	})
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close()

	var frame map[string]string
	if err := stream.Recv(&frame); err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if frame["filter"] != "bureau/fleet/*/agent/*" {
		t.Errorf("filter = %q, want 'bureau/fleet/*/agent/*'", frame["filter"])
	}

	cancel()
	serverGroup.Wait()
}
