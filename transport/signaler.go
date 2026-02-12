// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import "context"

// Signaler abstracts the mechanism for exchanging WebRTC session
// descriptions between peer daemons. The production implementation
// uses Matrix state events in #bureau/machine; tests use in-process
// channels.
//
// The signaling model is vanilla ICE: all ICE candidates are gathered
// before the SDP is published, so connection establishment requires
// exactly one signaling round-trip (offer → answer).
type Signaler interface {
	// PublishOffer publishes a complete SDP offer directed at a target
	// machine. localpart is the offerer's machine localpart, targetLocalpart
	// is the intended recipient. The implementation stores the SDP where the
	// target can find it (e.g., a Matrix state event with state_key
	// "<localpart>|<targetLocalpart>").
	PublishOffer(ctx context.Context, localpart, targetLocalpart, sdp string) error

	// PublishAnswer publishes a complete SDP answer in response to a
	// previously received offer. The state key matches the offer:
	// "<offererLocalpart>|<localpart>".
	PublishAnswer(ctx context.Context, offererLocalpart, localpart, sdp string) error

	// PollOffers returns all pending WebRTC offers directed at this machine.
	// The implementation filters for offers where the target matches localpart
	// and the timestamp is newer than what was last processed.
	PollOffers(ctx context.Context, localpart string) ([]SignalMessage, error)

	// PollAnswers returns all pending WebRTC answers to offers originated
	// by this machine. The implementation filters for answers where the
	// offerer matches localpart and the timestamp is newer than what was
	// last processed.
	PollAnswers(ctx context.Context, localpart string) ([]SignalMessage, error)
}

// SignalMessage represents a signaling message (offer or answer).
type SignalMessage struct {
	// PeerLocalpart is the machine localpart of the other party.
	// For received offers, this is the offerer. For received answers,
	// this is the answerer (target).
	PeerLocalpart string

	// SDP is the complete Session Description Protocol string with all
	// ICE candidates embedded.
	SDP string

	// Timestamp is the ISO 8601 creation time of the signal.
	Timestamp string
}
