// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

func mustRoomID(raw string) ref.RoomID {
	roomID, err := ref.ParseRoomID(raw)
	if err != nil {
		panic(fmt.Sprintf("invalid test room ID %q: %v", raw, err))
	}
	return roomID
}

// mockSignalingServer implements a minimal Matrix server for signaler tests.
// It stores state events keyed by (event_type, state_key) and serves them
// back via GET /state and PUT /state/{type}/{key}.
type mockSignalingServer struct {
	mu     sync.Mutex
	events map[string]map[string]json.RawMessage // event_type → state_key → content
}

func newMockSignalingServer() *mockSignalingServer {
	return &mockSignalingServer{
		events: make(map[string]map[string]json.RawMessage),
	}
}

func (m *mockSignalingServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Route: PUT /_matrix/client/v3/rooms/{roomID}/state/{eventType}/{stateKey}
	// Route: GET /_matrix/client/v3/rooms/{roomID}/state
	if request.Method == http.MethodPut {
		// Parse event type and state key from the raw path.
		// Path format: /_matrix/client/v3/rooms/!room:local/state/m.bureau.webrtc_offer/A|B
		rawPath := request.URL.RawPath
		if rawPath == "" {
			rawPath = request.URL.Path
		}

		// Find state/ in the path and split what follows.
		const statePrefix = "/state/"
		stateIndex := len(rawPath)
		for index := 0; index < len(rawPath)-len(statePrefix)+1; index++ {
			if rawPath[index:index+len(statePrefix)] == statePrefix {
				stateIndex = index + len(statePrefix)
				break
			}
		}

		remaining := rawPath[stateIndex:]
		slashIndex := -1
		for index := 0; index < len(remaining); index++ {
			if remaining[index] == '/' {
				slashIndex = index
				break
			}
		}

		var eventType, stateKey string
		if slashIndex >= 0 {
			eventType, _ = url.PathUnescape(remaining[:slashIndex])
			stateKey, _ = url.PathUnescape(remaining[slashIndex+1:])
		} else {
			eventType, _ = url.PathUnescape(remaining)
		}

		var content json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&content); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}

		if m.events[eventType] == nil {
			m.events[eventType] = make(map[string]json.RawMessage)
		}
		m.events[eventType][stateKey] = content

		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(`{"event_id":"$test"}`))
		return
	}

	if request.Method == http.MethodGet {
		// Return all state events.
		type stateEvent struct {
			Type     string          `json:"type"`
			StateKey *string         `json:"state_key"`
			Content  json.RawMessage `json:"content"`
		}

		var allEvents []stateEvent
		for eventType, stateKeys := range m.events {
			for stateKey, content := range stateKeys {
				keyCopy := stateKey
				allEvents = append(allEvents, stateEvent{
					Type:     eventType,
					StateKey: &keyCopy,
					Content:  content,
				})
			}
		}

		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(allEvents)
		return
	}

	http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
}

func newTestMatrixSignaler(t *testing.T, mock *mockSignalingServer) *MatrixSignaler {
	t.Helper()
	server := httptest.NewServer(mock)
	t.Cleanup(server.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: server.URL})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	testUserID, err := ref.ParseUserID("@machine/test:bureau.local")
	if err != nil {
		t.Fatalf("ParseUserID: %v", err)
	}
	session, err := client.SessionFromToken(testUserID, "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	return NewMatrixSignaler(session, mustRoomID("!machines:bureau.local"), nil)
}

