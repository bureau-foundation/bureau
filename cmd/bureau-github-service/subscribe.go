// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/forgesub"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/forge"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// subscribeRequest is the decoded request body for the
// "github/subscribe" stream action. Exactly one of Room or Entity
// must be set — Room for room-level subscriptions (all events for
// repos bound to a Bureau room, filtered by ForgeConfig), Entity for
// entity-level subscriptions (a specific issue, PR, or CI run).
type subscribeRequest struct {
	Room       string          `cbor:"room,omitempty"`
	Entity     forge.EntityRef `cbor:"entity,omitempty"`
	Persistent bool            `cbor:"persistent,omitempty"` // entity subscriptions only

	// Auto activates pending auto-subscriptions for the connecting
	// agent. Only applies to room subscriptions. Pending
	// auto-subscriptions (created by ProcessAutoSubscribe when
	// webhooks arrive) are claimed and live entity subscriptions
	// are created sharing the same event stream.
	Auto bool `cbor:"auto,omitempty"`
}

// heartbeatInterval is the time between heartbeat frames on a
// subscribe stream. The client should consider the connection dead
// if no frame of any type arrives within 2x this interval.
const heartbeatInterval = 30 * time.Second

// handleSubscribe is the stream handler for the "github/subscribe"
// action. Validates the token, decodes the request, registers a
// subscriber with the Manager, and enters the event loop.
//
// Unlike the ticket service's subscribe handler, there is no snapshot
// phase. Forge events are ephemeral — the Manager sends a caught_up
// frame at registration time, and live events follow immediately.
func (gs *GitHubService) handleSubscribe(ctx context.Context, token *servicetoken.Token, raw []byte, conn net.Conn) {
	encoder := codec.NewEncoder(conn)

	// Check grant.
	action := forge.ProviderAction(forge.ProviderGitHub, forge.ActionSubscribe)
	if !servicetoken.GrantsAllow(token.Grants, action, "") {
		encoder.Encode(forge.SubscribeFrame{
			Type:    forge.FrameError,
			Message: fmt.Sprintf("access denied: missing grant for %s", action),
		})
		return
	}

	// Decode request.
	var request subscribeRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		encoder.Encode(forge.SubscribeFrame{
			Type:    forge.FrameError,
			Message: "invalid request: " + err.Error(),
		})
		return
	}

	// Exactly one of Room or Entity must be set.
	hasRoom := request.Room != ""
	hasEntity := !request.Entity.IsZero()

	if hasRoom == hasEntity {
		encoder.Encode(forge.SubscribeFrame{
			Type:    forge.FrameError,
			Message: "exactly one of room or entity must be set",
		})
		return
	}

	// Validate the entity ref if one was provided.
	if hasEntity {
		if err := request.Entity.Validate(); err != nil {
			encoder.Encode(forge.SubscribeFrame{
				Type:    forge.FrameError,
				Message: err.Error(),
			})
			return
		}
	}

	done := make(chan struct{})
	subscriber := &forgesub.Subscriber{
		Channel: make(chan forgesub.SubscribeEvent, forgesub.SubscriberChannelSize),
		Done:    done,
	}

	if hasRoom {
		gs.handleRoomSubscription(ctx, encoder, subscriber, done, request.Room, request.Auto, token.Subject)
	} else {
		gs.handleEntitySubscription(ctx, encoder, subscriber, done, request.Entity, request.Persistent)
	}
}

