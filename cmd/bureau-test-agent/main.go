// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// bureau-test-agent exercises the full Bureau stack from inside a sandbox.
// It validates proxy identity, Matrix authentication, room discovery, and
// bidirectional messaging — the minimal set of operations that prove a
// sandbox is fully functional.
//
// Environment variables (set via template variable expansion):
//
//	BUREAU_PROXY_SOCKET  — Unix socket path to the proxy
//	BUREAU_MACHINE_NAME  — machine localpart (e.g., "machine/workstation")
//	BUREAU_SERVER_NAME   — Matrix server name (e.g., "bureau.local")
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/lib/proxyclient"
	"github.com/bureau-foundation/bureau/messaging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "TEST_AGENT: FATAL: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Read required environment variables.
	proxySocket := os.Getenv("BUREAU_PROXY_SOCKET")
	if proxySocket == "" {
		return fmt.Errorf("BUREAU_PROXY_SOCKET not set")
	}
	machineName := os.Getenv("BUREAU_MACHINE_NAME")
	if machineName == "" {
		return fmt.Errorf("BUREAU_MACHINE_NAME not set")
	}
	serverName := os.Getenv("BUREAU_SERVER_NAME")
	if serverName == "" {
		return fmt.Errorf("BUREAU_SERVER_NAME not set")
	}

	proxy := proxyclient.New(proxySocket, serverName)

	// Step 1: Verify proxy identity.
	log("checking proxy identity...")
	identity, err := proxy.Identity(ctx)
	if err != nil {
		return fmt.Errorf("identity check: %w", err)
	}
	if identity.UserID == "" {
		return fmt.Errorf("identity returned empty user_id")
	}
	log("identity: user_id=%s", identity.UserID)

	// Create a Matrix session for all subsequent operations. The proxy
	// client is only needed for proxy-specific operations (Identity above).
	session := proxyclient.NewProxySession(proxy, identity.UserID)

	// Step 2: Verify Matrix authentication.
	log("checking Matrix whoami...")
	whoamiUserID, err := session.WhoAmI(ctx)
	if err != nil {
		return fmt.Errorf("whoami: %w", err)
	}
	log("whoami: user_id=%s", whoamiUserID)

	// Step 3: Resolve the config room.
	configAlias := fmt.Sprintf("#bureau/config/%s:%s", machineName, serverName)
	log("resolving config room %s...", configAlias)
	configRoomID, err := session.ResolveAlias(ctx, configAlias)
	if err != nil {
		return fmt.Errorf("resolve config room %s: %w", configAlias, err)
	}
	log("config room: %s", configRoomID)

	// Step 4: Read payload (if present) and send ready signal.
	payloadContent := readPayloadJSON()
	readyMessage := "quickstart-test-ready"
	if payloadContent != "" {
		readyMessage += ": payload=" + payloadContent
		log("payload at startup: %s", payloadContent)
	} else {
		log("no payload file found")
	}
	log("sending ready signal...")
	if _, err := session.SendMessage(ctx, configRoomID, messaging.NewTextMessage(readyMessage)); err != nil {
		return fmt.Errorf("send ready message: %w", err)
	}
	log("ready signal sent")

	// Step 5: Wait for an incoming message from an external user via /sync
	// long-polling. Skip messages from self and from the machine daemon —
	// the daemon posts operational messages (service directory updates,
	// policy changes) to the config room that are not intended for the agent.
	machineUserID := fmt.Sprintf("@%s:%s", machineName, serverName)
	log("waiting for incoming message...")
	message, err := waitForMessage(ctx, session, configRoomID, whoamiUserID, machineUserID)
	if err != nil {
		return fmt.Errorf("wait for message: %w", err)
	}
	log("received: %q", message)

	// Step 6: Re-read payload (may have been hot-reloaded) and acknowledge.
	currentPayload := readPayloadJSON()
	ackBody := fmt.Sprintf("quickstart-test-ok: received '%s'", message)
	if currentPayload != "" {
		ackBody += " payload=" + currentPayload
		log("payload at ack: %s", currentPayload)
	}
	log("sending acknowledgment...")
	if _, err := session.SendMessage(ctx, configRoomID, messaging.NewTextMessage(ackBody)); err != nil {
		return fmt.Errorf("send ack message: %w", err)
	}
	log("acknowledgment sent")

	log("all checks passed")
	return nil
}

func log(format string, args ...any) {
	fmt.Fprintf(os.Stdout, "TEST_AGENT: "+format+"\n", args...)
}

const payloadPath = "/run/bureau/payload.json"

// readPayloadJSON reads the payload file bind-mounted at /run/bureau/payload.json
// and returns its content as a compact JSON string. Returns "" if the file does
// not exist (no payload was configured for this principal).
func readPayloadJSON() string {
	data, err := os.ReadFile(payloadPath)
	if err != nil {
		return ""
	}
	// Re-marshal to ensure compact JSON (no extra whitespace).
	var parsed any
	if json.Unmarshal(data, &parsed) != nil {
		// Return raw content if it's not valid JSON — the test harness
		// will catch the malformed content in its assertions.
		return string(data)
	}
	compact, err := json.Marshal(parsed)
	if err != nil {
		return string(data)
	}
	return string(compact)
}

// waitForMessage uses Matrix /sync long-polling to wait for a message from
// an external user (someone other than the agent itself or the machine
// daemon). The homeserver holds each /sync request for up to 30 seconds,
// returning immediately when new events arrive. Returns the message body.
func waitForMessage(ctx context.Context, session messaging.Session, roomID, ownUserID, machineUserID string) (string, error) {
	filter := buildRoomMessageFilter(roomID)

	// Initial /sync with timeout=0 to capture the stream position.
	response, err := session.Sync(ctx, messaging.SyncOptions{
		Timeout:    0,
		SetTimeout: true,
		Filter:     filter,
	})
	if err != nil {
		return "", fmt.Errorf("initial sync: %w", err)
	}
	sinceToken := response.NextBatch

	// Long-poll until a message from an external user arrives.
	for {
		response, err := session.Sync(ctx, messaging.SyncOptions{
			Since:      sinceToken,
			Timeout:    30000, // 30s server-side hold
			SetTimeout: true,
			Filter:     filter,
		})
		if err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("timed out waiting for message in room %s", roomID)
			}
			log("sync error (retrying): %v", err)
			continue
		}
		sinceToken = response.NextBatch

		joined, ok := response.Rooms.Join[roomID]
		if !ok {
			continue
		}

		for _, event := range joined.Timeline.Events {
			if event.Type != "m.room.message" {
				continue
			}
			if event.Sender == ownUserID || event.Sender == machineUserID {
				continue
			}
			msgtype, _ := event.Content["msgtype"].(string)
			if msgtype != "m.text" {
				continue
			}
			body, _ := event.Content["body"].(string)
			if body != "" {
				return body, nil
			}
		}
	}
}

// buildRoomMessageFilter builds an inline JSON filter for /sync that
// restricts the response to m.room.message events in a single room.
func buildRoomMessageFilter(roomID string) string {
	filter := map[string]any{
		"room": map[string]any{
			"rooms": []string{roomID},
			"timeline": map[string]any{
				"types": []string{"m.room.message"},
				"limit": 50,
			},
			"state":        map[string]any{"types": []string{}},
			"ephemeral":    map[string]any{"types": []string{}},
			"account_data": map[string]any{"types": []string{}},
		},
		"presence":     map[string]any{"types": []string{}},
		"account_data": map[string]any{"types": []string{}},
	}
	data, err := json.Marshal(filter)
	if err != nil {
		panic("building room message filter: " + err.Error())
	}
	return string(data)
}
