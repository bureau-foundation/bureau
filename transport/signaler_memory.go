// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"sync"
	"time"
)

// Compile-time interface checks.
var (
	_ Signaler       = (*MemorySignaler)(nil)
	_ SignalNotifier = (*MemorySignaler)(nil)
)

// MemorySignaler is an in-process Signaler for tests. Offers and answers
// are exchanged through an internal map, bypassing Matrix entirely. Two
// WebRTCTransport instances sharing the same MemorySignaler can establish
// PeerConnections without any network signaling.
//
// MemorySignaler also implements SignalNotifier — subscribers are woken
// immediately when signals are published, eliminating poll latency in tests.
type MemorySignaler struct {
	mu       sync.Mutex
	offers   map[string]SignalMessage // key: "offererID|targetID"
	answers  map[string]SignalMessage // key: "offererID|targetID"
	lastSeen map[string]time.Time     // key: "offererID|targetID" — tracks per-consumer state

	subscriberMu sync.Mutex
	subscribers  []chan struct{}
}

// NewMemorySignaler creates a new in-process signaler.
func NewMemorySignaler() *MemorySignaler {
	return &MemorySignaler{
		offers:   make(map[string]SignalMessage),
		answers:  make(map[string]SignalMessage),
		lastSeen: make(map[string]time.Time),
	}
}

// Subscribe returns a channel that receives a value whenever a new offer
// or answer is published. See SignalNotifier for semantics.
func (s *MemorySignaler) Subscribe() <-chan struct{} {
	channel := make(chan struct{}, 1)
	s.subscriberMu.Lock()
	s.subscribers = append(s.subscribers, channel)
	s.subscriberMu.Unlock()
	return channel
}

// notifySubscribers wakes all subscribers. Non-blocking send: if a
// subscriber hasn't consumed the previous notification, the new one
// is dropped (the subscriber will Poll on wakeup anyway).
func (s *MemorySignaler) notifySubscribers() {
	s.subscriberMu.Lock()
	defer s.subscriberMu.Unlock()
	for _, channel := range s.subscribers {
		select {
		case channel <- struct{}{}:
		default:
		}
	}
}

func (s *MemorySignaler) PublishOffer(_ context.Context, machineID, targetID, sdp string) error {
	s.mu.Lock()
	key := machineID + signalingSeparator + targetID
	s.offers[key] = SignalMessage{
		PeerID:    machineID,
		SDP:       sdp,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
	s.mu.Unlock()

	s.notifySubscribers()
	return nil
}

func (s *MemorySignaler) PublishAnswer(_ context.Context, offererID, machineID, sdp string) error {
	s.mu.Lock()
	key := offererID + signalingSeparator + machineID
	s.answers[key] = SignalMessage{
		PeerID:    machineID,
		SDP:       sdp,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
	s.mu.Unlock()

	s.notifySubscribers()
	return nil
}

func (s *MemorySignaler) PollOffers(_ context.Context, machineID string) ([]SignalMessage, error) {
	return s.pollSignals(machineID, s.offers, "offers", matchOfferKey)
}

func (s *MemorySignaler) PollAnswers(_ context.Context, machineID string) ([]SignalMessage, error) {
	return s.pollSignals(machineID, s.answers, "answers", matchAnswerKey)
}

// pollSignals iterates a signal store and returns messages whose keys match
// the given matcher, filtering out already-seen timestamps.
func (s *MemorySignaler) pollSignals(machineID string, store map[string]SignalMessage, storeLabel string, match signalKeyMatcher) ([]SignalMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var messages []SignalMessage

	for key, message := range store {
		if _, ok := match(key, machineID); !ok {
			continue
		}

		timestamp, err := time.Parse(time.RFC3339Nano, message.Timestamp)
		if err != nil {
			continue
		}

		seenKey := storeLabel + ":" + machineID + ":" + key
		if last, ok := s.lastSeen[seenKey]; ok && !timestamp.After(last) {
			continue
		}
		s.lastSeen[seenKey] = timestamp

		messages = append(messages, message)
	}

	return messages, nil
}
