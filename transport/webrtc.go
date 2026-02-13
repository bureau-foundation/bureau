// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// Compile-time interface checks.
var (
	_ Listener = (*WebRTCTransport)(nil)
	_ Dialer   = (*WebRTCTransport)(nil)
)

// signalingPollInterval is how often the transport polls for inbound
// signaling offers from peer daemons.
const signalingPollInterval = 2 * time.Second

// iceGatherTimeout is the maximum time to wait for ICE candidate gathering
// to complete before publishing the SDP.
const iceGatherTimeout = 15 * time.Second

// answerPollInterval is how often the dialer polls for an SDP answer after
// publishing an offer.
const answerPollInterval = 500 * time.Millisecond

// answerTimeout is the maximum time to wait for an SDP answer before giving up.
const answerTimeout = 30 * time.Second

// iceConnectTimeout is the maximum time to wait for a PeerConnection to
// reach the Connected state after setting the remote description.
const iceConnectTimeout = 30 * time.Second

// WebRTCTransport provides daemon-to-daemon communication over WebRTC
// data channels. It implements both Listener and Dialer because both
// directions share the same pool of PeerConnections.
//
// Each peer daemon gets one PeerConnection with potentially many data
// channels. Each DialContext call opens a new data channel on the existing
// PeerConnection (or establishes a new PeerConnection if none exists).
// The Serve side accepts inbound data channels from peers and dispatches
// them to the HTTP handler.
//
// Signaling uses the Signaler interface (Matrix state events in
// production, in-process channels in tests). Connection establishment uses
// vanilla ICE: all candidates are gathered before the SDP is published,
// so signaling requires exactly one round-trip.
type WebRTCTransport struct {
	signaler  Signaler
	localpart string
	logger    *slog.Logger

	// iceConfig is the ICE server configuration. Protected by configMu
	// because the daemon refreshes TURN credentials periodically.
	configMu  sync.RWMutex
	iceConfig ICEConfig

	// peers maps peer machine localpart → peerState.
	mu    sync.Mutex
	peers map[string]*peerState

	// inboundConnections carries data channels opened by remote peers,
	// wrapped as net.Conn. Serve() reads from this channel and dispatches
	// each connection to the HTTP handler.
	inboundConnections chan net.Conn

	// ready is closed when Serve has started the signaling poller and is
	// ready to accept inbound connections. Callers can wait on Ready()
	// before dialing.
	ready     chan struct{}
	readyOnce sync.Once

	// closed signals shutdown.
	closed    chan struct{}
	closeOnce sync.Once

	// channelCounter generates unique data channel labels.
	channelCounter atomic.Uint64
}

// peerState tracks the WebRTC PeerConnection to a single remote daemon.
// Protected by WebRTCTransport.mu — callers must hold the lock when
// accessing the peers map and when reading or modifying peerState fields.
type peerState struct {
	connection  *webrtc.PeerConnection
	localpart   string
	established chan struct{} // closed when ICE reaches Connected/Completed
}

// NewWebRTCTransport creates a WebRTC transport. The localpart identifies
// this machine in signaling (e.g., "machine/workstation"). The signaler
// provides the mechanism for exchanging SDP offers and answers.
func NewWebRTCTransport(signaler Signaler, localpart string, iceConfig ICEConfig, logger *slog.Logger) *WebRTCTransport {
	return &WebRTCTransport{
		signaler:           signaler,
		localpart:          localpart,
		iceConfig:          iceConfig,
		logger:             logger,
		peers:              make(map[string]*peerState),
		inboundConnections: make(chan net.Conn, 64),
		ready:              make(chan struct{}),
		closed:             make(chan struct{}),
	}
}

// Serve starts the WebRTC transport. It polls for inbound signaling offers
// and dispatches incoming data channels to the HTTP handler. Blocks until
// ctx is cancelled or Close is called.
// Ready returns a channel that is closed when Serve has started the
// signaling poller and is ready to accept inbound connections. This
// enables callers to synchronize without polling or sleeping.
func (wt *WebRTCTransport) Ready() <-chan struct{} {
	return wt.ready
}

