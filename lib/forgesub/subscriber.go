// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forgesub

import (
	"sync/atomic"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/forge"
)

// SubscriberChannelSize is the buffer size for per-subscriber event
// channels. Must be large enough to absorb burst webhook delivery
// without drops. If a subscriber's channel is full, the event is
// dropped and the subscriber is marked for resync.
const SubscriberChannelSize = 256

// SubscribeEvent is a single dispatch from the Manager to a
// subscriber. The Frame field contains a complete SubscribeFrame
// ready for CBOR encoding.
type SubscribeEvent struct {
	Frame forge.SubscribeFrame
}

// Subscriber represents a single connected subscribe stream. The
// Manager sends events via the Channel. The owner (connector service)
// reads from Channel, encodes frames, and writes them to the
// connection.
//
// The Done channel is closed by the subscriber's owner goroutine when
// the connection ends. The Manager detects this during fanout and
// removes the subscriber from its registries.
type Subscriber struct {
	// Channel receives dispatched events. Buffer size should be
	// SubscriberChannelSize to absorb bursts without drops.
	Channel chan SubscribeEvent

	// Resync is set to true when Channel overflows. The owner
	// should drain the channel, send a FrameResync frame, and
	// clear local state.
	Resync atomic.Bool

	// Done is closed by the owner when the connection ends.
	Done <-chan struct{}
}

// RoomSubscription pairs a Subscriber with a target room.
type RoomSubscription struct {
	*Subscriber
	RoomID ref.RoomID
}

// EntitySubscription pairs a Subscriber with entity-specific metadata.
type EntitySubscription struct {
	*Subscriber
	Entity     forge.EntityRef
	Persistent bool // if false, auto-removed on entity close
}

// trySend attempts a non-blocking send on the subscriber's channel.
// Returns false if the subscriber's Done channel is closed (the
// subscriber should be removed from the registry). On channel
// overflow, marks the subscriber for resync rather than blocking.
func trySend(subscriber *Subscriber, event SubscribeEvent) bool {
	// Check if the subscriber has disconnected.
	select {
	case <-subscriber.Done:
		return false
	default:
	}

	// Non-blocking send. On overflow, mark for resync.
	select {
	case subscriber.Channel <- event:
	default:
		subscriber.Resync.Store(true)
	}
	return true
}
