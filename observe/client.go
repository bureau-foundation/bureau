// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"

	"golang.org/x/sys/unix"
)

// DefaultDaemonSocket is the path where the daemon listens for
// observation requests. Separate from the launcher IPC socket
// (/run/bureau/launcher.sock) to keep observation traffic off the
// privileged IPC channel.
const DefaultDaemonSocket = "/run/bureau/observe.sock"

// Session represents an active observation session from the client's
// perspective. It wraps the daemon connection and provides terminal
// I/O relay with status reporting.
type Session struct {
	// Metadata received from the remote side on connect.
	Metadata MetadataPayload

	// connection is the daemon socket carrying the observation protocol.
	// Writes go here directly (unbuffered).
	connection io.ReadWriteCloser

	// reader provides reads that may include bytes buffered by the JSON
	// decoder during the handshake. All reads after Connect must go
	// through this reader, not through connection directly.
	reader io.Reader

	// writeMutex serializes writes from the input goroutine and the
	// SIGWINCH goroutine in Run.
	writeMutex sync.Mutex
}

// Connect establishes an observation session through the local daemon.
// It sends the ObserveRequest as JSON, reads the ObserveResponse, and
// if successful, reads the initial metadata message.
//
// daemonSocket is the path to the daemon's observation unix socket
// (typically DefaultDaemonSocket).
//
// The returned Session is ready for Run(). The caller must close it
// when done.
func Connect(daemonSocket string, request ObserveRequest) (*Session, error) {
	conn, err := net.Dial("unix", daemonSocket)
	if err != nil {
		return nil, fmt.Errorf("dial daemon socket %s: %w", daemonSocket, err)
	}

	// Send the observation request as JSON + newline.
	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(request); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send observe request: %w", err)
	}

	// Use a buffered reader for all reads from the connection. The JSON
	// handshake response is newline-delimited; ReadBytes('\n') consumes
	// exactly the response line without reading into the binary protocol
	// data. All subsequent ReadMessage calls also go through this reader
	// so that any bytes buffered during the handshake aren't lost.
	reader := bufio.NewReader(conn)

	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read observe response: %w", err)
	}
	var response ObserveResponse
	if err := json.Unmarshal(responseLine, &response); err != nil {
		conn.Close()
		return nil, fmt.Errorf("unmarshal observe response: %w", err)
	}
	if !response.OK {
		conn.Close()
		return nil, fmt.Errorf("observation denied: %s", response.Error)
	}

	// After the JSON handshake, the connection switches to the binary
	// observation protocol. Read the metadata message.
	metadataMessage, err := ReadMessage(reader)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read metadata message: %w", err)
	}
	if metadataMessage.Type != MessageTypeMetadata {
		conn.Close()
		return nil, fmt.Errorf("expected metadata message (type 0x%02x), got type 0x%02x",
			MessageTypeMetadata, metadataMessage.Type)
	}
	var metadata MetadataPayload
	if err := json.Unmarshal(metadataMessage.Payload, &metadata); err != nil {
		conn.Close()
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}

	// Merge Bureau-level identity from the daemon handshake into the
	// metadata from the relay. The relay only knows about the tmux session
	// (pane list, dimensions); the daemon knows the principal-to-machine
	// mapping. Fill in any fields the relay left empty.
	if metadata.Principal == "" {
		metadata.Principal = request.Principal
	}
	if metadata.Machine == "" {
		metadata.Machine = response.Machine
	}

	return &Session{
		Metadata:   metadata,
		connection: conn,
		reader:     reader,
	}, nil
}

