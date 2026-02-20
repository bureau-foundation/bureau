// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxyclient

import (
	"context"
	"encoding/json"
	"fmt"

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
	userID string
}

// Compile-time check: *ProxySession implements messaging.Session.
var _ messaging.Session = (*ProxySession)(nil)

// NewProxySession creates a ProxySession wrapping the given Client.
// The userID should be the fully-qualified Matrix user ID for this
// principal, typically obtained from Client.Identity or Client.Whoami.
func NewProxySession(client *Client, userID string) *ProxySession {
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

func (session *ProxySession) UserID() string {
	return session.userID
}

// Close is a no-op for ProxySession. The proxy manages credential
// lifecycle — the sandboxed process does not hold any access tokens.
func (session *ProxySession) Close() error {
	return nil
}

func (session *ProxySession) WhoAmI(ctx context.Context) (string, error) {
	return session.client.Whoami(ctx)
}

func (session *ProxySession) ResolveAlias(ctx context.Context, alias string) (ref.RoomID, error) {
	raw, err := session.client.ResolveAlias(ctx, alias)
	if err != nil {
		return ref.RoomID{}, err
	}
	roomID, err := ref.ParseRoomID(raw)
	if err != nil {
		return ref.RoomID{}, fmt.Errorf("proxy: resolve alias returned invalid room ID %q: %w", raw, err)
	}
	return roomID, nil
}

func (session *ProxySession) GetStateEvent(ctx context.Context, roomID ref.RoomID, eventType, stateKey string) (json.RawMessage, error) {
	return session.client.GetState(ctx, roomID.String(), eventType, stateKey)
}

func (session *ProxySession) GetRoomState(ctx context.Context, roomID ref.RoomID) ([]messaging.Event, error) {
	return session.client.GetRoomState(ctx, roomID.String())
}

func (session *ProxySession) SendStateEvent(ctx context.Context, roomID ref.RoomID, eventType, stateKey string, content any) (string, error) {
	return session.client.PutState(ctx, PutStateRequest{
		Room:      roomID.String(),
		EventType: eventType,
		StateKey:  stateKey,
		Content:   content,
	})
}

func (session *ProxySession) SendEvent(ctx context.Context, roomID ref.RoomID, eventType string, content any) (string, error) {
	return session.client.SendEvent(ctx, roomID.String(), eventType, content)
}

func (session *ProxySession) SendMessage(ctx context.Context, roomID ref.RoomID, content messaging.MessageContent) (string, error) {
	return session.client.SendMessage(ctx, roomID.String(), content)
}

func (session *ProxySession) CreateRoom(ctx context.Context, request messaging.CreateRoomRequest) (*messaging.CreateRoomResponse, error) {
	return session.client.CreateRoom(ctx, request)
}

func (session *ProxySession) InviteUser(ctx context.Context, roomID ref.RoomID, userID string) error {
	return session.client.InviteUser(ctx, roomID.String(), userID)
}

func (session *ProxySession) JoinRoom(ctx context.Context, roomID ref.RoomID) (ref.RoomID, error) {
	raw, err := session.client.JoinRoom(ctx, roomID.String())
	if err != nil {
		return ref.RoomID{}, err
	}
	result, err := ref.ParseRoomID(raw)
	if err != nil {
		return ref.RoomID{}, fmt.Errorf("proxy: join room returned invalid room ID %q: %w", raw, err)
	}
	return result, nil
}

func (session *ProxySession) JoinedRooms(ctx context.Context) ([]ref.RoomID, error) {
	rawIDs, err := session.client.JoinedRooms(ctx)
	if err != nil {
		return nil, err
	}
	roomIDs := make([]ref.RoomID, len(rawIDs))
	for index, raw := range rawIDs {
		roomIDs[index], err = ref.ParseRoomID(raw)
		if err != nil {
			return nil, fmt.Errorf("proxy: joined rooms returned invalid room ID %q: %w", raw, err)
		}
	}
	return roomIDs, nil
}

func (session *ProxySession) GetRoomMembers(ctx context.Context, roomID ref.RoomID) ([]messaging.RoomMember, error) {
	return session.client.GetRoomMembers(ctx, roomID.String())
}

func (session *ProxySession) GetDisplayName(ctx context.Context, userID string) (string, error) {
	return session.client.GetDisplayName(ctx, userID)
}

func (session *ProxySession) RoomMessages(ctx context.Context, roomID ref.RoomID, options messaging.RoomMessagesOptions) (*messaging.RoomMessagesResponse, error) {
	return session.client.RoomMessages(ctx, roomID.String(), options)
}

func (session *ProxySession) ThreadMessages(ctx context.Context, roomID ref.RoomID, threadRootID string, options messaging.ThreadMessagesOptions) (*messaging.ThreadMessagesResponse, error) {
	return session.client.ThreadMessages(ctx, roomID.String(), threadRootID, options)
}

func (session *ProxySession) Sync(ctx context.Context, options messaging.SyncOptions) (*messaging.SyncResponse, error) {
	return session.client.Sync(ctx, options)
}