func TestMatrixSignaler_PublishAndPollOffer(t *testing.T) {
	mock := newMockSignalingServer()
	signaler := newTestMatrixSignaler(t, mock)
	ctx := context.Background()

	// Publish an offer from machine-a to machine-b.
	if err := signaler.PublishOffer(ctx, "machine/a", "machine/b", "v=0\r\noffer-sdp"); err != nil {
		t.Fatalf("PublishOffer failed: %v", err)
	}

	// Verify the state event was stored with the correct key.
	mock.mu.Lock()
	offerEvents := mock.events[schema.EventTypeWebRTCOffer]
	if offerEvents == nil {
		t.Fatal("no offer events stored")
	}
	if _, ok := offerEvents["machine/a|machine/b"]; !ok {
		t.Errorf("offer not stored under expected state key 'machine/a|machine/b'")
	}
	mock.mu.Unlock()

	// Create a second signaler for machine-b and poll for offers.
	signalerB := newTestMatrixSignaler(t, mock)
	offers, err := signalerB.PollOffers(ctx, "machine/b")
	if err != nil {
		t.Fatalf("PollOffers failed: %v", err)
	}

	if len(offers) != 1 {
		t.Fatalf("expected 1 offer, got %d", len(offers))
	}
	if offers[0].PeerLocalpart != "machine/a" {
		t.Errorf("PeerLocalpart = %q, want %q", offers[0].PeerLocalpart, "machine/a")
	}
	if offers[0].SDP != "v=0\r\noffer-sdp" {
		t.Errorf("SDP = %q, want %q", offers[0].SDP, "v=0\r\noffer-sdp")
	}

	// Polling again should return nothing (already seen).
	offers, err = signalerB.PollOffers(ctx, "machine/b")
	if err != nil {
		t.Fatalf("second PollOffers failed: %v", err)
	}
	if len(offers) != 0 {
		t.Errorf("expected 0 offers on second poll, got %d", len(offers))
	}
}

func TestMatrixSignaler_PublishAndPollAnswer(t *testing.T) {
	mock := newMockSignalingServer()
	signaler := newTestMatrixSignaler(t, mock)
	ctx := context.Background()

	// Publish an answer from machine-b to machine-a's offer.
	if err := signaler.PublishAnswer(ctx, "machine/a", "machine/b", "v=0\r\nanswer-sdp"); err != nil {
		t.Fatalf("PublishAnswer failed: %v", err)
	}

	// Verify state key: offerer|answerer.
	mock.mu.Lock()
	answerEvents := mock.events[schema.EventTypeWebRTCAnswer]
	if answerEvents == nil {
		t.Fatal("no answer events stored")
	}
	if _, ok := answerEvents["machine/a|machine/b"]; !ok {
		t.Errorf("answer not stored under expected state key 'machine/a|machine/b'")
	}
	mock.mu.Unlock()

	// Machine-a polls for answers to its offers.
	signalerA := newTestMatrixSignaler(t, mock)
	answers, err := signalerA.PollAnswers(ctx, "machine/a")
	if err != nil {
		t.Fatalf("PollAnswers failed: %v", err)
	}

	if len(answers) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(answers))
	}
	if answers[0].PeerLocalpart != "machine/b" {
		t.Errorf("PeerLocalpart = %q, want %q", answers[0].PeerLocalpart, "machine/b")
	}
	if answers[0].SDP != "v=0\r\nanswer-sdp" {
		t.Errorf("SDP = %q, want %q", answers[0].SDP, "v=0\r\nanswer-sdp")
	}
}

func TestMatrixSignaler_IgnoresOtherEventTypes(t *testing.T) {
	mock := newMockSignalingServer()
	ctx := context.Background()

	// Inject a non-WebRTC event directly into the mock.
	mock.mu.Lock()
	mock.events["m.bureau.machine_status"] = map[string]json.RawMessage{
		"machine/a|machine/b": json.RawMessage(`{"status":"online"}`),
	}
	mock.mu.Unlock()

	signaler := newTestMatrixSignaler(t, mock)
	offers, err := signaler.PollOffers(ctx, "machine/b")
	if err != nil {
		t.Fatalf("PollOffers failed: %v", err)
	}
	if len(offers) != 0 {
		t.Errorf("expected 0 offers (non-WebRTC event should be ignored), got %d", len(offers))
	}
}

func TestMatrixSignaler_IgnoresOffersForOtherTargets(t *testing.T) {
	mock := newMockSignalingServer()
	signaler := newTestMatrixSignaler(t, mock)
	ctx := context.Background()

	// Publish an offer from A to C (not to B).
	if err := signaler.PublishOffer(ctx, "machine/a", "machine/c", "v=0\r\noffer-for-c"); err != nil {
		t.Fatalf("PublishOffer failed: %v", err)
	}

	// Machine B should see no offers.
	signalerB := newTestMatrixSignaler(t, mock)
	offers, err := signalerB.PollOffers(ctx, "machine/b")
	if err != nil {
		t.Fatalf("PollOffers failed: %v", err)
	}
	if len(offers) != 0 {
		t.Errorf("expected 0 offers for machine/b, got %d", len(offers))
	}
}
