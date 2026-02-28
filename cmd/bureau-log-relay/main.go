// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/service"
)

func main() {
	os.Exit(run())
}

// relayConfig holds parsed command-line options for the relay.
type relayConfig struct {
	command      []string
	exitCodeFile string

	// Capture mode fields. When relaySocket is non-empty, the relay
	// allocates a PTY pair, tees output to both stdout and the
	// telemetry relay, and passes stdin through to the child.
	relaySocket string
	tokenPath   string
	fleet       ref.Fleet
	machine     ref.Machine
	source      ref.Entity
	sessionID   string
}

// captureMode returns true if the relay should operate in capture mode
// (PTY interposition with telemetry output).
func (c *relayConfig) captureMode() bool {
	return c.relaySocket != ""
}

// flushSizeThreshold is the buffer size that triggers an immediate flush.
// 64 KB keeps individual CBOR messages and downstream CAS artifacts at a
// reasonable granularity for streaming and storage.
const flushSizeThreshold = 64 * 1024

// flushInterval is how often the relay flushes buffered output to the
// telemetry relay when the size threshold has not been reached. One
// second provides low-latency tailing for slow-output processes.
const flushInterval = 1 * time.Second

// readBufferSize is the size of the buffer used for reading from the
// PTY master. 32 KB balances syscall overhead against latency.
const readBufferSize = 32 * 1024

// run parses arguments and dispatches to the appropriate mode.
func run() int {
	config, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "bureau-log-relay: %v\n", err)
		return 1
	}

	if config.captureMode() {
		return runCapture(config)
	}
	return runPassthrough(config)
}

// runPassthrough runs the child with inherited stdin/stdout/stderr (no
// PTY interposition). This is the original behavior: the relay holds
// the outer PTY fds open until the child exits, fixing the tmux 3.4+
// race between PTY EOF and SIGCHLD.
func runPassthrough(config relayConfig) int {
	child := exec.Command(config.command[0], config.command[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr

	if err := child.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "bureau-log-relay: starting child: %v\n", err)
		return 126
	}

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	go forwardSignals(signals, child.Process)

	exitCode := waitForChild(child)
	writeExitCodeIfConfigured(config.exitCodeFile, exitCode)
	return exitCode
}

// runCapture runs the child behind a PTY pair, teeing output to both
// stdout (the tmux PTY) and the telemetry relay as CBOR output delta
// messages. Stdin from tmux is passed through to the child. Window
// resize signals (SIGWINCH) are propagated to the inner PTY.
func runCapture(config relayConfig) int {
	// Connect to the telemetry relay. If the token file is unreadable,
	// fail early — the launcher should have provisioned it.
	client, err := service.NewServiceClient(config.relaySocket, config.tokenPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bureau-log-relay: connecting to telemetry relay: %v\n", err)
		return 1
	}

	// Allocate the inner PTY pair.
	master, slavePath, err := openPTY()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bureau-log-relay: allocating PTY: %v\n", err)
		return 1
	}

	slave, err := os.OpenFile(slavePath, os.O_RDWR, 0)
	if err != nil {
		master.Close()
		fmt.Fprintf(os.Stderr, "bureau-log-relay: opening PTY slave %s: %v\n", slavePath, err)
		return 1
	}

	// Set the initial window size on the inner PTY to match the outer
	// terminal (tmux pane). Without this, the child starts with a 0x0
	// terminal and TUI programs misbehave.
	if winsize, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ); err == nil {
		_ = unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, winsize)
	}

	// Start the child on the inner PTY slave.
	child := exec.Command(config.command[0], config.command[1:]...)
	child.Stdin = slave
	child.Stdout = slave
	child.Stderr = slave
	child.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0, // fd 0 in child = slave PTY
	}

	if err := child.Start(); err != nil {
		slave.Close()
		master.Close()
		fmt.Fprintf(os.Stderr, "bureau-log-relay: starting child: %v\n", err)
		return 126
	}
	// Close slave in parent — the child has its own copy via fd 0/1/2.
	slave.Close()

	// Set up signal forwarding. SIGWINCH propagates terminal resizes
	// to the inner PTY. Other signals are forwarded to the child.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGWINCH)
	go handleSignals(signals, child.Process, master)

	// Set up the output buffer and flush machinery.
	buffer := &outputBuffer{}
	identity := submitIdentity{
		fleet:     config.fleet,
		machine:   config.machine,
		source:    config.source,
		sessionID: config.sessionID,
	}

	// Start the periodic flush goroutine. It sends buffered output to
	// the telemetry relay on a 1-second timer or when the tee loop
	// signals that the buffer has reached the size threshold.
	flushDone := make(chan struct{})
	flushSignal := make(chan struct{}, 1)
	var flushWait sync.WaitGroup
	flushWait.Add(1)
	go func() {
		defer flushWait.Done()
		flushLoop(buffer, client, identity, flushDone, flushSignal)
	}()

	// Start the stdin passthrough goroutine. Reads from os.Stdin
	// (tmux input) and writes to the PTY master (child input).
	go func() {
		_, _ = io.Copy(master, os.Stdin)
	}()

	// Tee output from the PTY master to both stdout and the buffer.
	// This runs in the current goroutine and blocks until the PTY
	// master returns EOF (child exited and all slave fds closed).
	teeOutput(master, os.Stdout, buffer, flushSignal)
	master.Close()

	// Wait for the child to exit and collect the exit code.
	exitCode := waitForChild(child)

	// Flush any remaining buffered output before exiting.
	close(flushDone)
	flushWait.Wait()
	flushRemaining(buffer, client, identity)

	writeExitCodeIfConfigured(config.exitCodeFile, exitCode)
	return exitCode
}

