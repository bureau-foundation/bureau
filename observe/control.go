// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/tmux"
)

// DefaultDebounceInterval is the default time to wait after the last
// layout-relevant notification before emitting a LayoutChanged event.
// 500ms is fast enough to feel responsive but coalesces rapid changes
// like dragging a pane splitter.
const DefaultDebounceInterval = 500 * time.Millisecond

// LayoutChanged is emitted by ControlClient when the tmux session's
// layout has changed. The daemon reads the full layout on receiving
// this event.
type LayoutChanged struct {
	// SessionName is the tmux session name (e.g., "bureau/iree/amdgpu/pm").
	SessionName string

	// Timestamp is when the debounce timer fired.
	Timestamp time.Time
}

// ControlClient monitors a tmux session for layout changes via tmux
// control mode (tmux -C). It attaches a control mode client to the
// session, reads the notification stream, debounces layout-relevant
// events, and emits LayoutChanged events on a channel.
//
// Layout-relevant notifications:
//   - %layout-change: pane split, resize, or close within a window
//   - %window-add: new window linked to the session
//   - %window-close: window closed (linked to session at time of close)
//   - %window-renamed: window name changed
//   - %unlinked-window-close: window closed (not linked to session at time of close, e.g., killed from another window)
//
// All other notifications (%output, %session-changed, %pane-mode-changed, etc.)
// are ignored.
//
// Control mode clients do not participate in tmux window size negotiation,
// so attaching a ControlClient does not constrain the terminal dimensions
// of real clients.
type ControlClient struct {
	server           *tmux.Server
	sessionName      string
	debounceInterval time.Duration
	clock            clock.Clock
	events           chan LayoutChanged

	// cancel stops the control mode subprocess and all goroutines.
	cancel context.CancelFunc

	// ready is closed when the initial attach-session response
	// completes and the control mode client is fully connected.
	// Notifications are only delivered after this point.
	ready     chan struct{}
	readyOnce sync.Once

	// done is closed when the reader goroutine exits. Wait on this
	// after cancelling to ensure clean shutdown.
	done chan struct{}

	// notificationsProcessed counts layout-relevant notifications
	// that have been received and processed (debounce timer reset).
	notificationsProcessed uint64
	// scannerDone is set when the scanner goroutine exits (EOF or
	// context cancellation). WaitForNotifications checks this to
	// avoid blocking forever when the tmux server dies.
	scannerDone          bool
	notificationsMu      sync.Mutex
	notificationsChanged *sync.Cond
}

// ControlClientOption configures a ControlClient.
type ControlClientOption func(*ControlClient)

// WithDebounceInterval sets the debounce interval for layout change
// events. The default is DefaultDebounceInterval (500ms).
func WithDebounceInterval(interval time.Duration) ControlClientOption {
	return func(client *ControlClient) {
		client.debounceInterval = interval
	}
}

// WithClock sets the clock used for debounce timers. The default is
// clock.Real(). Tests inject clock.Fake() for deterministic control.
func WithClock(c clock.Clock) ControlClientOption {
	return func(client *ControlClient) {
		client.clock = c
	}
}

// NewControlClient creates and starts a control mode client that
// monitors the given tmux session for layout changes. The returned
// client emits events on the Events() channel.
//
// The client runs until ctx is cancelled or the tmux session ends.
// Call Stop() or cancel the context to shut down.
func NewControlClient(ctx context.Context, server *tmux.Server, sessionName string, options ...ControlClientOption) (*ControlClient, error) {
	clientContext, cancel := context.WithCancel(ctx)

	client := &ControlClient{
		server:           server,
		sessionName:      sessionName,
		debounceInterval: DefaultDebounceInterval,
		clock:            clock.Real(),
		events:           make(chan LayoutChanged, 16),
		cancel:           cancel,
		ready:            make(chan struct{}),
		done:             make(chan struct{}),
	}
	client.notificationsChanged = sync.NewCond(&client.notificationsMu)
	for _, option := range options {
		option(client)
	}

	if err := client.start(clientContext); err != nil {
		cancel()
		return nil, err
	}
	return client, nil
}

// Events returns the channel that receives layout change events.
// The channel is closed when the client stops.
func (client *ControlClient) Events() <-chan LayoutChanged {
	return client.events
}

// Ready returns a channel that closes when the control mode subprocess
// is attached and the notification reader is actively scanning. Callers
// can wait on this before triggering tmux operations that produce layout
// notifications.
func (client *ControlClient) Ready() <-chan struct{} {
	return client.ready
}

// Stop shuts down the control mode client. Blocks until the reader
// goroutine exits and all resources are released.
func (client *ControlClient) Stop() {
	client.cancel()
	<-client.done
}

// NotificationsProcessed returns the number of layout-relevant
// notifications that have been received and processed (debounce
// timer created or reset).
func (client *ControlClient) NotificationsProcessed() uint64 {
	client.notificationsMu.Lock()
	defer client.notificationsMu.Unlock()
	return client.notificationsProcessed
}

// WaitForNotifications blocks until at least n layout-relevant
// notifications have been processed, or the scanner goroutine exits.
// Returns true if n notifications were reached, false if the scanner
// exited first (tmux server died or context was cancelled).
func (client *ControlClient) WaitForNotifications(n uint64) bool {
	client.notificationsMu.Lock()
	defer client.notificationsMu.Unlock()
	for client.notificationsProcessed < n && !client.scannerDone {
		client.notificationsChanged.Wait()
	}
	return client.notificationsProcessed >= n
}

