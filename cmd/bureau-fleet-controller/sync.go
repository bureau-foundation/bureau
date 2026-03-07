// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/messaging"
)

// syncFilter restricts the /sync response to event types the fleet
// controller cares about. Built from typed constants so that event type
// renames are caught at compile time.
//
// The timeline section includes the same types as the state section
// because state events can appear as timeline events during incremental
// sync. The limit is generous since the fleet controller needs to see
// all fleet and machine mutations.
var syncFilter = buildSyncFilter()

// buildSyncFilter constructs the Matrix /sync filter JSON from typed
// schema constants.
func buildSyncFilter() string {
	stateEventTypes := []ref.EventType{
		schema.EventTypeFleetService,
		schema.EventTypeMachineDefinition,
		schema.EventTypeFleetConfig,
		schema.EventTypeHALease,
		schema.EventTypeMachineStatus,
		schema.EventTypeMachineInfo,
		schema.EventTypeService,
		schema.EventTypeServiceStatus,
		schema.EventTypeMachineConfig,
		schema.EventTypeFleetAlert,
		schema.EventTypeTicket,
		schema.EventTypeTicketConfig,
		schema.EventTypeRelayLink,
		schema.EventTypeReservation,
		schema.EventTypeMachineDrain,
		schema.EventTypeDrainStatus,
		schema.EventTypeRelayPolicy,
	}

	// Timeline includes the same state event types (state events can
	// appear as timeline events with a non-nil state_key during
	// incremental sync).
	timelineEventTypes := make([]ref.EventType, len(stateEventTypes))
	copy(timelineEventTypes, stateEventTypes)

	emptyTypes := []string{}

	filter := map[string]any{
		"room": map[string]any{
			"state": map[string]any{
				"types": stateEventTypes,
			},
			"timeline": map[string]any{
				"types": timelineEventTypes,
				"limit": 100,
			},
			"ephemeral": map[string]any{
				"types": emptyTypes,
			},
			"account_data": map[string]any{
				"types": emptyTypes,
			},
		},
		"presence": map[string]any{
			"types": []string{"m.presence"},
		},
		"account_data": map[string]any{
			"types": emptyTypes,
		},
	}

	data, err := json.Marshal(filter)
	if err != nil {
		panic("building sync filter: " + err.Error())
	}
	return string(data)
}

// machineState tracks a machine's latest info and status from Matrix
// state events. Updated on each /sync response.
type machineState struct {
	info   *schema.MachineInfo
	status *schema.MachineStatus

	// assignments tracks fleet-managed PrincipalAssignment events on
	// this machine. Keyed by the assignment's principal user ID.
	assignments map[ref.UserID]*schema.PrincipalAssignment

	// configRoomID is the machine's config room ID, used for writing
	// PrincipalAssignment events.
	configRoomID ref.RoomID

	// pendingEchoEventID, when non-empty, holds the Matrix event ID
	// returned by the most recent writeMachineConfig call for this
	// machine. While set, processMachineConfigEvent skips /sync
	// events that are not the echo of that write, preventing stale
	// sync data from overwriting optimistic local updates made by
	// place() or unplace(). Cleared when the echo arrives.
	pendingEchoEventID ref.EventID

	// lastHeartbeat is the time the fleet controller last received a
	// MachineStatus event for this machine. Used for staleness
	// detection in checkMachineHealth.
	lastHeartbeat time.Time

	// healthState tracks the machine's current health: "online",
	// "suspect", or "offline".
	healthState string

	// presenceState is the most recent m.presence state for this
	// machine's daemon user. "online", "unavailable", or "offline".
	// Used as a fast-path liveness signal alongside heartbeat
	// staleness: when the homeserver reports a daemon offline (TCP
	// connection dropped), the fleet controller can suspect the
	// machine immediately rather than waiting for heartbeat expiry.
	// Empty if no presence event has been received.
	presenceState string
}

// fleetServiceState tracks a fleet service definition and its current
// instances across the fleet.
type fleetServiceState struct {
	definition *fleet.FleetServiceContent

	// instances maps machine user IDs to the PrincipalAssignment
	// the fleet controller wrote for this service on that machine.
	instances map[ref.UserID]*schema.PrincipalAssignment
}