// submitIdentity holds the identity fields for constructing
// SubmitRequest envelopes.
type submitIdentity struct {
	fleet     ref.Fleet
	machine   ref.Machine
	source    ref.Entity
	sessionID string
}

// outputBuffer accumulates output bytes and tracks the sequence number
// for output delta messages. Thread-safe via mutex.
type outputBuffer struct {
	mu       sync.Mutex
	data     []byte
	sequence uint64
}

// append adds bytes to the buffer. Returns true if the buffer has
// reached the size threshold and should be flushed.
func (b *outputBuffer) append(data []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, data...)
	return len(b.data) >= flushSizeThreshold
}

// swap returns the current buffer contents and resets the buffer.
// Returns nil if the buffer is empty. Increments the sequence number
// for each non-empty swap.
func (b *outputBuffer) swap() ([]byte, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.data) == 0 {
		return nil, 0
	}
	data := b.data
	sequence := b.sequence
	b.sequence++
	b.data = nil
	return data, sequence
}

// teeOutput reads from the PTY master and writes each chunk to both
// stdout (the tmux PTY) and the output buffer. When the buffer
// crosses the size threshold, it sends a non-blocking signal on
// flushSignal so the flush goroutine can drain the buffer promptly
// rather than waiting for the next timer tick.
func teeOutput(master *os.File, stdout *os.File, buffer *outputBuffer, flushSignal chan<- struct{}) {
	readBuffer := make([]byte, readBufferSize)
	for {
		count, err := master.Read(readBuffer)
		if count > 0 {
			// Write to stdout first — visible output takes priority
			// over telemetry. Short writes to stdout are not retried;
			// if the tmux pane is gone, we still capture the output.
			_, _ = stdout.Write(readBuffer[:count])
			if buffer.append(readBuffer[:count]) {
				// Buffer crossed the size threshold. Signal the
				// flush goroutine. Non-blocking: if a signal is
				// already pending, the flush loop will drain it.
				select {
				case flushSignal <- struct{}{}:
				default:
				}
			}
		}
		if err != nil {
			// EOF is normal when the child exits and all slave fds close.
			return
		}
	}
}

// flushLoop runs a periodic timer that flushes buffered output to the
// telemetry relay. Also drains immediately when the tee goroutine
// signals that the buffer has crossed the size threshold. Stops when
// the done channel is closed.
func flushLoop(buffer *outputBuffer, client *service.ServiceClient, identity submitIdentity, done <-chan struct{}, flushSignal <-chan struct{}) {
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			flushBuffer(buffer, client, identity)
		case <-flushSignal:
			flushBuffer(buffer, client, identity)
		}
	}
}