func (wt *WebRTCTransport) Serve(ctx context.Context, handler http.Handler) error {
	// Start the signaling poller in the background.
	go wt.signalingPoller(ctx)

	// Signal readiness: the poller is running and the listener is about
	// to start accepting connections.
	wt.readyOnce.Do(func() { close(wt.ready) })

	// Serve incoming data channels as HTTP.
	listener := &chanListener{
		connections: wt.inboundConnections,
		closed:      wt.closed,
	}

	server := &http.Server{
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute, // Long timeout for streaming.
	}

	go func() {
		select {
		case <-ctx.Done():
			server.Close()
		case <-wt.closed:
			server.Close()
		}
	}()

	err := server.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Address returns the machine localpart as the transport address. Peer
// daemons use this value (via MachineStatus.TransportAddress) to identify
// this machine for signaling.
func (wt *WebRTCTransport) Address() string {
	return wt.localpart
}

// Close shuts down all PeerConnections and stops the signaling poller.
func (wt *WebRTCTransport) Close() error {
	wt.closeOnce.Do(func() {
		close(wt.closed)
	})

	wt.mu.Lock()
	defer wt.mu.Unlock()

	for localpart, peer := range wt.peers {
		peer.connection.Close()
		delete(wt.peers, localpart)
	}
	return nil
}

// UpdateICEConfig replaces the ICE configuration for new PeerConnections.
// Existing PeerConnections continue using their current configuration;
// new connections will use the updated config.
func (wt *WebRTCTransport) UpdateICEConfig(config ICEConfig) {
	wt.configMu.Lock()
	defer wt.configMu.Unlock()
	wt.iceConfig = config
}

// DialContext opens a data channel to the peer identified by address
// (which is the peer's machine localpart). If no PeerConnection exists to
// that peer, it creates one by publishing an SDP offer and waiting for the
// answer. Each call creates a new ordered, reliable data channel.
func (wt *WebRTCTransport) DialContext(ctx context.Context, address string) (net.Conn, error) {
	select {
	case <-wt.closed:
		return nil, net.ErrClosed
	default:
	}

	peer, err := wt.getOrCreatePeer(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("establishing peer connection to %s: %w", address, err)
	}

	// Wait for the PeerConnection to be established.
	select {
	case <-peer.established:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-wt.closed:
		return nil, net.ErrClosed
	}

	return wt.openDataChannel(peer)
}

// getOrCreatePeer returns the peerState for the given peer localpart,
// creating and signaling a new PeerConnection if necessary. If another
// goroutine is already establishing a connection to this peer, callers
// wait for that attempt rather than starting a parallel one.
func (wt *WebRTCTransport) getOrCreatePeer(ctx context.Context, peerLocalpart string) (*peerState, error) {
	wt.mu.Lock()

	if peer, ok := wt.peers[peerLocalpart]; ok {
		state := peer.connection.ICEConnectionState()
		if state != webrtc.ICEConnectionStateFailed &&
			state != webrtc.ICEConnectionStateClosed {
			wt.mu.Unlock()
			return peer, nil
		}
		// Connection is dead. Tear down and re-establish.
		peer.connection.Close()
		delete(wt.peers, peerLocalpart)
	}

	// Create the PeerConnection and store it in the map before releasing
	// the lock. This ensures concurrent callers find this entry and wait
	// on peer.established instead of starting duplicate signaling.
	pc, err := wt.newPeerConnection()
	if err != nil {
		wt.mu.Unlock()
		return nil, fmt.Errorf("creating PeerConnection: %w", err)
	}

	peer := &peerState{
		connection:  pc,
		localpart:   peerLocalpart,
		established: make(chan struct{}),
	}
	wt.peers[peerLocalpart] = peer
	wt.mu.Unlock()

	// Run signaling outside the lock. On failure, clean up the map entry
	// so the next caller retries.
	if err := wt.establishOutbound(ctx, peer); err != nil {
		wt.mu.Lock()
		if current, ok := wt.peers[peerLocalpart]; ok && current == peer {
			delete(wt.peers, peerLocalpart)
		}
		wt.mu.Unlock()
		pc.Close()
		return nil, err
	}

	return peer, nil
}

// establishOutbound performs SDP signaling for a PeerConnection that is
// already stored in the peers map. The caller must have created the
// PeerConnection and registered it before calling this. On success the
// peer.established channel will be closed by the ICE state handler.
func (wt *WebRTCTransport) establishOutbound(ctx context.Context, peer *peerState) error {
	peerLocalpart := peer.localpart
	pc := peer.connection

	// Register inbound data channel handler (the peer may open channels to us).
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		wt.handleInboundDataChannel(dc, peerLocalpart)
	})

	// Monitor ICE connection state.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		wt.handleICEStateChange(peerLocalpart, peer, state)
	})

	// Create a trigger data channel to generate the SDP offer. The remote
	// side doesn't use this channel — it just forces pion to include a
	// data channel section in the SDP.
	if _, err := pc.CreateDataChannel("init", nil); err != nil {
		return fmt.Errorf("creating init data channel: %w", err)
	}

	// Create and set the local SDP offer.
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("creating SDP offer: %w", err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("setting local description: %w", err)
	}

	// Wait for ICE gathering to complete (vanilla ICE).
	select {
	case <-gatherComplete:
	case <-time.After(iceGatherTimeout):
		return fmt.Errorf("ICE gathering timed out after %s", iceGatherTimeout)
	case <-ctx.Done():
		return ctx.Err()
	}

	// Publish the complete SDP offer.
	completeSDP := pc.LocalDescription().SDP
	if err := wt.signaler.PublishOffer(ctx, wt.localpart, peerLocalpart, completeSDP); err != nil {
		return fmt.Errorf("publishing SDP offer: %w", err)
	}

	wt.logger.Info("WebRTC offer published", "peer", peerLocalpart)

	// Poll for the answer.
	answerSDP, err := wt.waitForAnswer(ctx, peerLocalpart)
	if err != nil {
		return fmt.Errorf("waiting for SDP answer from %s: %w", peerLocalpart, err)
	}

	// Set the remote description.
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("setting remote description: %w", err)
	}

	wt.logger.Info("WebRTC outbound connection established", "peer", peerLocalpart)
	return nil
}

