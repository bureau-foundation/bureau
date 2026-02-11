// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/bureau-foundation/bureau/lib/tmux"
	"golang.org/x/sys/unix"
)

// Relay runs the remote side of an observation session. It attaches to a
// tmux session and relays terminal I/O between the tmux PTY and the
// provided connection (typically a unix socket inherited from the daemon).
//
// The relay:
//   - Sends a metadata message describing the session
//   - Sends the ring buffer contents as a history message
//   - Copies PTY output → connection as data messages
//   - Copies connection data messages → PTY input
//   - Handles resize messages by setting the PTY window size
//   - Feeds all PTY output through the ring buffer for future observers
//
// sessionName is the tmux session to attach to (e.g., "bureau/iree/amdgpu/pm").
//
// readOnly controls whether input from the connection is relayed to the
// PTY. When true, the relay attaches with tmux attach -r and does not
// write to the PTY master.
//
// Relay blocks until the tmux session ends, the connection closes, or
// an unrecoverable error occurs. Returns nil on clean shutdown.
func Relay(connection io.ReadWriteCloser, server *tmux.Server, sessionName string, readOnly bool) error {
	// Allocate a PTY pair for tmux to attach to.
	master, slavePath, err := openPTY()
	if err != nil {
		return fmt.Errorf("allocate PTY: %w", err)
	}

	slave, err := os.OpenFile(slavePath, os.O_RDWR, 0)
	if err != nil {
		master.Close()
		return fmt.Errorf("open PTY slave %s: %w", slavePath, err)
	}

	// Build tmux attach command. Server.Command injects -S automatically.
	attachArgs := []string{"attach-session"}
	if readOnly {
		attachArgs = append(attachArgs, "-r")
	}
	attachArgs = append(attachArgs, "-t", sessionName)

	cmd := server.Command(attachArgs...)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0, // fd 0 in child = slave PTY
	}

	if err := cmd.Start(); err != nil {
		slave.Close()
		master.Close()
		return fmt.Errorf("start tmux attach: %w", err)
	}
	// Close slave in parent — the child has its own copy via fd 0/1/2.
	slave.Close()

	ring := NewRingBuffer(DefaultRingBufferSize)

	// Query tmux for session metadata and send it to the observer.
	metadata, err := queryMetadata(server, sessionName)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		master.Close()
		return fmt.Errorf("query tmux metadata: %w", err)
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		master.Close()
		return fmt.Errorf("marshal metadata: %w", err)
	}
	if err := WriteMessage(connection, NewMetadataMessage(metadataJSON)); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		master.Close()
		return fmt.Errorf("send metadata: %w", err)
	}

	// Send current ring buffer contents as history. Empty on initial connect
	// since the relay just started, but the protocol requires the message.
	historyData := ring.ReadFrom(0)
	if historyData == nil {
		historyData = []byte{}
	}
	if err := WriteMessage(connection, NewHistoryMessage(historyData)); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		master.Close()
		return fmt.Errorf("send history: %w", err)
	}

	// done is closed when any activity finishes, triggering cleanup.
	done := make(chan struct{})
	var doneOnce sync.Once
	triggerDone := func() { doneOnce.Do(func() { close(done) }) }

	var goroutineWait sync.WaitGroup

	// Goroutine: PTY master output → connection as data messages.
	// Also feeds output through the ring buffer for history.
	goroutineWait.Add(1)
	go func() {
		defer goroutineWait.Done()
		defer triggerDone()
		readBuffer := make([]byte, 4096)
		for {
			bytesRead, readErr := master.Read(readBuffer)
			if bytesRead > 0 {
				ring.Write(readBuffer[:bytesRead])
				if writeErr := WriteMessage(connection, NewDataMessage(readBuffer[:bytesRead])); writeErr != nil {
					// Connection write failure means the observer disconnected.
					// This is a normal shutdown trigger, not an error.
					return
				}
			}
			if readErr != nil {
				// EIO is the normal signal that the PTY slave closed
				// (tmux exited). Any other read error also ends the relay.
				return
			}
		}
	}()

	// Goroutine: connection messages → PTY master input or ioctl.
	goroutineWait.Add(1)
	go func() {
		defer goroutineWait.Done()
		defer triggerDone()
		for {
			message, readErr := ReadMessage(connection)
			if readErr != nil {
				// Connection read failure means the observer disconnected
				// or the connection was closed during shutdown.
				return
			}
			switch message.Type {
			case MessageTypeData:
				if !readOnly && len(message.Payload) > 0 {
					if _, writeErr := master.Write(message.Payload); writeErr != nil {
						// Write failure means tmux exited and the slave closed.
						return
					}
				}
			case MessageTypeResize:
				columns, rows, parseErr := ParseResizePayload(message.Payload)
				if parseErr != nil {
					// Malformed resize message — drop it and continue
					// rather than killing the whole session.
					continue
				}
				// Resize failure usually means the PTY is gone (shutdown).
				if resizeErr := setWindowSize(int(master.Fd()), columns, rows); resizeErr != nil {
					return
				}
			}
		}
	}()

	// Goroutine: wait for tmux process to exit.
	tmuxExited := make(chan error, 1)
	go func() {
		tmuxExited <- cmd.Wait()
		triggerDone()
	}()

	// Block until something triggers shutdown.
	<-done

	// Close connection to unblock the connection reader goroutine.
	connection.Close()
	// Signal tmux to exit if it hasn't already.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	// Close master to unblock the PTY reader goroutine.
	master.Close()

	goroutineWait.Wait()

	tmuxErr := <-tmuxExited

	// After the handshake completes, the relay's job is to forward bytes
	// until the session ends. Any of tmux exit, connection close, or
	// SIGTERM is a normal end condition. The only abnormal case is tmux
	// exiting with an error code when the relay did not initiate shutdown
	// (i.e., the connection was still open and we didn't send SIGTERM).
	//
	// Since we always send SIGTERM during cleanup regardless of who
	// triggered shutdown, we treat tmux exit code 0 and SIGTERM-killed
	// as normal. tmux also exits with code 1 when its controlling PTY
	// closes (which we do during cleanup), so we accept that too.
	if tmuxErr != nil && !isNormalTmuxExit(tmuxErr) {
		return fmt.Errorf("tmux exited: %w", tmuxErr)
	}

	return nil
}