// initialSync performs the first /sync and builds the fleet model from
// current room state. Returns the since token for incremental sync.
func (fc *FleetController) initialSync(ctx context.Context) (string, error) {
	sinceToken, response, err := service.InitialSync(ctx, fc.session, syncFilter)
	if err != nil {
		return "", err
	}

	fc.logger.Info("initial sync complete",
		"next_batch", sinceToken,
		"joined_rooms", len(response.Rooms.Join),
		"pending_invites", len(response.Rooms.Invite),
	)

	// Accept pending invites. The fleet controller may have been
	// invited to config rooms while it was offline.
	acceptedRooms := service.AcceptInvites(ctx, fc.session, response.Rooms.Invite, fc.logger)

	// Build the fleet model from all joined rooms' state.
	for roomID, room := range response.Rooms.Join {
		fc.processRoomState(roomID, room.State.Events, room.Timeline.Events)
	}

	// Accepted rooms don't appear in Rooms.Join until the next /sync
	// batch. Fetch their full state directly so they are modeled
	// before the socket opens.
	for _, roomID := range acceptedRooms {
		events, err := fc.session.GetRoomState(ctx, roomID)
		if err != nil {
			fc.logger.Error("failed to fetch state for accepted room",
				"room_id", roomID,
				"error", err,
			)
			continue
		}
		fc.processRoomState(roomID, events, nil)
	}

	// Cross-reference machine assignments with service definitions to
	// populate service.instances. During initial sync, events from
	// different rooms arrive in arbitrary order (fleet room with
	// service definitions vs config rooms with assignments), so the
	// incremental instance tracking in processMachineConfigEvent may
	// miss services that haven't been defined yet. This full rebuild
	// ensures consistency after all rooms are processed.
	fc.rebuildServiceInstances()

	// Second pass: process ticket events that need context for network
	// calls. Done after the first pass so relay links are indexed
	// before ticket events reference them for holder resolution.
	for roomID, room := range response.Rooms.Join {
		fc.processRoomStateWithContext(ctx, roomID, room.State.Events, room.Timeline.Events)
	}
	for _, roomID := range acceptedRooms {
		events, err := fc.session.GetRoomState(ctx, roomID)
		if err != nil {
			continue
		}
		fc.processRoomStateWithContext(ctx, roomID, events, nil)
	}

	fc.logger.Info("fleet model built",
		"machines", len(fc.machines),
		"services", len(fc.services),
		"definitions", len(fc.definitions),
		"config_rooms", len(fc.configRooms),
		"ops_rooms", len(fc.opsRooms),
		"reservations", len(fc.reservations),
	)

	// Process presence state from the initial sync. The homeserver
	// may include current presence for users sharing rooms. Processed
	// after building the fleet model so machine entries exist for
	// presence event matching.
	fc.processPresenceEvents(ctx, response.Presence.Events)

	// Emit structured notifications for everything discovered during
	// initial sync so observers can synchronize with the fleet
	// controller's model state without polling its API.
	fc.emitDiscoveryNotifications(ctx, nil, nil)

	// Publish the fleet controller's service binding to all config
	// rooms. On startup, all rooms are treated as new (nil previous
	// map), so every config room gets its binding refreshed. This
	// ensures consistency even if a binding was lost.
	fc.publishServiceBindings(ctx, nil)

	return sinceToken, nil
}

// processRoomState examines state and timeline events from a room to
// determine what kind of room it is (fleet, machine, config, or ops)
// and populates the in-memory model accordingly.
//
// Room classification is based on event types present:
//   - Fleet room events (FleetServiceContent, MachineDefinitionContent,
//     FleetConfigContent, HALeaseContent) indicate #bureau/fleet.
//   - Machine room events (MachineInfo, MachineStatus) indicate
//     #bureau/machine.
//   - Config room events (MachineConfig) indicate a per-machine
//     config room.
//   - Ops room events (Ticket with resource_request, RelayLink,
//     RelayPolicy) indicate a machine ops room.
func (fc *FleetController) processRoomState(roomID ref.RoomID, stateEvents, timelineEvents []messaging.Event) {
	for _, event := range stateEvents {
		if event.StateKey != nil {
			fc.processStateEvent(roomID, event)
		}
	}
	for _, event := range timelineEvents {
		if event.StateKey != nil {
			fc.processStateEvent(roomID, event)
		}
	}
}

// processRoomStateWithContext is the second pass over room state
// events, handling event types that need a context for network calls
// (ticket processing may grant reservations). Must be called after
// processRoomState so that relay links are indexed before ticket
// events reference them.
//
// Caller must hold fc.mu.
func (fc *FleetController) processRoomStateWithContext(ctx context.Context, roomID ref.RoomID, stateEvents, timelineEvents []messaging.Event) {
	for _, event := range stateEvents {
		if event.StateKey != nil {
			fc.processStateEventWithContext(ctx, roomID, event)
		}
	}
	for _, event := range timelineEvents {
		if event.StateKey != nil {
			fc.processStateEventWithContext(ctx, roomID, event)
		}
	}
}