// flushRemaining flushes any output remaining in the buffer after the
// child has exited. Called from the main goroutine after the flush
// loop has stopped.
func flushRemaining(buffer *outputBuffer, client *service.ServiceClient, identity submitIdentity) {
	flushBuffer(buffer, client, identity)
}

// flushBuffer swaps the buffer and sends the contents to the telemetry
// relay as an OutputDelta message. Errors are logged but not fatal —
// lost telemetry is preferable to blocking the relay or the child.
func flushBuffer(buffer *outputBuffer, client *service.ServiceClient, identity submitIdentity) {
	data, sequence := buffer.swap()
	if data == nil {
		return
	}

	delta := telemetry.OutputDelta{
		SessionID: identity.sessionID,
		Sequence:  sequence,
		Stream:    telemetry.OutputStreamCombined,
		Timestamp: time.Now().UnixNano(),
		Data:      data,
	}

	request := telemetry.SubmitRequest{
		Fleet:        identity.fleet,
		Machine:      identity.machine,
		Source:       identity.source,
		OutputDeltas: []telemetry.OutputDelta{delta},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Call(ctx, "submit", request, nil); err != nil {
		fmt.Fprintf(os.Stderr, "bureau-log-relay: telemetry flush failed (sequence %d, %d bytes): %v\n",
			sequence, len(data), err)
	}
}

// handleSignals forwards signals to the child process and handles
// SIGWINCH by propagating the outer terminal size to the inner PTY.
func handleSignals(signals <-chan os.Signal, process *os.Process, ptyMaster *os.File) {
	masterFd := int(ptyMaster.Fd())
	outerFd := int(os.Stdout.Fd())

	for sig := range signals {
		if sig == syscall.SIGWINCH {
			// Read the current outer terminal size and apply it to
			// the inner PTY. This propagates SIGWINCH to the child's
			// process group on the slave side.
			winsize, err := unix.IoctlGetWinsize(outerFd, unix.TIOCGWINSZ)
			if err == nil {
				_ = unix.IoctlSetWinsize(masterFd, unix.TIOCSWINSZ, winsize)
			}
			continue
		}
		if sysSig, ok := sig.(syscall.Signal); ok {
			_ = process.Signal(sysSig)
		}
	}
}

// forwardSignals reads signals from the channel and sends them to the
// child process. Stops when the channel is closed or the process is nil.
// Errors are ignored: the child may have already exited, in which case
// the signal delivery fails harmlessly.
func forwardSignals(signals <-chan os.Signal, process *os.Process) {
	for sig := range signals {
		if sysSig, ok := sig.(syscall.Signal); ok {
			_ = process.Signal(sysSig)
		}
	}
}

// waitForChild waits for the child process to exit and returns its
// exit code. Returns 1 for non-exit wait failures.
func waitForChild(child *exec.Cmd) int {
	if err := child.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "bureau-log-relay: waiting for child: %v\n", err)
		return 1
	}
	return 0
}

// writeExitCodeIfConfigured writes the exit code file if configured.
func writeExitCodeIfConfigured(exitCodeFile string, exitCode int) {
	if exitCodeFile != "" {
		if writeErr := writeExitCode(exitCodeFile, exitCode); writeErr != nil {
			fmt.Fprintf(os.Stderr, "bureau-log-relay: %v\n", writeErr)
		}
	}
}

// writeExitCode writes an exit code to the specified path using atomic
// rename. The temp file is written first, then renamed into place so
// that the inotify IN_MOVED_TO event fires only when the content is
// complete.
func writeExitCode(path string, exitCode int) error {
	content := []byte(strconv.Itoa(exitCode) + "\n")

	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, content, 0644); err != nil {
		return fmt.Errorf("writing exit code temp file %s: %w", tempPath, err)
	}

	if err := os.Rename(tempPath, path); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("renaming exit code file %s -> %s: %w", tempPath, path, err)
	}

	return nil
}

