// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// newDrainTestDaemon creates a daemon wired to a mock Matrix server with
// the ops room configured for drain coordination testing. Returns the
// daemon and mock state for test control. The daemon has a fake clock
// at a fixed epoch so DrainedAt timestamps are deterministic.
func newDrainTestDaemon(t *testing.T) (*Daemon, *mockMatrixState) {
	t.Helper()

	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	machine, fleetRef := testMachineSetup(t, "testmachine", "bureau.local")

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := matrixClient.SessionFromToken(machine.UserID(), "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon, _ := newTestDaemon(t)
	daemon.session = session
	daemon.machine = machine
	daemon.fleet = fleetRef
	daemon.configRoomID = mustRoomID("!config:test")
	daemon.machineRoomID = mustRoomID("!machine:test")
	daemon.serviceRoomID = mustRoomID("!service:test")
	daemon.fleetRoomID = mustRoomID("!fleet:test")
	daemon.opsRoomID = mustRoomID("!ops:test")
	daemon.logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

	return daemon, matrixState
}

// TestProcessMachineDrainPublishesDrainStatus verifies that when a drain
// event is active in the ops room, the daemon publishes m.bureau.drain_status
// with the correct in-flight count and state key.
func TestProcessMachineDrainPublishesDrainStatus(t *testing.T) {
	t.Parallel()

	daemon, matrixState := newDrainTestDaemon(t)
	ctx := context.Background()

	// Set up an active drain in the ops room.
	holder, err := ref.ParseEntityUserID("@bureau/fleet/test/agent/benchmark:bureau.local")
	if err != nil {
		t.Fatalf("ParseEntity: %v", err)
	}

	drainContent := schema.MachineDrainContent{
		Services:          []ref.UserID{ref.MustParseUserID("@bureau/fleet/test/service/stt/whisper:bureau.local")},
		ReservationHolder: holder,
		RequestedAt:       "2026-03-01T12:00:00Z",
	}
	matrixState.setStateEvent(daemon.opsRoomID.String(),
		schema.EventTypeMachineDrain, "", drainContent)

	// No running sandboxes — in-flight should be 0.
	daemon.processMachineDrain(ctx)

	// Read back the drain_status the daemon published.
	stateKey := daemon.machine.UserID().StateKey()
	var status schema.DrainStatusContent
	readDrainStatus(t, matrixState, daemon.opsRoomID.String(), stateKey, &status)

	if !status.Acknowledged {
		t.Error("drain_status should be acknowledged")
	}
	if status.InFlight != 0 {
		t.Errorf("InFlight = %d, want 0", status.InFlight)
	}
	expectedDrainedAt := daemon.clock.Now().UTC().Format(time.RFC3339)
	if status.DrainedAt != expectedDrainedAt {
		t.Errorf("DrainedAt = %q, want %q", status.DrainedAt, expectedDrainedAt)
	}
}

// TestProcessMachineDrainReportsInFlightCount verifies that when sandboxes
// are running, the daemon reports the correct in-flight count and does NOT
// set DrainedAt.
func TestProcessMachineDrainReportsInFlightCount(t *testing.T) {
	t.Parallel()

	daemon, matrixState := newDrainTestDaemon(t)
	ctx := context.Background()

	// Set up an active drain.
	holder, err := ref.ParseEntityUserID("@bureau/fleet/test/agent/benchmark:bureau.local")
	if err != nil {
		t.Fatalf("ParseEntity: %v", err)
	}
	matrixState.setStateEvent(daemon.opsRoomID.String(),
		schema.EventTypeMachineDrain, "", schema.MachineDrainContent{
			Services:          []ref.UserID{ref.MustParseUserID("@bureau/fleet/test/service/stt/whisper:bureau.local")},
			ReservationHolder: holder,
			RequestedAt:       "2026-03-01T12:00:00Z",
		})

	// Simulate running sandboxes by placing entries in the lifecycle map.
	principal1 := testEntity(t, daemon.fleet, "agent/worker-1")
	principal2 := testEntity(t, daemon.fleet, "agent/worker-2")
	daemon.lifecycle[principal1] = principalLifecycle{phase: phaseRunning}
	daemon.lifecycle[principal2] = principalLifecycle{phase: phaseRunning}

	daemon.processMachineDrain(ctx)

	stateKey := daemon.machine.UserID().StateKey()
	var status schema.DrainStatusContent
	readDrainStatus(t, matrixState, daemon.opsRoomID.String(), stateKey, &status)

	if !status.Acknowledged {
		t.Error("drain_status should be acknowledged")
	}
	if status.InFlight != 2 {
		t.Errorf("InFlight = %d, want 2", status.InFlight)
	}
	if status.DrainedAt != "" {
		t.Errorf("DrainedAt should be empty when InFlight > 0, got %q", status.DrainedAt)
	}
}

// TestProcessMachineDrainClearedPublishesClear verifies that when the drain
// event is cleared (empty content or zero holder), the daemon publishes a
// cleared drain_status.
func TestProcessMachineDrainClearedPublishesClear(t *testing.T) {
	t.Parallel()

	daemon, matrixState := newDrainTestDaemon(t)
	ctx := context.Background()

	// Set up a cleared drain (empty JSON object — ReservationHolder is zero).
	matrixState.setStateEvent(daemon.opsRoomID.String(),
		schema.EventTypeMachineDrain, "", json.RawMessage("{}"))

	daemon.processMachineDrain(ctx)

	// The daemon should publish a cleared drain_status (empty JSON object).
	stateKey := daemon.machine.UserID().StateKey()
	raw := readRawStateEvent(t, matrixState, daemon.opsRoomID.String(),
		string(schema.EventTypeDrainStatus), stateKey)

	// Cleared drain_status is "{}" — an empty JSON object.
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("Unmarshal drain_status: %v", err)
	}
	if len(parsed) != 0 {
		t.Errorf("cleared drain_status should be empty object, got %v", parsed)
	}
}