// processStateEvent routes a single state event to the appropriate
// model update based on its type.
func (fc *FleetController) processStateEvent(roomID ref.RoomID, event messaging.Event) {
	switch event.Type {
	case schema.EventTypeFleetService:
		fc.processFleetServiceEvent(event)
	case schema.EventTypeMachineDefinition:
		fc.processMachineDefinitionEvent(event)
	case schema.EventTypeFleetConfig:
		fc.processFleetConfigEvent(event)

	// Self-identifying events: the sender IS the entity in the state_key.
	// Validate that the sender's server matches the state_key's server
	// to prevent federation spoofing (a malicious federated server
	// publishing events for entities on our server).
	case schema.EventTypeHALease:
		if !fc.verifySenderMatchesStateKey(event) {
			return
		}
		fc.processHALeaseEvent(event)
	case schema.EventTypeMachineInfo:
		if !fc.verifySenderMatchesStateKey(event) {
			return
		}
		fc.processMachineInfoEvent(event)
	case schema.EventTypeMachineStatus:
		if !fc.verifySenderMatchesStateKey(event) {
			return
		}
		fc.processMachineStatusEvent(event)
	case schema.EventTypeDrainStatus:
		if !fc.verifySenderMatchesStateKey(event) {
			return
		}
		fc.processDrainStatusEvent(roomID, event)

	// Third-party events: the sender is NOT the entity in the state_key.
	// These are published by the FC, CLI, or admin on behalf of the entity.
	// Security relies on room power levels, not sender-server matching.
	case schema.EventTypeMachineConfig:
		fc.processMachineConfigEvent(roomID, event)
	case schema.EventTypeRelayLink:
		fc.processRelayLinkEvent(roomID, event)
	}
}

// verifySenderMatchesStateKey validates that the event sender's
// homeserver matches the server encoded in the state_key. For
// self-identifying events (where the entity publishes about itself),
// this prevents federation spoofing: a malicious federated server
// cannot publish machine_status, machine_info, ha_lease, or
// drain_status events with state_keys that claim to be entities on
// our server.
//
// Returns true if the servers match or the state_key cannot be parsed
// (unparseable state_keys are rejected later by the handler). Returns
// false and logs a warning if the servers differ.
func (fc *FleetController) verifySenderMatchesStateKey(event messaging.Event) bool {
	if event.StateKey == nil || event.Sender.IsZero() {
		return true
	}
	stateKeyUserID, err := ref.ParseUserIDFromStateKey(*event.StateKey)
	if err != nil {
		// Unparseable state_key — let the handler reject it with a
		// more specific error message.
		return true
	}
	if event.Sender.Server() != stateKeyUserID.Server() {
		fc.logger.Warn("rejecting cross-server entity event: sender server does not match state_key server",
			"event_type", event.Type,
			"sender", event.Sender,
			"sender_server", event.Sender.Server(),
			"state_key", *event.StateKey,
			"state_key_server", stateKeyUserID.Server(),
		)
		return false
	}
	return true
}

// processStateEventWithContext routes state events that need a
// context for network calls. Called separately from processStateEvent
// because ticket processing may write Matrix events (grant, close).
//
// Caller must hold fc.mu.
func (fc *FleetController) processStateEventWithContext(ctx context.Context, roomID ref.RoomID, event messaging.Event) {
	switch event.Type {
	case schema.EventTypeTicket:
		fc.processOpsRoomTicketEvent(ctx, roomID, event)
	}
}

// processFleetServiceEvent parses a fleet service definition and
// updates the services map.
func (fc *FleetController) processFleetServiceEvent(event messaging.Event) {
	stateKeyRaw := *event.StateKey
	stateKeyUserID, err := ref.ParseUserIDFromStateKey(stateKeyRaw)
	if err != nil {
		fc.logger.Warn("ignoring fleet service event with unparseable state_key",
			"state_key", stateKeyRaw,
			"error", err,
		)
		return
	}

	if len(event.Content) == 0 {
		delete(fc.services, stateKeyUserID)
		return
	}

	content, err := parseEventContent[fleet.FleetServiceContent](event)
	if err != nil {
		fc.logger.Warn("failed to parse fleet service event",
			"state_key", stateKeyRaw,
			"error", err,
		)
		return
	}

	state, exists := fc.services[stateKeyUserID]
	if !exists {
		state = &fleetServiceState{
			instances: make(map[ref.UserID]*schema.PrincipalAssignment),
		}
		fc.services[stateKeyUserID] = state
	}
	state.definition = content
}

