// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxyclient

import (
	"context"
	"encoding/json"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

// ProxySession implements messaging.Session by delegating to a
// proxyclient.Client. Each method calls the corresponding /v1/matrix/*
// endpoint on the proxy, which injects credentials and forwards to the
// homeserver.
//
// Use NewProxySession to create one after obtaining the agent's user ID
// via Client.Identity or Client.Whoami.
type ProxySession struct {
	client *Client
	userID ref.UserID
}

// Compile-time check: *ProxySession implements messaging.Session.
var _ messaging.Session = (*ProxySession)(nil)

// NewProxySession creates a ProxySession wrapping the given Client.
// The userID should be the fully-qualified Matrix user ID for this
// principal, typically obtained from Client.Identity or Client.Whoami.
func NewProxySession(client *Client, userID ref.UserID) *ProxySession {
	return &ProxySession{
		client: client,
		userID: userID,
	}
}

// Client returns the underlying proxyclient.Client. Use this for
// proxy-specific operations (Identity, Grants, Services, HTTPClient)
// that are not part of the MatrixSession interface.
func (session *ProxySession) Client() *Client {
	return session.client
}

func (session *ProxySession) UserID() ref.UserID {
	return session.userID
}

// Close is a no-op for ProxySession. The proxy manages credential
// lifecycle — the sandboxed process does not hold any access tokens.
func (session *ProxySession) Close() error {
	return nil
}

func (session *ProxySession) WhoAmI(ctx context.Context) (ref.UserID, error) {
	return session.client.Whoami(ctx)
}

func (session *ProxySession) ResolveAlias(ctx context.Context, alias string) (ref.RoomID, error) {
	return session.client.ResolveAlias(ctx, alias)
}

func (session *ProxySession) GetStateEvent(ctx context.Context, roomID ref.RoomID, eventType, stateKey string) (json.RawMessage, error) {
	return session.client.GetState(ctx, roomID, eventType, stateKey)
}

func (session *ProxySession) GetRoomState(ctx context.Context, roomID ref.RoomID) ([]messaging.Event, error) {
	return session.client.GetRoomState(ctx, roomID)
}

func (session *ProxySession) SendStateEvent(ctx context.Context, roomID ref.RoomID, eventType, stateKey string, content any) (string, error) {
	return session.client.PutState(ctx, PutStateRequest{
		Room:      roomID,
		EventType: eventType,
		StateKey:  stateKey,
		Content:   content,
	})
}

func (session *ProxySession) SendEvent(ctx context.Context, roomID ref.RoomID, eventType string, content any) (string, error) {
	return session.client.SendEvent(ctx, roomID, eventType, content)
}

func (session *ProxySession) SendMessage(ctx context.Context, roomID ref.RoomID, content messaging.MessageContent) (string, error) {
	return session.client.SendMessage(ctx, roomID, content)
}

func (session *ProxySession) CreateRoom(ctx context.Context, request messaging.CreateRoomRequest) (*messaging.CreateRoomResponse, error) {
	return session.client.CreateRoom(ctx, request)
}

func (session *ProxySession) InviteUser(ctx context.Context, roomID ref.RoomID, userID ref.UserID) error {
	return session.client.InviteUser(ctx, roomID, userID)
}

func (session *ProxySession) JoinRoom(ctx context.Context, roomID ref.RoomID) (ref.RoomID, error) {
	return session.client.JoinRoom(ctx, roomID)
}

func (session *ProxySession) JoinedRooms(ctx context.Context) ([]ref.RoomID, error) {
	return session.client.JoinedRooms(ctx)
}

func (session *ProxySession) GetRoomMembers(ctx context.Context, roomID ref.RoomID) ([]messaging.RoomMember, error) {
	return session.client.GetRoomMembers(ctx, roomID)
}

func (session *ProxySession) GetDisplayName(ctx context.Context, userID ref.UserID) (string, error) {
	return session.client.GetDisplayName(ctx, userID)
}

func (session *ProxySession) RoomMessages(ctx context.Context, roomID ref.RoomID, options messaging.RoomMessagesOptions) (*messaging.RoomMessagesResponse, error) {
	return session.client.RoomMessages(ctx, roomID, options)
}

func (session *ProxySession) ThreadMessages(ctx context.Context, roomID ref.RoomID, threadRootID string, options messaging.ThreadMessagesOptions) (*messaging.ThreadMessagesResponse, error) {
	return session.client.ThreadMessages(ctx, roomID, threadRootID, options)
}

func (session *ProxySession) Sync(ctx context.Context, options messaging.SyncOptions) (*messaging.SyncResponse, error) {
	return session.client.Sync(ctx, options)
}