// openPTY allocates a PTY master/slave pair using the Linux devpts
// interface. Returns the master as an *os.File and the filesystem
// path to the slave device.
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

// parseArgs extracts relay configuration from the argument list. The relay
// accepts arguments in the form:
//
//	bureau-log-relay [flags] [--] <command> [args...]
//
// Flags must appear before the "--" separator (or before the command if
// no separator is used). The optional "--" separator is consumed and not
// passed to the child. At least one argument (the command) must follow.
//
// Capture mode flags (all required together):
//
//	--relay=<socket-path>          telemetry relay Unix socket
//	--token=<token-path>           telemetry relay service token file
//	--fleet=<fleet-ref>            fleet identity (MarshalText format)
//	--machine=<machine-ref>        machine identity (MarshalText format)
//	--source=<entity-ref>          principal entity (MarshalText format)
//	--session-id=<id>              session identifier
func parseArgs(args []string) (relayConfig, error) {
	if len(args) == 0 {
		return relayConfig{}, fmt.Errorf("usage: bureau-log-relay [flags] [--] <command> [args...]\n\n" +
			"This binary is spawned by the launcher. It is not intended for direct use.")
	}

	var config relayConfig
	var fleetText, machineText, sourceText string

	type flagBinding struct {
		prefix string
		target *string
	}
	flags := []flagBinding{
		{"--exit-code-file=", &config.exitCodeFile},
		{"--relay=", &config.relaySocket},
		{"--token=", &config.tokenPath},
		{"--fleet=", &fleetText},
		{"--machine=", &machineText},
		{"--source=", &sourceText},
		{"--session-id=", &config.sessionID},
	}

	for len(args) > 0 {
		if args[0] == "--" {
			args = args[1:]
			break
		}

		matched := false
		for _, binding := range flags {
			remaining, consumed, err := parseFlag(args, binding.prefix, binding.target)
			if err != nil {
				return relayConfig{}, err
			}
			if consumed {
				args = remaining
				matched = true
				break
			}
		}
		if !matched {
			// Not a known flag and not "--" — start of the command.
			break
		}
	}

	if len(args) == 0 {
		return relayConfig{}, fmt.Errorf("no command specified after flags")
	}
	config.command = args

	// Validate capture mode: either all capture flags are set or none.
	captureFlags := []string{config.relaySocket, config.tokenPath, fleetText, machineText, sourceText, config.sessionID}
	captureSet := 0
	for _, flag := range captureFlags {
		if flag != "" {
			captureSet++
		}
	}
	if captureSet > 0 && captureSet < len(captureFlags) {
		return relayConfig{}, fmt.Errorf("capture mode requires all of --relay, --token, --fleet, --machine, --source, --session-id")
	}

	// Parse identity refs when in capture mode.
	if config.relaySocket != "" {
		if err := config.fleet.UnmarshalText([]byte(fleetText)); err != nil {
			return relayConfig{}, fmt.Errorf("parsing --fleet %q: %w", fleetText, err)
		}
		if err := config.machine.UnmarshalText([]byte(machineText)); err != nil {
			return relayConfig{}, fmt.Errorf("parsing --machine %q: %w", machineText, err)
		}
		if err := config.source.UnmarshalText([]byte(sourceText)); err != nil {
			return relayConfig{}, fmt.Errorf("parsing --source %q: %w", sourceText, err)
		}
	}

	return config, nil
}

// parseFlag checks if the first argument starts with the given prefix.
// If so, extracts the value, validates it is non-empty, sets the target,
// and returns the advanced argument slice. Returns an error if the flag
// is present but the value is empty (e.g. "--relay=").
func parseFlag(args []string, prefix string, target *string) (remaining []string, consumed bool, err error) {
	if len(args) == 0 || !strings.HasPrefix(args[0], prefix) {
		return args, false, nil
	}
	value := strings.TrimPrefix(args[0], prefix)
	if value == "" {
		flagName := strings.TrimSuffix(prefix, "=")
		return nil, false, fmt.Errorf("%s requires a non-empty value", flagName)
	}
	*target = value
	return args[1:], true, nil
}