// processMachineDefinitionEvent parses a machine definition and
// updates the definitions map.
func (fc *FleetController) processMachineDefinitionEvent(event messaging.Event) {
	stateKey := *event.StateKey
	if len(event.Content) == 0 {
		delete(fc.definitions, stateKey)
		return
	}

	content, err := parseEventContent[fleet.MachineDefinitionContent](event)
	if err != nil {
		fc.logger.Warn("failed to parse machine definition event",
			"state_key", stateKey,
			"error", err,
		)
		return
	}

	fc.definitions[stateKey] = content
}

// processFleetConfigEvent parses a fleet config and updates the
// config map.
func (fc *FleetController) processFleetConfigEvent(event messaging.Event) {
	stateKeyRaw := *event.StateKey
	stateKeyUserID, err := ref.ParseUserIDFromStateKey(stateKeyRaw)
	if err != nil {
		fc.logger.Warn("ignoring fleet config event with unparseable state_key",
			"state_key", stateKeyRaw,
			"error", err,
		)
		return
	}

	if len(event.Content) == 0 {
		delete(fc.config, stateKeyUserID)
		return
	}

	content, err := parseEventContent[fleet.FleetConfigContent](event)
	if err != nil {
		fc.logger.Warn("failed to parse fleet config event",
			"state_key", stateKeyRaw,
			"error", err,
		)
		return
	}

	fc.config[stateKeyUserID] = content
}

// processHALeaseEvent parses an HA lease and updates the leases map.
func (fc *FleetController) processHALeaseEvent(event messaging.Event) {
	stateKeyRaw := *event.StateKey
	stateKeyUserID, err := ref.ParseUserIDFromStateKey(stateKeyRaw)
	if err != nil {
		fc.logger.Warn("ignoring HA lease event with unparseable state_key",
			"state_key", stateKeyRaw,
			"error", err,
		)
		return
	}

	if len(event.Content) == 0 {
		delete(fc.leases, stateKeyUserID)
		return
	}

	content, err := parseEventContent[fleet.HALeaseContent](event)
	if err != nil {
		fc.logger.Warn("failed to parse HA lease event",
			"state_key", stateKeyRaw,
			"error", err,
		)
		return
	}

	fc.leases[stateKeyUserID] = content
}

// processMachineInfoEvent parses a machine info event and updates the
// machines map.
func (fc *FleetController) processMachineInfoEvent(event messaging.Event) {
	stateKeyRaw := *event.StateKey
	if len(event.Content) == 0 {
		return
	}

	machineUserID, ok := fc.machineInFleet(stateKeyRaw)
	if !ok {
		return
	}

	content, err := parseEventContent[schema.MachineInfo](event)
	if err != nil {
		fc.logger.Warn("failed to parse machine info event",
			"state_key", stateKeyRaw,
			"error", err,
		)
		return
	}

	machine, exists := fc.machines[machineUserID]
	if !exists {
		machine = &machineState{
			assignments: make(map[ref.UserID]*schema.PrincipalAssignment),
		}
		fc.machines[machineUserID] = machine
	}
	machine.info = content
}

// machineInFleet validates that a state_key parses as a machine user
// ID belonging to this fleet controller's fleet. Returns the parsed
// UserID and true on success. Returns (zero, false) with a warning log
// if the state_key doesn't parse as a machine user ID or belongs to a
// different fleet. This prevents cross-fleet contamination when a fleet
// controller is accidentally invited to a config room owned by another
// fleet.
func (fc *FleetController) machineInFleet(stateKey string) (ref.UserID, bool) {
	machine, err := ref.ParseMachineStateKey(stateKey)
	if err != nil {
		fc.logger.Warn("ignoring machine event with unparseable state_key",
			"state_key", stateKey,
			"error", err,
		)
		return ref.UserID{}, false
	}
	if machine.Fleet().Localpart() != fc.fleet.Localpart() {
		fc.logger.Warn("ignoring machine event from foreign fleet",
			"state_key", stateKey,
			"expected_fleet", fc.fleet.Localpart(),
			"actual_fleet", machine.Fleet().Localpart(),
		)
		return ref.UserID{}, false
	}
	return machine.UserID(), true
}