// openPTY allocates a PTY master/slave pair using the Linux devpts interface.
// Returns the master as an *os.File and the filesystem path to the slave.
func openPTY() (master *os.File, slavePath string, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/ptmx: %w", err)
	}

	fd := int(master.Fd())

	ptyNumber, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, "", fmt.Errorf("get PTY number (TIOCGPTN): %w", err)
	}

	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("unlock PTY slave (TIOCSPTLCK): %w", err)
	}

	slavePath = fmt.Sprintf("/dev/pts/%d", ptyNumber)
	return master, slavePath, nil
}

// setWindowSize sets the terminal dimensions on a PTY master fd using
// TIOCSWINSZ. This propagates SIGWINCH to the foreground process group
// attached to the slave side.
func setWindowSize(fd int, columns, rows uint16) error {
	winsize := &unix.Winsize{
		Col: columns,
		Row: rows,
	}
	return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, winsize)
}

// queryMetadata queries the tmux server for pane information about the
// given session and constructs a MetadataPayload.
func queryMetadata(server *tmux.Server, sessionName string) (MetadataPayload, error) {
	output, err := server.Run("list-panes",
		"-t", sessionName,
		"-F", "#{pane_index}\t#{pane_current_command}\t#{pane_width}\t#{pane_height}\t#{pane_active}")
	if err != nil {
		return MetadataPayload{}, fmt.Errorf("query session %q metadata: %w", sessionName, err)
	}

	var panes []PaneInfo
	for _, line := range splitLines(output) {
		fields := strings.Split(line, "\t")
		if len(fields) != 5 {
			return MetadataPayload{}, fmt.Errorf("unexpected list-panes output: %q (expected 5 tab-separated fields)", line)
		}
		index, err := strconv.Atoi(fields[0])
		if err != nil {
			return MetadataPayload{}, fmt.Errorf("parse pane index %q: %w", fields[0], err)
		}
		width, err := strconv.Atoi(fields[2])
		if err != nil {
			return MetadataPayload{}, fmt.Errorf("parse pane width %q: %w", fields[2], err)
		}
		height, err := strconv.Atoi(fields[3])
		if err != nil {
			return MetadataPayload{}, fmt.Errorf("parse pane height %q: %w", fields[3], err)
		}
		panes = append(panes, PaneInfo{
			Index:   index,
			Command: fields[1],
			Width:   width,
			Height:  height,
			Active:  fields[4] == "1",
		})
	}

	return MetadataPayload{
		Session: sessionName,
		Panes:   panes,
	}, nil
}

// isNormalTmuxExit returns true if the tmux exit represents normal
// termination. During relay shutdown, tmux can exit in several ways:
//   - Exit code 0: session ended normally
//   - Exit code 1: controlling PTY closed or session destroyed externally
//   - Killed by SIGTERM: relay sent SIGTERM during cleanup
func isNormalTmuxExit(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	if exitErr.ExitCode() == 0 || exitErr.ExitCode() == 1 {
		return true
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if ok && status.Signaled() && status.Signal() == syscall.SIGTERM {
		return true
	}
	return false
}