// waitForAnswer polls the signaler for an SDP answer from the specified peer.
func (wt *WebRTCTransport) waitForAnswer(ctx context.Context, peerLocalpart string) (string, error) {
	deadline := time.After(answerTimeout)
	ticker := time.NewTicker(answerPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return "", fmt.Errorf("timed out after %s", answerTimeout)
		case <-ctx.Done():
			return "", ctx.Err()
		case <-wt.closed:
			return "", net.ErrClosed
		case <-ticker.C:
			answers, err := wt.signaler.PollAnswers(ctx, wt.localpart)
			if err != nil {
				wt.logger.Warn("polling for SDP answer failed", "error", err)
				continue
			}
			for _, answer := range answers {
				if answer.PeerLocalpart == peerLocalpart {
					return answer.SDP, nil
				}
			}
		}
	}
}

// signalingPoller runs in the background and checks for incoming SDP offers
// from peer daemons.
func (wt *WebRTCTransport) signalingPoller(ctx context.Context) {
	ticker := time.NewTicker(signalingPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-wt.closed:
			return
		case <-ticker.C:
			wt.processInboundOffers(ctx)
		}
	}
}

// processInboundOffers checks for new SDP offers and answers them.
func (wt *WebRTCTransport) processInboundOffers(ctx context.Context) {
	offers, err := wt.signaler.PollOffers(ctx, wt.localpart)
	if err != nil {
		wt.logger.Warn("polling for SDP offers failed", "error", err)
		return
	}

	for _, offer := range offers {
		wt.mu.Lock()
		existing, hasExisting := wt.peers[offer.PeerLocalpart]
		wt.mu.Unlock()

		if hasExisting {
			state := existing.connection.ICEConnectionState()
			if state != webrtc.ICEConnectionStateFailed &&
				state != webrtc.ICEConnectionStateClosed {
				// Signaling race: we already have a connection (or are
				// establishing one) to this peer.
				// Tie-breaking: lexicographically smaller localpart is the
				// canonical offerer. If the peer should be the offerer (their
				// localpart < ours), accept their offer and tear down our
				// outbound attempt. Otherwise, ignore their offer.
				if offer.PeerLocalpart > wt.localpart {
					// We are the canonical offerer. Ignore their offer.
					continue
				}
				// They are the canonical offerer. Tear down our connection.
				wt.mu.Lock()
				existing.connection.Close()
				delete(wt.peers, offer.PeerLocalpart)
				wt.mu.Unlock()
			} else {
				// Existing connection is dead. Clean it up.
				wt.mu.Lock()
				existing.connection.Close()
				delete(wt.peers, offer.PeerLocalpart)
				wt.mu.Unlock()
			}
		}

		if err := wt.answerOffer(ctx, offer); err != nil {
			wt.logger.Error("answering WebRTC offer failed",
				"peer", offer.PeerLocalpart,
				"error", err,
			)
		}
	}
}