// processMachineStatusEvent parses a machine status event and updates
// the machines map. Each status event is treated as a heartbeat: the
// machine's lastHeartbeat timestamp is set to the current clock time,
// and its healthState is set to "online". If the machine was previously
// offline or suspect, recovery is logged.
func (fc *FleetController) processMachineStatusEvent(event messaging.Event) {
	stateKeyRaw := *event.StateKey
	if len(event.Content) == 0 {
		return
	}
	machineUserID, ok := fc.machineInFleet(stateKeyRaw)
	if !ok {
		return
	}

	content, err := parseEventContent[schema.MachineStatus](event)
	if err != nil {
		fc.logger.Warn("failed to parse machine status event",
			"state_key", stateKeyRaw,
			"error", err,
		)
		return
	}

	machine, exists := fc.machines[machineUserID]
	if !exists {
		machine = &machineState{
			assignments: make(map[ref.UserID]*schema.PrincipalAssignment),
		}
		fc.machines[machineUserID] = machine
	}

	previousHealthState := machine.healthState

	machine.status = content
	machine.lastHeartbeat = fc.clock.Now()
	machine.healthState = healthOnline

	if previousHealthState == healthOffline || previousHealthState == healthSuspect {
		fc.logger.Info("machine recovered",
			"machine", machineUserID,
			"previous_state", previousHealthState,
		)
	}
}

// processPresenceEvents updates machine presence state from m.presence
// events received via /sync. The sender (a full Matrix user ID) is
// matched against tracked machine entries. Presence provides a
// fast-path liveness signal: when the homeserver reports a daemon
// offline (TCP connection dropped), the fleet controller can
// immediately suspect the machine rather than waiting for heartbeat
// staleness.
//
// When a presence state transition is detected, a FleetPresenceChanged
// notification is published to the fleet room for operator visibility.
//
// Caller must hold fc.mu.
func (fc *FleetController) processPresenceEvents(ctx context.Context, events []messaging.PresenceEvent) {
	for _, event := range events {
		if event.Type != "m.presence" {
			continue
		}
		if event.Sender.IsZero() {
			continue
		}

		machine, exists := fc.machines[event.Sender]
		if !exists {
			continue
		}

		previousPresence := machine.presenceState
		machine.presenceState = event.Content.Presence

		if previousPresence != event.Content.Presence {
			fc.logger.Info("machine presence changed",
				"machine", event.Sender,
				"previous", previousPresence,
				"current", event.Content.Presence,
			)
			fc.sendFleetNotification(ctx,
				fleet.NewFleetPresenceChangedMessage(event.Sender.String(), previousPresence, event.Content.Presence))
		}
	}
}

// processMachineConfigEvent parses a machine config event from a
// per-machine config room and extracts fleet-managed
// PrincipalAssignment entries.
func (fc *FleetController) processMachineConfigEvent(roomID ref.RoomID, event messaging.Event) {
	stateKeyRaw := *event.StateKey
	if len(event.Content) == 0 {
		return
	}
	machineUserID, ok := fc.machineInFleet(stateKeyRaw)
	if !ok {
		return
	}

	content, err := parseEventContent[schema.MachineConfig](event)
	if err != nil {
		fc.logger.Warn("failed to parse machine config event",
			"room_id", roomID,
			"state_key", stateKeyRaw,
			"error", err,
		)
		return
	}

	// Track the config room mapping.
	fc.configRooms[machineUserID] = roomID

	// Ensure the machine entry exists so we can check pending echoes.
	machine, exists := fc.machines[machineUserID]
	if !exists {
		machine = &machineState{
			assignments: make(map[ref.UserID]*schema.PrincipalAssignment),
		}
		fc.machines[machineUserID] = machine
	}
	machine.configRoomID = roomID

	// If a write is pending for this machine, skip stale /sync
	// events to prevent overwriting optimistic local updates from
	// place() or unplace(). Only the echo of our own write (matching
	// event ID) clears the pending state and applies normally.
	if !machine.pendingEchoEventID.IsZero() {
		if event.EventID == machine.pendingEchoEventID {
			machine.pendingEchoEventID = ref.EventID{}
		} else {
			return
		}
	}

	// Extract fleet-managed assignments from the config event and
	// update the service instance index incrementally.
	oldAssignments := machine.assignments

	fleetAssignments := make(map[ref.UserID]*schema.PrincipalAssignment)
	for index := range content.Principals {
		assignment := &content.Principals[index]
		if isFleetManaged(assignment) {
			fleetAssignments[assignment.Principal.UserID()] = assignment
		}
	}

	// Remove stale service instance entries for assignments that
	// are no longer on this machine.
	for serviceUserID := range oldAssignments {
		if _, stillAssigned := fleetAssignments[serviceUserID]; !stillAssigned {
			if serviceState, exists := fc.services[serviceUserID]; exists {
				delete(serviceState.instances, machineUserID)
			}
		}
	}

	// Add or update service instance entries for current assignments.
	for serviceUserID, assignment := range fleetAssignments {
		if serviceState, exists := fc.services[serviceUserID]; exists {
			serviceState.instances[machineUserID] = assignment
		}
	}

	machine.assignments = fleetAssignments
}