// TestProcessMachineDrainNoDrainEvent verifies that when no drain event
// exists in the ops room (M_NOT_FOUND), the daemon does nothing — no
// drain_status is published.
func TestProcessMachineDrainNoDrainEvent(t *testing.T) {
	t.Parallel()

	daemon, matrixState := newDrainTestDaemon(t)
	ctx := context.Background()

	// Don't set any drain event. processMachineDrain should gracefully
	// handle the M_NOT_FOUND error.
	daemon.processMachineDrain(ctx)

	// Verify no drain_status was published.
	stateKey := daemon.machine.UserID().StateKey()
	raw := readRawStateEvent(t, matrixState, daemon.opsRoomID.String(),
		string(schema.EventTypeDrainStatus), stateKey)
	if raw != nil {
		t.Errorf("no drain_status should be published when no drain event exists, got %s", raw)
	}
}

// TestProcessMachineDrainViaSync verifies the full sync path: a drain
// event arriving via /sync in the ops room triggers processMachineDrain.
func TestProcessMachineDrainViaSync(t *testing.T) {
	t.Parallel()

	daemon, matrixState := newDrainTestDaemon(t)
	ctx := context.Background()

	// Set up the drain event both in state (for GetStateEvent) and as
	// a sync event (for processSyncResponse to detect the ops room change).
	holder, err := ref.ParseEntityUserID("@bureau/fleet/test/agent/benchmark:bureau.local")
	if err != nil {
		t.Fatalf("ParseEntity: %v", err)
	}
	drainContent := schema.MachineDrainContent{
		Services:          []ref.UserID{ref.MustParseUserID("@bureau/fleet/test/service/stt/whisper:bureau.local")},
		ReservationHolder: holder,
		RequestedAt:       "2026-03-01T12:00:00Z",
	}
	matrixState.setStateEvent(daemon.opsRoomID.String(),
		schema.EventTypeMachineDrain, "", drainContent)

	// Build a sync response with the ops room having a state change.
	drainStateKey := ""
	drainContentMap := mustContentMap(drainContent)
	syncResponse := &messaging.SyncResponse{
		Rooms: messaging.RoomsSection{
			Join: map[ref.RoomID]messaging.JoinedRoom{
				daemon.opsRoomID: {
					Timeline: messaging.TimelineSection{
						Events: []messaging.Event{
							{
								Type:     schema.EventTypeMachineDrain,
								StateKey: &drainStateKey,
								Content:  drainContentMap,
								Sender:   mustParseUserID("@bureau/fleet/test/service/fleet-controller:bureau.local"),
							},
						},
					},
				},
			},
		},
	}

	daemon.processSyncResponse(ctx, syncResponse)

	// processSyncResponse should have called processMachineDrain, which
	// publishes a drain_status.
	stateKey := daemon.machine.UserID().StateKey()
	var status schema.DrainStatusContent
	readDrainStatus(t, matrixState, daemon.opsRoomID.String(), stateKey, &status)

	if !status.Acknowledged {
		t.Error("drain_status should be acknowledged after sync")
	}
}

// mustContentMap marshals a value to JSON and unmarshals to map[string]any,
// matching the in-memory representation of Matrix event content.
func mustContentMap(value any) map[string]any {
	data, err := json.Marshal(value)
	if err != nil {
		panic("mustContentMap marshal: " + err.Error())
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		panic("mustContentMap unmarshal: " + err.Error())
	}
	return result
}

// readDrainStatus reads a drain_status state event from the mock and
// unmarshals it into the provided DrainStatusContent.
func readDrainStatus(t *testing.T, state *mockMatrixState, roomID, stateKey string, out *schema.DrainStatusContent) {
	t.Helper()
	raw := readRawStateEvent(t, state, roomID, string(schema.EventTypeDrainStatus), stateKey)
	if raw == nil {
		t.Fatal("drain_status state event not found in mock")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal drain_status: %v", err)
	}
}

// readRawStateEvent reads a raw state event from the mock. Returns nil
// if the event does not exist.
func readRawStateEvent(t *testing.T, state *mockMatrixState, roomID, eventType, stateKey string) json.RawMessage {
	t.Helper()
	state.mu.Lock()
	defer state.mu.Unlock()
	key := roomID + "\x00" + eventType + "\x00" + stateKey
	return state.stateEvents[key]
}
