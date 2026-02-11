// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package transport provides daemon-to-daemon communication for cross-machine
// service routing in Bureau's fleet.
//
// The package defines two interfaces: [Listener] accepts inbound connections
// from peer daemons (Serve, Address, Close), and [Dialer] establishes
// outbound connections to remote peers (DialContext). The daemon uses a
// Listener to receive forwarded requests and routes them to local provider
// proxies. It uses a Dialer to forward local requests to remote peers.
// Sandboxes and proxies never interact with transport directly; they see
// Unix sockets and HTTP.
//
// The production implementation, [WebRTCTransport], uses pion/webrtc data
// channels with ICE/TURN for NAT traversal. Each pair of peer daemons
// shares a single PeerConnection with SCTP-multiplexed data channels,
// avoiding head-of-line blocking between concurrent service requests.
// [WebRTCTransport] implements both Listener and Dialer on a single
// instance. [HTTPTransport] wraps a Dialer as an http.RoundTripper for
// integration with standard HTTP client code.
//
// Signaling is abstracted behind the [Signaler] interface, which publishes
// and polls SDP offers and answers. [MatrixSignaler] uses Matrix state
// events in the #bureau/machines room for production signaling, with
// pipe-separated state keys (offerer|target) for routing.
// [MemorySignaler] provides an in-process implementation for tests.
// [SignalMessage] carries the SDP payload and ICE candidates in vanilla
// ICE mode (all candidates gathered before signaling).
//
// When both peers attempt to connect simultaneously, a deterministic
// tie-breaking rule resolves the conflict: the peer whose Matrix localpart
// is lexicographically smaller becomes the offerer, and the other peer
// drops its redundant PeerConnection.
//
// [ICEConfig] holds STUN/TURN server configuration. [ICEConfigFromTURN]
// converts the homeserver's time-limited TURN credentials (from the
// messaging package) into pion ICE server entries. [DataChannelConn]
// wraps a detached pion data channel as a net.Conn with deadline support.
package transport