// isFleetManaged returns true if a PrincipalAssignment has the
// "fleet_managed" label set to any non-empty value.
func isFleetManaged(assignment *schema.PrincipalAssignment) bool {
	if assignment.Labels == nil {
		return false
	}
	_, exists := assignment.Labels["fleet_managed"]
	return exists
}

// handleSync processes an incremental /sync response. Called by the
// sync loop for each response.
//
// Invite acceptance runs without the lock (Matrix I/O). Model updates
// and reconciliation run under fc.mu to prevent concurrent access from
// socket handler goroutines.
func (fc *FleetController) handleSync(ctx context.Context, response *messaging.SyncResponse) {
	// Accept invites without holding the lock (Matrix I/O).
	// Accepted rooms don't appear in Rooms.Join until the next /sync
	// batch. Fetch their full state now so they are modeled in this
	// sync cycle — same approach as initialSync.
	var acceptedRoomStates []acceptedRoom
	if len(response.Rooms.Invite) > 0 {
		acceptedRooms := service.AcceptInvites(ctx, fc.session, response.Rooms.Invite, fc.logger)
		if len(acceptedRooms) > 0 {
			fc.logger.Info("accepted room invites", "count", len(acceptedRooms))
		}
		for _, roomID := range acceptedRooms {
			events, err := fc.session.GetRoomState(ctx, roomID)
			if err != nil {
				fc.logger.Error("failed to fetch state for accepted room",
					"room_id", roomID,
					"error", err,
				)
				continue
			}
			acceptedRoomStates = append(acceptedRoomStates, acceptedRoom{
				roomID: roomID,
				events: events,
			})
		}
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()

	// Snapshot config rooms and services before processing so we can
	// detect new discoveries and emit notifications.
	previousConfigRooms := make(map[ref.UserID]bool, len(fc.configRooms))
	for machineUserID := range fc.configRooms {
		previousConfigRooms[machineUserID] = true
	}
	previousServices := make(map[ref.UserID]bool, len(fc.services))
	for serviceUserID := range fc.services {
		previousServices[serviceUserID] = true
	}

	// Clean up state for rooms the fleet controller has been removed
	// from. Process leaves before joins so that a leave+rejoin in the
	// same sync batch processes cleanly.
	for roomID := range response.Rooms.Leave {
		fc.processLeave(roomID)
	}

	// Process state changes in joined rooms (first pass: model updates).
	for roomID, room := range response.Rooms.Join {
		fc.processRoomSync(roomID, room)
	}

	// Process state for rooms accepted in this sync cycle.
	for index := range acceptedRoomStates {
		fc.processRoomState(acceptedRoomStates[index].roomID, acceptedRoomStates[index].events, nil)
	}

	// Cross-reference machine assignments with service definitions.
	// Events from different rooms arrive in arbitrary order within a
	// sync response (fleet room with service definitions vs config
	// rooms with assignments). The incremental instance tracking in
	// processMachineConfigEvent may miss services whose
	// FleetServiceContent arrived in the same batch or a later batch
	// than the MachineConfig. This is the same issue initialSync
	// solves at line 186 — it applies equally to incremental syncs.
	fc.rebuildServiceInstances()

	// Second pass: process ticket events that need context for
	// network calls (reservation grants, relay ticket status updates).
	// Done after the first pass so relay links are indexed before
	// ticket events reference them for holder resolution.
	for roomID, room := range response.Rooms.Join {
		stateEvents := collectStateEvents(room)
		for _, event := range stateEvents {
			fc.processStateEventWithContext(ctx, roomID, event)
		}
	}
	for index := range acceptedRoomStates {
		for _, event := range acceptedRoomStates[index].events {
			if event.StateKey != nil {
				fc.processStateEventWithContext(ctx, acceptedRoomStates[index].roomID, event)
			}
		}
	}

	// Update machine presence state from m.presence events. Presence
	// is processed after room events (which may create machine entries)
	// and before health evaluation (which uses presenceState).
	fc.processPresenceEvents(ctx, response.Presence.Events)

	// Emit notifications for newly discovered config rooms and services.
	fc.emitDiscoveryNotifications(ctx, previousConfigRooms, previousServices)

	// Publish the fleet controller's service binding to newly
	// discovered config rooms so agents with required_services:
	// ["fleet"] can discover the fleet controller socket.
	fc.publishServiceBindings(ctx, previousConfigRooms)

	// Reconcile after processing all events: compare desired replica
	// counts with actual instance counts and place/unplace as needed.
	fc.reconcile(ctx)

	// Evaluate machine health after reconciliation so that newly
	// stale machines trigger failover in the same sync cycle.
	fc.checkMachineHealth(ctx)

	// Advance reservation queues (grant next pending, check
	// preemption), check drain completion (grant when all services
	// acknowledge or timeout), and enforce duration limits on active
	// reservations.
	fc.advanceReservationQueues(ctx)
	fc.checkDrainCompletion(ctx)
	fc.checkReservationExpiry(ctx)
}

// processLeave handles a room leave event. If the room was a known
// config room or ops room, the associated state is cleaned up. If it
// was the fleet or machine room, the event is logged as a warning.
func (fc *FleetController) processLeave(roomID ref.RoomID) {
	// Check if this is an ops room and clean up reservation state.
	if machineUserID, isOpsRoom := fc.opsRoomMachines[roomID]; isOpsRoom {
		fc.logger.Info("ops room left, clearing reservation state",
			"room_id", roomID,
			"machine", machineUserID,
		)
		delete(fc.opsRooms, machineUserID)
		delete(fc.opsRoomMachines, roomID)
		delete(fc.reservations, machineUserID)

		// Clean up relay links for this room.
		for key := range fc.relayLinks {
			if key.roomID == roomID {
				delete(fc.relayLinks, key)
			}
		}
		return
	}

	// Check if this is a config room and clean up the associated
	// machine state.
	for machineUserID, configRoom := range fc.configRooms {
		if configRoom != roomID {
			continue
		}
		fc.logger.Info("config room left, removing machine assignments",
			"room_id", roomID,
			"machine", machineUserID,
		)
		if machine, exists := fc.machines[machineUserID]; exists {
			for serviceUserID := range machine.assignments {
				if serviceState, exists := fc.services[serviceUserID]; exists {
					delete(serviceState.instances, machineUserID)
				}
			}
			machine.assignments = make(map[ref.UserID]*schema.PrincipalAssignment)
			machine.configRoomID = ref.RoomID{}
		}
		delete(fc.configRooms, machineUserID)
		return
	}

	// Check if this is the machine room.
	if roomID == fc.machineRoomID {
		fc.logger.Warn("left machine room", "room_id", roomID)
		// Clean up all machine state since we can no longer
		// receive heartbeats. Replace with a fresh map rather
		// than deleting during iteration. Also clear all service
		// instances since they reference machines.
		fc.machines = make(map[ref.UserID]*machineState)
		for _, serviceState := range fc.services {
			serviceState.instances = make(map[ref.UserID]*schema.PrincipalAssignment)
		}
		return
	}

	// Check if this is the fleet room.
	if roomID == fc.fleetRoomID {
		fc.logger.Warn("left fleet room", "room_id", roomID)
		return
	}
}

// processRoomSync handles state changes in a single room during
// incremental sync.
func (fc *FleetController) processRoomSync(roomID ref.RoomID, room messaging.JoinedRoom) {
	stateEvents := collectStateEvents(room)
	if len(stateEvents) == 0 {
		return
	}

	for _, event := range stateEvents {
		fc.processStateEvent(roomID, event)
	}
}

// collectStateEvents extracts state events from a joined room's sync
// response (both the state section and state events in the timeline).
func collectStateEvents(room messaging.JoinedRoom) []messaging.Event {
	var events []messaging.Event
	events = append(events, room.State.Events...)
	for _, event := range room.Timeline.Events {
		if event.StateKey != nil {
			events = append(events, event)
		}
	}
	return events
}

// rebuildServiceInstances does a full cross-reference of machine
// assignments against service definitions to populate
// fleetServiceState.instances. Called after initial sync where events
// from different rooms arrive in arbitrary order (fleet room service
// definitions vs config room assignments), so incremental tracking
// in processMachineConfigEvent may have missed services that hadn't
// been defined yet.
func (fc *FleetController) rebuildServiceInstances() {
	// Clear all existing instance tracking.
	for _, serviceState := range fc.services {
		serviceState.instances = make(map[ref.UserID]*schema.PrincipalAssignment)
	}

	// Rebuild from machine assignments.
	for machineUserID, machine := range fc.machines {
		for serviceUserID, assignment := range machine.assignments {
			if serviceState, exists := fc.services[serviceUserID]; exists {
				serviceState.instances[machineUserID] = assignment
			}
		}
	}
}

// acceptedRoom pairs a room ID with its full state, fetched after
// accepting an invite. Used by handleSync to process newly joined rooms
// in the same sync cycle as the invite acceptance.
type acceptedRoom struct {
	roomID ref.RoomID
	events []messaging.Event
}

// parseEventContent parses a Matrix event's Content map into a typed
// struct via JSON round-trip. This matches how the Matrix homeserver
// delivers events: the SDK gives us map[string]any, and we need typed
// Go structs.
func parseEventContent[T any](event messaging.Event) (*T, error) {
	contentJSON, err := json.Marshal(event.Content)
	if err != nil {
		return nil, err
	}
	var content T
	if err := json.Unmarshal(contentJSON, &content); err != nil {
		return nil, err
	}
	return &content, nil
}

// emitDiscoveryNotifications sends structured notification messages to
// the fleet room for config rooms and services that are new since the
// previous snapshot. Pass nil maps for initial sync (everything is new)
// or snapshots of the previous state for incremental sync.
func (fc *FleetController) emitDiscoveryNotifications(ctx context.Context, previousConfigRooms, previousServices map[ref.UserID]bool) {
	for machineUserID, roomID := range fc.configRooms {
		if previousConfigRooms != nil && previousConfigRooms[machineUserID] {
			continue
		}
		fc.sendFleetNotification(ctx,
			fleet.NewFleetConfigRoomDiscoveredMessage(machineUserID.String(), roomID.String()))
	}

	for serviceUserID := range fc.services {
		if previousServices != nil && previousServices[serviceUserID] {
			continue
		}
		fc.sendFleetNotification(ctx,
			fleet.NewFleetServiceDiscoveredMessage(serviceUserID.String()))
	}
}

// publishServiceBindings publishes the fleet controller's
// m.bureau.service_binding state event to newly discovered config rooms.
// This enables agents with required_services: ["fleet"] to discover
// the fleet controller socket via daemon service directory resolution.
//
// Pass nil previousConfigRooms for initial sync (publish to all config
// rooms) or a snapshot of the previous state for incremental sync (only
// new rooms).
//
// Caller must hold fc.mu.
func (fc *FleetController) publishServiceBindings(ctx context.Context, previousConfigRooms map[ref.UserID]bool) {
	binding := schema.ServiceBindingContent{Principal: fc.serviceEntity}

	for machineUserID, roomID := range fc.configRooms {
		if previousConfigRooms != nil && previousConfigRooms[machineUserID] {
			continue
		}

		_, err := fc.configStore.SendStateEvent(ctx, roomID, schema.EventTypeServiceBinding, "fleet", binding)
		if err != nil {
			fc.logger.Warn("failed to publish fleet service binding",
				"machine", machineUserID,
				"config_room", roomID,
				"error", err,
			)
			continue
		}

		fc.logger.Info("published fleet service binding",
			"machine", machineUserID,
			"config_room", roomID,
		)
		fc.sendFleetNotification(ctx,
			fleet.NewFleetServiceBindingPublishedMessage(machineUserID.String(), roomID.String()))
	}
}

// sendFleetNotification posts a structured notification message to the
// fleet room. These are m.room.message events with custom msgtypes,
// following the same pattern as daemon notifications.
func (fc *FleetController) sendFleetNotification(ctx context.Context, content any) {
	if fc.session == nil {
		return
	}
	eventID, err := fc.session.SendEvent(ctx, fc.fleetRoomID, schema.MatrixEventTypeMessage, content)
	if err != nil {
		fc.logger.Error("failed to send fleet notification",
			"error", err,
		)
	} else {
		fc.logger.Info("fleet notification sent",
			"event_id", eventID,
			"fleet_room", fc.fleetRoomID,
		)
	}
}
