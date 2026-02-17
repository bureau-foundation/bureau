// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
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
	stateEventTypes := []string{
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
	}

	// Timeline includes the same state event types (state events can
	// appear as timeline events with a non-nil state_key during
	// incremental sync).
	timelineEventTypes := make([]string, len(stateEventTypes))
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
			"types": emptyTypes,
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
	// this machine. Keyed by the assignment's principal localpart.
	assignments map[string]*schema.PrincipalAssignment

	// configRoomID is the machine's config room ID, used for writing
	// PrincipalAssignment events.
	configRoomID string

	// pendingEchoEventID, when non-empty, holds the Matrix event ID
	// returned by the most recent writeMachineConfig call for this
	// machine. While set, processMachineConfigEvent skips /sync
	// events that are not the echo of that write, preventing stale
	// sync data from overwriting optimistic local updates made by
	// place() or unplace(). Cleared when the echo arrives.
	pendingEchoEventID string

	// lastHeartbeat is the time the fleet controller last received a
	// MachineStatus event for this machine. Used for staleness
	// detection in checkMachineHealth.
	lastHeartbeat time.Time

	// healthState tracks the machine's current health: "online",
	// "suspect", or "offline".
	healthState string
}

// fleetServiceState tracks a fleet service definition and its current
// instances across the fleet.
type fleetServiceState struct {
	definition *schema.FleetServiceContent

	// instances maps machine localparts to the PrincipalAssignment
	// the fleet controller wrote for this service on that machine.
	instances map[string]*schema.PrincipalAssignment
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

	fc.logger.Info("fleet model built",
		"machines", len(fc.machines),
		"services", len(fc.services),
		"definitions", len(fc.definitions),
		"config_rooms", len(fc.configRooms),
	)

	// Emit structured notifications for everything discovered during
	// initial sync so observers can synchronize with the fleet
	// controller's model state without polling its API.
	fc.emitDiscoveryNotifications(ctx, nil, nil)

	return sinceToken, nil
}