// handleRoomSubscription registers a room subscriber and runs the
// event loop. The room string is resolved to a ref.RoomID before
// registration. When auto is true, pending auto-subscriptions for
// the agent are claimed and activated as live entity subscriptions
// sharing the same event stream.
func (gs *GitHubService) handleRoomSubscription(
	ctx context.Context,
	encoder *codec.Encoder,
	subscriber *forgesub.Subscriber,
	done chan struct{},
	room string,
	auto bool,
	subject ref.UserID,
) {
	roomID, err := ref.ParseRoomID(room)
	if err != nil {
		encoder.Encode(forge.SubscribeFrame{
			Type:    forge.FrameError,
			Message: "invalid room ID: " + err.Error(),
		})
		return
	}

	subscription := &forgesub.RoomSubscription{
		Subscriber: subscriber,
		RoomID:     roomID,
	}

	if err := gs.manager.AddRoomSubscriber(subscription); err != nil {
		encoder.Encode(forge.SubscribeFrame{
			Type:    forge.FrameError,
			Message: err.Error(),
		})
		return
	}

	// Activate pending auto-subscriptions if requested. Entity
	// subscriptions share the same subscriber channel so events
	// multiplex onto this stream.
	var autoEntitySubscriptions []*forgesub.EntitySubscription
	if auto {
		agentLocalpart := subject.Localpart()
		claimed := gs.manager.ClaimAutoSubscriptions(agentLocalpart)
		for _, entityRef := range claimed {
			entitySubscription := &forgesub.EntitySubscription{
				Subscriber:       subscriber,
				Entity:           entityRef,
				Persistent:       true,
				SuppressCaughtUp: true,
			}
			if addErr := gs.manager.AddEntitySubscriber(entitySubscription); addErr != nil {
				gs.logger.Warn("failed to activate auto-subscription",
					"entity", entityRef,
					"error", addErr,
				)
				continue
			}
			autoEntitySubscriptions = append(autoEntitySubscriptions, entitySubscription)
			gs.logger.Info("auto-subscription activated",
				"agent", agentLocalpart,
				"entity", entityRef,
			)
		}
	}

	gs.logger.Info("forge subscribe stream started",
		"type", "room",
		"room_id", roomID,
		"auto_subscriptions", len(autoEntitySubscriptions),
	)

	defer func() {
		close(done)
		gs.manager.RemoveRoomSubscriber(subscription)
		for _, entitySubscription := range autoEntitySubscriptions {
			gs.manager.RemoveEntitySubscriber(entitySubscription)
		}
		gs.logger.Info("forge subscribe stream ended",
			"type", "room",
			"room_id", roomID,
		)
	}()

	gs.subscribeEventLoop(ctx, encoder, subscriber)
}

// handleEntitySubscription registers an entity subscriber and runs
// the event loop.
func (gs *GitHubService) handleEntitySubscription(
	ctx context.Context,
	encoder *codec.Encoder,
	subscriber *forgesub.Subscriber,
	done chan struct{},
	entity forge.EntityRef,
	persistent bool,
) {
	subscription := &forgesub.EntitySubscription{
		Subscriber: subscriber,
		Entity:     entity,
		Persistent: persistent,
	}

	if err := gs.manager.AddEntitySubscriber(subscription); err != nil {
		encoder.Encode(forge.SubscribeFrame{
			Type:    forge.FrameError,
			Message: err.Error(),
		})
		return
	}

	gs.logger.Info("forge subscribe stream started",
		"type", "entity",
		"entity", entity,
		"persistent", persistent,
	)

	defer func() {
		close(done)
		gs.manager.RemoveEntitySubscriber(subscription)
		gs.logger.Info("forge subscribe stream ended",
			"type", "entity",
			"entity", entity,
		)
	}()

	gs.subscribeEventLoop(ctx, encoder, subscriber)
}

// subscribeEventLoop reads events from the subscriber channel and
// writes them as CBOR frames to the connection. Runs until the
// context is cancelled or the connection fails.
//
// On channel overflow (resync flag set), the loop drains buffered
// events, writes a resync frame followed by a caught_up frame, and
// resumes. Unlike the ticket service, there is no snapshot to
// re-collect: forge events are ephemeral, so the resync simply
// signals that some events were missed.
func (gs *GitHubService) subscribeEventLoop(ctx context.Context, encoder *codec.Encoder, subscriber *forgesub.Subscriber) {
	heartbeat := gs.clock.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case event := <-subscriber.Channel:
			if subscriber.Resync.CompareAndSwap(true, false) {
				// Drain remaining stale events.
				for len(subscriber.Channel) > 0 {
					<-subscriber.Channel
				}

				if err := encoder.Encode(forge.SubscribeFrame{Type: forge.FrameResync}); err != nil {
					gs.logger.Debug("subscribe stream write error during resync", "error", err)
					return
				}
				if err := encoder.Encode(forge.SubscribeFrame{Type: forge.FrameCaughtUp}); err != nil {
					gs.logger.Debug("subscribe stream write error during resync", "error", err)
					return
				}
				continue
			}

			// Normal event forwarding.
			if err := encoder.Encode(event.Frame); err != nil {
				gs.logger.Debug("subscribe stream write error", "error", err)
				return
			}

		case <-heartbeat.C:
			if err := encoder.Encode(forge.SubscribeFrame{Type: forge.FrameHeartbeat}); err != nil {
				gs.logger.Debug("subscribe stream heartbeat error", "error", err)
				return
			}
		}
	}
}
