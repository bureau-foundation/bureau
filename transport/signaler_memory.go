// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"sync"
	"time"
)

// Compile-time interface check.
var _ Signaler = (*MemorySignaler)(nil)

// MemorySignaler is an in-process Signaler for tests. Offers and answers
// are exchanged through an internal map, bypassing Matrix entirely. Two
// WebRTCTransport instances sharing the same MemorySignaler can establish
// PeerConnections without any network signaling.
type MemorySignaler struct {
	mu       sync.Mutex
	offers   map[string]SignalMessage // key: "offerer|target"
	answers  map[string]SignalMessage // key: "offerer|target"
	lastSeen map[string]time.Time     // key: "offerer|target" — tracks per-consumer state
}

// NewMemorySignaler creates a new in-process signaler.
func NewMemorySignaler() *MemorySignaler {
	return &MemorySignaler{
		offers:   make(map[string]SignalMessage),
		answers:  make(map[string]SignalMessage),
		lastSeen: make(map[string]time.Time),
	}
}

func (s *MemorySignaler) PublishOffer(_ context.Context, localpart, targetLocalpart, sdp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := localpart + signalingSeparator + targetLocalpart
	s.offers[key] = SignalMessage{
		PeerLocalpart: localpart,
		SDP:           sdp,
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	return nil
}

func (s *MemorySignaler) PublishAnswer(_ context.Context, offererLocalpart, localpart, sdp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := offererLocalpart + signalingSeparator + localpart
	s.answers[key] = SignalMessage{
		PeerLocalpart: localpart,
		SDP:           sdp,
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	return nil
}

func (s *MemorySignaler) PollOffers(_ context.Context, localpart string) ([]SignalMessage, error) {
	return s.pollSignals(localpart, s.offers, "offers", matchOfferKey)
}

func (s *MemorySignaler) PollAnswers(_ context.Context, localpart string) ([]SignalMessage, error) {
	return s.pollSignals(localpart, s.answers, "answers", matchAnswerKey)
}

// pollSignals iterates a signal store and returns messages whose keys match
// the given matcher, filtering out already-seen timestamps.
func (s *MemorySignaler) pollSignals(localpart string, store map[string]SignalMessage, storeLabel string, match signalKeyMatcher) ([]SignalMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var messages []SignalMessage

	for key, msg := range store {
		if _, ok := match(key, localpart); !ok {
			continue
		}

		timestamp, err := time.Parse(time.RFC3339Nano, msg.Timestamp)
		if err != nil {
			continue
		}

		seenKey := storeLabel + ":" + localpart + ":" + key
		if last, ok := s.lastSeen[seenKey]; ok && !timestamp.After(last) {
			continue
		}
		s.lastSeen[seenKey] = timestamp

		messages = append(messages, msg)
	}

	return messages, nil
}