// processRoomState examines state and timeline events from a room to
// determine what kind of room it is (fleet, machine, or config) and
// populates the in-memory model accordingly.
//
// Room classification is based on event types present:
//   - Fleet room events (FleetServiceContent, MachineDefinitionContent,
//     FleetConfigContent, HALeaseContent) indicate #bureau/fleet.
//   - Machine room events (MachineInfo, MachineStatus) indicate
//     #bureau/machine.
//   - Config room events (MachineConfig) indicate a per-machine
//     config room.
func (fc *FleetController) processRoomState(roomID string, stateEvents, timelineEvents []messaging.Event) {
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

// processStateEvent routes a single state event to the appropriate
// model update based on its type.
func (fc *FleetController) processStateEvent(roomID string, event messaging.Event) {
	switch event.Type {
	case schema.EventTypeFleetService:
		fc.processFleetServiceEvent(event)
	case schema.EventTypeMachineDefinition:
		fc.processMachineDefinitionEvent(event)
	case schema.EventTypeFleetConfig:
		fc.processFleetConfigEvent(event)
	case schema.EventTypeHALease:
		fc.processHALeaseEvent(event)
	case schema.EventTypeMachineInfo:
		fc.processMachineInfoEvent(event)
	case schema.EventTypeMachineStatus:
		fc.processMachineStatusEvent(event)
	case schema.EventTypeMachineConfig:
		fc.processMachineConfigEvent(roomID, event)
	}
}

// processFleetServiceEvent parses a fleet service definition and
// updates the services map.
func (fc *FleetController) processFleetServiceEvent(event messaging.Event) {
	stateKey := *event.StateKey
	if len(event.Content) == 0 {
		delete(fc.services, stateKey)
		return
	}

	content, err := parseEventContent[schema.FleetServiceContent](event)
	if err != nil {
		fc.logger.Warn("failed to parse fleet service event",
			"state_key", stateKey,
			"error", err,
		)
		return
	}

	state, exists := fc.services[stateKey]
	if !exists {
		state = &fleetServiceState{
			instances: make(map[string]*schema.PrincipalAssignment),
		}
		fc.services[stateKey] = state
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

	content, err := parseEventContent[schema.MachineDefinitionContent](event)
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
	stateKey := *event.StateKey
	if len(event.Content) == 0 {
		delete(fc.config, stateKey)
		return
	}

	content, err := parseEventContent[schema.FleetConfigContent](event)
	if err != nil {
		fc.logger.Warn("failed to parse fleet config event",
			"state_key", stateKey,
			"error", err,
		)
		return
	}

	fc.config[stateKey] = content
}

// processHALeaseEvent parses an HA lease and updates the leases map.
func (fc *FleetController) processHALeaseEvent(event messaging.Event) {
	stateKey := *event.StateKey
	if len(event.Content) == 0 {
		delete(fc.leases, stateKey)
		return
	}

	content, err := parseEventContent[schema.HALeaseContent](event)
	if err != nil {
		fc.logger.Warn("failed to parse HA lease event",
			"state_key", stateKey,
			"error", err,
		)
		return
	}

	fc.leases[stateKey] = content
}

// processMachineInfoEvent parses a machine info event and updates the
// machines map.
func (fc *FleetController) processMachineInfoEvent(event messaging.Event) {
	stateKey := *event.StateKey
	if len(event.Content) == 0 {
		return
	}

	content, err := parseEventContent[schema.MachineInfo](event)
	if err != nil {
		fc.logger.Warn("failed to parse machine info event",
			"state_key", stateKey,
			"error", err,
		)
		return
	}

	machine, exists := fc.machines[stateKey]
	if !exists {
		machine = &machineState{
			assignments: make(map[string]*schema.PrincipalAssignment),
		}
		fc.machines[stateKey] = machine
	}
	machine.info = content
}

// processMachineStatusEvent parses a machine status event and updates
// the machines map. Each status event is treated as a heartbeat: the
// machine's lastHeartbeat timestamp is set to the current clock time,
// and its healthState is set to "online". If the machine was previously
// offline or suspect, recovery is logged.
func (fc *FleetController) processMachineStatusEvent(event messaging.Event) {
	stateKey := *event.StateKey
	if len(event.Content) == 0 {
		return
	}

	content, err := parseEventContent[schema.MachineStatus](event)
	if err != nil {
		fc.logger.Warn("failed to parse machine status event",
			"state_key", stateKey,
			"error", err,
		)
		return
	}

	machine, exists := fc.machines[stateKey]
	if !exists {
		machine = &machineState{
			assignments: make(map[string]*schema.PrincipalAssignment),
		}
		fc.machines[stateKey] = machine
	}

	previousHealthState := machine.healthState

	machine.status = content
	machine.lastHeartbeat = fc.clock.Now()
	machine.healthState = healthOnline

	if previousHealthState == healthOffline || previousHealthState == healthSuspect {
		fc.logger.Info("machine recovered",
			"machine", stateKey,
			"previous_state", previousHealthState,
		)
	}
}

// processMachineConfigEvent parses a machine config event from a
// per-machine config room and extracts fleet-managed
// PrincipalAssignment entries.
func (fc *FleetController) processMachineConfigEvent(roomID string, event messaging.Event) {
	stateKey := *event.StateKey
	if len(event.Content) == 0 {
		return
	}

	content, err := parseEventContent[schema.MachineConfig](event)
	if err != nil {
		fc.logger.Warn("failed to parse machine config event",
			"room_id", roomID,
			"state_key", stateKey,
			"error", err,
		)
		return
	}

	// Track the config room mapping.
	fc.configRooms[stateKey] = roomID

	// Ensure the machine entry exists so we can check pending echoes.
	machine, exists := fc.machines[stateKey]
	if !exists {
		machine = &machineState{
			assignments: make(map[string]*schema.PrincipalAssignment),
		}
		fc.machines[stateKey] = machine
	}
	machine.configRoomID = roomID

	// If a write is pending for this machine, skip stale /sync
	// events to prevent overwriting optimistic local updates from
	// place() or unplace(). Only the echo of our own write (matching
	// event ID) clears the pending state and applies normally.
	if machine.pendingEchoEventID != "" {
		if event.EventID == machine.pendingEchoEventID {
			machine.pendingEchoEventID = ""
		} else {
			return
		}
	}

	// Extract fleet-managed assignments from the config event and
	// update the service instance index incrementally.
	machineLocalpart := stateKey
	oldAssignments := machine.assignments

	fleetAssignments := make(map[string]*schema.PrincipalAssignment)
	for index := range content.Principals {
		assignment := &content.Principals[index]
		if isFleetManaged(assignment) {
			fleetAssignments[assignment.Localpart] = assignment
		}
	}

	// Remove stale service instance entries for assignments that
	// are no longer on this machine.
	for localpart := range oldAssignments {
		if _, stillAssigned := fleetAssignments[localpart]; !stillAssigned {
			if serviceState, exists := fc.services[localpart]; exists {
				delete(serviceState.instances, machineLocalpart)
			}
		}
	}

	// Add or update service instance entries for current assignments.
	for localpart, assignment := range fleetAssignments {
		if serviceState, exists := fc.services[localpart]; exists {
			serviceState.instances[machineLocalpart] = assignment
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
	previousConfigRooms := make(map[string]bool, len(fc.configRooms))
	for machineLocalpart := range fc.configRooms {
		previousConfigRooms[machineLocalpart] = true
	}
	previousServices := make(map[string]bool, len(fc.services))
	for serviceLocalpart := range fc.services {
		previousServices[serviceLocalpart] = true
	}

	// Clean up state for rooms the fleet controller has been removed
	// from. Process leaves before joins so that a leave+rejoin in the
	// same sync batch processes cleanly.
	for roomID := range response.Rooms.Leave {
		fc.processLeave(roomID)
	}

	// Process state changes in joined rooms.
	for roomID, room := range response.Rooms.Join {
		fc.processRoomSync(roomID, room)
	}

	// Process state for rooms accepted in this sync cycle.
	for _, accepted := range acceptedRoomStates {
		fc.processRoomState(accepted.roomID, accepted.events, nil)
	}

	// Emit notifications for newly discovered config rooms and services.
	fc.emitDiscoveryNotifications(ctx, previousConfigRooms, previousServices)

	// Reconcile after processing all events: compare desired replica
	// counts with actual instance counts and place/unplace as needed.
	fc.reconcile(ctx)

	// Evaluate machine health after reconciliation so that newly
	// stale machines trigger failover in the same sync cycle.
	fc.checkMachineHealth(ctx)
}

// processLeave handles a room leave event. If the room was a known
// config room, the associated machine state is cleaned up. If it was
// the fleet or machine room, the event is logged as a warning.
func (fc *FleetController) processLeave(roomID string) {
	// Check if this is a config room and clean up the associated
	// machine state.
	for machineLocalpart, configRoom := range fc.configRooms {
		if configRoom != roomID {
			continue
		}
		fc.logger.Info("config room left, removing machine assignments",
			"room_id", roomID,
			"machine", machineLocalpart,
		)
		if machine, exists := fc.machines[machineLocalpart]; exists {
			for localpart := range machine.assignments {
				if serviceState, exists := fc.services[localpart]; exists {
					delete(serviceState.instances, machineLocalpart)
				}
			}
			machine.assignments = make(map[string]*schema.PrincipalAssignment)
			machine.configRoomID = ""
		}
		delete(fc.configRooms, machineLocalpart)
		return
	}

	// Check if this is the machine room.
	if roomID == fc.machineRoomID {
		fc.logger.Warn("left machine room", "room_id", roomID)
		// Clean up all machine state since we can no longer
		// receive heartbeats. Replace with a fresh map rather
		// than deleting during iteration. Also clear all service
		// instances since they reference machines.
		fc.machines = make(map[string]*machineState)
		for _, serviceState := range fc.services {
			serviceState.instances = make(map[string]*schema.PrincipalAssignment)
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
func (fc *FleetController) processRoomSync(roomID string, room messaging.JoinedRoom) {
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
		serviceState.instances = make(map[string]*schema.PrincipalAssignment)
	}

	// Rebuild from machine assignments.
	for machineLocalpart, machine := range fc.machines {
		for localpart, assignment := range machine.assignments {
			if serviceState, exists := fc.services[localpart]; exists {
				serviceState.instances[machineLocalpart] = assignment
			}
		}
	}
}

// acceptedRoom pairs a room ID with its full state, fetched after
// accepting an invite. Used by handleSync to process newly joined rooms
// in the same sync cycle as the invite acceptance.
type acceptedRoom struct {
	roomID string
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
func (fc *FleetController) emitDiscoveryNotifications(ctx context.Context, previousConfigRooms, previousServices map[string]bool) {
	for machineLocalpart, roomID := range fc.configRooms {
		if previousConfigRooms != nil && previousConfigRooms[machineLocalpart] {
			continue
		}
		fc.sendFleetNotification(ctx,
			schema.NewFleetConfigRoomDiscoveredMessage(machineLocalpart, roomID))
	}

	for serviceLocalpart := range fc.services {
		if previousServices != nil && previousServices[serviceLocalpart] {
			continue
		}
		fc.sendFleetNotification(ctx,
			schema.NewFleetServiceDiscoveredMessage(serviceLocalpart))
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
