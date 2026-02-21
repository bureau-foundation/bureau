// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// signalingSeparator separates the offerer and target machine localparts
// in a signaling state event's state key. The pipe character is not valid
// in Matrix localparts (allowed: a-z, 0-9, ., _, =, -, /) so it provides
// an unambiguous boundary.
const signalingSeparator = "|"

// Compile-time interface check.
var _ Signaler = (*MatrixSignaler)(nil)

// MatrixSignaler implements Signaler using Matrix state events in the
// machine room (#bureau/machine). Offers and answers are published as
// state events with event types m.bureau.webrtc_offer and
// m.bureau.webrtc_answer respectively.
type MatrixSignaler struct {
	session       *messaging.DirectSession
	machineRoomID ref.RoomID
	logger        *slog.Logger

	// lastSeen tracks the most recent timestamp we've processed for each
	// peer's signals. This prevents re-processing old offers/answers.
	mu       sync.Mutex
	lastSeen map[string]time.Time // key: state_key of the signal event
}

// NewMatrixSignaler creates a Matrix-backed signaler. machineRoomID is the
// room ID for #bureau/machine where signaling state events are published.
func NewMatrixSignaler(session *messaging.DirectSession, machineRoomID ref.RoomID, logger *slog.Logger) *MatrixSignaler {
	return &MatrixSignaler{
		session:       session,
		machineRoomID: machineRoomID,
		logger:        logger,
		lastSeen:      make(map[string]time.Time),
	}
}

// PublishOffer publishes a complete SDP offer directed at the target machine.
func (s *MatrixSignaler) PublishOffer(ctx context.Context, localpart, targetLocalpart, sdp string) error {
	stateKey := localpart + signalingSeparator + targetLocalpart
	content := schema.WebRTCSignal{
		SDP:       sdp,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
	_, err := s.session.SendStateEvent(ctx, s.machineRoomID,
		schema.EventTypeWebRTCOffer, stateKey, content)
	if err != nil {
		return fmt.Errorf("publishing WebRTC offer (state_key=%s): %w", stateKey, err)
	}
	return nil
}

// PublishAnswer publishes a complete SDP answer in response to an offer.
func (s *MatrixSignaler) PublishAnswer(ctx context.Context, offererLocalpart, localpart, sdp string) error {
	stateKey := offererLocalpart + signalingSeparator + localpart
	content := schema.WebRTCSignal{
		SDP:       sdp,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
	_, err := s.session.SendStateEvent(ctx, s.machineRoomID,
		schema.EventTypeWebRTCAnswer, stateKey, content)
	if err != nil {
		return fmt.Errorf("publishing WebRTC answer (state_key=%s): %w", stateKey, err)
	}
	return nil
}

// PollOffers returns new SDP offers directed at this machine.
func (s *MatrixSignaler) PollOffers(ctx context.Context, localpart string) ([]SignalMessage, error) {
	return s.pollSignals(ctx, localpart, schema.EventTypeWebRTCOffer, matchOfferKey)
}

// PollAnswers returns new SDP answers to offers originated by this machine.
func (s *MatrixSignaler) PollAnswers(ctx context.Context, localpart string) ([]SignalMessage, error) {
	return s.pollSignals(ctx, localpart, schema.EventTypeWebRTCAnswer, matchAnswerKey)
}

// pollSignals fetches room state and returns signal messages matching the
// given event type whose state keys pass the matcher. The matcher extracts
// the peer localpart from each state key.
func (s *MatrixSignaler) pollSignals(ctx context.Context, localpart string, eventType ref.EventType, match signalKeyMatcher) ([]SignalMessage, error) {
	events, err := s.session.GetRoomState(ctx, s.machineRoomID)
	if err != nil {
		return nil, fmt.Errorf("fetching room state: %w", err)
	}

	var messages []SignalMessage

	for _, event := range events {
		if event.Type != eventType {
			continue
		}
		stateKey := ""
		if event.StateKey != nil {
			stateKey = *event.StateKey
		}

		peerLocalpart, ok := match(stateKey, localpart)
		if !ok {
			continue
		}

		signal, ok := s.parseSignal(event.Content)
		if !ok {
			continue
		}

		if !s.isNewer(stateKey, signal.Timestamp) {
			continue
		}

		messages = append(messages, SignalMessage{
			PeerLocalpart: peerLocalpart,
			SDP:           signal.SDP,
			Timestamp:     signal.Timestamp,
		})
	}

	return messages, nil
}

// parseSignal extracts a WebRTCSignal from a Matrix event's content map.
func (s *MatrixSignaler) parseSignal(content map[string]any) (schema.WebRTCSignal, bool) {
	raw, err := json.Marshal(content)
	if err != nil {
		return schema.WebRTCSignal{}, false
	}
	var signal schema.WebRTCSignal
	if err := json.Unmarshal(raw, &signal); err != nil {
		return schema.WebRTCSignal{}, false
	}
	if signal.SDP == "" || signal.Timestamp == "" {
		return schema.WebRTCSignal{}, false
	}
	return signal, true
}

// isNewer returns true if the given timestamp is newer than the last-seen
// timestamp for this state key. Also marks the timestamp as seen.
func (s *MatrixSignaler) isNewer(stateKey, timestampStr string) bool {
	timestamp, err := time.Parse(time.RFC3339Nano, timestampStr)
	if err != nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if last, ok := s.lastSeen[stateKey]; ok && !timestamp.After(last) {
		return false
	}
	s.lastSeen[stateKey] = timestamp
	return true
}
