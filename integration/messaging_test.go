// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestTwoAgentMessaging verifies end-to-end messaging between two sandboxed
// principals. Each principal has its own proxy that injects the correct Matrix
// credentials. The test proves: credential encryption and delivery for multiple
// principals on the same machine, proxy credential isolation (each proxy injects
// only its own token), and bidirectional message exchange through the Matrix
// homeserver via the proxy HTTP service.
func TestTwoAgentMessaging(t *testing.T) {
	const machineName = "machine/messaging"
	const aliceLocalpart = "test/alice"
	const bobLocalpart = "test/bob"
	machineUserID := "@machine/messaging:" + testServerName
	aliceUserID := "@test/alice:" + testServerName
	bobUserID := "@test/bob:" + testServerName

	launcherBinary := resolvedBinary(t, "LAUNCHER_BINARY")
	daemonBinary := resolvedBinary(t, "DAEMON_BINARY")
	proxyBinary := resolvedBinary(t, "PROXY_BINARY")

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	// Resolve global rooms and invite the machine.
	machinesRoomID, err := admin.ResolveAlias(ctx, "#bureau/machines:"+testServerName)
	if err != nil {
		t.Fatalf("resolve machines room: %v", err)
	}
	servicesRoomID, err := admin.ResolveAlias(ctx, "#bureau/services:"+testServerName)
	if err != nil {
		t.Fatalf("resolve services room: %v", err)
	}

	if err := admin.InviteUser(ctx, machinesRoomID, machineUserID); err != nil {
		t.Fatalf("invite machine to machines room: %v", err)
	}
	if err := admin.InviteUser(ctx, servicesRoomID, machineUserID); err != nil {
		t.Fatalf("invite machine to services room: %v", err)
	}

	// Create isolated directories.
	stateDir := t.TempDir()
	socketDir := tempSocketDir(t)

	launcherSocket := filepath.Join(socketDir, "launcher.sock")
	tmuxSocket := filepath.Join(socketDir, "tmux.sock")
	observeSocket := filepath.Join(socketDir, "observe.sock")
	relaySocket := filepath.Join(socketDir, "relay.sock")
	principalSocketBase := filepath.Join(socketDir, "p") + "/"
	adminSocketBase := filepath.Join(socketDir, "a") + "/"
	os.MkdirAll(principalSocketBase, 0755)
	os.MkdirAll(adminSocketBase, 0755)

	tokenFile := filepath.Join(stateDir, "reg-token")
	if err := os.WriteFile(tokenFile, []byte(testRegistrationToken), 0600); err != nil {
		t.Fatalf("write registration token: %v", err)
	}

	launcherWorkspaceRoot := filepath.Join(stateDir, "workspace")

	// Start the launcher.
	startProcess(t, "launcher", launcherBinary,
		"--homeserver", testHomeserverURL,
		"--registration-token-file", tokenFile,
		"--machine-name", machineName,
		"--server-name", testServerName,
		"--state-dir", stateDir,
		"--socket", launcherSocket,
		"--tmux-socket", tmuxSocket,
		"--socket-base-path", principalSocketBase,
		"--admin-base-path", adminSocketBase,
		"--workspace-root", launcherWorkspaceRoot,
		"--proxy-binary", proxyBinary,
	)

	waitForFile(t, launcherSocket, 15*time.Second)

	// Get the machine's public key for credential encryption.
	machineKeyJSON := waitForStateEvent(t, admin, machinesRoomID,
		"m.bureau.machine_key", machineName, 10*time.Second)
	var machineKey struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(machineKeyJSON, &machineKey); err != nil {
		t.Fatalf("unmarshal machine key: %v", err)
	}

	// Start the daemon.
	startProcess(t, "daemon", daemonBinary,
		"--homeserver", testHomeserverURL,
		"--machine-name", machineName,
		"--server-name", testServerName,
		"--state-dir", stateDir,
		"--launcher-socket", launcherSocket,
		"--observe-socket", observeSocket,
		"--relay-socket", relaySocket,
		"--tmux-socket", tmuxSocket,
		"--admin-user", "bureau-admin",
		"--admin-base-path", adminSocketBase,
		"--status-interval", "2s",
	)

	// Wait for daemon readiness.
	waitForStateEvent(t, admin, machinesRoomID,
		"m.bureau.machine_status", machineName, 15*time.Second)

	// Resolve the config room.
	configAlias := "#bureau/config/" + machineName + ":" + testServerName
	configRoomID, err := admin.ResolveAlias(ctx, configAlias)
	if err != nil {
		t.Fatalf("config room not created: %v", err)
	}
	if _, err := admin.JoinRoom(ctx, configRoomID); err != nil {
		t.Fatalf("admin join config room: %v", err)
	}

	// Register both principal accounts on the homeserver.
	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: testHomeserverURL,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	aliceSession, err := matrixClient.Register(ctx, messaging.RegisterRequest{
		Username:          aliceLocalpart,
		Password:          "alice-test-password",
		RegistrationToken: testRegistrationToken,
	})
	if err != nil {
		t.Fatalf("register alice: %v", err)
	}
	aliceToken := aliceSession.AccessToken()
	aliceSession.Close()

	bobSession, err := matrixClient.Register(ctx, messaging.RegisterRequest{
		Username:          bobLocalpart,
		Password:          "bob-test-password",
		RegistrationToken: testRegistrationToken,
	})
	if err != nil {
		t.Fatalf("register bob: %v", err)
	}
	bobToken := bobSession.AccessToken()
	bobSession.Close()

	// Encrypt and push credentials for both principals.
	for _, principal := range []struct {
		localpart string
		userID    string
		token     string
	}{
		{aliceLocalpart, aliceUserID, aliceToken},
		{bobLocalpart, bobUserID, bobToken},
	} {
		credentialBundle := map[string]string{
			"MATRIX_TOKEN":          principal.token,
			"MATRIX_USER_ID":        principal.userID,
			"MATRIX_HOMESERVER_URL": testHomeserverURL,
		}
		credentialJSON, err := json.Marshal(credentialBundle)
		if err != nil {
			t.Fatalf("marshal credentials for %s: %v", principal.localpart, err)
		}
		ciphertext, err := sealed.Encrypt(credentialJSON, []string{machineKey.PublicKey})
		if err != nil {
			t.Fatalf("encrypt credentials for %s: %v", principal.localpart, err)
		}
		_, err = admin.SendStateEvent(ctx, configRoomID, "m.bureau.credentials", principal.localpart, map[string]any{
			"version":        1,
			"principal":      principal.userID,
			"encrypted_for":  []string{machineUserID},
			"keys":           []string{"MATRIX_TOKEN", "MATRIX_USER_ID", "MATRIX_HOMESERVER_URL"},
			"ciphertext":     ciphertext,
			"provisioned_by": "@bureau-admin:" + testServerName,
			"provisioned_at": time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			t.Fatalf("push credentials for %s: %v", principal.localpart, err)
		}
	}

	// Push MachineConfig with both principals. AllowJoin enables them to
	// accept room invitations through their proxy.
	_, err = admin.SendStateEvent(ctx, configRoomID, "m.bureau.machine_config", machineName, map[string]any{
		"principals": []map[string]any{
			{
				"localpart":  aliceLocalpart,
				"template":   "",
				"auto_start": true,
				"matrix_policy": map[string]any{
					"allow_join": true,
				},
			},
			{
				"localpart":  bobLocalpart,
				"template":   "",
				"auto_start": true,
				"matrix_policy": map[string]any{
					"allow_join": true,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("push machine config: %v", err)
	}

	// Wait for both proxy sockets. Each socket's appearance signals that
	// the launcher decrypted that principal's credentials, spawned a proxy
	// process, and the proxy registered its Matrix HTTP service.
	aliceProxySocket := principalSocketBase + aliceLocalpart + ".sock"
	bobProxySocket := principalSocketBase + bobLocalpart + ".sock"
	waitForFile(t, aliceProxySocket, 15*time.Second)
	waitForFile(t, bobProxySocket, 15*time.Second)

	// Build HTTP clients for each proxy.
	aliceHTTP := proxyHTTPClient(aliceProxySocket)
	bobHTTP := proxyHTTPClient(bobProxySocket)

	// Create a shared room and invite both principals. The admin creates
	// the room because agents can't (MatrixPolicy doesn't grant
	// AllowRoomCreate, and createRoom isn't in the proxy's endpoint
	// allowlist). This mirrors production: admins or coordinator agents
	// create rooms and invite participants.
	chatRoom, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:   "Alice-Bob Test Chat",
		Preset: "private_chat",
		Invite: []string{aliceUserID, bobUserID},
	})
	if err != nil {
		t.Fatalf("create chat room: %v", err)
	}
	chatRoomID := chatRoom.RoomID
	t.Logf("created chat room %s", chatRoomID)

	// --- Sub-test: verify proxy credential isolation ---
	t.Run("ProxyIdentity", func(t *testing.T) {
		aliceIdentity := proxyWhoami(t, aliceHTTP)
		if aliceIdentity != aliceUserID {
			t.Errorf("alice whoami = %q, want %q", aliceIdentity, aliceUserID)
		}
		bobIdentity := proxyWhoami(t, bobHTTP)
		if bobIdentity != bobUserID {
			t.Errorf("bob whoami = %q, want %q", bobIdentity, bobUserID)
		}
	})

	// --- Sub-test: agents join through their proxies ---
	t.Run("JoinRoom", func(t *testing.T) {
		proxyJoinRoom(t, aliceHTTP, chatRoomID)
		proxyJoinRoom(t, bobHTTP, chatRoomID)
	})

	// --- Sub-test: bidirectional message exchange ---
	t.Run("MessageExchange", func(t *testing.T) {
		// Alice sends a message through her proxy.
		aliceEventID := proxySendMessage(t, aliceHTTP, chatRoomID, "Hello from Alice")
		t.Logf("alice sent message: %s", aliceEventID)

		// Bob sends a reply through his proxy.
		bobEventID := proxySendMessage(t, bobHTTP, chatRoomID, "Hello from Bob")
		t.Logf("bob sent message: %s", bobEventID)

		// Alice reads the conversation through her proxy via /sync.
		// Both messages should be visible: her own and Bob's.
		aliceEvents := proxySyncRoomTimeline(t, aliceHTTP, chatRoomID)
		assertMessagePresent(t, aliceEvents, aliceUserID, "Hello from Alice")
		assertMessagePresent(t, aliceEvents, bobUserID, "Hello from Bob")

		// Bob reads the conversation through his proxy.
		bobEvents := proxySyncRoomTimeline(t, bobHTTP, chatRoomID)
		assertMessagePresent(t, bobEvents, aliceUserID, "Hello from Alice")
		assertMessagePresent(t, bobEvents, bobUserID, "Hello from Bob")
	})
}
