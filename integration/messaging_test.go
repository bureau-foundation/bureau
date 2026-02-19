// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestTwoAgentMessaging verifies end-to-end messaging between two sandboxed
// principals. Each principal has its own proxy that injects the correct Matrix
// credentials. The test proves: credential encryption and delivery for multiple
// principals on the same machine, proxy credential isolation (each proxy injects
// only its own token), and bidirectional message exchange through the Matrix
// homeserver via the proxy HTTP service.
func TestTwoAgentMessaging(t *testing.T) {
	t.Parallel()

	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	machine := newTestMachine(t, fleet, "messaging")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	alice := registerPrincipal(t, "test/alice", "alice-test-password")
	bob := registerPrincipal(t, "test/bob", "bob-test-password")

	proxySockets := deployPrincipals(t, admin, machine, deploymentConfig{
		Principals: []principalSpec{
			{Account: alice, MatrixPolicy: &schema.MatrixPolicy{AllowJoin: true}},
			{Account: bob, MatrixPolicy: &schema.MatrixPolicy{AllowJoin: true}},
		},
	})

	aliceHTTP := proxyHTTPClient(proxySockets[alice.Localpart])
	bobHTTP := proxyHTTPClient(proxySockets[bob.Localpart])

	// --- Sub-test: verify proxy credential isolation ---
	t.Run("ProxyIdentity", func(t *testing.T) {
		aliceIdentity := proxyWhoami(t, aliceHTTP)
		if aliceIdentity != alice.UserID {
			t.Errorf("alice whoami = %q, want %q", aliceIdentity, alice.UserID)
		}
		bobIdentity := proxyWhoami(t, bobHTTP)
		if bobIdentity != bob.UserID {
			t.Errorf("bob whoami = %q, want %q", bobIdentity, bob.UserID)
		}
	})

	// --- Sub-test: agents join through their proxies ---
	t.Run("JoinRoom", func(t *testing.T) {
		// Admin creates the room because agents don't have a
		// matrix/create-room grant. This mirrors production: admins or
		// coordinator agents create rooms and invite participants.
		chatRoom, err := admin.CreateRoom(t.Context(), messaging.CreateRoomRequest{
			Name:   "Alice-Bob Test Chat",
			Preset: "private_chat",
			Invite: []string{alice.UserID, bob.UserID},
		})
		if err != nil {
			t.Fatalf("create chat room: %v", err)
		}
		chatRoomID := chatRoom.RoomID
		t.Logf("created chat room %s", chatRoomID)

		proxyJoinRoom(t, aliceHTTP, chatRoomID)
		proxyJoinRoom(t, bobHTTP, chatRoomID)

		// --- Sub-test: bidirectional message exchange ---
		t.Run("MessageExchange", func(t *testing.T) {
			aliceEventID := proxySendMessage(t, aliceHTTP, chatRoomID, "Hello from Alice")
			t.Logf("alice sent message: %s", aliceEventID)

			bobEventID := proxySendMessage(t, bobHTTP, chatRoomID, "Hello from Bob")
			t.Logf("bob sent message: %s", bobEventID)

			// Alice reads the conversation — both messages should be visible.
			// No sleep needed: proxySendMessage returned 200 OK with event_id,
			// meaning the homeserver has persisted the events.
			aliceEvents := proxySyncRoomTimeline(t, aliceHTTP, chatRoomID)
			assertMessagePresent(t, aliceEvents, alice.UserID, "Hello from Alice")
			assertMessagePresent(t, aliceEvents, bob.UserID, "Hello from Bob")

			// Bob reads the conversation.
			bobEvents := proxySyncRoomTimeline(t, bobHTTP, chatRoomID)
			assertMessagePresent(t, bobEvents, alice.UserID, "Hello from Alice")
			assertMessagePresent(t, bobEvents, bob.UserID, "Hello from Bob")
		})
	})
}