// start launches the tmux control mode subprocess and the reader
// goroutine. Called once from NewControlClient.
func (client *ControlClient) start(ctx context.Context) error {
	cmd := client.server.CommandContext(ctx, "-C", "attach-session", "-t", client.sessionName)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("create stdout pipe: %w", err)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("create stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start tmux control mode: %w", err)
	}

	go client.readNotifications(ctx, stdout, stdin, cmd)

	return nil
}

// readNotifications reads the tmux control mode notification stream,
// filters for layout-relevant events, and drives the debounce timer.
// Runs as a goroutine for the lifetime of the control client.
func (client *ControlClient) readNotifications(ctx context.Context, stdout io.Reader, stdin io.WriteCloser, cmd *exec.Cmd) {
	defer close(client.done)
	defer close(client.events)
	// cmd.Wait() waits for the process to exit and for I/O pipes to
	// close. We must close stdin before Wait to prevent deadlock when
	// the process is killed but stdin's write end is still open.
	defer func() {
		stdin.Close()
		cmd.Wait()
	}()
	// Broadcast scannerDone before cmd.Wait() (LIFO: last defer
	// runs first). This unblocks WaitForNotifications even if the
	// tmux subprocess hasn't exited yet (e.g., server crashed but
	// the control mode process is still alive).
	//
	// Also close client.ready if the scanner exits before the initial
	// attach response completes. Without this, callers blocking on
	// Ready() would hang forever if the tmux process dies during
	// attach (e.g., the tmux server crashed under load).
	defer func() {
		client.readyOnce.Do(func() { close(client.ready) })
		client.notificationsMu.Lock()
		client.scannerDone = true
		client.notificationsMu.Unlock()
		client.notificationsChanged.Broadcast()
	}()

	scanner := bufio.NewScanner(stdout)

	// Track whether we're inside a %begin/%end response block.
	// Notifications never appear inside response blocks, but the
	// block content lines could look like notifications if we don't
	// track state.
	insideResponseBlock := false

	// Wait for the initial attach-session response to complete before
	// signalling readiness. When tmux -C attach-session connects, the
	// server sends a %begin/%end response block containing session info.
	// Until that block completes, the control mode client is not fully
	// attached and won't receive real-time notifications. If we signal
	// ready too early, tests can send tmux commands whose notifications
	// arrive before the client is listening — causing missed events.
	attachComplete := false

	var debounceTimer *clock.Timer
	// Guard against firing a stale timer after reset. Each layout-
	// relevant notification increments the generation; the fire
	// handler checks that the generation hasn't changed.
	var debounceGeneration uint64
	var generationMutex sync.Mutex

	for scanner.Scan() {
		line := scanner.Text()

		// Track response blocks to avoid false notification matches.
		if strings.HasPrefix(line, "%begin ") {
			insideResponseBlock = true
			continue
		}
		if strings.HasPrefix(line, "%end ") || strings.HasPrefix(line, "%error ") {
			insideResponseBlock = false
			if !attachComplete {
				attachComplete = true
				client.readyOnce.Do(func() { close(client.ready) })
			}
			continue
		}
		if insideResponseBlock {
			continue
		}

		if !isLayoutNotification(line) {
			continue
		}

		// Layout-relevant notification received. Reset the debounce
		// timer. If it's already running, stop it and start fresh.
		generationMutex.Lock()
		debounceGeneration++
		currentGeneration := debounceGeneration
		generationMutex.Unlock()

		if debounceTimer != nil {
			debounceTimer.Stop()
		}
		debounceTimer = client.clock.AfterFunc(client.debounceInterval, func() {
			generationMutex.Lock()
			latestGeneration := debounceGeneration
			generationMutex.Unlock()

			if currentGeneration != latestGeneration {
				// A newer notification arrived after this timer was
				// created. That notification has its own timer; this
				// one is stale.
				return
			}

			select {
			case client.events <- LayoutChanged{
				SessionName: client.sessionName,
				Timestamp:   client.clock.Now(),
			}:
			case <-ctx.Done():
			}
		})

		// Broadcast after AfterFunc so that WaitForNotifications
		// callers see both the incremented counter and the
		// registered timer on the clock.
		client.notificationsMu.Lock()
		client.notificationsProcessed++
		client.notificationsMu.Unlock()
		client.notificationsChanged.Broadcast()
	}

	// Scanner finished — either EOF (tmux exited) or context cancelled
	// (which kills the process, closing stdout). Stop any pending timer.
	// The scannerDone broadcast is handled by the deferred function above.
	if debounceTimer != nil {
		debounceTimer.Stop()
	}
}

// isLayoutNotification returns true if the tmux control mode notification
// line indicates a layout change that should trigger a layout sync.
func isLayoutNotification(line string) bool {
	// %layout-change window-id window-layout window-visible-layout window-flags
	if strings.HasPrefix(line, "%layout-change ") {
		return true
	}
	// %window-add window-id
	if strings.HasPrefix(line, "%window-add ") {
		return true
	}
	// %window-close window-id
	if strings.HasPrefix(line, "%window-close ") {
		return true
	}
	// %window-renamed window-id name
	if strings.HasPrefix(line, "%window-renamed ") {
		return true
	}
	// %unlinked-window-close window-id — window closed while not linked
	// to the attached session (e.g., killed from another window via
	// kill-window -t session:N).
	if strings.HasPrefix(line, "%unlinked-window-close ") {
		return true
	}
	return false
}