// answerOffer creates a PeerConnection in response to an incoming SDP offer.
func (wt *WebRTCTransport) answerOffer(ctx context.Context, offer SignalMessage) error {
	pc, err := wt.newPeerConnection()
	if err != nil {
		return fmt.Errorf("creating PeerConnection: %w", err)
	}

	peer := &peerState{
		connection:  pc,
		localpart:   offer.PeerLocalpart,
		established: make(chan struct{}),
	}

	// Register inbound data channel handler.
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		wt.handleInboundDataChannel(dc, offer.PeerLocalpart)
	})

	// Monitor ICE connection state.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		wt.handleICEStateChange(offer.PeerLocalpart, peer, state)
	})

	// Set the remote offer.
	remoteOffer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offer.SDP,
	}
	if err := pc.SetRemoteDescription(remoteOffer); err != nil {
		pc.Close()
		return fmt.Errorf("setting remote description: %w", err)
	}

	// Create answer.
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return fmt.Errorf("creating SDP answer: %w", err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		return fmt.Errorf("setting local description: %w", err)
	}

	// Wait for ICE gathering to complete.
	select {
	case <-gatherComplete:
	case <-time.After(iceGatherTimeout):
		pc.Close()
		return fmt.Errorf("ICE gathering timed out after %s", iceGatherTimeout)
	case <-ctx.Done():
		pc.Close()
		return ctx.Err()
	}

	// Publish the complete answer.
	completeSDP := pc.LocalDescription().SDP
	if err := wt.signaler.PublishAnswer(ctx, offer.PeerLocalpart, wt.localpart, completeSDP); err != nil {
		pc.Close()
		return fmt.Errorf("publishing SDP answer: %w", err)
	}

	// Store the peer.
	wt.mu.Lock()
	wt.peers[offer.PeerLocalpart] = peer
	wt.mu.Unlock()

	wt.logger.Info("WebRTC inbound connection answered",
		"peer", offer.PeerLocalpart,
	)

	return nil
}

// handleInboundDataChannel wraps an incoming data channel as a net.Conn
// and pushes it to the inbound connection channel for the HTTP handler.
func (wt *WebRTCTransport) handleInboundDataChannel(dc *webrtc.DataChannel, peerLocalpart string) {
	// The "init" data channel is a trigger used by establishOutbound to
	// force pion to include a data channel section in the SDP offer.
	// Neither side sends data on it. Accepting it into the http.Server
	// would waste a goroutine (blocked reading forever until ReadTimeout)
	// and — more critically — pion's SCTP implementation can exhibit
	// internal lock contention when multiple streams on the same
	// association have concurrent blocked reads. Discarding the init
	// channel avoids this.
	if dc.Label() == "init" {
		dc.OnOpen(func() {
			dc.Close()
		})
		return
	}

	wt.logger.Debug("inbound data channel received",
		"peer", peerLocalpart,
		"label", dc.Label(),
	)
	dc.OnOpen(func() {
		wt.logger.Debug("inbound data channel opened",
			"peer", peerLocalpart,
			"label", dc.Label(),
		)
		rawChannel, err := dc.Detach()
		if err != nil {
			wt.logger.Error("detaching inbound data channel failed",
				"peer", peerLocalpart,
				"label", dc.Label(),
				"error", err,
			)
			return
		}

		conn := NewDataChannelConn(
			rawChannel,
			wt.localpart+"/"+dc.Label(),
			peerLocalpart+"/"+dc.Label(),
		)

		select {
		case wt.inboundConnections <- conn:
		case <-wt.closed:
			conn.Close()
		}
	})
}

