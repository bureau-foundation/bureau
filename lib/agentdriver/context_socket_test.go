// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agentdriver

import (
	"context"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/testutil"
)

func TestWrapperContextSocket(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	socketPath := socketDir + "/context.sock"

	expected := "ctx-abc12345"
	server := newWrapperContextSocketServer(socketPath, func() string {
		return expected
	}, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.Serve(ctx)

	waitForSocket(t, socketPath)

	client := service.NewServiceClientFromToken(socketPath, nil)
	var response currentContextResponse
	if err := client.Call(context.Background(), "current-context", nil, &response); err != nil {
		t.Fatalf("Call current-context: %v", err)
	}
	if response.ContextID != expected {
		t.Errorf("context_id = %q, want %q", response.ContextID, expected)
	}
}

func TestWrapperContextSocketEmpty(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	socketPath := socketDir + "/context.sock"

	server := newWrapperContextSocketServer(socketPath, func() string {
		return ""
	}, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.Serve(ctx)

	waitForSocket(t, socketPath)

	client := service.NewServiceClientFromToken(socketPath, nil)
	var response currentContextResponse
	if err := client.Call(context.Background(), "current-context", nil, &response); err != nil {
		t.Fatalf("Call current-context: %v", err)
	}
	if response.ContextID != "" {
		t.Errorf("context_id = %q, want empty", response.ContextID)
	}
}

func TestWrapperContextSocketConcurrent(t *testing.T) {
	socketDir := testutil.SocketDir(t)
	socketPath := socketDir + "/context.sock"

	// Shared mutable context ID, simulating the checkpoint tracker's
	// behavior. The atomic ensures the test is race-detector clean.
	var currentID atomic.Value
	currentID.Store("")

	server := newWrapperContextSocketServer(socketPath, func() string {
		return currentID.Load().(string)
	}, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.Serve(ctx)

	waitForSocket(t, socketPath)

	// Run concurrent readers while updating the context ID.
	var waitGroup sync.WaitGroup
	const readers = 8
	const iterations = 20
	for i := range readers {
		waitGroup.Add(1)
		go func(readerIndex int) {
			defer waitGroup.Done()
			client := service.NewServiceClientFromToken(socketPath, nil)
			for range iterations {
				var response currentContextResponse
				if err := client.Call(context.Background(), "current-context", nil, &response); err != nil {
					t.Errorf("reader %d: Call: %v", readerIndex, err)
					return
				}
				// The value should be either empty or a valid ctx-* ID.
				// We don't assert the exact value because the writer
				// is racing, but we verify no garbled reads.
				id := response.ContextID
				if id != "" && len(id) < 4 {
					t.Errorf("reader %d: garbled context_id: %q", readerIndex, id)
				}
			}
		}(i)
	}

	// Writer: update the context ID while readers are active.
	for i := range iterations {
		currentID.Store("ctx-" + string(rune('A'+i)))
		runtime.Gosched()
	}

	waitGroup.Wait()
}

// waitForSocket polls until the socket is accepting connections.
// Uses an actual connect attempt rather than os.Stat because the
// socket file exists after bind(2) but connections are only accepted
// after listen(2). Bounded by the test context timeout.
func waitForSocket(t *testing.T, socketPath string) {
	t.Helper()
	for {
		conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		if t.Context().Err() != nil {
			t.Fatalf("socket %s not accepting connections before test context expired", socketPath)
		}
		runtime.Gosched()
	}
}
