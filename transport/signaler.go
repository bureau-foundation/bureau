// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"strings"
)

// Signaler abstracts the mechanism for exchanging WebRTC session
// descriptions between peer daemons. The production implementation
// uses Matrix state events in #bureau/machine; tests use in-process
// channels.
//
// The signaling model is vanilla ICE: all ICE candidates are gathered
// before the SDP is published, so connection establishment requires
// exactly one signaling round-trip (offer → answer).
//
// All machine identity parameters are full Matrix user IDs (e.g.,
// "@bureau/fleet/prod/machine/workstation:server") for federation
// safety — bare localparts would collide across servers.
type Signaler interface {
	// PublishOffer publishes a complete SDP offer directed at a target
	// machine. machineID is the offerer's full Matrix user ID, targetID
	// is the intended recipient. The implementation stores the SDP where
	// the target can find it (e.g., a Matrix state event with state_key
	// "machineID|targetID").
	PublishOffer(ctx context.Context, machineID, targetID, sdp string) error

	// PublishAnswer publishes a complete SDP answer in response to a
	// previously received offer. The state key matches the offer:
	// "offererID|machineID".
	PublishAnswer(ctx context.Context, offererID, machineID, sdp string) error

	// PollOffers returns all pending WebRTC offers directed at this machine.
	// The implementation filters for offers where the target matches machineID
	// and the timestamp is newer than what was last processed.
	PollOffers(ctx context.Context, machineID string) ([]SignalMessage, error)

	// PollAnswers returns all pending WebRTC answers to offers originated
	// by this machine. The implementation filters for answers where the
	// offerer matches machineID and the timestamp is newer than what was
	// last processed.
	PollAnswers(ctx context.Context, machineID string) ([]SignalMessage, error)
}

// signalKeyMatcher extracts the peer's user ID from a signaling state key
// given the local machine's user ID, or returns ok=false if the key
// doesn't match.
type signalKeyMatcher func(stateKey, machineID string) (peerID string, ok bool)

// matchOfferKey matches signaling state keys for offers directed at machineID.
// Offer keys have the form "offerer|target"; this returns the offerer.
func matchOfferKey(stateKey, machineID string) (string, bool) {
	suffix := signalingSeparator + machineID
	if !strings.HasSuffix(stateKey, suffix) {
		return "", false
	}
	peer := strings.TrimSuffix(stateKey, suffix)
	if peer == "" {
		return "", false
	}
	return peer, true
}

// matchAnswerKey matches signaling state keys for answers to offers from
// machineID. Answer keys have the form "offerer|target"; this returns the target.
func matchAnswerKey(stateKey, machineID string) (string, bool) {
	prefix := machineID + signalingSeparator
	if !strings.HasPrefix(stateKey, prefix) {
		return "", false
	}
	peer := strings.TrimPrefix(stateKey, prefix)
	if peer == "" {
		return "", false
	}
	return peer, true
}

// SignalNotifier is an optional interface that Signaler implementations
// can provide to enable event-driven signaling. When the signaler
// supports notifications, the transport wakes immediately on new signals
// instead of waiting for the next poll interval.
//
// MemorySignaler implements this for instant wakeup in tests.
// MatrixSignaler does not — it falls back to periodic polling.
type SignalNotifier interface {
	// Subscribe returns a channel that receives a value whenever new
	// signals are published. The channel is buffered (size 1) and sends
	// are non-blocking, so missed notifications are harmless — the
	// transport always calls Poll to get the actual data.
	//
	// Each call returns a new independent channel. The caller does not
	// need to unsubscribe; the channel is simply abandoned when no
	// longer needed.
	Subscribe() <-chan struct{}
}

// SignalMessage represents a signaling message (offer or answer).
type SignalMessage struct {
	// PeerID is the full Matrix user ID of the other party.
	// For received offers, this is the offerer. For received answers,
	// this is the answerer (target).
	PeerID string

	// SDP is the complete Session Description Protocol string with all
	// ICE candidates embedded.
	SDP string

	// Timestamp is the ISO 8601 creation time of the signal.
	Timestamp string
}