// Run relays terminal I/O between the observation session and the
// provided reader/writer (typically os.Stdin and os.Stdout). It handles:
//
//   - Writing history dump to output (for local tmux scrollback capture)
//   - Copying remote terminal output → output (stdout)
//   - Copying input (stdin) → remote terminal input
//   - Sending resize messages on SIGWINCH
//   - Printing status messages on connect
//
// Run blocks until the session ends, the reader is exhausted, or an
// error occurs. Returns nil on clean shutdown.
func (session *Session) Run(input io.Reader, output io.Writer) error {
	// Read and replay the history message to populate local scrollback.
	historyMessage, err := ReadMessage(session.reader)
	if err != nil {
		return fmt.Errorf("read history message: %w", err)
	}
	if historyMessage.Type != MessageTypeHistory {
		return fmt.Errorf("expected history message (type 0x%02x), got type 0x%02x",
			MessageTypeHistory, historyMessage.Type)
	}
	if len(historyMessage.Payload) > 0 {
		if _, err := output.Write(historyMessage.Payload); err != nil {
			return fmt.Errorf("write history to output: %w", err)
		}
	}

	// Print a connect status line.
	statusLine := fmt.Sprintf("Connected to %s on %s\r\n",
		session.Metadata.Principal, session.Metadata.Machine)
	if _, err := output.Write([]byte(statusLine)); err != nil {
		return fmt.Errorf("write status line: %w", err)
	}

	// done is closed when any goroutine finishes, triggering shutdown.
	done := make(chan struct{})
	var doneOnce sync.Once
	triggerDone := func() { doneOnce.Do(func() { close(done) }) }

	var goroutineWait sync.WaitGroup

	// Goroutine: remote data messages → output (stdout).
	goroutineWait.Add(1)
	go func() {
		defer goroutineWait.Done()
		defer triggerDone()
		for {
			message, readErr := ReadMessage(session.reader)
			if readErr != nil {
				return
			}
			if message.Type == MessageTypeData && len(message.Payload) > 0 {
				if _, writeErr := output.Write(message.Payload); writeErr != nil {
					return
				}
			}
		}
	}()

	// Goroutine: input (stdin) → data messages to remote.
	goroutineWait.Add(1)
	go func() {
		defer goroutineWait.Done()
		defer triggerDone()
		readBuffer := make([]byte, 4096)
		for {
			bytesRead, readErr := input.Read(readBuffer)
			if bytesRead > 0 {
				if writeErr := session.writeMessage(NewDataMessage(readBuffer[:bytesRead])); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	// Goroutine: SIGWINCH → resize messages.
	sigwinchChannel := make(chan os.Signal, 1)
	signal.Notify(sigwinchChannel, unix.SIGWINCH)
	goroutineWait.Add(1)
	go func() {
		defer goroutineWait.Done()
		defer signal.Stop(sigwinchChannel)
		for {
			select {
			case <-sigwinchChannel:
				columns, rows, sizeErr := getTerminalSize(output)
				if sizeErr != nil {
					continue
				}
				if writeErr := session.writeMessage(NewResizeMessage(columns, rows)); writeErr != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	// Block until any goroutine triggers shutdown.
	<-done

	// Close the connection to unblock all goroutines.
	session.connection.Close()

	goroutineWait.Wait()

	return nil
}

// writeMessage sends a protocol message over the connection, serializing
// concurrent writes from the input and SIGWINCH goroutines.
func (session *Session) writeMessage(message Message) error {
	session.writeMutex.Lock()
	defer session.writeMutex.Unlock()
	return WriteMessage(session.connection, message)
}

// Close terminates the observation session and releases resources.
func (session *Session) Close() error {
	if session.connection != nil {
		return session.connection.Close()
	}
	return nil
}

// getTerminalSize reads the current terminal dimensions from the writer's
// underlying file descriptor. Returns columns and rows. Returns an error
// if the writer is not backed by a file descriptor or the ioctl fails.
func getTerminalSize(output io.Writer) (columns, rows uint16, err error) {
	// Try to get a file descriptor from the output writer. This works
	// when output is *os.File (the common case with os.Stdout).
	type filer interface {
		Fd() uintptr
	}
	f, ok := output.(filer)
	if !ok {
		return 0, 0, fmt.Errorf("output does not expose a file descriptor")
	}
	winsize, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, fmt.Errorf("TIOCGWINSZ: %w", err)
	}
	return winsize.Col, winsize.Row, nil
}