// handleICEStateChange monitors PeerConnection state and manages the
// established signal.
func (wt *WebRTCTransport) handleICEStateChange(peerLocalpart string, peer *peerState, state webrtc.ICEConnectionState) {
	wt.logger.Info("ICE state change",
		"peer", peerLocalpart,
		"state", state.String(),
	)

	switch state {
	case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
		// Signal that the connection is ready for data channels.
		select {
		case <-peer.established:
			// Already signaled.
		default:
			close(peer.established)
		}

	case webrtc.ICEConnectionStateFailed:
		wt.logger.Warn("WebRTC connection failed, will re-establish on next dial",
			"peer", peerLocalpart,
		)
		// Don't remove from peers map here — DialContext/getOrCreatePeer
		// checks the state and re-establishes if needed.

	case webrtc.ICEConnectionStateClosed:
		wt.mu.Lock()
		if current, ok := wt.peers[peerLocalpart]; ok && current == peer {
			delete(wt.peers, peerLocalpart)
		}
		wt.mu.Unlock()
	}
}

// openDataChannel creates a new ordered, reliable data channel on the
// peer's PeerConnection and returns it as a net.Conn.
func (wt *WebRTCTransport) openDataChannel(peer *peerState) (net.Conn, error) {
	counter := wt.channelCounter.Add(1)
	label := fmt.Sprintf("http-%d", counter)

	wt.logger.Debug("opening data channel",
		"label", label,
		"peer", peer.localpart,
	)

	ordered := true
	dc, err := peer.connection.CreateDataChannel(label, &webrtc.DataChannelInit{
		Ordered: &ordered,
	})
	if err != nil {
		return nil, fmt.Errorf("creating data channel %s: %w", label, err)
	}

	// Wait for the data channel to open.
	openChan := make(chan struct{})
	dc.OnOpen(func() {
		wt.logger.Debug("data channel opened", "label", label, "peer", peer.localpart)
		close(openChan)
	})

	select {
	case <-openChan:
	case <-time.After(10 * time.Second):
		wt.logger.Warn("data channel open timed out", "label", label, "peer", peer.localpart)
		dc.Close()
		return nil, fmt.Errorf("data channel %s did not open within 10s", label)
	case <-wt.closed:
		dc.Close()
		return nil, net.ErrClosed
	}

	rawChannel, err := dc.Detach()
	if err != nil {
		dc.Close()
		return nil, fmt.Errorf("detaching data channel %s: %w", label, err)
	}

	return NewDataChannelConn(
		rawChannel,
		wt.localpart+"/"+label,
		peer.localpart+"/"+label,
	), nil
}

// newPeerConnection creates a pion PeerConnection with the current ICE config.
func (wt *WebRTCTransport) newPeerConnection() (*webrtc.PeerConnection, error) {
	wt.configMu.RLock()
	config := webrtc.Configuration{
		ICEServers: wt.iceConfig.Servers,
	}
	wt.configMu.RUnlock()

	// Use a SettingEngine to enable data channel detach (required for
	// stream-oriented ReadWriteCloser access) and loopback ICE candidates
	// (required for same-machine transport and test environments where
	// loopback is the only available interface).
	settingEngine := webrtc.SettingEngine{}
	settingEngine.DetachDataChannels()
	settingEngine.SetIncludeLoopbackCandidate(true)

	api := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine))
	return api.NewPeerConnection(config)
}

// chanListener implements net.Listener by reading connections from a channel.
// Used by Serve() to feed incoming data channel connections to http.Server.
type chanListener struct {
	connections <-chan net.Conn
	closed      <-chan struct{}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case conn, ok := <-l.connections:
		if !ok {
			return nil, net.ErrClosed
		}
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	return nil // Lifecycle managed by WebRTCTransport.Close()
}

func (l *chanListener) Addr() net.Addr {
	return &dataChannelAddr{label: "webrtc-listener"}
}
