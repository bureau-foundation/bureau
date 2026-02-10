// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"strings"
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
	s.mu.Lock()
	defer s.mu.Unlock()

	suffix := signalingSeparator + localpart
	var messages []SignalMessage

	for key, msg := range s.offers {
		if !strings.HasSuffix(key, suffix) {
			continue
		}

		timestamp, err := time.Parse(time.RFC3339Nano, msg.Timestamp)
		if err != nil {
			continue
		}

		// Use a consumer-specific last-seen key: "offers:<localpart>:<stateKey>"
		seenKey := "offers:" + localpart + ":" + key
		if last, ok := s.lastSeen[seenKey]; ok && !timestamp.After(last) {
			continue
		}
		s.lastSeen[seenKey] = timestamp

		messages = append(messages, msg)
	}

	return messages, nil
}

func (s *MemorySignaler) PollAnswers(_ context.Context, localpart string) ([]SignalMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prefix := localpart + signalingSeparator
	var messages []SignalMessage

	for key, msg := range s.answers {
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		timestamp, err := time.Parse(time.RFC3339Nano, msg.Timestamp)
		if err != nil {
			continue
		}

		// Use a consumer-specific last-seen key.
		seenKey := "answers:" + localpart + ":" + key
		if last, ok := s.lastSeen[seenKey]; ok && !timestamp.After(last) {
			continue
		}
		s.lastSeen[seenKey] = timestamp

		messages = append(messages, msg)
	}

	return messages, nil
}
